package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// settings.json (version 2) replaces the old config.json, which is no longer read.
//
// Settings hold only what the user chooses. Runtime data (self-test dates) lives in state.json.

const settingsVersion = 2

const (
	MachineProxmox  = "proxmox"
	MachineTrueNAS  = "truenas"
	MachineClient   = "client"    // a computer running this app in client mode
	MachineWakeOnly = "wake_only" // only woken after outages, never shut down
)

var machineTypes = []string{MachineProxmox, MachineTrueNAS, MachineClient, MachineWakeOnly}

type Settings struct {
	Version int             `json:"version"`
	Role    string          `json:"role"` // "server" or "client"
	Server  *ServerSettings `json:"server,omitempty"`
	Client  *ClientSettings `json:"client,omitempty"`
}

type ServerSettings struct {
	NUT              NUTSettings `json:"nut"`
	PollSeconds      int         `json:"poll_seconds"`
	ListenPort       int         `json:"listen_port"`        // port clients connect to
	WakeAfterRestart bool        `json:"wake_after_restart"` // the server may have lost power too
	WebhookURL       string      `json:"webhook_url"`        // default Discord webhook for every event
	Machines         []Machine   `json:"machines"`
	SelfTests        SelfTests   `json:"self_tests"`
}

type NUTSettings struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username,omitempty"` // only needed for self-tests on most setups
	Password string `json:"password,omitempty"`
}

// Machine is anything the server powers off and/or wakes, tied to the UPS that feeds it.
type Machine struct {
	Name       string        `json:"name"`
	Type       string        `json:"type"`
	UPS        string        `json:"ups"`
	ShutdownAt int           `json:"shutdown_at_percent,omitempty"` // shut down when on battery at or below this
	Proxmox    *ProxmoxAPI   `json:"proxmox,omitempty"`
	TrueNAS    *TrueNASAPI   `json:"truenas,omitempty"`
	ClientKey  string        `json:"client_key,omitempty"`
	Wake       *WakeSettings `json:"wake,omitempty"`
	WebhookURL string        `json:"webhook_url,omitempty"` // overrides the server's default webhook
}

type ProxmoxAPI struct {
	Host      string `json:"host"`  // https://192.168.1.10:8006
	Node      string `json:"node"`  // node name shown in the Proxmox UI
	Token     string `json:"token"` // USER@REALM!TOKENID=SECRET
	VerifyTLS bool   `json:"verify_tls"`
}

type TrueNASAPI struct {
	Host      string `json:"host"` // must be https://
	Username  string `json:"username"`
	APIKey    string `json:"api_key"`
	VerifyTLS bool   `json:"verify_tls"`
}

type WakeSettings struct {
	MAC       string `json:"mac"`
	AtPercent int    `json:"at_percent"`          // wake once recharged to at least this
	Broadcast string `json:"broadcast,omitempty"` // defaults to 255.255.255.255
}

type SelfTests struct {
	Enabled     bool     `json:"enabled"`
	Type        string   `json:"type"` // quick or deep
	EveryMonths int      `json:"every_months"`
	UPS         []string `json:"ups"`
	WebhookURL  string   `json:"webhook_url,omitempty"` // defaults to the server webhook
}

type ClientSettings struct {
	ServerAddress     string `json:"server_address"`
	ServerFingerprint string `json:"server_fingerprint"`
	Name              string `json:"name"`
	Key               string `json:"key"`
	FailsafeSeconds   int    `json:"failsafe_seconds"` // shut down if the server is silent this long while on battery (0 = off)
	WebhookURL        string `json:"webhook_url"`
}

func (s *Settings) isServer() bool { return s.Role == "server" && s.Server != nil }
func (s *Settings) isClient() bool { return s.Role == "client" && s.Client != nil }

// webhookFor returns the machine's own webhook, or the server default.
func (s *ServerSettings) webhookFor(m Machine) string {
	if m.WebhookURL != "" {
		return m.WebhookURL
	}
	return s.WebhookURL
}

func (s *ServerSettings) testWebhook() string {
	if s.SelfTests.WebhookURL != "" {
		return s.SelfTests.WebhookURL
	}
	return s.WebhookURL
}

// shutsDown reports whether the server powers this machine off (everything but wake_only).
func (m Machine) shutsDown() bool { return m.Type != MachineWakeOnly }

