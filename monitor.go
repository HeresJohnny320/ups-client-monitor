package main

import (
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	nut "github.com/robbiet480/go.nut"
)

// upsEvent is one poll's reading of one UPS.
type upsEvent struct {
	ups         string
	status      string
	charge      int
	chargeKnown bool
	onBattery   bool
	lowBattery  bool // NUT's LB flag: critical, shut down now, even if charge is unreadable
}

// reachedShutdown reports whether a machine with this shutdown % should go down now.
func (e upsEvent) reachedShutdown(limit int) bool {
	return e.onBattery && (e.lowBattery || (e.chargeKnown && e.charge <= limit))
}

var lastTestAttempt = make(map[string]time.Time) // only used by the monitor goroutine

// startBackgroundTasks runs forever, following the live settings: server polls NUT,
// a paired client holds its connection to the server, anything else waits for setup.
func startBackgroundTasks(store *settingsStore) {
	armWakesOnStart(store.Get())
	waitingLogged := false
	for {
		store.ReloadIfChanged()
		s := store.Get()
		if s.isServer() {
			hub.ensure(store, s.Server.ListenPort)
			hub.prune(s.Server.Machines)
		} else {
			hub.stop()
		}

		switch {
		case s.isServer():
			pollNUT(store, s.Server)
			store.wait(time.Duration(s.Server.PollSeconds) * time.Second)
		case s.isClient() && s.Client.ServerAddress != "":
			runClientLink(store, *s.Client)
		default:
			if !waitingLogged {
				log.Println("[INFO] Waiting for setup. Run ups-monitor in a terminal to configure.")
				waitingLogged = true
			}
			store.wait(time.Minute)
		}
	}
}

func nutConnect(n NUTSettings) (nut.Client, error) {
	client, err := nut.Connect(n.Host, n.Port)
	if err != nil {
		return client, err
	}
	if n.Username != "" {
		if _, err := client.Authenticate(n.Username, n.Password); err != nil {
			client.Disconnect()
			return client, fmt.Errorf("authentication failed: %w", err)
		}
	}
	return client, nil
}

func pollNUT(store *settingsStore, srv *ServerSettings) {
	stateMu.Lock()
	lastPollStart = time.Now()
	stateMu.Unlock()

	client, err := nutConnect(srv.NUT)
	if err != nil {
		setNutState(false, err)
		return
	}
	defer client.Disconnect()

	runScheduledTests(store, srv)

	upsList, err := client.GetUPSList()
	if err != nil {
		setNutState(false, fmt.Errorf("reading UPS list: %w", err))
		return
	}
	setNutState(true, nil)
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
		charge, chargeKnown := parseCharge(chargeStr)
		e := upsEvent{
			ups: ups.Name, status: status, charge: charge, chargeKnown: chargeKnown,
			onBattery: isOnBatteryStatus(status), lowBattery: strings.Contains(status, "LB"),
		}

		stateMu.Lock()
		upsReadings[ups.Name] = upsReading{Status: status, Charge: charge, ChargeKnown: chargeKnown, Seen: time.Now()}
		stateMu.Unlock()

		handleAPIShutdowns(srv, e)
		handleClientShutdowns(srv, e)
		handleWakes(srv, e)
	}
}

// handleAPIShutdowns sends one Proxmox/TrueNAS API shutdown per machine per outage,
// retrying failures every minute.
func handleAPIShutdowns(srv *ServerSettings, e upsEvent) {
	stateMu.Lock()
	defer stateMu.Unlock()

	for _, m := range srv.Machines {
		if m.UPS != e.ups || (m.Type != MachineProxmox && m.Type != MachineTrueNAS) {
			continue
		}
		if !e.onBattery {
			if s := remoteStates[m.Name]; s != nil && s.done {
				log.Printf("[INFO] %s is back online; re-arming remote shutdown for %s.", e.ups, m.Name)
			}
			delete(remoteStates, m.Name)
			continue
		}
		if m.SkipShutdown || !e.reachedShutdown(m.ShutdownAt) {
			continue
		}

		s := remoteStates[m.Name]
		if s == nil {
			s = &remoteState{}
			remoteStates[m.Name] = s
		}
		if s.done || s.inFlight || time.Since(s.lastAttempt) < remoteRetryInterval {
			continue
		}
		s.inFlight = true
		s.lastAttempt = time.Now()

		webhook := srv.webhookFor(m)
		if s.failures == 0 {
			msg := fmt.Sprintf("⚠️ **UPS Shutdown**: Battery at %d%%. Sending shutdown to `%s` (%s).", e.charge, m.Name, m.Type)
			log.Println(msg)
			sendDiscordWebhookAsync(webhook, msg)
		}

		go func(t ShutdownTarget, s *remoteState) {
			reason := fmt.Sprintf("UPS %s on battery (%s), charge %d%%", e.ups, e.status, e.charge)
			err := remoteShutdown(t, reason)

			stateMu.Lock()
			defer stateMu.Unlock()
			s.inFlight = false
			if err != nil {
				s.failures++
				log.Printf("[ERROR] Remote shutdown of %s failed (attempt %d): %v", t.Name, s.failures, err)
				if s.failures == 1 {
					sendDiscordWebhookAsync(webhook, fmt.Sprintf("❌ **Remote Shutdown Failed**: `%s`: %v. Retrying every minute.", t.Name, err))
				}
				return
			}
			s.done = true
			msg := fmt.Sprintf("🔌 **Remote Shutdown**: `%s` accepted the shutdown command.", t.Name)
			log.Println(msg)
			sendDiscordWebhookAsync(webhook, msg)
		}(m.apiTarget(), s)
	}
}

