package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"time"
)

// "ups-monitor -install" sets the program up as a background service that starts at boot:
// systemd on Linux, launchd on macOS, a startup task on Windows, pfSense's own service on
// pfSense, and a plain background process elsewhere. Running it again (or re-running the
// install script) updates it in place and keeps the settings.

const (
	systemdUnitPath = "/etc/systemd/system/ups-monitor.service"
	launchdPlist    = "/Library/LaunchDaemons/com.upsmonitor.plist"
	launchdLabel    = "com.upsmonitor"
	windowsTaskName = "UPS Monitor"
	oldWindowsTask  = "UPSMonitor" // what older README instructions created
)

// pfRunQuiet runs a command whose failure is expected and harmless (stopping something
// that may not exist), without printing its errors. Tests replace it.
var pfRunQuiet = func(name string, args ...string) error {
	return exec.Command(name, args...).Run()
}

// installTarget says where the program and its settings go, and what starts it at boot.
type installTarget struct {
	bin     string // the program, as the system will see it
	dir     string // its settings folder
	manager string // systemd, launchd, schtasks or none
}

func installTargetFor(goos string) installTarget {
	switch goos {
	case "windows":
		pf := os.Getenv("ProgramFiles")
		if pf == "" {
			pf = `C:\Program Files`
		}
		return installTarget{bin: filepath.Join(pf, "UPS Monitor", "ups-monitor.exe"), dir: windowsSettingsDir(), manager: "schtasks"}
	case "darwin":
		return installTarget{bin: "/usr/local/bin/ups-monitor", dir: serviceSettingsDir, manager: "launchd"}
	}
	t := installTarget{bin: "/usr/local/bin/ups-monitor", dir: serviceSettingsDir, manager: "none"}
	if goos == "linux" {
		if fi, err := os.Stat(pfPath("run/systemd/system")); err == nil && fi.IsDir() {
			t.manager = "systemd"
		}
	}
	return t
}

func systemdUnit(t installTarget) string {
	return fmt.Sprintf(`[Unit]
Description=UPS Monitor (shutdown and Wake-on-LAN)
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=%s -daemon -config-dir %s
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
`, t.bin, t.dir)
}

func launchdPlistXML(t installTarget) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>-daemon</string>
        <string>-config-dir</string>
        <string>%s</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
