package main

import (
	"embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// pfSense support: "ups-monitor -install-pfsense" turns this binary into a pfSense add-on with
// a page under Services > UPS Monitor, a dashboard widget, an entry on Status > Services and a
// boot script. The web files are built into the binary so they always match its version.

//go:embed pfsense/*.php pfsense/*.inc
var pfsenseFiles embed.FS

// pfRoot prefixes every path the installer touches; tests point it at a temporary folder.
var pfRoot = "/"

// pfRun runs a command on the firewall; tests replace it to record calls instead.
var pfRun = func(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

const (
	pfBinPath = "usr/local/bin/ups-monitor"
	pfRCPath  = "usr/local/etc/rc.d/ups_monitor.sh"
	pfPHPPath = "usr/local/bin/php"
)

// pfWebFiles maps embedded files to where pfSense expects them.
var pfWebFiles = map[string]string{
	"pfsense/ups_monitor.php": "usr/local/www/ups_monitor.php",
	"pfsense/ups_monitor.inc": "usr/local/pkg/ups_monitor.inc",
	"pfsense/widget.php":      "usr/local/www/widgets/widgets/ups_monitor.widget.php",
	"pfsense/widget.inc":      "usr/local/www/widgets/include/ups_monitor.inc",
}

// pfRCScript starts the monitor at boot (pfSense runs every rc.d/*.sh with "start") and lets
// Status > Services start, stop and restart it. daemon(8) restarts it if it ever exits.
const pfRCScript = `#!/bin/sh
# UPS Monitor service for pfSense. Installed by "ups-monitor -install-pfsense".

BIN=/usr/local/bin/ups-monitor
DIR=/usr/local/etc/ups-monitor
PIDFILE=/var/run/ups_monitor.pid

is_running() {
	[ -f "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null
}

case "$1" in
start)
	if is_running; then
		echo "ups-monitor is already running."
		exit 0
	fi
	/usr/sbin/daemon -f -r -R 10 -P "$PIDFILE" "$BIN" -daemon -config-dir "$DIR"
	;;
stop)
	if is_running; then
		kill "$(cat "$PIDFILE")"
		i=0
		while is_running && [ $i -lt 20 ]; do
			sleep 0.5
			i=$((i + 1))
		done
	fi
	rm -f "$PIDFILE"
	;;
restart)
	"$0" stop
	"$0" start
	;;
status)
	if is_running; then
		echo "ups-monitor is running."
	else
		echo "ups-monitor is not running."
		exit 1
	fi
	;;
*)
	echo "usage: $0 {start|stop|restart|status}"
	exit 1
	;;
esac
`

func pfPath(rel string) string { return filepath.Join(pfRoot, rel) }

func checkPfSense() error {
	if pfRoot != "/" {
		return nil // test install into a temporary folder
	}
	if runtime.GOOS != "freebsd" {
		return errors.New("-install-pfsense only works on pfSense (download the freebsd build)")
	}
	if os.Geteuid() != 0 {
		return errors.New("run this as root (the admin account over SSH, or Diagnostics > Command Prompt)")
	}
	if _, err := os.Stat("/usr/local/www/guiconfig.inc"); err != nil {
		return errors.New("this doesn't look like pfSense (no /usr/local/www/guiconfig.inc)")
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// writeAtomic writes via a temp file and rename, so a running binary or page is never half-written.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	tmp := path + ".new"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// runRegister runs register.php with pfSense's PHP to add or remove the menu entry and service.
func runRegister(action string) error {
	script, err := pfsenseFiles.ReadFile("pfsense/register.php")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp("", "ups-monitor-register-*.php")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(script); err != nil {
		f.Close()
		return err
	}
	f.Close()
	return pfRun(pfPath(pfPHPPath), "-f", f.Name(), action)
}

func installPfSense() error {
	if err := checkPfSense(); err != nil {
		return err
	}
	step := func(msg string) { fmt.Println("  " + msg) }
	fmt.Printf("Installing UPS Monitor %s for pfSense...\n", version)

	// The binary itself. Copy then rename, because the old one may be running.
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.EvalSymlinks(exe)
	bin := pfPath(pfBinPath)
	if exe != bin {
		os.MkdirAll(filepath.Dir(bin), 0755)
		if err := copyFile(exe, bin+".new", 0755); err != nil {
			return fmt.Errorf("copying the program to %s: %w", bin, err)
		}
		if err := os.Rename(bin+".new", bin); err != nil {
			return err
		}
	}
	step("program:       /" + pfBinPath)

	if err := os.MkdirAll(pfPath(pfsenseSettingsDir), 0700); err != nil {
		return err
	}
	step("settings:      " + pfsenseSettingsDir)

	for src, dst := range pfWebFiles {
		data, err := pfsenseFiles.ReadFile(src)
		if err != nil {
			return err
		}
		if err := writeAtomic(pfPath(dst), data, 0644); err != nil {
			return fmt.Errorf("writing /%s: %w", dst, err)
		}
	}
	step("web page:      Services > UPS Monitor")
	step("widget:        UPS Monitor (add it from the dashboard's + menu)")

	if err := writeAtomic(pfPath(pfRCPath), []byte(pfRCScript), 0755); err != nil {
		return err
	}
	step("boot script:   /" + pfRCPath)

	if err := runRegister("install"); err != nil {
		return fmt.Errorf("adding the menu entry and service: %w", err)
	}
	if err := pfRun(pfPath(pfRCPath), "restart"); err != nil {
		return fmt.Errorf("starting the service: %w", err)
	}
	step("service:       started (Status > Services > ups_monitor)")

	fmt.Println()
	fmt.Println("Done. In the pfSense web UI, open Services > UPS Monitor to set it up.")
	fmt.Println("Settings live in " + pfsenseSettingsDir + ", which pfSense's config backups don't include.")
	return nil
}

func uninstallPfSense() error {
	if err := checkPfSense(); err != nil {
		return err
	}
	fmt.Println("Removing UPS Monitor from pfSense...")
	if _, err := os.Stat(pfPath(pfRCPath)); err == nil {
		pfRun(pfPath(pfRCPath), "stop")
	}
	if err := runRegister("uninstall"); err != nil {
		fmt.Println("  warning: could not remove the menu entry and service:", err)
	}
	for _, dst := range pfWebFiles {
		os.Remove(pfPath(dst))
	}
	os.Remove(pfPath(pfRCPath))
	os.Remove(pfPath(pfBinPath))
	fmt.Println("Done. Your settings were kept in " + pfsenseSettingsDir + "; delete that folder to remove them too.")
	return nil
}
