package main

import (
	"errors"
	"strings"
	"testing"
)

// fakeUPS answers LIST CMD and INSTCMD like upsd, with the same error texts the NUT library returns.
type fakeUPS struct {
	commands   []string // what LIST CMD reports
	listFails  bool     // an upsd that won't list commands
	denyAccess bool     // the NUT user may not run instant commands
	sent       []string // INSTCMD commands received
}

const (
	errNotSupported = "The specified UPS doesn’t support the instant command in the request"
	errAccessDenied = "The client’s host and/or authentication details (username, password) are not sufficient to execute the requested command"
)

func (f *fakeUPS) SendCommand(cmd string) ([]string, error) {
	if strings.HasPrefix(cmd, "LIST CMD ") {
		if f.listFails {
			return nil, errors.New("error reading response: EOF")
		}
		resp := []string{"BEGIN LIST CMD ups1"}
		for _, c := range f.commands {
			resp = append(resp, "CMD ups1 "+c)
		}
		return append(resp, "END LIST CMD ups1"), nil
	}
	name := strings.TrimPrefix(cmd, "INSTCMD ups1 ")
	f.sent = append(f.sent, name)
	for _, c := range f.commands {
		if c == name {
			if f.denyAccess {
				return nil, errors.New(errAccessDenied)
			}
			return []string{"OK"}, nil
		}
	}
	return nil, errors.New(errNotSupported)
}

func TestSelfTestPicksSupportedCommand(t *testing.T) {
	cases := []struct {
		name     string
		commands []string
		testType string
		want     string
	}{
		// Most usbhid-ups models (APC, CyberPower): no plain test.battery.start.
		{"quick/deep only, quick", []string{"beeper.toggle", "test.battery.start.quick", "test.battery.start.deep", "test.battery.stop"}, "quick", "test.battery.start.quick"},
		{"quick/deep only, deep", []string{"test.battery.start.quick", "test.battery.start.deep"}, "deep", "test.battery.start.deep"},
		// Drivers that only have the generic command.
		{"plain only, quick", []string{"test.battery.start", "test.battery.stop"}, "quick", "test.battery.start"},
		{"plain only, deep", []string{"test.battery.start"}, "deep", "test.battery.start"},
		// When both exist, the specific one wins.
		{"all three, quick", []string{"test.battery.start", "test.battery.start.quick", "test.battery.start.deep"}, "quick", "test.battery.start.quick"},
	}
	for _, c := range cases {
		ups := &fakeUPS{commands: c.commands}
		got, err := startUpsTest(ups, "ups1", c.testType)
		if err != nil || got != c.want {
			t.Errorf("%s: got %q, %v; want %q", c.name, got, err, c.want)
		}
		if len(ups.sent) != 1 {
			t.Errorf("%s: sent %v, want exactly one command", c.name, ups.sent)
		}
	}
}

func TestSelfTestExplainsWhatIsMissing(t *testing.T) {
	_, err := startUpsTest(&fakeUPS{commands: []string{"beeper.toggle", "load.off"}}, "ups1", "quick")
	if err == nil || !strings.Contains(err.Error(), "doesn't offer a battery self-test") {
		t.Errorf("no test commands: %v", err)
	}

	_, err = startUpsTest(&fakeUPS{commands: []string{"test.battery.start.quick"}}, "ups1", "deep")
	if err == nil || !strings.Contains(err.Error(), "doesn't offer a deep battery test (it has: test.battery.start.quick)") {
		t.Errorf("deep wanted, only quick available: %v", err)
	}
}

func TestSelfTestWithoutCommandList(t *testing.T) {
	// If upsd won't list commands, try each candidate and skip the unsupported ones.
	ups := &fakeUPS{commands: []string{"test.battery.start"}, listFails: true}
	got, err := startUpsTest(ups, "ups1", "quick")
	if err != nil || got != "test.battery.start" {
		t.Fatalf("got %q, %v", got, err)
	}
	if strings.Join(ups.sent, ",") != "test.battery.start.quick,test.battery.start" {
		t.Errorf("tried %v", ups.sent)
	}
}

func TestSelfTestStopsOnLoginError(t *testing.T) {
	// A permissions problem is reported as-is (and gets the NUT user hint), not retried.
	ups := &fakeUPS{commands: []string{"test.battery.start.quick", "test.battery.start"}, denyAccess: true}
	_, err := startUpsTest(ups, "ups1", "quick")
	if err == nil || !strings.Contains(err.Error(), "authentication") {
		t.Fatalf("want the access-denied error, got %v", err)
	}
	if len(ups.sent) != 1 {
		t.Errorf("retried after a login error: %v", ups.sent)
	}
}