// ---------- Self-tests ----------

func runScheduledTests(store *settingsStore, srv *ServerSettings) {
	t := srv.SelfTests
	if !t.Enabled {
		return
	}
	store.state.startSchedule(t.UPS)
	for _, ups := range t.UPS {
		next := store.state.LastTest(ups).AddDate(0, t.EveryMonths, 0)
		// A failed start is retried hourly instead of on every poll.
		if time.Now().After(next) && time.Since(lastTestAttempt[ups]) > testRetryInterval {
			lastTestAttempt[ups] = time.Now()
			log.Printf("[SCHEDULER] Triggering scheduled test for: %s", ups)
			runSingleUpsTest(store, srv, ups, t.Type)
		}
	}
}

func runSelfTest(store *settingsStore, testType string) {
	s := store.Get()
	if !s.isServer() {
		return
	}
	for _, ups := range s.Server.SelfTests.UPS {
		runSingleUpsTest(store, s.Server, ups, testType)
	}
}

func runSingleUpsTest(store *settingsStore, srv *ServerSettings, upsName string, testType string) {
	webhook := srv.testWebhook()
	client, err := nutConnect(srv.NUT)
	if err != nil {
		log.Printf("[TEST] Connection failed for %s: %v", upsName, err)
		sendDiscordWebhook(webhook, "❌ Connection failed for "+upsName)
		return
	}
	defer client.Disconnect()

	cmdName, err := startUpsTest(&client, upsName, testType)
	if err != nil {
		hint := ""
		if strings.Contains(err.Error(), "authentication") {
			hint = " (self-tests need a NUT user allowed to run instant commands)"
		}
		log.Printf("[TEST] Could not start a %s test on %s: %v%s", testType, upsName, err, hint)
		sendDiscordWebhook(webhook, "❌ Failed to start test on "+upsName)
		return
	}
	log.Printf("[TEST] Started %s on %s", cmdName, upsName)
	sendDiscordWebhook(webhook, "🔍 Started test on "+upsName)
	go monitorTestResult(*srv, upsName)
	store.state.MarkTested(upsName)
}

// nutCommander is the part of the NUT client the self-test needs (a fake in tests).
type nutCommander interface {
	SendCommand(cmd string) ([]string, error)
}

// Battery-test commands to try, best first. Many UPSes (e.g. most usbhid-ups models)
// only offer the .quick/.deep variants, others only the plain one.
var testCommands = map[string][]string{
	"quick": {"test.battery.start.quick", "test.battery.start"},
	"deep":  {"test.battery.start.deep", "test.battery.start"},
}

// startUpsTest starts a battery self-test using whichever command this UPS supports,
// and returns the command it used.
func startUpsTest(client nutCommander, upsName, testType string) (string, error) {
	candidates := testCommands[testType]
	if candidates == nil {
		return "", fmt.Errorf("unknown test type %q", testType)
	}
	// Ask the UPS which commands it has. If that fails, just try the candidates in order.
	if supported, err := upsCommands(client, upsName); err == nil {
		var usable, tests []string
		for _, c := range candidates {
			if supported[c] {
				usable = append(usable, c)
			}
		}
		if len(usable) == 0 {
			for c := range supported {
				if strings.HasPrefix(c, "test.battery") {
					tests = append(tests, c)
				}
			}
			sort.Strings(tests)
			if len(tests) == 0 {
				return "", errors.New("this UPS doesn't offer a battery self-test through NUT")
			}
			return "", fmt.Errorf("this UPS doesn't offer a %s battery test (it has: %s)", testType, strings.Join(tests, ", "))
		}
		candidates = usable
	}

	var lastErr error
	for _, c := range candidates {
		_, err := client.SendCommand(fmt.Sprintf("INSTCMD %s %s", upsName, c))
		if err == nil {
			return c, nil
		}
		lastErr = err
		if !strings.Contains(err.Error(), "support the instant command") {
			break // a real error (e.g. login), not just an unsupported command
		}
	}
	return "", lastErr
}

