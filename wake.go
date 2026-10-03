package main

import (
	"fmt"
	"log"
	"time"
)

// Wake-on-LAN recovery.
//
// Machines are only woken after a power event, never on a timer, so a machine you
// turn off on purpose stays off. A power event is the UPS going on battery, or the
// server itself starting (it may have lost power too; see no_wake_on_start).
// Once the UPS is back on mains and recharged to the target's %, a few packets are
// sent a few minutes apart, in case the first arrives while the machine is still
// shutting down.

const (
	wakeAttempts      = 3
	wakeRetryInterval = 5 * time.Minute
)

// wakePending counts the packets still to send to each wake target (by name). Guarded by stateMu.
var wakePending = make(map[string]int)

// armWakesOnStart queues a wake for every machine when the server starts.
func armWakesOnStart(s Settings) {
	if !s.isServer() || !s.Server.WakeAfterRestart {
		return
	}
	stateMu.Lock()
	defer stateMu.Unlock()
	n := 0
	for _, m := range s.Server.Machines {
		if m.Wake != nil {
			wakePending[m.Name] = wakeAttempts
			n++
		}
	}
	if n > 0 {
		log.Printf("[WOL] Server started: %d machine(s) will be woken once their UPS is charged enough.", n)
	}
}

func handleWakes(srv *ServerSettings, e upsEvent) {
	stateMu.Lock()
	defer stateMu.Unlock()

	for _, m := range srv.Machines {
		w := m.Wake
		if w == nil || m.UPS != e.ups {
			continue
		}
		if e.onBattery {
			wakePending[m.Name] = wakeAttempts
			delete(lastWakeAttempt, m.Name)
			continue
		}
		left := wakePending[m.Name]
		if left <= 0 || !e.chargeKnown || e.charge < w.AtPercent || time.Since(lastWakeAttempt[m.Name]) < wakeRetryInterval {
			continue
		}
		attempt := wakeAttempts - left + 1
		if attempt == 1 {
			msg := fmt.Sprintf("✅ **Power Restored**: Waking node `%s` (%s). Battery at %d%%.", m.Name, w.MAC, e.charge)
			log.Println(msg)
			sendDiscordWebhookAsync(srv.webhookFor(m), msg)
		} else {
			log.Printf("[WOL] Re-sending wake packet to %s (%d/%d).", m.Name, attempt, wakeAttempts)
		}
		wakeNode(w.MAC, w.Broadcast)
		lastWakeAttempt[m.Name] = time.Now()
		wakePending[m.Name] = left - 1
	}
}

// wakeState describes a machine's wake status for the status screen. Caller holds stateMu.
func wakeState(m Machine) string {
	if m.Wake == nil {
		return ""
	}
	left := wakePending[m.Name]
	r, seen := upsReadings[m.UPS]
	switch {
	case left <= 0 && lastWakeAttempt[m.Name].IsZero():
		return "idle (wakes after an outage)"
	case left <= 0:
		return "woken " + formatAgo(lastWakeAttempt[m.Name])
	case seen && (r.Status == "" || isOnBatteryStatus(r.Status)):
		return fmt.Sprintf("will wake when power returns and battery ≥ %d%%", m.Wake.AtPercent)
	case left < wakeAttempts:
		return fmt.Sprintf("waking (%d/%d packets sent)", wakeAttempts-left, wakeAttempts)
	case seen && r.ChargeKnown:
		return fmt.Sprintf("waiting for %d%% (now %d%%)", m.Wake.AtPercent, r.Charge)
	}
	return fmt.Sprintf("waiting for %d%%", m.Wake.AtPercent)
}
