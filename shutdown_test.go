package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestShutdownCommands(t *testing.T) {
	join := func(cmds [][]string) string {
		var out []string
		for _, c := range cmds {
			out = append(out, strings.Join(c, " "))
		}
		return strings.Join(out, " | ")
	}
	want := map[string]string{
		"windows": "shutdown /s /t 0",
		"darwin":  `osascript -e tell app "System Events" to shut down | shutdown -h now`,
		"linux":   "midclt call system.shutdown UPS battery low | midclt call system.shutdown | shutdown -h now | poweroff",
		"freebsd": "midclt call system.shutdown UPS battery low | midclt call system.shutdown | shutdown -p now",
		"openbsd": "shutdown -p now",
		"netbsd":  "shutdown -p now",
		"illumos": "shutdown -y -g0 -i5",
		"solaris": "shutdown -y -g0 -i5",
	}
	for goos, w := range want {
		if got := join(shutdownCommands(goos)); got != w {
			t.Errorf("%s:\n got  %s\n want %s", goos, got, w)
		}
	}
	if shutdownCommands("android") != nil {
		t.Error("phones aren't supported")
	}
}

// Every OS that CI builds a release for must know how to shut itself down.
func TestEveryBuiltOSCanShutDown(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/build.yml")
	if err != nil {
		t.Fatal(err)
	}
	matches := regexp.MustCompile(`goos: (\w+)`).FindAllStringSubmatch(string(data), -1)
	if len(matches) < 5 {
		t.Fatalf("found only %d build targets in build.yml", len(matches))
	}
	for _, m := range matches {
		if len(shutdownCommands(m[1])) == 0 {
			t.Errorf("CI builds for %s, but there's no shutdown command for it", m[1])
		}
	}
}
