package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNoShutdownAtFullCharge(t *testing.T) {
	ob := func(charge int, lb bool) upsEvent {
		return upsEvent{ups: "ups1", charge: charge, chargeKnown: true, onBattery: true, lowBattery: lb, confirmed: true}
	}
	// The reported bug: "Battery at 100%. Telling client ... to shut down" (LB during a blip/self-test).
	if ob(100, true).reachedShutdown(30) || ob(99, true).reachedShutdown(30) {
		t.Error("shut down at 99-100% because of LB")
	}
	if ob(100, false).reachedShutdown(100) {
		t.Error("a machine set to 100% shut down while the battery was still full")
	}
	if !ob(98, true).reachedShutdown(30) {
		t.Error("LB below 99% must still shut down")
	}
	if !ob(30, false).reachedShutdown(30) {
		t.Error("reaching the shutdown % must shut down")
	}
	// Charge unreadable: LB is all there is to go on, so it still counts.
	if !(upsEvent{onBattery: true, lowBattery: true, confirmed: true}).reachedShutdown(30) {
		t.Error("LB with an unreadable charge must still shut down")
	}
}

func TestSelfTestIgnoresLowBatteryFlag(t *testing.T) {
	e := upsEvent{ups: "ups1", charge: 80, chargeKnown: true, onBattery: true, lowBattery: true, confirmed: true, testing: true}
	if e.reachedShutdown(50) {
		t.Error("LB raised by a self-test shut a machine down")
	}
	e.charge = 40 // a real outage during the test: the charge itself still counts
	if !e.reachedShutdown(50) {
		t.Error("a real drop below the shutdown % during a self-test was ignored")
	}
}

func TestOnBatteryMustRepeat(t *testing.T) {
	obStreak = map[string]int{}
	stateMu.Lock()
	testUntil = map[string]time.Time{}
	stateMu.Unlock()

	if e := readingEvent("ups1", "OB DISCHRG", 90, true); e.confirmed {
		t.Fatal("a single on-battery reading was treated as an outage")
	}
	if e := readingEvent("ups1", "OB DISCHRG", 89, true); !e.confirmed {
		t.Fatal("two on-battery readings in a row weren't treated as an outage")
	}
	readingEvent("ups1", "OL CHRG", 89, true) // power back: start counting again
	if e := readingEvent("ups1", "OB DISCHRG", 88, true); e.confirmed {
		t.Fatal("the count didn't restart after power came back")
	}
	nutFailed(errors.New("connection lost")) // NUT dropping out also starts the count again
	if e := readingEvent("ups1", "OB DISCHRG", 88, true); e.confirmed {
		t.Fatal("the count survived NUT disconnecting")
	}

	stateMu.Lock()
	testUntil["ups2"] = time.Now().Add(time.Minute)
	stateMu.Unlock()
	if e := readingEvent("ups2", "OB", 100, true); !e.testing {
		t.Error("a self-test we started wasn't noticed")
	}
	if e := readingEvent("ups3", "OL TEST", 100, true); !e.testing {
		t.Error("NUT's TEST flag wasn't noticed")
	}
}

func TestStatusFlags(t *testing.T) {
	cases := map[string]bool{
		"OL":            false,
		"OL CHRG":       false,
		"OL LB":         false, // on mains, recharging after an outage: not an outage
		"OB DISCHRG":    true,
		"OB DISCHRG LB": true,
		"FSD OB":        true,
		"":              false,
	}
	for status, want := range cases {
		if got := isOnBatteryStatus(status); got != want {
			t.Errorf("%q: on battery = %v, want %v", status, got, want)
		}
	}
}

func TestDiscordWakeMessageHasNoMAC(t *testing.T) {
	got := make(chan string, 1)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- string(body)
	}))
	defer hook.Close()

	resetWakeState()
	m := wakeMachine("nas", "aa:bb:cc:dd:ee:01", "ups1", 40)
	m.WebhookURL = hook.URL
	stateMu.Lock()
	armWake(m)
	stateMu.Unlock()
	handleWakes(&ServerSettings{Machines: []Machine{m}}, ev("ups1", 100, false))

	select {
	case msg := <-got:
		if !strings.Contains(msg, "Waking node `nas`") || strings.Contains(strings.ToLower(msg), "aa:bb:cc") {
			t.Errorf("Discord message should name the machine but not its MAC: %s", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no Discord message was sent")
	}
}
