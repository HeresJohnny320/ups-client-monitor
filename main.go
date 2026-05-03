package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/linde12/gowol"
	nut "github.com/robbiet480/go.nut"
)

type WakeTarget struct {
	Name       string `json:"name"`
	MAC        string `json:"mac"`
	UpsName    string `json:"ups_name"`
	WakeLimit  int    `json:"battery_percent"`
	WebhookURL string `json:"webhook_url"`
}

type Config struct {
	Mode               string       `json:"mode"`
	Host               string       `json:"host"`
	Username           string       `json:"username"`
	Password           string       `json:"password"`
	IntervalSeconds    int          `json:"interval_seconds"`
	ClientUpsName      string       `json:"client_ups_name,omitempty"`
	ClientLimit        int          `json:"battery_percent,omitempty"`
	ClientWebhookURL   string       `json:"client_webhook_url,omitempty"`
	WakeTargets        []WakeTarget `json:"wake_targets,omitempty"`
	TestEnabled        bool         `json:"test_enabled"`
	TestType           string       `json:"test_type"`
	TestIntervalMonths int          `json:"test_interval_months"`
	TestWebhookURL     string       `json:"test_webhook_url"`
	LastTestDate       time.Time    `json:"last_test_date"`
}

var lastWakeAttempt = make(map[string]time.Time)

