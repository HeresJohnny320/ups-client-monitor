package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The background monitor listens on a private Unix socket in the settings folder.
// Running the app again in a terminal connects to it to view status and edit settings live.

type ctlRequest struct {
	Cmd      string    `json:"cmd"` // ping, get_settings, set_settings, status, action, link_info, pairing, test_machine, wake, stop
	Settings *Settings `json:"settings,omitempty"`
	Action   string    `json:"action,omitempty"`
	Machine  *Machine  `json:"machine,omitempty"` // test_machine, wake (may be unsaved, straight from a form)
	Name     string    `json:"name,omitempty"`    // pairing: the client machine's name
}

type ctlResponse struct {
	OK       bool        `json:"ok"`
	Error    string      `json:"error,omitempty"`
	Settings *Settings   `json:"settings,omitempty"`
	Status   *StatusInfo `json:"status,omitempty"`
	Link     *LinkInfo   `json:"link,omitempty"`
	Pairing  []Pairing   `json:"pairing,omitempty"`
	Info     string      `json:"info,omitempty"`
}

// Pairing is a client's pairing code for one of the server's addresses.
type Pairing struct {
	Address string `json:"address"`
	Code    string `json:"code"`
}

type UPSInfo struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	Charge      int    `json:"charge"`
	ChargeKnown bool   `json:"charge_known"`
}

// MachineInfo is one machine's plan and live state, for the status screen.
type MachineInfo struct {
	Name          string `json:"name"`
	Type          string `json:"type"`
	UPS           string `json:"ups"`
	ShutdownAt    int    `json:"shutdown_at"`
	ShutdownState string `json:"shutdown_state,omitempty"` // empty for wake-only machines
	WakeAt        int    `json:"wake_at"`
	WakeState     string `json:"wake_state,omitempty"` // empty without Wake-on-LAN
}

// LinkInfo is what the server UI needs to build pairing codes.
type LinkInfo struct {
	Fingerprint string   `json:"fingerprint"`
	Port        int      `json:"port"`
	Addresses   []string `json:"addresses"`
}

type StatusInfo struct {
	PID          int                  `json:"pid"`
	Version      string               `json:"version"`
	Started      time.Time            `json:"started"`
	SettingsPath string               `json:"settings_path"`
	Role         string               `json:"role"`
	Source       string               `json:"source"` // "server" for linked clients, otherwise NUT
	NutHost      string               `json:"nut_host"`
	NutConnected bool                 `json:"nut_connected"`
	NutError     string               `json:"nut_error"`
	LastPoll     time.Time            `json:"last_poll"`
	UPS          []UPSInfo            `json:"ups"`
	Machines     []MachineInfo        `json:"machines"`
	LastSelfTest map[string]time.Time `json:"last_self_test,omitempty"`
	Logs         []string             `json:"logs"`
}

var monitorStarted = time.Now()

func socketPath() string {
	p := filepath.Join(appDir, "control.sock")
	if len(p) < 100 {
		return p
	}
	// Unix socket paths are limited to ~104 bytes, so long settings paths use a short private dir.
	sum := sha256.Sum256([]byte(appDir))
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("ups-monitor-%x", sum[:6]))
	os.MkdirAll(dir, 0700)
	return filepath.Join(dir, "control.sock")
}

// logRing keeps the last lines written to the log for the UI.
type logRing struct {
	mu    sync.Mutex
	lines []string
	part  string
}

var recentLogs = &logRing{}

func (r *logRing) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	text := r.part + string(p)
	parts := strings.Split(text, "\n")
	r.part = parts[len(parts)-1]
	r.lines = append(r.lines, parts[:len(parts)-1]...)
	if over := len(r.lines) - 200; over > 0 {
		r.lines = append([]string(nil), r.lines[over:]...)
	}
	return len(p), nil
}

func (r *logRing) Lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}

// listenControl claims the control socket, failing if another monitor already owns it.
func listenControl() (net.Listener, error) {
	path := socketPath()
	if _, err := ctlCall(ctlRequest{Cmd: "ping"}); err == nil {
		return nil, errors.New("another UPS monitor is already running for this settings folder")
	}
	os.Remove(path) // stale socket from a crash
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	os.Chmod(path, 0600)
	return ln, nil
}

