package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
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
	Name      string `json:"name"`
	MAC       string `json:"mac"`
	UpsName   string `json:"ups_name"`
	WakeLimit int    `json:"battery_percent"`
}

type Config struct {
	Mode               string       `json:"mode"`
	Host               string       `json:"host"`
	Username           string       `json:"username"`
	Password           string       `json:"password"`
	IntervalSeconds    int          `json:"interval_seconds"`
	ClientUpsName      string       `json:"client_ups_name,omitempty"`
	ClientLimit        int          `json:"battery_percent,omitempty"`
	WakeTargets        []WakeTarget `json:"wake_targets,omitempty"`
	TestEnabled        bool         `json:"test_enabled"`
	TestType           string       `json:"test_type"`
	TestIntervalMonths int          `json:"test_interval_months"`
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
	fmt.Println("Available Commands: 'test', 'test-long', 'status', 'exit'")
	for {
		fmt.Print("ups-cli> ")
		input, _ := reader.ReadString('\n')
		cmd := strings.TrimSpace(strings.ToLower(input))

		switch cmd {
		case "test":
			runSelfTest(&config, "quick")
		case "test-long":
			runSelfTest(&config, "deep")
		case "status":
			log.Println("Manual status check requested.")
		case "exit":
			log.Println("Exiting application...")
			return
		case "":
			continue
		default:
			fmt.Println("Unknown command. Options: test, test-long, status, exit")
		}
	}
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

				// CLIENT MODE
				if config.Mode == "client" && ups.Name == config.ClientUpsName {
					if isOnBattery && charge <= config.ClientLimit {
						log.Printf("CRITICAL: Battery Low (%d%%). Triggering Shutdown...", charge)
						shutdownSystem()
						return
					}
				}

				// SERVER MODE
				if config.Mode == "server" && !isOnBattery {
					for _, target := range config.WakeTargets {
						if target.UpsName == ups.Name && charge >= target.WakeLimit {
							if time.Since(lastWakeAttempt[target.MAC]) > 15*time.Minute {
								log.Printf("Power Stable. Waking Node: %s (%s)", target.Name, target.MAC)
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
		log.Printf("[ERROR] Connection to NUT failed: %v", err)
		return
	}
	defer client.Disconnect()
	client.Authenticate(conf.Username, conf.Password)

	cmdName := "test.battery.start"
	if testType == "deep" {
		cmdName = "test.battery.start.deep"
	}

	upsName := conf.ClientUpsName
	if upsName == "" && len(conf.WakeTargets) > 0 {
		upsName = conf.WakeTargets[0].UpsName
	}

	rawCommand := fmt.Sprintf("INSTCMD %s %s", upsName, cmdName)

	_, err = client.SendCommand(rawCommand)
	if err != nil {
		log.Printf("[ERROR] Failed to start test for %s: %v", upsName, err)
	} else {
		log.Printf("[SUCCESS] %s self-test initiated on %s", testType, upsName)
		conf.LastTestDate = time.Now()
		saveConfig(conf)
	}
}

func wakeNode(mac string) {
	packet, err := gowol.NewMagicPacket(mac)
	if err != nil {
		log.Printf("WOL Error: %v", err)
		return
	}
	packet.Send("255.255.255.255")
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
		LastTestDate:       time.Now(),
	}

	if mode == "server" {
		conf.WakeTargets = []WakeTarget{
			{Name: "ExampleNode", MAC: "AA:BB:CC:DD:EE:FF", UpsName: "ups1", WakeLimit: 70},
		}
	} else {
		conf.ClientUpsName = "ups1"
		conf.ClientLimit = 25
	}

	os.MkdirAll(filepath.Dir(path), 0755)
	saveConfig(&conf)

	fmt.Printf("\nSUCCESS: Config created at %s\nPlease edit and restart the application.\n", path)
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
