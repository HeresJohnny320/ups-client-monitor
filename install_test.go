package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeSystem points the installer at a temporary root and records the commands it runs.
func fakeSystem(t *testing.T, systemd bool) (root string, calls *[]string) {
	root = t.TempDir()
	if systemd {
		os.MkdirAll(filepath.Join(root, "run/systemd/system"), 0755)
	}
	var recorded []string
	record := func(name string, args ...string) error {
		recorded = append(recorded, name+" "+strings.Join(args, " "))
		return nil
	}
	oldRoot, oldRun, oldQuiet := pfRoot, pfRun, pfRunQuiet
	pfRoot, pfRun, pfRunQuiet = root, record, record
	t.Cleanup(func() { pfRoot, pfRun, pfRunQuiet = oldRoot, oldRun, oldQuiet })
	return root, &recorded
}

func TestInstallWithSystemd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd is Linux-only")
	}
	root, calls := fakeSystem(t, true)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // no old settings to migrate

	if err := installService(); err != nil {
		t.Fatal(err)
	}
	checkInstalled(t, filepath.Join(root, "usr/local/bin/ups-monitor"), 0755)
	checkInstalled(t, filepath.Join(root, serviceSettingsDir), 0700)
	unit, err := os.ReadFile(filepath.Join(root, systemdUnitPath))
	if err != nil || !strings.Contains(string(unit), "ExecStart=/usr/local/bin/ups-monitor -daemon -config-dir /usr/local/etc/ups-monitor") ||
		!strings.Contains(string(unit), "Restart=always") || !strings.Contains(string(unit), "WantedBy=multi-user.target") {
		t.Fatalf("unit file wrong: %v\n%s", err, unit)
	}
	want := "systemctl stop ups-monitor\nsystemctl daemon-reload\nsystemctl enable ups-monitor\nsystemctl restart ups-monitor"
	if got := strings.Join(*calls, "\n"); got != want {
		t.Errorf("ran:\n%s\nwant:\n%s", got, want)
	}

	// Re-running it is an update: the program is replaced, settings stay.
	os.WriteFile(filepath.Join(root, serviceSettingsDir, "settings.json"), []byte(`{"keep":"me"}`), 0600)
	os.WriteFile(filepath.Join(root, "usr/local/bin/ups-monitor"), []byte("old version"), 0755)
	*calls = nil
	if err := installService(); err != nil {
		t.Fatalf("re-running the installer: %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "usr/local/bin/ups-monitor")); string(data) == "old version" {
		t.Error("re-running the installer didn't replace the old program")
	}
	if data, _ := os.ReadFile(filepath.Join(root, serviceSettingsDir, "settings.json")); string(data) != `{"keep":"me"}` {
		t.Error("re-running the installer changed the settings")
	}

	*calls = nil
	if err := uninstallService(); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{systemdUnitPath, "usr/local/bin/ups-monitor"} {
		if _, err := os.Stat(filepath.Join(root, gone)); err == nil {
			t.Errorf("%s still there after uninstalling", gone)
		}
	}
	if _, err := os.Stat(filepath.Join(root, serviceSettingsDir, "settings.json")); err != nil {
		t.Error("uninstalling removed the settings")
	}
	if !strings.Contains(strings.Join(*calls, "\n"), "systemctl disable --now ups-monitor") {
		t.Errorf("uninstall didn't disable the service: %v", *calls)
	}
}

func TestInstallWithoutSystemd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip()
	}
	root, calls := fakeSystem(t, false) // e.g. Alpine or a container
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := installService(); err != nil {
		t.Fatal(err)
	}
	checkInstalled(t, filepath.Join(root, "usr/local/bin/ups-monitor"), 0755)
	if _, err := os.Stat(filepath.Join(root, systemdUnitPath)); err == nil {
		t.Error("wrote a systemd unit on a system without systemd")
	}
	if len(*calls) != 0 {
		t.Errorf("ran service commands without a service manager: %v", *calls)
	}
}

func TestSettingsMigration(t *testing.T) {
	root, _ := fakeSystem(t, false)
	older, newer := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(older, "settings.json"), []byte("older"), 0600)
	os.WriteFile(filepath.Join(newer, "settings.json"), []byte("newer"), 0600)
	os.WriteFile(filepath.Join(newer, "link-key.pem"), []byte("key"), 0600)
	past := time.Now().Add(-time.Hour)
	os.Chtimes(filepath.Join(older, "settings.json"), past, past)

	if from, err := migrateSettings(serviceSettingsDir, []string{older, newer}); err != nil || from != newer {
		t.Fatalf("copied from %q, want the most recently changed folder", from)
	}
	if data, _ := os.ReadFile(filepath.Join(root, serviceSettingsDir, "settings.json")); string(data) != "newer" {
		t.Error("settings weren't copied")
	}
	if data, _ := os.ReadFile(filepath.Join(root, serviceSettingsDir, "link-key.pem")); string(data) != "key" {
		t.Error("the server identity wasn't copied, so clients would have to pair again")
	}
	// Once the service has settings, nothing is copied over them.
	os.WriteFile(filepath.Join(older, "settings.json"), []byte("changed later"), 0600)
	if from, _ := migrateSettings(serviceSettingsDir, []string{older}); from != "" {
		t.Error("overwrote the service's existing settings")
	}
}

func TestServiceDefinitions(t *testing.T) {
	mac := installTargetFor("darwin")
	plist := launchdPlistXML(mac)
	for _, want := range []string{"<string>com.upsmonitor</string>", "<string>/usr/local/bin/ups-monitor</string>",
		"<string>-daemon</string>", "<string>/usr/local/etc/ups-monitor</string>", "<key>KeepAlive</key>"} {
		if !strings.Contains(plist, want) {
			t.Errorf("launchd plist is missing %s", want)
		}
	}
	win := installTarget{bin: `C:\Program Files\UPS Monitor\ups-monitor.exe`, dir: `C:\ProgramData\ups-monitor`, manager: "schtasks"}
	got := strings.Join(windowsTaskArgs(win), " ")
	want := `/Create /TN UPS Monitor /TR "C:\Program Files\UPS Monitor\ups-monitor.exe" -daemon -config-dir "C:\ProgramData\ups-monitor" /SC ONSTART /RU SYSTEM /RL HIGHEST /F`
	if got != want {
		t.Errorf("Windows task:\n got  %s\n want %s", got, want)
	}
	if installTargetFor("windows").manager != "schtasks" || installTargetFor("darwin").manager != "launchd" {
		t.Error("wrong service manager")
	}
}

// The Windows installer failed with "mkdir \C:": system paths were joined onto "/".
func TestSystemPathsAreUsedAsIs(t *testing.T) {
	if pfRoot != "/" {
		t.Fatal("pfRoot should be / outside the install tests")
	}
	if runtime.GOOS == "windows" {
		for _, p := range []string{windowsSettingsDir(), installTargetFor("windows").bin, `C:\ProgramData\ups-monitor`} {
			if got := pfPath(p); got != p {
				t.Errorf("pfPath(%q) = %q, want it unchanged", p, got)
			}
		}
		return
	}
	cases := map[string]string{"/usr/local/etc/ups-monitor": "/usr/local/etc/ups-monitor", "usr/local/bin/ups-monitor": "/usr/local/bin/ups-monitor"}
	for in, want := range cases {
		if got := pfPath(in); got != want {
			t.Errorf("pfPath(%q) = %q, want %q", in, got, want)
		}
	}
}