func serveControl(ln net.Listener, store *settingsStore, stop chan struct{}) {
	var stopOnce sync.Once
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func(conn net.Conn) {
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(30 * time.Second))
			var req ctlRequest
			if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&req); err != nil {
				return
			}
			resp := handleControl(req, store)
			json.NewEncoder(conn).Encode(resp)
			if req.Cmd == "stop" {
				stopOnce.Do(func() { close(stop) })
			}
		}(conn)
	}
}

func handleControl(req ctlRequest, store *settingsStore) ctlResponse {
	switch req.Cmd {
	case "ping", "stop":
		return ctlResponse{OK: true}
	case "get_settings":
		s := store.Get()
		return ctlResponse{OK: true, Settings: &s}
	case "set_settings":
		if req.Settings == nil {
			return ctlResponse{Error: "no settings sent"}
		}
		s, err := store.Set(*req.Settings)
		if err != nil {
			return ctlResponse{Error: err.Error()}
		}
		log.Printf("[CONFIG] Settings updated from the UI (%s mode).", strings.ToUpper(s.Role))
		return ctlResponse{OK: true, Settings: &s}
	case "status":
		st := statusSnapshot(store.Get())
		st.LastSelfTest = store.state.Snapshot()
		return ctlResponse{OK: true, Status: &st}
	case "link_info":
		fp, err := hub.identity()
		if err != nil {
			return ctlResponse{Error: err.Error()}
		}
		port := defaultLinkPort
		if s := store.Get(); s.isServer() {
			port = s.Server.ListenPort
		}
		return ctlResponse{OK: true, Link: &LinkInfo{Fingerprint: fp, Port: port, Addresses: lanAddresses()}}
	case "pairing":
		codes, err := pairingCodes(store, req.Name)
		if err != nil {
			return ctlResponse{Error: err.Error()}
		}
		return ctlResponse{OK: true, Pairing: codes}
	case "test_machine":
		if req.Machine == nil {
			return ctlResponse{Error: "no machine sent"}
		}
		m := req.Machine.clone()
		m.Type = strings.ToLower(m.Type)
		if m.Type != MachineProxmox && m.Type != MachineTrueNAS {
			return ctlResponse{Error: "only Proxmox and TrueNAS machines have a login to test"}
		}
		info, err := checkRemoteTarget(m.apiTarget())
		if err != nil {
			return ctlResponse{Error: err.Error()}
		}
		return ctlResponse{OK: true, Info: info}
	case "wake":
		if req.Machine == nil || req.Machine.Wake == nil || req.Machine.Wake.MAC == "" {
			return ctlResponse{Error: "fill in the Wake-on-LAN MAC first"}
		}
		w := req.Machine.Wake
		if err := wakeNode(w.MAC, w.Broadcast); err != nil {
			return ctlResponse{Error: err.Error()}
		}
		return ctlResponse{OK: true, Info: "Magic packet sent to " + w.MAC}
	case "action":
		s := store.Get()
		if s.Role == "" {
			return ctlResponse{Error: "the monitor is not configured yet"}
		}
		if s.isClient() && req.Action != "test-webhook" {
			return ctlResponse{Error: "clients can only send test webhooks; the server runs everything else"}
		}
		switch req.Action {
		case "test":
			go runSelfTest(store, "quick")
		case "test-long":
			go runSelfTest(store, "deep")
		case "test-webhook":
			go testAllWebhooks(s)
		case "check-targets":
			go checkShutdownTargets(s)
		default:
			return ctlResponse{Error: "unknown action " + req.Action}
		}
		return ctlResponse{OK: true}
	}
	return ctlResponse{Error: "unknown command " + req.Cmd}
}