</dict>
</plist>
`, launchdLabel, t.bin, t.dir)
}

// windowsTaskArgs creates the startup task, replacing any earlier one.
func windowsTaskArgs(t installTarget) []string {
	return []string{"/Create", "/TN", windowsTaskName,
		"/TR", fmt.Sprintf(`"%s" -daemon -config-dir "%s"`, t.bin, t.dir),
		"/SC", "ONSTART", "/RU", "SYSTEM", "/RL", "HIGHEST", "/F"}
}

// stopMonitorIn stops a monitor that keeps its settings in dir, if one is running.
func stopMonitorIn(dir string) bool {
	saved := appDir
	defer func() { appDir = saved }()
	appDir = dir
	if _, err := ctlCall(ctlRequest{Cmd: "ping"}); err != nil {
		return false
	}
	ctlCall(ctlRequest{Cmd: "stop"})
	for i := 0; i < 30; i++ {
		time.Sleep(500 * time.Millisecond)
		if _, err := ctlCall(ctlRequest{Cmd: "ping"}); err != nil {
			return true
		}
	}
	return true
}

func monitorRunningIn(dir string) bool {
	saved := appDir
	defer func() { appDir = saved }()
	appDir = dir
	_, err := ctlCall(ctlRequest{Cmd: "ping"})
	return err == nil
}

// oldSettingsDirs lists the per-user settings folders a setup made before installing as a
// service may be in: the current user's, and with sudo, the user who ran sudo.
func oldSettingsDirs() []string {
	var dirs []string
	if base, err := os.UserConfigDir(); err == nil {
		dirs = append(dirs, filepath.Join(base, "ups-monitor"))
	}
	if name := os.Getenv("SUDO_USER"); name != "" && name != "root" {
		if u, err := user.Lookup(name); err == nil {
			sub := ".config"
			if runtime.GOOS == "darwin" {
				sub = filepath.Join("Library", "Application Support")
			}
			dirs = append(dirs, filepath.Join(u.HomeDir, sub, "ups-monitor"))
		}
	}
	return dirs
}

// migrateSettings copies settings from an old per-user folder into the service's folder
// (if it has none yet), so a setup made before installing as a service carries over.
// The most recently changed one wins. It returns the folder it copied from, if any, and
// fails without touching anything else if the copy doesn't work.
func migrateSettings(dir string, candidates []string) (string, error) {
	if _, err := os.Stat(filepath.Join(pfPath(dir), "settings.json")); err == nil {
		return "", nil
	}
	var from string
	var newest time.Time
	for _, c := range candidates {
		if c == dir {
			continue
		}
		if fi, err := os.Stat(filepath.Join(c, "settings.json")); err == nil && fi.ModTime().After(newest) {
			from, newest = c, fi.ModTime()
		}
	}
	if from == "" {
		return "", nil
	}
	if err := os.MkdirAll(pfPath(dir), 0700); err != nil {
		return "", fmt.Errorf("creating %s: %w", dir, err)
	}
	for _, f := range []string{"settings.json", "state.json", "link-cert.pem", "link-key.pem"} {
		src := filepath.Join(from, f)
		if fi, err := os.Stat(src); err == nil {
			if err := copyFile(src, filepath.Join(pfPath(dir), f), fi.Mode().Perm()|0200); err != nil {
				return "", fmt.Errorf("copying your settings from %s: %w", from, err)
			}
		}
	}
	return from, nil
}

func pfSenseDetected() bool {
	if runtime.GOOS != "freebsd" {
		return false
	}
	_, err := os.Stat(pfPath("usr/local/www/guiconfig.inc"))
	return err == nil
}

// installService installs or updates the program as a background service and starts it.
func installService() error {
	if pfSenseDetected() {
		return installPfSense()
	}
	if pfRoot == "/" {
		if err := requireAdmin(); err != nil {
			return err
		}
	}
	t := installTargetFor(runtime.GOOS)
	step := func(msg string) { fmt.Println("  " + msg) }
	fmt.Printf("Installing UPS Monitor %s...\n", version)

	old, err := migrateSettings(t.dir, oldSettingsDirs())
	if err != nil {
		return err // nothing has been stopped or changed yet
	}
	if old != "" {
		step("settings:      copied from " + old)
		stopMonitorIn(old) // the old copy would otherwise keep running alongside
	}
	// A LaunchAgent from the old macOS instructions would start a second copy at login.
	if name := os.Getenv("SUDO_USER"); runtime.GOOS == "darwin" && name != "" {
		if u, err := user.Lookup(name); err == nil {
			agent := filepath.Join(u.HomeDir, "Library", "LaunchAgents", launchdLabel+".plist")
			if _, err := os.Stat(agent); err == nil {
				step("note:          remove the old " + agent + " so it doesn't start a second copy")
			}
		}
	}
	if err := os.MkdirAll(pfPath(t.dir), 0700); err != nil {
		return err
	}
	step("settings:      " + t.dir)

	// Stop what's running so the program file can be replaced.
	switch t.manager {
	case "systemd":
		pfRunQuiet("systemctl", "stop", "ups-monitor")
	case "launchd":
		pfRunQuiet("launchctl", "bootout", "system/"+launchdLabel)
	case "schtasks":
		pfRunQuiet("schtasks", "/End", "/TN", windowsTaskName)
		if pfRunQuiet("schtasks", "/Delete", "/TN", oldWindowsTask, "/F") == nil {
			step("old task:      removed \"" + oldWindowsTask + "\" (replaced by \"" + windowsTaskName + "\")")
		}
	}
	stopMonitorIn(t.dir)

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.EvalSymlinks(exe)
	bin := pfPath(t.bin)
	if exe != bin {
		if err := os.MkdirAll(filepath.Dir(bin), 0755); err != nil {
			return err
		}
		newPath := newBinaryPath(bin)
		if err := copyFile(exe, newPath, 0755); err != nil {
			return fmt.Errorf("copying the program to %s: %w", t.bin, err)
		}
		if _, err := os.Stat(bin); err == nil {
			err = swapBinary(bin, newPath)
		} else {
			err = os.Rename(newPath, bin)
		}
		if err != nil {
			os.Remove(newPath)
			return err
		}
	}
	step("program:       " + t.bin)

	switch t.manager {
	case "systemd":
		if err := writeAtomic(pfPath(systemdUnitPath), []byte(systemdUnit(t)), 0644); err != nil {
			return err
		}
		pfRun("systemctl", "daemon-reload")
		if err := pfRun("systemctl", "enable", "ups-monitor"); err != nil {
			return fmt.Errorf("enabling the service: %w", err)
		}
		if err := pfRun("systemctl", "restart", "ups-monitor"); err != nil {
			return fmt.Errorf("starting the service: %w", err)
		}
		step("service:       systemd unit ups-monitor (systemctl status ups-monitor)")
	case "launchd":
		if err := writeAtomic(pfPath(launchdPlist), []byte(launchdPlistXML(t)), 0644); err != nil {
			return err
		}
		if err := pfRunQuiet("launchctl", "bootstrap", "system", launchdPlist); err != nil {
			if err := pfRun("launchctl", "load", "-w", launchdPlist); err != nil {
				return fmt.Errorf("starting the service: %w", err)
			}
		}
		step("service:       launchd " + launchdLabel + " (starts at boot)")
	case "schtasks":
		if err := pfRun("schtasks", windowsTaskArgs(t)...); err != nil {
			return fmt.Errorf("creating the startup task: %w", err)
		}
		if err := pfRun("schtasks", "/Run", "/TN", windowsTaskName); err != nil {
			return fmt.Errorf("starting the task: %w", err)
		}
		step("service:       task \"" + windowsTaskName + "\" (starts at boot as SYSTEM)")
	default:
		cmd := exec.Command(bin, "-daemon", "-config-dir", t.dir)
		cmd.SysProcAttr = detachedProcAttr()
		if pfRoot == "/" {
			if err := cmd.Start(); err != nil {
				return fmt.Errorf("starting the monitor: %w", err)
			}
			cmd.Process.Release()
		}
		step("started:       in the background (no systemd here, see below to start it at boot)")
	}

	if pfRoot == "/" {
		for i := 0; i < 20 && !monitorRunningIn(t.dir); i++ {
			time.Sleep(500 * time.Millisecond)
		}
		if !monitorRunningIn(t.dir) {
			fmt.Printf("\nInstalled, but the monitor didn't answer yet. Check %s.\n", filepath.Join(t.dir, "activity.log"))
			return nil
		}
	}

	fmt.Println()
	admin := "sudo ups-monitor"
	if runtime.GOOS == "windows" {
		admin = `"` + t.bin + `" (from an Administrator terminal)`
	}
	if t.manager == "none" {
		fmt.Println("Done. It's running. Open the settings with: " + admin)
		fmt.Printf("This system has no systemd, so to start it at boot, add this to /etc/rc.local or your init system:\n  %s -daemon -config-dir %s &\n", t.bin, t.dir)
	} else {
		fmt.Println("Done. It's running and starts at boot. Open the settings with: " + admin)
	}
	fmt.Println("Run the install command again any time to update it; your settings are kept.")
	return nil
}

// uninstallService stops the service and removes it and the program; settings are kept.
func uninstallService() error {
	if pfSenseDetected() {
		return uninstallPfSense()
	}
	if pfRoot == "/" {
		if err := requireAdmin(); err != nil {
			return err
		}
	}
	t := installTargetFor(runtime.GOOS)
	fmt.Println("Removing UPS Monitor...")
	switch t.manager {
	case "systemd":
		pfRunQuiet("systemctl", "disable", "--now", "ups-monitor")
		os.Remove(pfPath(systemdUnitPath))
		pfRunQuiet("systemctl", "daemon-reload")
	case "launchd":
		pfRunQuiet("launchctl", "bootout", "system/"+launchdLabel)
		os.Remove(pfPath(launchdPlist))
	case "schtasks":
		pfRunQuiet("schtasks", "/End", "/TN", windowsTaskName)
		pfRunQuiet("schtasks", "/Delete", "/TN", windowsTaskName, "/F")
	}
	stopMonitorIn(t.dir)
	bin := pfPath(t.bin)
	if err := os.Remove(bin); err != nil && !errors.Is(err, os.ErrNotExist) {
		// Windows can't delete the program that's running this; move it aside instead.
		os.Rename(bin, bin+".old")
	}
	if runtime.GOOS == "windows" {
		os.Remove(filepath.Dir(bin))
	}
	fmt.Printf("Done. Your settings were kept in %s; delete that folder to remove them too.\n", t.dir)
	return nil
}
