package main

import (
	"fmt"
	"log"
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
		if !e.reachedShutdown(m.ShutdownAt) {
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

	cmdName := "test.battery.start"
	if testType == "deep" {
		cmdName = "test.battery.start.deep"
	}
	if _, err := client.SendCommand(fmt.Sprintf("INSTCMD %s %s", upsName, cmdName)); err != nil {
		log.Printf("[TEST] Failed to start %s on %s: %v (self-tests need a NUT user allowed to run instant commands)", cmdName, upsName, err)
		sendDiscordWebhook(webhook, "❌ Failed to start test on "+upsName)
		return
	}
	log.Printf("[TEST] Started %s on %s", cmdName, upsName)
	sendDiscordWebhook(webhook, "🔍 Started test on "+upsName)
	go monitorTestResult(*srv, upsName)
	store.state.MarkTested(upsName)
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