func statusSnapshot(s Settings) StatusInfo {
	stateMu.Lock()
	defer stateMu.Unlock()

	st := StatusInfo{
		PID:          os.Getpid(),
		Version:      version,
		Started:      monitorStarted,
		SettingsPath: settingsPath(),
		Role:         s.Role,
		NutConnected: nutConnected,
		NutError:     nutLastError,
		LastPoll:     lastPollStart,
		Logs:         recentLogs.Lines(),
	}
	for name, r := range upsReadings {
		st.UPS = append(st.UPS, UPSInfo{Name: name, Status: r.Status, Charge: r.Charge, ChargeKnown: r.ChargeKnown})
	}

	if s.isClient() {
		c := s.Client
		st.Source = "server"
		st.NutHost = c.ServerAddress
		st.NutConnected = link.connected
		st.NutError = link.err
		if c.ServerAddress == "" {
			st.NutError = "not paired yet"
		}
		state := "waiting for the server's order"
		if link.shutdownDone {
			state = "shutting down"
		}
		st.Machines = append(st.Machines, MachineInfo{Name: "this machine", Type: MachineClient,
			UPS: link.upsName, ShutdownAt: link.shutdownAt, ShutdownState: state})
	}

	if s.isServer() {
		st.NutHost = fmt.Sprintf("%s:%d", s.Server.NUT.Host, s.Server.NUT.Port)
		for _, m := range s.Server.Machines {
			mi := MachineInfo{Name: m.Name, Type: m.Type, UPS: m.UPS, ShutdownAt: m.ShutdownAt, WakeState: wakeState(m)}
			if m.Wake != nil {
				mi.WakeAt = m.Wake.AtPercent
			}
			switch m.Type {
			case MachineClient:
				mi.ShutdownState = clientLinkState(m.Name)
			case MachineProxmox, MachineTrueNAS:
				mi.ShutdownState = "armed"
				if r := remoteStates[m.Name]; r != nil {
					switch {
					case r.done:
						mi.ShutdownState = "shutdown sent"
					case r.inFlight:
						mi.ShutdownState = "sending shutdown"
					case r.failures > 0:
						mi.ShutdownState = fmt.Sprintf("failed %d time(s), retrying", r.failures)
					}
				}
			}
			st.Machines = append(st.Machines, mi)
		}
	}
	return st
}

// pairingCodes builds a client machine's pairing code for each of the server's addresses.
func pairingCodes(store *settingsStore, name string) ([]Pairing, error) {
	s := store.Get()
	if !s.isServer() {
		return nil, errors.New("pairing codes come from a server")
	}
	var client *Machine
	for _, m := range s.Server.Machines {
		if m.Type == MachineClient && m.Name == name {
			m := m
			client = &m
		}
	}
	if client == nil {
		return nil, fmt.Errorf("no client machine named %q", name)
	}
	fp, err := hub.identity()
	if err != nil {
		return nil, err
	}
	addrs := lanAddresses()
	if len(addrs) == 0 {
		addrs = []string{"SERVER-IP"}
	}
	var out []Pairing
	for _, a := range addrs {
		hostPort := net.JoinHostPort(a, strconv.Itoa(s.Server.ListenPort))
		out = append(out, Pairing{Address: a, Code: pairingCode(hostPort, client.Name, client.ClientKey, fp)})
	}
	return out, nil
}

// ctlCall sends one request to the running monitor.
func ctlCall(req ctlRequest) (ctlResponse, error) {
	var resp ctlResponse
	conn, err := net.DialTimeout("unix", socketPath(), 2*time.Second)
	if err != nil {
		return resp, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return resp, err
	}
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return resp, err
	}
	if !resp.OK {
		return resp, errors.New(resp.Error)
	}
	return resp, nil
}

// spawnMonitor starts a detached background monitor and waits until its socket answers.
func spawnMonitor() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "-daemon", "-config-dir", appDir)
	cmd.SysProcAttr = detachedProcAttr()
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	deadline := time.After(10 * time.Second)
	for {
		if _, err := ctlCall(ctlRequest{Cmd: "ping"}); err == nil {
			return nil
		}
		select {
		case err := <-exited:
			return fmt.Errorf("background monitor exited (%v): %s", err, lastLogLine())
		case <-deadline:
			return errors.New("background monitor did not start within 10s")
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func lastLogLine() string {
	data, err := os.ReadFile(filepath.Join(appDir, "activity.log"))
	if err != nil {
		return "see activity.log"
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	return lines[len(lines)-1]
}