func main() {
	baseDir, _ := os.UserConfigDir()
	appDir := filepath.Join(baseDir, "ups-monitor")
	os.MkdirAll(appDir, 0755)

	logFile, err := os.OpenFile(filepath.Join(appDir, "activity.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err == nil {
		defer logFile.Close()
		multi := io.MultiWriter(os.Stdout, logFile)
		log.SetOutput(multi)
	}

	config := loadConfig()
	log.Printf("--- UPS Monitor (%s Mode) Started ---", strings.ToUpper(config.Mode))

	go startBackgroundTasks(&config)

	reader := bufio.NewReader(os.Stdin)
	fmt.Println("Available Commands: 'test', 'test-long', 'test-webhook', 'status', 'exit'")
	for {
		fmt.Print("ups-cli> ")
		input, _ := reader.ReadString('\n')
		cmd := strings.TrimSpace(strings.ToLower(input))

		switch cmd {
		case "test":
			runSelfTest(&config, "quick")
		case "test-long":
			runSelfTest(&config, "deep")
		case "test-webhook":
			testAllWebhooks(&config)
		case "status":
			log.Println("Manual status check requested.")
		case "exit":
			log.Println("Exiting application...")
			return
		case "":
			continue
		default:
			fmt.Println("Unknown command. Options: test, test-long, test-webhook, status, exit")
		}
	}
}

func sendDiscordWebhook(url, message string) {
	if url == "" {
		return
	}
	payload := map[string]string{"content": message}
	body, _ := json.Marshal(payload)
	httpClient := &http.Client{Timeout: 5 * time.Second}
	resp, err := httpClient.Post(url, "application/json", bytes.NewBuffer(body))
	if err != nil {
		log.Printf("[WEBHOOK ERROR] %v", err)
		return
	}
	defer resp.Body.Close()
}

func testAllWebhooks(config *Config) {
	log.Println("[INFO] Testing configured webhooks...")
	testMsg := "🧪 **Webhook Test**: This is a test message from your UPS Monitor to verify connectivity."

	// Test the general Test Webhook
	if config.TestWebhookURL != "" {
		log.Println("- Testing: Global Test Webhook")
		sendDiscordWebhook(config.TestWebhookURL, testMsg+" (General Test Channel)")
	}

	// Test mode-specific webhooks
	if config.Mode == "client" && config.ClientWebhookURL != "" {
		log.Println("- Testing: Client Shutdown Webhook")
		sendDiscordWebhook(config.ClientWebhookURL, testMsg+" (System Shutdown Channel)")
	} else if config.Mode == "server" {
		for _, target := range config.WakeTargets {
			if target.WebhookURL != "" {
				log.Printf("- Testing: Wake Target Webhook for %s", target.Name)
				sendDiscordWebhook(target.WebhookURL, fmt.Sprintf("%s (Wake Node: %s)", testMsg, target.Name))
			}
		}
	}
	log.Println("[SUCCESS] Webhook test commands sent.")
}

func startBackgroundTasks(config *Config) {
	for {
		client, err := nut.Connect(config.Host)
		if err != nil {
			time.Sleep(10 * time.Second)
			continue
		}

		_, err = client.Authenticate(config.Username, config.Password)
		if err != nil {
			client.Disconnect()
			time.Sleep(10 * time.Second)
			continue
		}

		if config.TestEnabled && !config.LastTestDate.IsZero() {
			nextTestDate := config.LastTestDate.AddDate(0, config.TestIntervalMonths, 0)
			if time.Now().After(nextTestDate) {
				log.Println("[SCHEDULER] Triggering scheduled UPS test...")
				runSelfTest(config, config.TestType)
			}
		}

		upsList, err := client.GetUPSList()
		if err == nil {
			for _, ups := range upsList {
				var chargeStr, status string
				for _, v := range ups.Variables {
					if v.Name == "battery.charge" {
						chargeStr = fmt.Sprintf("%v", v.Value)
					}
					if v.Name == "ups.status" {
						status = fmt.Sprintf("%v", v.Value)
					}
				}

				charge, _ := strconv.Atoi(strings.TrimSpace(chargeStr))
				isOnBattery := strings.Contains(status, "OB") || strings.Contains(status, "LB")

				if config.Mode == "client" && ups.Name == config.ClientUpsName {
					if isOnBattery && charge <= config.ClientLimit {
						msg := fmt.Sprintf("⚠️ **UPS Shutdown**: Battery at %d%%. Shutting down system now.", charge)
						log.Println(msg)
						sendDiscordWebhook(config.ClientWebhookURL, msg)
						shutdownSystem()
						return
					}
				}

				if config.Mode == "server" && !isOnBattery {
					for _, target := range config.WakeTargets {
						if target.UpsName == ups.Name && charge >= target.WakeLimit {
							if time.Since(lastWakeAttempt[target.MAC]) > 15*time.Minute {
								msg := fmt.Sprintf("✅ **Power Restored**: Waking node `%s` (%s). Battery at %d%%.", target.Name, target.MAC, charge)
								log.Println(msg)
								sendDiscordWebhook(target.WebhookURL, msg)
								wakeNode(target.MAC)
								lastWakeAttempt[target.MAC] = time.Now()
							}
						}
					}
				}
			}
		}
		client.Disconnect()
		time.Sleep(time.Duration(config.IntervalSeconds) * time.Second)
	}
}

func runSelfTest(conf *Config, testType string) {
	log.Printf("[TEST] Running %s self-test...", testType)
	client, err := nut.Connect(conf.Host)
	if err != nil {
		errMsg := fmt.Sprintf("❌ **UPS Test Error**: Connection failed: %v", err)
		sendDiscordWebhook(conf.TestWebhookURL, errMsg)
		return
	}
	defer client.Disconnect()
	client.Authenticate(conf.Username, conf.Password)

	cmdName := "test.battery.start"
	if testType == "deep" {
		cmdName = "test.battery.start.deep"
	}

	upsName := conf.ClientUpsName
	if conf.Mode == "server" && len(conf.WakeTargets) > 0 {
		upsName = conf.WakeTargets[0].UpsName
	}

	rawCommand := fmt.Sprintf("INSTCMD %s %s", upsName, cmdName)
	_, err = client.SendCommand(rawCommand)

	if err != nil {
		errMsg := fmt.Sprintf("❌ **UPS Test Failed**: Could not start `%s` on `%s`: %v", testType, upsName, err)
		sendDiscordWebhook(conf.TestWebhookURL, errMsg)
	} else {
		startMsg := fmt.Sprintf("🔍 **UPS Self-Test Started**: `%s` test initiated on `%s`.", testType, upsName)
		sendDiscordWebhook(conf.TestWebhookURL, startMsg)

		go monitorTestResult(conf, upsName)

		conf.LastTestDate = time.Now()
		saveConfig(conf)
	}
}

func monitorTestResult(conf *Config, upsName string) {
	time.Sleep(15 * time.Second)

	for i := 0; i < 30; i++ {
		client, err := nut.Connect(conf.Host)
		if err != nil {
			time.Sleep(10 * time.Second)
			continue
		}
		client.Authenticate(conf.Username, conf.Password)

		upsList, _ := client.GetUPSList()
		var result string
		for _, ups := range upsList {
			if ups.Name == upsName {
				for _, v := range ups.Variables {
					if v.Name == "ups.test.result" {
						result = fmt.Sprintf("%v", v.Value)
					}
				}
			}
		}
		client.Disconnect()

		resLower := strings.ToLower(result)
		if result != "" && !strings.Contains(resLower, "in progress") && !strings.Contains(resLower, "no test") {
			statusEmoji := "✅"
			if strings.Contains(resLower, "fail") || strings.Contains(resLower, "bad") || strings.Contains(resLower, "error") {
				statusEmoji = "🚨"
			}

			finalMsg := fmt.Sprintf("%s **UPS Test Result**: `%s` reported: **%s**", statusEmoji, upsName, result)
			sendDiscordWebhook(conf.TestWebhookURL, finalMsg)
			return
		}
		time.Sleep(10 * time.Second)
	}
	sendDiscordWebhook(conf.TestWebhookURL, fmt.Sprintf("⚠️ **UPS Test Timeout**: Result polling for `%s` timed out.", upsName))
}

func wakeNode(mac string) {
	packet, err := gowol.NewMagicPacket(mac)
	if err == nil {
		packet.Send("255.255.255.255")
	}
}

func loadConfig() Config {
	baseDir, _ := os.UserConfigDir()
	path := filepath.Join(baseDir, "ups-monitor", "config.json")
	file, err := os.ReadFile(path)
	if err != nil {
		return interactiveSetup(path)
	}
	var conf Config
	json.Unmarshal(file, &conf)
	return conf
}

func saveConfig(conf *Config) {
	baseDir, _ := os.UserConfigDir()
	path := filepath.Join(baseDir, "ups-monitor", "config.json")
	data, _ := json.MarshalIndent(conf, "", "  ")
	os.WriteFile(path, data, 0644)
}

func interactiveSetup(path string) Config {
	reader := bufio.NewReader(os.Stdin)
	fmt.Println("No configuration found.")
	fmt.Print("Choose mode (server/client): ")
	mode, _ := reader.ReadString('\n')
	mode = strings.TrimSpace(strings.ToLower(mode))

	conf := Config{
		Mode:               mode,
		Host:               "127.0.0.1",
		Username:           "monuser",
		Password:           "password",
		IntervalSeconds:    30,
		TestEnabled:        false,
		TestType:           "quick",
		TestIntervalMonths: 1,
		TestWebhookURL:     "WEBHOOK_URL_HERE",
		LastTestDate:       time.Now(),
	}

	if mode == "server" {
		conf.WakeTargets = []WakeTarget{
			{Name: "Node1", MAC: "00:11:22:33:44:55", UpsName: "ups1", WakeLimit: 70, WebhookURL: "WEBHOOK_URL_HERE"},
		}
	} else {
		conf.ClientUpsName = "ups1"
		conf.ClientLimit = 25
		conf.ClientWebhookURL = "WEBHOOK_URL_HERE"
	}

	os.MkdirAll(filepath.Dir(path), 0755)
	saveConfig(&conf)
	fmt.Printf("\nSUCCESS: Config created at %s\nPlease edit and restart.\n", path)
	os.Exit(0)
	return conf
}

func shutdownSystem() {
	switch runtime.GOOS {
	case "windows":
		exec.Command("shutdown", "/s", "/t", "0").Run()
	case "darwin":
		exec.Command("osascript", "-e", "tell app \"System Events\" to shut down").Run()
	case "linux":
		if _, err := exec.LookPath("midclt"); err == nil {
			exec.Command("midclt", "call", "system.shutdown").Run()
		} else {
			exec.Command("shutdown", "-h", "now").Run()
		}
	}
}
