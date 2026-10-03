package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/linde12/gowol"
	"golang.org/x/term"
)

// upsReading is the last state seen for a UPS, shown by the 'status' command.
type upsReading struct {
	Status      string
	Charge      int
	ChargeKnown bool
	Seen        time.Time
}

// remoteState tracks one shutdown target during an outage. It is reset when its UPS is back online.
type remoteState struct {
	inFlight    bool
	done        bool
	failures    int
	lastAttempt time.Time
}

const (
	remoteRetryInterval = time.Minute
	testRetryInterval   = time.Hour
)

var (
	lastWakeAttempt = make(map[string]time.Time)

	stateMu       sync.Mutex
	upsReadings   = make(map[string]upsReading)
	remoteStates  = make(map[string]*remoteState)
	nutConnected  bool
	nutChecked    bool
	nutLastError  string
	lastPollStart time.Time
)

// version is set at build time with -ldflags "-X main.version=..."; local builds say "dev".
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	daemon := flag.Bool("daemon", false, "run the monitor in the foreground with no UI (for systemd, launchd, Task Scheduler)")
	dir := flag.String("config-dir", defaultAppDir(), "settings folder holding settings.json, state.json, activity.log and the control socket")
	flag.Parse()
	if *showVersion {
		fmt.Println("ups-monitor", version)
		return
	}
	appDir = *dir
	os.MkdirAll(appDir, 0755)

	// Services have no terminal, so they run the monitor directly like before.
	if *daemon || !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		runDaemon()
		return
	}
	runUI()
}

// runDaemon is the long-running monitor. It serves the control socket so the UI can attach.
func runDaemon() {
	logFile, err := os.OpenFile(filepath.Join(appDir, "activity.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	writers := []io.Writer{recentLogs}
	if err == nil {
		defer logFile.Close()
		writers = append(writers, logFile)
	}
	// stdout last: a closed stdout must not stop the log file from being written.
	log.SetOutput(io.MultiWriter(append(writers, os.Stdout)...))

	store, err := openSettingsStore(settingsPath())
	if err != nil {
		log.Fatalf("[FATAL] %v", err)
	}
	ln, err := listenControl()
	if err != nil {
		log.Fatalf("[FATAL] %v", err)
	}
	defer ln.Close()

	if role := store.Get().Role; role == "" {
		log.Printf("--- UPS Monitor Started (not configured yet: run ups-monitor in a terminal to set it up) --- version %s", version)
	} else {
		log.Printf("--- UPS Monitor (%s Mode) Started --- version %s", strings.ToUpper(role), version)
	}

	stop := make(chan struct{})
	go serveControl(ln, store, stop)
	go startBackgroundTasks(store)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	select {
	case sig := <-sigCh:
		log.Printf("Received %v, exiting...", sig)
	case <-stop:
		log.Println("Stop requested from the UI, exiting...")
	}
}

// runUI attaches to the running monitor (starting one if needed) and opens the settings TUI.
func runUI() {
	log.SetOutput(io.Discard) // the TUI owns the terminal; the monitor writes the log

	if _, err := ctlCall(ctlRequest{Cmd: "ping"}); err != nil {
		if _, err := readSettingsFile(settingsPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "Cannot start: %v\nFix or remove the file and try again.\n", err)
			os.Exit(1)
		}
		fmt.Printf("No UPS monitor is running for %s.\n", appDir)
		fmt.Println("(If it runs as a service under another account such as root, quit and run this with sudo.)")
		fmt.Print("Start it in the background now? [Y/n]: ")
		answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "" && a != "y" && a != "yes" {
			return
		}
		if err := spawnMonitor(); err != nil {
			fmt.Fprintf(os.Stderr, "Could not start the monitor: %v\n", err)
			os.Exit(1)
		}
	}

	if err := runTUI(); err != nil {
		fmt.Fprintf(os.Stderr, "UI error: %v\n", err)
		os.Exit(1)
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
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		log.Printf("[WEBHOOK ERROR] Discord returned %s", resp.Status)
	}
}

func formatAgo(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return time.Since(t).Round(time.Second).String() + " ago"
}

func setNutState(connected bool, err error) {
	stateMu.Lock()
	defer stateMu.Unlock()
	if connected != nutConnected || !nutChecked {
		if connected {
			log.Println("[INFO] Connected to NUT server.")
		} else {
			log.Printf("[ERROR] Cannot reach NUT server: %v", err)
		}
	}
	nutChecked = true
	nutConnected = connected
	nutLastError = ""
	if err != nil {
		nutLastError = err.Error()
	}
}

// sendDiscordWebhookAsync avoids holding locks or delaying the monitor while Discord responds.
func sendDiscordWebhookAsync(url, message string) {
	go sendDiscordWebhook(url, message)
}

// isOnBatteryStatus reports whether a NUT ups.status means the UPS is running on battery.
func isOnBatteryStatus(status string) bool {
	return strings.Contains(status, "OB") || strings.Contains(status, "LB")
}

func parseCharge(s string) (int, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, false
	}
	return int(f), true
}

func wakeNode(mac, broadcast string) error {
	if broadcast == "" {
		broadcast = "255.255.255.255"
	}
	packet, err := gowol.NewMagicPacket(mac)
	if err != nil {
		log.Printf("[WOL ERROR] Invalid MAC %s: %v", mac, err)
		return err
	}
	if err := packet.Send(broadcast); err != nil {
		log.Printf("[WOL ERROR] Sending to %s via %s: %v", mac, broadcast, err)
		return err
	}
	return nil
}

func shutdownSystem() error {
	if os.Getenv("UPS_MONITOR_DRY_RUN") == "1" {
		log.Println("[DRY RUN] Would shut down this machine now (UPS_MONITOR_DRY_RUN=1).")
		return nil
	}
	switch runtime.GOOS {
	case "windows":
		return exec.Command("shutdown", "/s", "/t", "0").Run()
	case "darwin":
		return exec.Command("osascript", "-e", "tell app \"System Events\" to shut down").Run()
	case "linux":
		if _, err := exec.LookPath("midclt"); err == nil {
			// TrueNAS 25.04+ requires a reason argument; older releases reject it.
			if err := exec.Command("midclt", "call", "system.shutdown", "UPS battery low").Run(); err == nil {
				return nil
			}
			return exec.Command("midclt", "call", "system.shutdown").Run()
		}
		return exec.Command("shutdown", "-h", "now").Run()
	}
	return fmt.Errorf("shutdown not supported on %s", runtime.GOOS)
}
