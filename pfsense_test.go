package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// installs into a temporary folder that stands in for pfSense's root, recording the
// commands that would run on the firewall.
func TestPfSenseInstallAndUninstall(t *testing.T) {
	root := t.TempDir()
	var calls []string
	oldRoot, oldRun := pfRoot, pfRun
	pfRoot = root
	pfRun = func(name string, args ...string) error {
		rel, _ := filepath.Rel(root, name)
		rel = filepath.ToSlash(rel) // Windows test runners use backslashes
		if strings.HasSuffix(name, "php") {
			// The register script is a temporary file; record only that it ran and how.
			calls = append(calls, rel+" "+args[0]+" register.php "+args[2])
			script, err := os.ReadFile(args[1])
			if err != nil || !strings.Contains(string(script), "installedpackages/menu") {
				t.Errorf("register script missing or wrong: %v", err)
			}
		} else {
			calls = append(calls, rel+" "+strings.Join(args, " "))
		}
		return nil
	}
	t.Cleanup(func() { pfRoot, pfRun = oldRoot, oldRun })

	if err := installPfSense(); err != nil {
		t.Fatal(err)
	}

	for _, dst := range pfWebFiles {
		checkInstalled(t, filepath.Join(root, dst), 0644)
	}
	checkInstalled(t, filepath.Join(root, pfRCPath), 0755)
	checkInstalled(t, filepath.Join(root, pfBinPath), 0755)
	checkInstalled(t, filepath.Join(root, pfsenseSettingsDir), 0700)
	want := []string{
		"usr/local/bin/php -f register.php install",
		"usr/local/etc/rc.d/ups_monitor.sh restart",
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("install ran:\n%s\nwant:\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}

	// Installing again (an update) must work over the existing files.
	calls = nil
	if err := installPfSense(); err != nil {
		t.Fatalf("reinstall: %v", err)
	}

	calls = nil
	if err := uninstallPfSense(); err != nil {
		t.Fatal(err)
	}
	want = []string{
		"usr/local/etc/rc.d/ups_monitor.sh stop",
		"usr/local/bin/php -f register.php uninstall",
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("uninstall ran:\n%s\nwant:\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
	for _, p := range append([]string{pfRCPath, pfBinPath}, mapValues(pfWebFiles)...) {
		if _, err := os.Stat(filepath.Join(root, p)); err == nil {
			t.Errorf("/%s was not removed", p)
		}
	}
	if _, err := os.Stat(filepath.Join(root, pfsenseSettingsDir)); err != nil {
		t.Error("uninstall removed the settings folder; it should keep it")
	}
}

func TestPfSenseRefusesOtherSystems(t *testing.T) {
	if pfRoot != "/" {
		t.Skip()
	}
	// On the machine running the tests (not pfSense) the real installer must refuse.
	if err := installPfSense(); err == nil {
		t.Fatal("installer ran on a system that isn't pfSense")
	}
}

func TestEmbeddedPfSenseFilesMatchRepo(t *testing.T) {
	for src := range pfWebFiles {
		embedded, _ := pfsenseFiles.ReadFile(src)
		onDisk, err := os.ReadFile(src)
		if err != nil || string(embedded) != string(onDisk) {
			t.Errorf("%s: embedded copy differs from the file in the repo", src)
		}
	}
}

// checkInstalled fails if path is missing (or an empty file), or on Unix if its permissions
// differ. Windows has no Unix permission bits; the installer only runs on pfSense anyway.
func checkInstalled(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Errorf("%s was not installed: %v", path, err)
		return
	}
	if !fi.IsDir() && fi.Size() == 0 {
		t.Errorf("%s is empty", path)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != mode {
		t.Errorf("%s has mode %v, want %v", path, fi.Mode().Perm(), mode)
	}
}

func mapValues(m map[string]string) []string {
	var out []string
	for _, v := range m {
		out = append(out, v)
	}
	return out
}
