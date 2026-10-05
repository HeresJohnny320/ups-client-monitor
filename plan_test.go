package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestPowerPlanOrder(t *testing.T) {
	wake := func(at int) *WakeSettings { return &WakeSettings{MAC: "aa:bb:cc:dd:ee:01", AtPercent: at} }
	machines := []Machine{
		{Name: "pve1", Type: MachineProxmox, UPS: "ups2", ShutdownAt: 50, Wake: wake(80)},
		{Name: "desk", Type: MachineClient, UPS: "ups1", ShutdownAt: 30}, // other UPS
		{Name: "nas", Type: MachineTrueNAS, UPS: "ups2", ShutdownAt: 90, Wake: wake(40)},
		{Name: "lab", Type: MachineProxmox, UPS: "ups2", ShutdownAt: 70, SkipShutdown: true, Wake: wake(60)},
		{Name: "printer", Type: MachineWakeOnly, UPS: "ups2", Wake: wake(20)},
		{Name: "pve2", Type: MachineProxmox, UPS: "ups2", ShutdownAt: 50}, // same % as pve1, never woken
	}
	var got []string
	for _, r := range powerPlan(machines, "ups2") {
		got = append(got, fmt.Sprintf("%s off=%d wake=%d idx=%d", r.Machine.Name, r.ShutdownRank, r.WakeRank, r.Index))
	}
	want := []string{
		"nas off=1 wake=2 idx=2",     // 90%: first to go down, woken 2nd (40%)
		"pve1 off=2 wake=4 idx=0",    // 50%: shares 2nd place with pve2
		"pve2 off=2 wake=0 idx=5",    // ...and is never woken
		"lab off=0 wake=3 idx=3",     // shutdown turned off: after those that shut down
		"printer off=0 wake=1 idx=4", // wake-only: last, but woken first (20%)
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("plan:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for n, w := range map[int]string{1: "1st", 2: "2nd", 3: "3rd", 4: "4th", 11: "11th", 12: "12th", 13: "13th", 21: "21st", 22: "22nd"} {
		if ordinal(n) != w {
			t.Errorf("ordinal(%d) = %s, want %s", n, ordinal(n), w)
		}
	}
}

func TestShutdownSwitchOff(t *testing.T) {
	resetWakeState()
	stateMu.Lock()
	remoteStates = make(map[string]*remoteState)
	clientStates = make(map[string]*clientState)
	stateMu.Unlock()

	srv := &ServerSettings{Machines: []Machine{
		{Name: "lab", Type: MachineProxmox, UPS: "ups1", ShutdownAt: 90, SkipShutdown: true,
			Proxmox: &ProxmoxAPI{Host: "https://127.0.0.1:1", Node: "pve", Token: "root@pam!ups=x"},
			Wake:    &WakeSettings{MAC: "aa:bb:cc:dd:ee:01", AtPercent: 40, Broadcast: "127.0.0.1"}},
		{Name: "desk", Type: MachineClient, UPS: "ups1", ShutdownAt: 90, SkipShutdown: true, ClientKey: "0123456789abcdef"},
	}}
	low := upsEvent{ups: "ups1", status: "OB LB", charge: 5, chargeKnown: true, onBattery: true, lowBattery: true}
	handleAPIShutdowns(srv, low)
	handleClientShutdowns(srv, low)
	stateMu.Lock()
	apiStarted, clientOrdered := remoteStates["lab"], clientStates["desk"]
	stateMu.Unlock()
	if apiStarted != nil || clientOrdered != nil {
		t.Fatal("a machine with shutdown turned off was shut down, even at 5% with LB")
	}

	// It is still woken once power comes back.
	handleWakes(srv, low)
	handleWakes(srv, upsEvent{ups: "ups1", status: "OL", charge: 50, chargeKnown: true})
	if wakesSent("lab") != 1 {
		t.Error("a machine with shutdown turned off should still be woken after the outage")
	}
}
