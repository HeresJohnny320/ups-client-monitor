package main

import (
	"os"
	"path/filepath"
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
		if fi, err := os.Stat(filepath.Join(root, dst)); err != nil || fi.Mode().Perm() != 0644 {
			t.Errorf("/%s not installed as 0644: %v", dst, err)
		}
	}
	if fi, err := os.Stat(filepath.Join(root, pfRCPath)); err != nil || fi.Mode().Perm() != 0755 {
		t.Errorf("boot script not installed as executable: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(root, pfBinPath)); err != nil || fi.Mode().Perm() != 0755 || fi.Size() == 0 {
		t.Errorf("program not copied: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(root, pfsenseSettingsDir)); err != nil || fi.Mode().Perm() != 0700 {
		t.Errorf("settings folder not created private: %v", err)
	}
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

func mapValues(m map[string]string) []string {
	var out []string
	for _, v := range m {
		out = append(out, v)
	}
	return out
}
