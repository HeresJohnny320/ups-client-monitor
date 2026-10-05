package main

import (
	"bytes"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// startTestServer runs the client listener with one paired client and returns its pairing code.
func startTestServer(t *testing.T, key string) (*settingsStore, string) {
	appDir = t.TempDir()
	stateMu.Lock()
	link = clientLink{}
	clientStates = make(map[string]*clientState)
	stateMu.Unlock()

	port := freePort(t)
	srv := newServerSettings()
	srv.ListenPort = port
	srv.Machines = []Machine{{Name: "desk", Type: MachineClient, UPS: "ups1", ShutdownAt: 30, ClientKey: key}}
	store := &settingsStore{
		s:       Settings{Version: settingsVersion, Role: "server", Server: srv},
		path:    filepath.Join(appDir, "settings.json"),
		changed: make(chan struct{}, 1),
		state:   openStateStore(filepath.Join(appDir, "state.json")),
	}
	hub.ensure(store, port)
	t.Cleanup(hub.stop)
	fp, err := hub.identity()
	if err != nil {
		t.Fatal(err)
	}
	return store, pairingCode(fmt.Sprintf("127.0.0.1:%d", port), "desk", key, fp)
}

func clientSettingsFromCode(t *testing.T, code string) ClientSettings {
	addr, id, key, fp, err := parsePairingCode(code)
	if err != nil {
		t.Fatal(err)
	}
	return ClientSettings{ServerAddress: addr, Name: id, Key: key, ServerFingerprint: fp}
}

func TestLinkShutdownOrder(t *testing.T) {
	key := newClientKey()
	srv, code := startTestServer(t, key)
	cli := clientSettingsFromCode(t, code)

	shutdowns := make(chan struct{}, 10)
	systemShutdown = func() error { shutdowns <- struct{}{}; return nil }
	t.Cleanup(func() { systemShutdown = shutdownSystem })

	cliStore := &settingsStore{
		s:       Settings{Version: settingsVersion, Role: "client", Client: &cli},
		path:    filepath.Join(t.TempDir(), "settings.json"),
		changed: make(chan struct{}, 1),
	}
	go runClientLink(cliStore, cli)
	waitFor(t, "client to connect", func() bool { return hub.get("desk") != nil })

	ev := func(ups string, charge int, onBattery bool) upsEvent {
		status := "OL"
		if onBattery {
			status = "OB"
		}
		return upsEvent{ups: ups, status: status, charge: charge, chargeKnown: true, onBattery: onBattery}
	}
	conf := srv.Get().Server
	// On battery but above the 30% limit: no order.
	handleClientShutdowns(conf, ev("ups1", 50, true))
	select {
	case <-shutdowns:
		t.Fatal("client shut down above its limit")
	case <-time.After(300 * time.Millisecond):
	}

	// A different UPS reaching its limit does not affect this client.
	handleClientShutdowns(conf, ev("ups2", 10, true))

	handleClientShutdowns(conf, ev("ups1", 25, true))
	select {
	case <-shutdowns:
	case <-time.After(5 * time.Second):
		t.Fatal("client never received the shutdown order")
	}
	waitFor(t, "server to receive the ack", func() bool {
		stateMu.Lock()
		defer stateMu.Unlock()
		s := clientStates["desk"]
		return s != nil && s.acked
	})

	// Power back: the server re-arms the client.
	handleClientShutdowns(conf, ev("ups1", 40, false))
	stateMu.Lock()
	_, still := clientStates["desk"]
	stateMu.Unlock()
	if still {
		t.Error("client state not reset after power returned")
	}
}

func TestLinkRejectsWrongKey(t *testing.T) {
	_, code := startTestServer(t, newClientKey())
	cli := clientSettingsFromCode(t, code)
	cli.Key = newClientKey()
	_, err := dialServer(cli)
	if err == nil || !strings.Contains(err.Error(), "wrong key") {
		t.Fatalf("expected wrong key refusal, got %v", err)
	}
}

func TestLinkRejectsWrongServer(t *testing.T) {
	_, code := startTestServer(t, newClientKey())
	cli := clientSettingsFromCode(t, code)
	cli.ServerFingerprint = strings.Repeat("ab", 32) // a different (impostor) server
	_, err := dialServer(cli)
	if err == nil || !strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("expected fingerprint mismatch, got %v", err)
	}
}

func TestPairingCodeRoundTrip(t *testing.T) {
	code := pairingCode("192.168.1.5:3494", "Gaming PC", "k3y_-", strings.Repeat("0f", 32))
	addr, id, key, fp, err := parsePairingCode(code)
	if err != nil || addr != "192.168.1.5:3494" || id != "Gaming PC" || key != "k3y_-" || fp != strings.Repeat("0f", 32) {
		t.Fatalf("round trip failed: %q %q %q %q %v", addr, id, key, fp, err)
	}
	// Line breaks picked up when copying a wrapped code are ignored.
	if a, _, _, _, err := parsePairingCode(code[:20] + "\n  " + code[20:]); err != nil || a != addr {
		t.Errorf("wrapped code not accepted: %v", err)
	}
	if _, _, _, _, err := parsePairingCode("https://example.com"); err == nil {
		t.Error("expected error for a non-pairing URL")
	}
}

func TestCheckClients(t *testing.T) {
	key := newClientKey()
	srv, code := startTestServer(t, key)
	srv.s.Server.Machines = append(srv.s.Server.Machines,
		Machine{Name: "laptop", Type: MachineClient, UPS: "ups1", ShutdownAt: 20, ClientKey: newClientKey(), SkipShutdown: true})

	cli := clientSettingsFromCode(t, code)
	cliStore := &settingsStore{
		s:       Settings{Version: settingsVersion, Role: "client", Client: &cli},
		path:    filepath.Join(t.TempDir(), "settings.json"),
		changed: make(chan struct{}, 1),
	}
	go runClientLink(cliStore, cli)
	waitFor(t, "client to connect", func() bool { return hub.get("desk") != nil })

	var buf bytes.Buffer
	log.SetOutput(&buf)
	checkClients(srv.Get())
	log.SetOutput(os.Stderr)
	out := buf.String()

	host, _ := os.Hostname()
	for _, want := range []string{
		"desk (client): OK, connected from 127.0.0.1",
		"(" + host + ", version " + version + ")",
		"Shuts down at 30% on ups1.",
		"laptop (client): NOT CONNECTED",
		"reach this server on TCP",
		"Automatic shutdown is turned off for it.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("check output is missing %q:\n%s", want, out)
		}
	}
}
