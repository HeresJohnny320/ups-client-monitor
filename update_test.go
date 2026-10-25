package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAssetName(t *testing.T) {
	cases := map[[3]string]string{
		{"linux", "amd64", ""}:     "ups-monitor-linux-amd64",
		{"linux", "arm", "6"}:      "ups-monitor-linux-armv6",
		{"windows", "386", ""}:     "ups-monitor-windows-386.exe",
		{"freebsd", "arm64", ""}:   "ups-monitor-freebsd-arm64",
		{"illumos", "amd64", ""}:   "ups-monitor-illumos-amd64",
		{"darwin", "arm64", ""}:    "ups-monitor-darwin-arm64",
		{"windows", "arm64", ""}:   "ups-monitor-windows-arm64.exe",
		{"linux", "riscv64", ""}:   "ups-monitor-linux-riscv64",
		{"openbsd", "amd64", ""}:   "ups-monitor-openbsd-amd64",
		{"linux", "arm", "7"}:      "ups-monitor-linux-armv7",
		{"netbsd", "arm64", ""}:    "ups-monitor-netbsd-arm64",
		{"solaris", "amd64", ""}:   "ups-monitor-solaris-amd64",
		{"dragonfly", "amd64", ""}: "ups-monitor-dragonfly-amd64",
	}
	for in, want := range cases {
		if got := assetName(in[0], in[1], in[2]); got != want {
			t.Errorf("%v: got %s, want %s", in, got, want)
		}
	}
	if !strings.HasPrefix(ownAssetName(), "ups-monitor-") {
		t.Error(ownAssetName())
	}
}

func TestEveryBuiltPlatformHasAnAssetName(t *testing.T) {
	// The names CI publishes must be the names the updater looks for.
	data, err := os.ReadFile(".github/workflows/build.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(line, "goos:") {
			continue
		}
		field := func(key string) string {
			i := strings.Index(line, key+": ")
			if i < 0 {
				return ""
			}
			v := line[i+len(key)+2:]
			v = strings.Trim(strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == '}' })[0], ` "`)
			return v
		}
		ext := field("ext")
		want := "ups-monitor-" + field("name") + ext
		if got := assetName(field("goos"), field("goarch"), field("goarm")); got != want {
			t.Errorf("CI publishes %s but the updater would look for %s", want, got)
		}
	}
}

func TestNewerVersion(t *testing.T) {
	yes := [][2]string{{"v3.10", "v3.9"}, {"v4.0", "v3.12"}, {"v3.6.1", "v3.6"}, {"alpha-12", "alpha-9"}}
	no := [][2]string{{"v3.9", "v3.10"}, {"v3.6", "v3.6"}, {"alpha-9", "alpha-12"}, {"v9.0", "alpha-3"}, {"alpha-30", "v3.6"}, {"dev", "v3.6"}}
	for _, p := range yes {
		if !newerVersion(p[0], p[1]) {
			t.Errorf("%s should be newer than %s", p[0], p[1])
		}
	}
	for _, p := range no {
		if newerVersion(p[0], p[1]) {
			t.Errorf("%s shouldn't count as newer than %s", p[0], p[1])
		}
	}
}

func TestPickUpdate(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 10, d, 0, 0, 0, 0, time.UTC) }
	asset := "ups-monitor-linux-amd64"
	rel := func(tag string, pre bool, d int, assets ...string) ghRelease {
		r := ghRelease{Tag: tag, Prerelease: pre, Published: day(d)}
		for _, a := range append(assets, asset) {
			r.Assets = append(r.Assets, ghAsset{Name: a, URL: "https://example/" + tag + "/" + a})
		}
		return r
	}
	rels := []ghRelease{
		rel("alpha-20", true, 20),
		rel("v3.7", false, 15),
		rel("alpha-17", true, 14),
		rel("v3.6", false, 10),
		{Tag: "v3.8-draft", Draft: true, Published: day(25), Assets: []ghAsset{{Name: asset}}},
		{Tag: "v3.9", Published: day(22)}, // no build for this system
	}

	cases := []struct{ current, want string }{
		{"v3.6", "v3.7"},         // stable: the newest stable, never an alpha or a draft
		{"v3.7", ""},             // already the newest stable
		{"alpha-17", "alpha-20"}, // alpha: the newest build of any kind
		{"alpha-20", ""},
		{"v3.5", "v3.7"}, // not listed any more: compared by version number
		{"v3.8", ""},     // a newer stable than anything published: nothing to do
	}
	for _, c := range cases {
		got, note := pickUpdate(c.current, rels, asset)
		gotTag := ""
		if got != nil {
			gotTag = got.Tag
		}
		if gotTag != c.want || note != "" {
			t.Errorf("running %s: got %q (note %q), want %q", c.current, gotTag, note, c.want)
		}
	}
	if got, note := pickUpdate("dev", rels, asset); got != nil || !strings.Contains(note, "development build") {
		t.Errorf("a dev build must not update: %v %q", got, note)
	}
	if got, note := pickUpdate("v3.6", rels, "ups-monitor-plan9-amd64"); got != nil || !strings.Contains(note, "no release has a build") {
		t.Errorf("no build for this system: %v %q", got, note)
	}
}

