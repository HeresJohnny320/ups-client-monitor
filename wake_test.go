package main

import (
	"net"
	"strings"
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

// ev is a confirmed reading: on battery long enough to count as a real outage.
func ev(ups string, charge int, onBattery bool) upsEvent {
	return upsEvent{ups: ups, charge: charge, chargeKnown: true, onBattery: onBattery, confirmed: onBattery}
}

func clientMachine(name, ups string, shutdownAt, wakeAt int) Machine {
	return Machine{Name: name, Type: MachineClient, UPS: ups, ShutdownAt: shutdownAt, ClientKey: "0123456789abcdef",
		Wake: &WakeSettings{MAC: "aa:bb:cc:dd:ee:0" + name[:1], AtPercent: wakeAt, Broadcast: "127.0.0.1"}}
}

// outage runs one reading through the shutdown and wake handlers, like a monitor poll.
func outage(srv *ServerSettings, e upsEvent) {
	handleClientShutdowns(srv, e)
	handleWakes(srv, e)
}

func TestWakeOnlyWhatWasShutDown(t *testing.T) {
	resetWakeState()
	stateMu.Lock()
	clientStates = make(map[string]*clientState)
	stateMu.Unlock()
	srv := &ServerSettings{Machines: []Machine{
		clientMachine("nas", "ups2", 70, 40),
		clientMachine("pve", "ups2", 50, 80),
		wakeMachine("printer", "aa:bb:cc:dd:ee:09", "ups2", 20),
		clientMachine("desk", "ups1", 90, 50),
	}}

	// A blip: on battery at 98%, nothing reaches its shutdown %, power comes back.
	// Nothing was shut down, so nothing is woken (this used to wake everything).
	outage(srv, ev("ups2", 98, true))
	outage(srv, ev("ups2", 98, false))
	stateMu.Lock()
	woken := len(lastWakeAttempt)
	stateMu.Unlock()
	if woken != 0 {
		t.Fatal("a blip that shut nothing down woke machines")
	}

	// A real outage on ups2 down to 60%: only the NAS (70%) is shut down and armed.
	outage(srv, ev("ups2", 60, true))
	outage(srv, ev("ups2", 30, false)) // power back, below the NAS's 40% wake level
	if wakesSent("nas") != 0 {
		t.Fatal("woke before reaching the wake %")
	}
	outage(srv, ev("ups2", 50, false))
	if wakesSent("nas") != 1 || wakesSent("pve") != 0 || wakesSent("printer") != 0 || wakesSent("desk") != 0 {
		t.Fatalf("after the outage: nas=%d pve=%d printer=%d desk=%d (only the NAS was shut down)",
			wakesSent("nas"), wakesSent("pve"), wakesSent("printer"), wakesSent("desk"))
	}

	// Retries wait for the interval, then stop after wakeAttempts packets.
	outage(srv, ev("ups2", 55, false))
	if wakesSent("nas") != 1 {
		t.Fatal("re-sent before the retry interval")
	}
	for i := 0; i < 5; i++ {
		stateMu.Lock()
		lastWakeAttempt["nas"] = time.Now().Add(-wakeRetryInterval - time.Second)
		stateMu.Unlock()
		outage(srv, ev("ups2", 90, false))
	}
	if wakesSent("nas") != wakeAttempts {
		t.Fatalf("nas got %d packets, want %d", wakesSent("nas"), wakeAttempts)
	}

	// The UPS running out (low battery): everything on it may lose power, so all of it is
	// woken afterwards, including machines it doesn't shut down.
	resetWakeState()
	outage(srv, upsEvent{ups: "ups2", status: "OB LB", charge: 8, chargeKnown: true, onBattery: true, lowBattery: true, confirmed: true})
	outage(srv, ev("ups2", 100, false))
	if wakesSent("nas") != 1 || wakesSent("pve") != 1 || wakesSent("printer") != 1 || wakesSent("desk") != 0 {
		t.Fatalf("after low battery: nas=%d pve=%d printer=%d desk=%d (desk is on another UPS)",
			wakesSent("nas"), wakesSent("pve"), wakesSent("printer"), wakesSent("desk"))
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

func TestWakeGoesToEachPrivateSubnet(t *testing.T) {
	cidr := func(s string) net.Addr {
		ip, n, err := net.ParseCIDR(s)
		if err != nil {
			t.Fatal(err)
		}
		n.IP = ip // keep the host address, like an interface does
		return n
	}
	addrs := []net.Addr{
		cidr("192.168.1.1/24"),  // LAN
		cidr("10.0.20.1/24"),    // a VLAN
		cidr("172.16.5.9/16"),   // another private range
		cidr("192.168.1.77/24"), // same LAN again: sent once
		cidr("203.0.113.5/24"),  // public WAN: skipped, never broadcast to the ISP
		cidr("100.64.3.2/10"),   // CGNAT WAN: skipped
		cidr("10.9.9.9/32"),     // VPN host route: no broadcast address
		cidr("10.8.0.1/31"),     // point-to-point: no broadcast address
		cidr("fd00::1/64"),      // IPv6 has no broadcast
	}
	got := strings.Join(privateBroadcasts(addrs), ",")
	if want := "192.168.1.255,10.0.20.255,172.16.255.255"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}

	if a := wakeAddrs("192.168.20.255"); len(a) != 1 || a[0] != "192.168.20.255" {
		t.Errorf("a configured broadcast address must be used as-is, got %v", a)
	}
	for _, a := range wakeAddrs("") {
		if a == "" {
			t.Error("empty address")
		}
	}
}
