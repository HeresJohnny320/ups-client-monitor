package main

import (
	"fmt"
	"log"
	"time"
)

// Wake-on-LAN recovery.
//
// Machines are only woken after a power event, never on a timer, so a machine you
// turn off on purpose stays off. A machine is woken if this server shut it down, if its
// UPS reported low battery (it may have lost power), or after the server itself starts
// (it may have lost power too; see wake_after_restart). A short blip wakes nothing.
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

// armWake queues a wake for a machine once its UPS is back on mains. Caller holds stateMu.
func armWake(m Machine) {
	if m.Wake != nil {
		wakePending[m.Name] = wakeAttempts
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
			// Machines are armed when they're shut down (see armWake). If the UPS is about to
			// run out, everything on it may lose power, so arm the rest too.
			if e.confirmed && e.lowBattery && !e.testing && (!e.chargeKnown || e.charge < fullCharge) {
				armWake(m)
			}
			if wakePending[m.Name] > 0 {
				delete(lastWakeAttempt, m.Name) // wake promptly once power is back
			}
			continue
		}
		left := wakePending[m.Name]
		if left <= 0 || !e.chargeKnown || e.charge < w.AtPercent || time.Since(lastWakeAttempt[m.Name]) < wakeRetryInterval {
			continue
		}
		attempt := wakeAttempts - left + 1
		if attempt == 1 {
			// The MAC address stays out of Discord; the local log keeps it for troubleshooting.
			log.Printf("✅ **Power Restored**: Waking node `%s` (%s). Battery at %d%%.", m.Name, w.MAC, e.charge)
			sendDiscordWebhookAsync(srv.webhookFor(m), fmt.Sprintf("✅ **Power Restored**: Waking node `%s`. Battery at %d%%.", m.Name, e.charge))
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