// upsCommands returns the instant commands a UPS supports (NUT's LIST CMD).
func upsCommands(client nutCommander, upsName string) (map[string]bool, error) {
	resp, err := client.SendCommand("LIST CMD " + upsName)
	if err != nil {
		return nil, err
	}
	prefix := "CMD " + upsName + " "
	out := map[string]bool{}
	for _, line := range resp {
		if strings.HasPrefix(line, prefix) {
			out[strings.TrimSpace(strings.TrimPrefix(line, prefix))] = true
		}
	}
	return out, nil
}

func monitorTestResult(srv ServerSettings, upsName string) {
	webhook := srv.testWebhook()
	time.Sleep(15 * time.Second)

	for i := 0; i < 30; i++ {
		client, err := nutConnect(srv.NUT)
		if err != nil {
			time.Sleep(10 * time.Second)
			continue
		}
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
			log.Printf("[TEST] %s reported: %s", upsName, result)
			sendDiscordWebhook(webhook, fmt.Sprintf("%s **UPS Test Result**: `%s` reported: **%s**", statusEmoji, upsName, result))
			return
		}
		time.Sleep(10 * time.Second)
	}
	log.Printf("[TEST] Result polling for %s timed out", upsName)
	sendDiscordWebhook(webhook, fmt.Sprintf("⚠️ **UPS Test Timeout**: Result polling for `%s` timed out.", upsName))
}

// ---------- Manual actions ----------

func testAllWebhooks(s Settings) {
	log.Println("[INFO] Testing configured webhooks...")
	testMsg := "🧪 **Webhook Test**: This is a test message from your UPS Monitor to verify connectivity."
	sent := map[string]bool{}
	send := func(url, label string) {
		if url == "" || sent[url] {
			return
		}
		sent[url] = true
		log.Printf("- Testing: %s", label)
		sendDiscordWebhook(url, fmt.Sprintf("%s (%s)", testMsg, label))
	}
	if s.isClient() {
		send(s.Client.WebhookURL, "Client: "+s.Client.Name)
	}
	if s.isServer() {
		send(s.Server.WebhookURL, "Server default")
		send(s.Server.SelfTests.WebhookURL, "Self-tests")
		for _, m := range s.Server.Machines {
			send(m.WebhookURL, "Machine: "+m.Name)
		}
	}
	log.Println("[SUCCESS] Webhook test commands sent.")
}

// checkClients reports each paired client's connection, for the "Check clients" action.
func checkClients(s Settings) {
	if !s.isServer() {
		return
	}
	checked := 0
	for _, m := range s.Server.Machines {
		if m.Type != MachineClient {
			continue
		}
		checked++
		note := ""
		if m.SkipShutdown {
			note = " Automatic shutdown is turned off for it."
		}
		if info, ok := describeClient(m.Name); ok {
			log.Printf("[CHECK] %s (client): OK, %s. Shuts down at %d%% on %s.%s", m.Name, info, m.ShutdownAt, m.UPS, note)
			continue
		}
		log.Printf("[CHECK] %s (client): NOT CONNECTED. Make sure ups-monitor runs on it in client mode with this server's pairing code, "+
			"and that it can reach this server on TCP %d.%s", m.Name, s.Server.ListenPort, note)
	}
	if checked == 0 {
		log.Println("[INFO] No client machines configured.")
	}
}

// checkShutdownTargets logs in to every Proxmox/TrueNAS machine without shutting it down.
func checkShutdownTargets(s Settings) {
	if !s.isServer() {
		return
	}
	checked := 0
	for _, m := range s.Server.Machines {
		if m.Type != MachineProxmox && m.Type != MachineTrueNAS {
			continue
		}
		checked++
		info, err := checkRemoteTarget(m.apiTarget())
		if err != nil {
			log.Printf("[CHECK] %s (%s): FAILED: %v", m.Name, m.Type, err)
			continue
		}
		log.Printf("[CHECK] %s (%s): OK, %s. Shuts down at %d%% on %s.", m.Name, m.Type, info, m.ShutdownAt, m.UPS)
	}
	if checked == 0 {
		log.Println("[INFO] No Proxmox or TrueNAS machines configured.")
	}
}