// fakeReleases serves a releases list with one build and its checksums, like GitHub does.
func fakeReleases(t *testing.T, tag string, bin []byte, sums string) (*httptest.Server, *ghRelease) {
	asset := ownAssetName()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	rel := &ghRelease{Tag: tag, Published: time.Now(), Assets: []ghAsset{
		{Name: asset, URL: srv.URL + "/" + asset},
		{Name: "SHA256SUMS.txt", URL: srv.URL + "/SHA256SUMS.txt"},
	}}
	mux.HandleFunc("/releases", func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode([]ghRelease{*rel}) })
	mux.HandleFunc("/"+asset, func(w http.ResponseWriter, r *http.Request) { w.Write(bin) })
	mux.HandleFunc("/SHA256SUMS.txt", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, sums) })
	t.Setenv("UPS_MONITOR_UPDATE_URL", srv.URL+"/releases")
	t.Cleanup(srv.Close)
	return srv, rel
}

func TestDownloadIsVerified(t *testing.T) {
	bin := []byte("pretend this is a program")
	sum := sha256.Sum256(bin)
	good := hex.EncodeToString(sum[:]) + "  " + ownAssetName() + "\n"
	dst := filepath.Join(t.TempDir(), "new")

	_, rel := fakeReleases(t, "v9.9", bin, "deadbeef  other-file\n"+good)
	if err := downloadVerified(rel, ownAssetName(), dst); err != nil {
		t.Fatalf("a correct download was refused: %v", err)
	}
	if data, _ := os.ReadFile(dst); string(data) != string(bin) {
		t.Fatal("the download wasn't saved")
	}

	for name, sums := range map[string]string{
		"wrong checksum":  strings.Repeat("0", 64) + "  " + ownAssetName() + "\n",
		"not in the list": "deadbeef  other-file\n",
	} {
		os.Remove(dst)
		_, rel := fakeReleases(t, "v9.9", bin, sums)
		if err := downloadVerified(rel, ownAssetName(), dst); err == nil {
			t.Errorf("%s: the download was accepted", name)
		}
		if _, err := os.Stat(dst); err == nil {
			t.Errorf("%s: an unverified download was written to disk", name)
		}
	}

	noSums := &ghRelease{Tag: "v9.9", Assets: []ghAsset{{Name: ownAssetName(), URL: "http://example/x"}}}
	if err := downloadVerified(noSums, ownAssetName(), dst); err == nil || !strings.Contains(err.Error(), "SHA256SUMS") {
		t.Errorf("a release without checksums must be refused: %v", err)
	}
}

func TestPlainHTTPRefusedOutsideTests(t *testing.T) {
	t.Setenv("UPS_MONITOR_UPDATE_URL", "")
	if _, err := httpGet("http://example.com/x", 10); err == nil || !strings.Contains(err.Error(), "plain http") {
		t.Errorf("plain http download allowed: %v", err)
	}
}

func TestSelfUpdateChecks(t *testing.T) {
	old := version
	t.Cleanup(func() { version = old })
	// Other tests leave on-battery readings behind; start from mains power.
	stateMu.Lock()
	upsReadings = make(map[string]upsReading)
	link.lastOnBattery = false
	stateMu.Unlock()

	// Up to date: nothing is downloaded.
	version = "v9.9"
	fakeReleases(t, "v9.9", []byte("x"), "")
	if _, err := selfUpdate(); !errors.Is(err, errUpToDate) {
		t.Errorf("want up to date, got %v", err)
	}

	// Never during an outage.
	version = "v1.0"
	stateMu.Lock()
	upsReadings["ups1"] = upsReading{Status: "OB DISCHRG", Charge: 80, ChargeKnown: true}
	stateMu.Unlock()
	t.Cleanup(func() {
		stateMu.Lock()
		delete(upsReadings, "ups1")
		stateMu.Unlock()
	})
	if _, err := selfUpdate(); err == nil || !strings.Contains(err.Error(), "on battery") {
		t.Errorf("updated during an outage: %v", err)
	}

	if newBinaryPath(`C:\ups\ups-monitor.exe`) != `C:\ups\ups-monitor.new.exe` || newBinaryPath("/usr/local/bin/ups-monitor") != "/usr/local/bin/ups-monitor.new" {
		t.Error("new binary path is wrong")
	}
}
