package main

import (
	"bufio"
	"encoding/json"
	"fmt"
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
	Mode            string       `json:"mode"`
	Host            string       `json:"host"`
	Username        string       `json:"username"`
	Password        string       `json:"password"`
	IntervalSeconds int          `json:"interval_seconds"`
	ClientUpsName   string       `json:"client_ups_name,omitempty"`
	ClientLimit     int          `json:"battery_percent,omitempty"`
	WakeTargets     []WakeTarget `json:"wake_targets,omitempty"`
}

var lastWakeAttempt = make(map[string]time.Time)

func main() {
	config := loadConfig()
	fmt.Printf("--- UPS Monitor (%s Mode) ---\n", strings.ToUpper(config.Mode))

	for {
		client, err := nut.Connect(config.Host)
		if err != nil {
			log.Printf("Connection lost. Retrying: %v", err)
			time.Sleep(10 * time.Second)
			continue
		}

		_, err = client.Authenticate(config.Username, config.Password)
		if err != nil {
			log.Printf("Auth failed: %v", err)
			client.Disconnect()
			time.Sleep(10 * time.Second)
			continue
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
					fmt.Printf("[%s] %d%% (%s)\n", ups.Name, charge, status)
					if isOnBattery && charge <= config.ClientLimit {
						fmt.Printf("CRITICAL: %s low battery (%d%%). Shutting down...\n", ups.Name, charge)
						shutdownSystem()
						return
					}
				}

				// SERVER MODE
				if config.Mode == "server" && !isOnBattery {
					for _, target := range config.WakeTargets {
						if target.UpsName == ups.Name && charge >= target.WakeLimit {
							if time.Since(lastWakeAttempt[target.MAC]) > 15*time.Minute {
								fmt.Printf("Power OK on %s. Waking %s (%s)...\n", ups.Name, target.Name, target.MAC)
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

func wakeNode(mac string) {
	packet, err := gowol.NewMagicPacket(mac)
	if err != nil {
		log.Printf("WOL Error for %s: %v", mac, err)
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

func interactiveSetup(path string) Config {
	reader := bufio.NewReader(os.Stdin)
	fmt.Println("No configuration found.")
	fmt.Print("Choose mode (server/client): ")
	mode, _ := reader.ReadString('\n')
	mode = strings.TrimSpace(strings.ToLower(mode))

	conf := Config{
		Mode:            mode,
		Host:            "127.0.0.1",
		Username:        "monuser",
		Password:        "password",
		IntervalSeconds: 30,
	}

	if mode == "server" {
		conf.WakeTargets = []WakeTarget{
			{Name: "FileServer", MAC: "AA:BB:CC:DD:EE:FF", UpsName: "ups1", WakeLimit: 70},
			{Name: "ComputeNode", MAC: "11:22:33:44:55:66", UpsName: "ups2", WakeLimit: 80},
		}
	} else {
		conf.ClientUpsName = "ups1"
		conf.ClientLimit = 25
	}

	os.MkdirAll(filepath.Dir(path), 0755)
	data, _ := json.MarshalIndent(conf, "", "  ")
	os.WriteFile(path, data, 0644)

	fmt.Printf("\nSUCCESS: Clean config created at %s\nPlease edit the file and restart the application.\n", path)
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