// apiTarget adapts a Proxmox/TrueNAS machine to the remote shutdown code.
func (m Machine) apiTarget() ShutdownTarget {
	t := ShutdownTarget{Name: m.Name, Type: m.Type}
	if m.Proxmox != nil {
		t.Host, t.Node, t.Token, t.VerifyTLS = m.Proxmox.Host, m.Proxmox.Node, m.Proxmox.Token, m.Proxmox.VerifyTLS
	}
	if m.TrueNAS != nil {
		t.Host, t.Username, t.Token, t.VerifyTLS = m.TrueNAS.Host, m.TrueNAS.Username, m.TrueNAS.APIKey, m.TrueNAS.VerifyTLS
	}
	return t
}

// ---------- Files ----------

// appDir is the settings folder (settings.json, state.json, activity.log, control socket).
// It defaults to the user config dir and can be overridden with -config-dir.
var appDir string

func defaultAppDir() string {
	baseDir, _ := os.UserConfigDir()
	return filepath.Join(baseDir, "ups-monitor")
}

func settingsPath() string { return filepath.Join(appDir, "settings.json") }
func statePath() string    { return filepath.Join(appDir, "state.json") }

// legacyConfigExists reports an old config.json, which this version ignores.
func legacyConfigExists() bool {
	_, err := os.Stat(filepath.Join(appDir, "config.json"))
	return err == nil
}

func newServerSettings() *ServerSettings {
	return &ServerSettings{
		NUT:              NUTSettings{Host: "127.0.0.1", Port: 3493},
		PollSeconds:      10,
		ListenPort:       defaultLinkPort,
		WakeAfterRestart: true,
		SelfTests:        SelfTests{Type: "quick", EveryMonths: 3},
	}
}

func newClientSettings() *ClientSettings {
	return &ClientSettings{FailsafeSeconds: 60}
}

// newSettings returns the starting settings for a freshly chosen role.
func newSettings(role string) Settings {
	s := Settings{Version: settingsVersion, Role: role}
	if role == "server" {
		s.Server = newServerSettings()
	} else {
		s.Client = newClientSettings()
	}
	return s
}

// readSettingsFile loads settings.json. A missing file returns os.ErrNotExist.
func readSettingsFile(path string) (Settings, error) {
	var s Settings
	data, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("could not parse %s: %w", path, err)
	}
	if s.Version != settingsVersion {
		return s, fmt.Errorf("%s has version %d, this program expects %d", path, s.Version, settingsVersion)
	}
	normalizeSettings(&s)
	if err := validateSettings(&s); err != nil {
		return s, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

func normalizeSettings(s *Settings) {
	s.Version = settingsVersion
	s.Role = strings.ToLower(strings.TrimSpace(s.Role))
	if s.Role == "server" && s.Server == nil {
		s.Server = newServerSettings()
	}
	if s.Role == "client" && s.Client == nil {
		s.Client = newClientSettings()
	}
	if srv := s.Server; srv != nil {
		if srv.NUT.Port == 0 {
			srv.NUT.Port = 3493
		}
		if srv.PollSeconds <= 0 {
			srv.PollSeconds = 10
		}
		if srv.ListenPort == 0 {
			srv.ListenPort = defaultLinkPort
		}
		if srv.SelfTests.Type == "" {
			srv.SelfTests.Type = "quick"
		}
		for i := range srv.Machines {
			m := &srv.Machines[i]
			m.Type = strings.ToLower(strings.TrimSpace(m.Type))
			// Drop blocks that don't belong to the machine's type (e.g. after changing it in the UI).
			if m.Type != MachineProxmox {
				m.Proxmox = nil
			}
			if m.Type != MachineTrueNAS {
				m.TrueNAS = nil
			}
			if m.Type != MachineClient {
				m.ClientKey = ""
			}
			if m.Type == MachineWakeOnly {
				m.ShutdownAt = 0
			}
			if m.Wake != nil && strings.TrimSpace(m.Wake.MAC) == "" && m.Type != MachineWakeOnly {
				m.Wake = nil
			}
		}
	}
	if c := s.Client; c != nil {
		c.ServerFingerprint = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(c.ServerFingerprint), ":", ""))
	}
}

func validateSettings(s *Settings) error {
	switch s.Role {
	case "server":
		return validateServer(s.Server)
	case "client":
		return validateClient(s.Client)
	}
	return fmt.Errorf("invalid role %q (use \"server\" or \"client\")", s.Role)
}

func validatePercent(what string, p int) error {
	if p < 0 || p > 100 {
		return fmt.Errorf("%s must be 0-100%%, got %d", what, p)
	}
	return nil
}

