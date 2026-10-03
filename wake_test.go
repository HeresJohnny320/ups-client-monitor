package main

import (
	"testing"
	"time"
)

func resetWakeState() {
	stateMu.Lock()
	defer stateMu.Unlock()
	wakePending = make(map[string]int)
	lastWakeAttempt = make(map[string]time.Time)
}

func wakesSent(name string) int {
	stateMu.Lock()
	defer stateMu.Unlock()
	left, armed := wakePending[name]
	if !armed {
		return 0
	}
	return wakeAttempts - left
}

// wakeMachine is a wake-only machine; 127.0.0.1 keeps test packets off the real network.
func wakeMachine(name, mac, ups string, at int) Machine {
	return Machine{Name: name, Type: MachineWakeOnly, UPS: ups, Wake: &WakeSettings{MAC: mac, AtPercent: at, Broadcast: "127.0.0.1"}}
}

func ev(ups string, charge int, onBattery bool) upsEvent {
	return upsEvent{ups: ups, charge: charge, chargeKnown: true, onBattery: onBattery}
}

func TestWakeOnlyAfterOutage(t *testing.T) {
	resetWakeState()
	srv := &ServerSettings{Machines: []Machine{
		wakeMachine("nas", "aa:bb:cc:dd:ee:01", "ups2", 40),
		wakeMachine("pve", "aa:bb:cc:dd:ee:02", "ups2", 80),
		wakeMachine("desk", "aa:bb:cc:dd:ee:03", "ups1", 50),
		{Name: "no-wake", Type: MachineClient, UPS: "ups2", ShutdownAt: 30, ClientKey: "0123456789abcdef"},
	}}

	// On mains with no outage: nothing is woken (a machine turned off on purpose stays off).
	handleWakes(srv, ev("ups2", 100, false))
	stateMu.Lock()
	woken := len(lastWakeAttempt)
	stateMu.Unlock()
	if woken != 0 {
		t.Fatal("woke a machine without an outage")
	}

	// ups2 goes on battery, then power returns at 30%.
	handleWakes(srv, ev("ups2", 60, true))
	handleWakes(srv, ev("ups2", 30, false))
	if wakesSent("nas") != 0 || wakesSent("pve") != 0 {
		t.Fatal("woke before reaching the wake %")
	}

	// 50%: the NAS (40%) wakes, Proxmox (80%) keeps waiting, ups1's desk is untouched.
	handleWakes(srv, ev("ups2", 50, false))
	if wakesSent("nas") != 1 || wakesSent("pve") != 0 {
		t.Fatalf("at 50%%: nas=%d pve=%d", wakesSent("nas"), wakesSent("pve"))
	}
	stateMu.Lock()
	_, deskPending := wakePending["desk"]
	_, noWakePending := wakePending["no-wake"]
	stateMu.Unlock()
	if deskPending || noWakePending {
		t.Fatal("armed a machine on another UPS or without Wake-on-LAN")
	}

	// Retries wait for the interval, then stop after wakeAttempts packets.
	handleWakes(srv, ev("ups2", 55, false))
	if wakesSent("nas") != 1 {
		t.Fatal("re-sent before the retry interval")
	}
	for i := 0; i < 5; i++ {
		stateMu.Lock()
		lastWakeAttempt["nas"] = time.Now().Add(-wakeRetryInterval - time.Second)
		stateMu.Unlock()
		handleWakes(srv, ev("ups2", 90, false))
	}
	if wakesSent("nas") != wakeAttempts {
		t.Fatalf("nas got %d packets, want %d", wakesSent("nas"), wakeAttempts)
	}
	if wakesSent("pve") != 1 {
		t.Fatalf("pve should wake at 90%%, got %d packets", wakesSent("pve"))
	}

	// The next outage re-arms everything on that UPS.
	handleWakes(srv, ev("ups2", 95, true))
	stateMu.Lock()
	rearmed := wakePending["nas"] == wakeAttempts
	stateMu.Unlock()
	if !rearmed {
		t.Fatal("a new outage did not re-arm the wake")
	}
}

func TestWakeOnServerStart(t *testing.T) {
	resetWakeState()
	s := Settings{Role: "server", Server: &ServerSettings{Machines: []Machine{wakeMachine("nas", "aa:bb:cc:dd:ee:01", "ups1", 40)}}}

	s.Server.WakeAfterRestart = false
	armWakesOnStart(s)
	handleWakes(s.Server, ev("ups1", 100, false))
	if wakesSent("nas") != 0 {
		t.Fatal("woke on start although wake_after_restart is off")
	}

	s.Server.WakeAfterRestart = true
	armWakesOnStart(s)
	handleWakes(s.Server, ev("ups1", 100, false))
	if wakesSent("nas") != 1 {
		t.Fatal("did not wake after the server started")
	}
}
