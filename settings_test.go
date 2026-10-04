package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestOldConfigIsIgnored(t *testing.T) {
	appDir = t.TempDir()
	old := `{"mode":"server","host":"10.0.0.2","username":"u","password":"p","wake_targets":[{"name":"pc","mac":"aa:bb:cc:dd:ee:ff"}]}`
	os.WriteFile(filepath.Join(appDir, "config.json"), []byte(old), 0600)

	store, err := openSettingsStore(settingsPath())
	if err != nil {
		t.Fatal(err)
	}
	if s := store.Get(); s.Role != "" || s.Server != nil {
		t.Fatalf("old config.json leaked into settings: %+v", s)
	}
	if !legacyConfigExists() {
		t.Error("old config.json not detected")
	}
	if data, _ := os.ReadFile(filepath.Join(appDir, "config.json")); string(data) != old {
		t.Error("old config.json was modified")
	}
}

func TestSettingsRoundTripAndPermissions(t *testing.T) {
	appDir = t.TempDir()
	store, _ := openSettingsStore(settingsPath())
	s := newSettings("server")
	s.Server.Machines = []Machine{
		{Name: "pve1", Type: MachineProxmox, UPS: "ups2", ShutdownAt: 50,
			Proxmox: &ProxmoxAPI{Host: "https://10.0.0.5:8006", Node: "pve1", Token: "root@pam!ups=x"},
			Wake:    &WakeSettings{MAC: "aa:bb:cc:dd:ee:01", AtPercent: 80}},
		{Name: "nas", Type: MachineTrueNAS, UPS: "ups2", ShutdownAt: 90,
			TrueNAS: &TrueNASAPI{Host: "https://10.0.0.6", Username: "truenas_admin", APIKey: "k"}},
		{Name: "desk", Type: MachineClient, UPS: "ups1", ShutdownAt: 30, ClientKey: newClientKey()},
		{Name: "printer", Type: MachineWakeOnly, UPS: "ups1", Wake: &WakeSettings{MAC: "aa:bb:cc:dd:ee:02", AtPercent: 20}},
	}
	if _, err := store.Set(s); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(settingsPath())
	if err != nil {
		t.Fatal(err)
	}
	// Windows has no Unix permission bits (the per-user AppData folder keeps it private there).
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0600 {
		t.Fatalf("settings.json should be private (0600), got %v", fi.Mode().Perm())
	}
	back, err := readSettingsFile(settingsPath())
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Server.Machines) != 4 || back.Server.Machines[0].Wake.AtPercent != 80 || back.Server.Machines[1].TrueNAS.Username != "truenas_admin" {
		t.Fatalf("round trip lost data: %+v", back.Server.Machines)
	}
	// Runtime data stays out of the settings file.
	data, _ := os.ReadFile(settingsPath())
	if strings.Contains(string(data), "last_self_test") {
		t.Error("state leaked into settings.json")
	}
}

func TestSettingsValidation(t *testing.T) {
	base := func() Settings {
		s := newSettings("server")
		s.Server.Machines = []Machine{{Name: "pve1", Type: MachineProxmox, UPS: "ups1", ShutdownAt: 50,
			Proxmox: &ProxmoxAPI{Host: "https://10.0.0.5:8006", Node: "pve1", Token: "t"}}}
		return s
	}
	cases := map[string]func(s *Settings){
		"duplicate name":       func(s *Settings) { s.Server.Machines = append(s.Server.Machines, s.Server.Machines[0]) },
		"missing UPS":          func(s *Settings) { s.Server.Machines[0].UPS = "" },
		"proxmox without node": func(s *Settings) { s.Server.Machines[0].Proxmox.Node = "" },
		"bad MAC":              func(s *Settings) { s.Server.Machines[0].Wake = &WakeSettings{MAC: "nope", AtPercent: 50} },
		"percent over 100":     func(s *Settings) { s.Server.Machines[0].ShutdownAt = 120 },
		"truenas over http": func(s *Settings) {
			s.Server.Machines[0] = Machine{Name: "nas", Type: MachineTrueNAS, UPS: "ups1",
				TrueNAS: &TrueNASAPI{Host: "http://10.0.0.6", Username: "a", APIKey: "k"}}
		},
		"wake-only without MAC": func(s *Settings) {
			s.Server.Machines[0] = Machine{Name: "printer", Type: MachineWakeOnly, UPS: "ups1"}
		},
	}
	for name, breakIt := range cases {
		s := base()
		breakIt(&s)
		normalizeSettings(&s)
		if err := validateSettings(&s); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
	s := base()
	normalizeSettings(&s)
	if err := validateSettings(&s); err != nil {
		t.Fatalf("valid settings rejected: %v", err)
	}
}

func TestChangingTypeDropsOtherBlocks(t *testing.T) {
	s := newSettings("server")
	s.Server.Machines = []Machine{{Name: "box", Type: MachineTrueNAS, UPS: "ups1",
		Proxmox: &ProxmoxAPI{Host: "x"}, TrueNAS: &TrueNASAPI{Host: "https://nas", Username: "a", APIKey: "k"}, ClientKey: "leftover"}}
	normalizeSettings(&s)
	m := s.Server.Machines[0]
	if m.Proxmox != nil || m.ClientKey != "" || m.TrueNAS == nil {
		t.Fatalf("stale blocks kept: %+v", m)
	}
}

func TestUnpairedClientIsValid(t *testing.T) {
	s := newSettings("client")
	if err := validateSettings(&s); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(s)
	if strings.Contains(string(data), `"server":`) {
		t.Error("client settings should not contain a server block")
	}
}

func TestWrongVersionRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(path, []byte(`{"version":1,"role":"server"}`), 0600)
	if _, err := readSettingsFile(path); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected a version error, got %v", err)
	}
}