func validateServer(srv *ServerSettings) error {
	if strings.TrimSpace(srv.NUT.Host) == "" {
		return errors.New("NUT host is empty")
	}
	if srv.NUT.Port < 1 || srv.NUT.Port > 65535 {
		return fmt.Errorf("NUT port must be 1-65535, got %d", srv.NUT.Port)
	}
	if srv.ListenPort < 1 || srv.ListenPort > 65535 {
		return fmt.Errorf("client port must be 1-65535, got %d", srv.ListenPort)
	}
	if t := srv.SelfTests; t.Type != "quick" && t.Type != "deep" {
		return fmt.Errorf("self-test type must be quick or deep, got %q", t.Type)
	}
	if srv.SelfTests.Enabled && srv.SelfTests.EveryMonths < 1 {
		return errors.New("self-tests must run at least every 12 months (every_months ≥ 1)")
	}

	names := make(map[string]bool)
	for _, m := range srv.Machines {
		if strings.TrimSpace(m.Name) == "" {
			return errors.New("a machine has no name")
		}
		if names[m.Name] {
			return fmt.Errorf("two machines are named %q", m.Name)
		}
		names[m.Name] = true
		if m.UPS == "" {
			return fmt.Errorf("%s: choose the UPS that powers it", m.Name)
		}
		if err := validatePercent(m.Name+": shutdown battery", m.ShutdownAt); err != nil {
			return err
		}
		switch m.Type {
		case MachineProxmox:
			p := m.Proxmox
			if p == nil || p.Host == "" || p.Node == "" || p.Token == "" {
				return fmt.Errorf("%s: Proxmox needs host, node name and API token", m.Name)
			}
			if _, err := normalizeHost(p.Host, ""); err != nil {
				return fmt.Errorf("%s: %w", m.Name, err)
			}
		case MachineTrueNAS:
			t := m.TrueNAS
			if t == nil || t.Host == "" || t.Username == "" || t.APIKey == "" {
				return fmt.Errorf("%s: TrueNAS needs host, username and API key", m.Name)
			}
			u, err := normalizeHost(t.Host, "")
			if err != nil {
				return fmt.Errorf("%s: %w", m.Name, err)
			}
			if u.Scheme != "https" {
				return fmt.Errorf("%s: TrueNAS host must use https (API keys sent over http are revoked)", m.Name)
			}
		case MachineClient:
			if len(m.ClientKey) < 16 {
				return fmt.Errorf("%s: client key is missing", m.Name)
			}
		case MachineWakeOnly:
			if m.Wake == nil {
				return fmt.Errorf("%s: a wake-only machine needs a Wake-on-LAN MAC", m.Name)
			}
		default:
			return fmt.Errorf("%s: unknown type %q", m.Name, m.Type)
		}
		if w := m.Wake; w != nil {
			if _, err := net.ParseMAC(w.MAC); err != nil {
				return fmt.Errorf("%s: invalid MAC address %q", m.Name, w.MAC)
			}
			if err := validatePercent(m.Name+": wake battery", w.AtPercent); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateClient(c *ClientSettings) error {
	if c.FailsafeSeconds < 0 {
		return errors.New("failsafe seconds cannot be negative")
	}
	if c.ServerAddress == "" {
		return nil // not paired yet
	}
	if c.Name == "" || c.Key == "" {
		return errors.New("client name and key are required; paste the pairing code from the server")
	}
	if fp, err := hex.DecodeString(c.ServerFingerprint); err != nil || len(fp) != 32 {
		return errors.New("server fingerprint must be 64 hex characters; paste the pairing code from the server")
	}
	return nil
}

// cloneSettings deep-copies settings so the UI and monitor never share mutable data.
func cloneSettings(s Settings) Settings {
	out := s
	if s.Server != nil {
		srv := *s.Server
		srv.SelfTests.UPS = append([]string(nil), s.Server.SelfTests.UPS...)
		srv.Machines = make([]Machine, len(s.Server.Machines))
		for i, m := range s.Server.Machines {
			srv.Machines[i] = m.clone()
		}
		out.Server = &srv
	}
	if s.Client != nil {
		c := *s.Client
		out.Client = &c
	}
	return out
}

func (m Machine) clone() Machine {
	if m.Proxmox != nil {
		p := *m.Proxmox
		m.Proxmox = &p
	}
	if m.TrueNAS != nil {
		t := *m.TrueNAS
		m.TrueNAS = &t
	}
	if m.Wake != nil {
		w := *m.Wake
		m.Wake = &w
	}
	return m
}

func writePrivateJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	os.MkdirAll(filepath.Dir(path), 0755)
	// Settings hold passwords and API tokens, so keep them private.
	if err := os.WriteFile(path, data, 0600); err != nil {
		return err
	}
	return os.Chmod(path, 0600)
}

// ---------- Live settings ----------

// settingsStore holds the live settings shared by the monitor loop and the control socket.
type settingsStore struct {
	mu      sync.Mutex
	s       Settings
	path    string
	modTime time.Time
	badMod  time.Time     // modTime of an on-disk edit that failed to load, so it is logged once
	changed chan struct{} // wakes the monitor loop after an edit
	state   *stateStore
}

// openSettingsStore loads settings.json. A missing file starts the monitor unconfigured (empty role).
func openSettingsStore(path string) (*settingsStore, error) {
	st := &settingsStore{path: path, changed: make(chan struct{}, 1), state: openStateStore(statePath())}
	s, err := readSettingsFile(path)
	if errors.Is(err, os.ErrNotExist) {
		st.s = Settings{Version: settingsVersion}
		if legacyConfigExists() {
			log.Println("[CONFIG] Found config.json from an older version. It is no longer read (and left untouched); set up again in the UI.")
		}
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	st.s = s
	if fi, err := os.Stat(path); err == nil {
		st.modTime = fi.ModTime()
	}
	return st, nil
}

func (st *settingsStore) Get() Settings {
	st.mu.Lock()
	defer st.mu.Unlock()
	return cloneSettings(st.s)
}

// Set validates, saves, and applies new settings immediately.
func (st *settingsStore) Set(s Settings) (Settings, error) {
	s = cloneSettings(s)
	normalizeSettings(&s)
	if err := validateSettings(&s); err != nil {
		return Settings{}, err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := writePrivateJSON(st.path, s); err != nil {
		return Settings{}, fmt.Errorf("saving settings: %w", err)
	}
	if fi, err := os.Stat(st.path); err == nil {
		st.modTime = fi.ModTime()
	}
	st.s = s
	if s.isServer() {
		// Start the schedule now instead of testing a newly added UPS immediately.
		st.state.startSchedule(s.Server.SelfTests.UPS)
	}
	st.notify()
	return cloneSettings(s), nil
}

// ReloadIfChanged picks up hand edits to settings.json while the monitor runs.
func (st *settingsStore) ReloadIfChanged() {
	fi, err := os.Stat(st.path)
	if err != nil {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if fi.ModTime().Equal(st.modTime) || fi.ModTime().Equal(st.badMod) {
		return
	}
	s, err := readSettingsFile(st.path)
	if err != nil {
		st.badMod = fi.ModTime()
		log.Printf("[CONFIG] Ignoring edited settings.json, keeping current settings: %v", err)
		return
	}
	st.s = s
	st.modTime = fi.ModTime()
	log.Println("[CONFIG] settings.json changed on disk; reloaded.")
	st.notify()
}

func (st *settingsStore) notify() {
	select {
	case st.changed <- struct{}{}:
	default:
	}
}

// wait sleeps for d, returning early when the settings are edited.
func (st *settingsStore) wait(d time.Duration) {
	select {
	case <-time.After(d):
	case <-st.changed:
	}
}

// ---------- Runtime state ----------

type State struct {
	LastSelfTest map[string]time.Time `json:"last_self_test"`
}

type stateStore struct {
	mu   sync.Mutex
	path string
	s    State
}

func openStateStore(path string) *stateStore {
	st := &stateStore{path: path, s: State{LastSelfTest: map[string]time.Time{}}}
	if data, err := os.ReadFile(path); err == nil {
		json.Unmarshal(data, &st.s)
		if st.s.LastSelfTest == nil {
			st.s.LastSelfTest = map[string]time.Time{}
		}
	}
	return st
}

func (st *stateStore) LastTest(ups string) time.Time {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.s.LastSelfTest[ups]
}

func (st *stateStore) Snapshot() map[string]time.Time {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make(map[string]time.Time, len(st.s.LastSelfTest))
	for k, v := range st.s.LastSelfTest {
		out[k] = v
	}
	return out
}

func (st *stateStore) MarkTested(ups string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.s.LastSelfTest[ups] = time.Now()
	st.saveLocked()
}

// startSchedule records "now" for UPSes that have never been tested.
func (st *stateStore) startSchedule(upsNames []string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	changed := false
	for _, u := range upsNames {
		if _, ok := st.s.LastSelfTest[u]; !ok {
			st.s.LastSelfTest[u] = time.Now()
			changed = true
		}
	}
	if changed {
		st.saveLocked()
	}
}

func (st *stateStore) saveLocked() {
	if err := writePrivateJSON(st.path, st.s); err != nil {
		log.Printf("[ERROR] Saving state: %v", err)
	}
}
