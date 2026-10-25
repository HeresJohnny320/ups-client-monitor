package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Self-update from the project's GitHub releases. The program only ever downloads its own
// build for this system from the official repository, checks it against the release's
// SHA256SUMS.txt, makes sure it starts, and then restarts into it.

const updateRepo = "HeresJohnny320/ups-client-monitor"

// updateTimeout is how long an "update now" request may take, mostly downloading.
const updateTimeout = 5 * time.Minute

// errUpToDate means there's nothing newer to install.
var errUpToDate = errors.New("already up to date")

type ghAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

type ghRelease struct {
	Tag        string    `json:"tag_name"`
	Draft      bool      `json:"draft"`
	Prerelease bool      `json:"prerelease"`
	Published  time.Time `json:"published_at"`
	Assets     []ghAsset `json:"assets"`
}

func (r *ghRelease) asset(name string) string {
	for _, a := range r.Assets {
		if a.Name == name {
			return a.URL
		}
	}
	return ""
}

// UpdateInfo is what the UI shows about updates.
type UpdateInfo struct {
	Current   string    `json:"current"`
	Latest    string    `json:"latest,omitempty"`
	Available bool      `json:"available"`
	Checked   time.Time `json:"checked"`
	Note      string    `json:"note,omitempty"` // why there's nothing to install, or why the check failed
}

var (
	updateMu       sync.Mutex
	lastUpdateInfo *UpdateInfo
	updating       bool
)

// releasesURL lists the releases. UPS_MONITOR_UPDATE_URL points it at a local copy for
// testing, which is also the only case where plain http downloads are allowed.
func releasesURL() (url string, testing bool) {
	if u := os.Getenv("UPS_MONITOR_UPDATE_URL"); u != "" {
		return u, true
	}
	return "https://api.github.com/repos/" + updateRepo + "/releases?per_page=50", false
}

// assetName is the release file for a platform, matching the names CI publishes.
func assetName(goos, goarch, goarm string) string {
	arch := goarch
	if goarch == "arm" {
		arch = "armv" + goarm
	}
	ext := ""
	if goos == "windows" {
		ext = ".exe"
	}
	return "ups-monitor-" + goos + "-" + arch + ext
}

func ownAssetName() string {
	goarm := "7"
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "GOARM" && s.Value != "" {
				goarm = strings.Split(s.Value, ",")[0]
			}
		}
	}
	return assetName(runtime.GOOS, runtime.GOARCH, goarm)
}

// versionNumbers reads "v3.10" as [3 10] and "alpha-12" as [12].
func versionNumbers(v string) ([]int, bool) {
	v = strings.TrimPrefix(strings.TrimPrefix(v, "alpha-"), "v")
	var out []int
	for _, p := range strings.Split(v, ".") {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, false
		}
		out = append(out, n)
	}
	return out, len(out) > 0
}

// newerVersion reports whether a is newer than b. Only versions of the same kind compare
// (v3.10 > v3.9, alpha-12 > alpha-9).
func newerVersion(a, b string) bool {
	if strings.HasPrefix(a, "alpha-") != strings.HasPrefix(b, "alpha-") {
		return false
	}
	pa, oka := versionNumbers(a)
	pb, okb := versionNumbers(b)
	if !oka || !okb {
		return false
	}
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x != y {
			return x > y
		}
	}
	return false
}

// pickUpdate chooses the release to update to, or returns nil and a note saying why not.
// Stable builds only move to stable releases; alpha builds take the newest build of any kind.
func pickUpdate(current string, rels []ghRelease, asset string) (*ghRelease, string) {
	if current == "" || current == "dev" {
		return nil, "this is a development build; updates are for released builds"
	}
	alpha := strings.HasPrefix(current, "alpha-")
	var cur, newest *ghRelease
	for i := range rels {
		r := &rels[i]
		if r.Tag == current {
			cur = r
		}
		if r.Draft || (r.Prerelease && !alpha) || r.asset(asset) == "" {
			continue
		}
		if newest == nil || r.Published.After(newest.Published) {
			newest = r
		}
	}
	switch {
	case newest == nil:
		return nil, "no release has a build for this system (" + asset + ")"
	case newest.Tag == current:
		return nil, ""
	case cur != nil:
		if newest.Published.After(cur.Published) {
			return newest, ""
		}
		return nil, ""
	case newerVersion(newest.Tag, current):
		// This build's own release isn't listed (e.g. renamed), so compare version numbers.
		return newest, ""
	}
	return nil, ""
}

func httpGet(url string, limit int64) ([]byte, error) {
	_, testing := releasesURL()
	if !strings.HasPrefix(url, "https://") && !testing {
		return nil, fmt.Errorf("refusing to download over plain http: %s", url)
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "ups-monitor/"+version)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is larger than expected", url)
	}
	return data, nil
}

// checkForUpdate asks GitHub for the releases and remembers the answer for the UI.
func checkForUpdate() (UpdateInfo, *ghRelease) {
	info := UpdateInfo{Current: version, Checked: time.Now()}
	var rel *ghRelease
	url, _ := releasesURL()
	data, err := httpGet(url, 4<<20)
	var rels []ghRelease
	if err == nil {
		err = json.Unmarshal(data, &rels)
	}
	if err != nil {
		info.Note = "couldn't check for updates: " + err.Error()
	} else {
		rel, info.Note = pickUpdate(version, rels, ownAssetName())
		if rel != nil {
			info.Latest, info.Available = rel.Tag, true
		} else if info.Note == "" {
			info.Latest = version
		}
	}
	updateMu.Lock()
	lastUpdateInfo = &info
	updateMu.Unlock()
	return info, rel
}

func cachedUpdateInfo() *UpdateInfo {
	updateMu.Lock()
	defer updateMu.Unlock()
	if lastUpdateInfo == nil {
		return nil
	}
	i := *lastUpdateInfo
	return &i
}

// downloadVerified saves a release's build for this system to dst after checking it
// against the release's SHA256SUMS.txt.
func downloadVerified(rel *ghRelease, asset, dst string) error {
	binURL, sumsURL := rel.asset(asset), rel.asset("SHA256SUMS.txt")
	if binURL == "" {
		return fmt.Errorf("%s has no build for this system (%s)", rel.Tag, asset)
	}
	if sumsURL == "" {
		return fmt.Errorf("%s has no SHA256SUMS.txt, so its download can't be checked", rel.Tag)
	}
	sums, err := httpGet(sumsURL, 64<<10)
	if err != nil {
		return err
	}
	var want string
	for _, line := range strings.Split(string(sums), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == asset {
			want = strings.ToLower(f[0])
		}
	}
	if want == "" {
		return fmt.Errorf("%s isn't listed in %s's SHA256SUMS.txt", asset, rel.Tag)
	}
	bin, err := httpGet(binURL, 200<<20)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(bin)
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(want)) != 1 {
		return fmt.Errorf("the download of %s doesn't match its checksum, so it wasn't installed", rel.Tag)
	}
	return os.WriteFile(dst, bin, 0755)
}

// checkNewBinary makes sure the download really runs and is the version we expect.
func checkNewBinary(path, tag string) error {
	cmd := exec.Command(path, "-version")
	done := make(chan error, 1)
	var out []byte
	go func() {
		var err error
		out, err = cmd.CombinedOutput()
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("the new version didn't start: %v", err)
		}
	case <-time.After(20 * time.Second):
		cmd.Process.Kill()
		return errors.New("the new version didn't start in time")
	}
	if !strings.Contains(string(out), tag) {
		return fmt.Errorf("the download says it is %q, not %s", strings.TrimSpace(string(out)), tag)
	}
	return nil
}

// outageInProgress reports whether any UPS this program knows about is on battery.
func outageInProgress() bool {
	stateMu.Lock()
	defer stateMu.Unlock()
	if link.lastOnBattery {
		return true
	}
	for _, r := range upsReadings {
		if isOnBatteryStatus(r.Status) {
			return true
		}
	}
	return false
}

// newBinaryPath is where the download goes before it replaces the running program.
// On Windows it must still end in .exe so it can be run for the check.
func newBinaryPath(exe string) string {
	if strings.HasSuffix(strings.ToLower(exe), ".exe") {
		return exe[:len(exe)-4] + ".new.exe"
	}
	return exe + ".new"
}

// cleanupAfterUpdate removes files a previous update left behind.
func cleanupAfterUpdate() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	exe, _ = filepath.EvalSymlinks(exe)
	os.Remove(exe + ".old")
	os.Remove(newBinaryPath(exe))
}

// selfUpdate installs the newest release and restarts into it. It returns the version it
// is updating to; the restart happens a moment later so the caller can report back first.
func selfUpdate() (string, error) {
	updateMu.Lock()
	if updating {
		updateMu.Unlock()
		return "", errors.New("an update is already in progress")
	}
	updating = true
	updateMu.Unlock()
	ok := false
	defer func() {
		if !ok {
			updateMu.Lock()
			updating = false
			updateMu.Unlock()
		}
	}()

	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	exe, _ = filepath.EvalSymlinks(exe)
	tag, newPath, err := prepareUpdate(exe)
	if err != nil {
		return "", err
	}
	log.Printf("[UPDATE] Updating from %s to %s and restarting.", version, tag)
	ok = true
	go func() {
		time.Sleep(time.Second) // let the reply to whoever asked go out first
		if err := installAndRestart(exe, newPath); err != nil {
			log.Printf("[UPDATE] Couldn't finish the update: %v", err)
			os.Remove(newPath)
			updateMu.Lock()
			updating = false
			updateMu.Unlock()
		}
	}()
	return tag, nil
}

// prepareUpdate finds the newest release, downloads it next to exe, checks it against the
// release's checksums and makes sure it starts. It returns the version and where it is.
func prepareUpdate(exe string) (tag, newPath string, err error) {
	if outageInProgress() {
		return "", "", errors.New("not updating while a UPS is on battery; try again once power is back")
	}
	info, rel := checkForUpdate()
	if rel == nil {
		if info.Note != "" {
			return "", "", errors.New(info.Note)
		}
		return "", "", errUpToDate
	}
	newPath = newBinaryPath(exe)
	if err := downloadVerified(rel, ownAssetName(), newPath); err != nil {
		os.Remove(newPath)
		return "", "", err
	}
	if err := checkNewBinary(newPath, rel.Tag); err != nil {
		os.Remove(newPath)
		return "", "", err
	}
	return rel.Tag, newPath, nil
}

// updateFileOnly installs the newest release over the program file at exe without
// restarting anything, for when no monitor is running.
func updateFileOnly(exe string) (string, error) {
	tag, newPath, err := prepareUpdate(exe)
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "freebsd" && exe == "/"+pfBinPath {
		if _, err := os.Stat("/" + pfRCPath); err == nil {
			// On pfSense the installer also refreshes the web page and (re)starts the service.
			cmd := exec.Command(newPath, "-install-pfsense")
			cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
			err := cmd.Run()
			os.Remove(newPath)
			return tag, err
		}
	}
	if err := swapBinary(exe, newPath); err != nil {
		os.Remove(newPath)
		return "", err
	}
	return tag, nil
}

// installAndRestart puts the new program in place and restarts into it.
func installAndRestart(exe, newPath string) error {
	// On pfSense the installer also refreshes the web page, then restarts the service.
	if runtime.GOOS == "freebsd" && exe == "/"+pfBinPath {
		if _, err := os.Stat("/" + pfRCPath); err == nil {
			cmd := exec.Command(newPath, "-install-pfsense")
			cmd.SysProcAttr = detachedProcAttr()
			return cmd.Start()
		}
	}
	return replaceAndRestart(exe, newPath)
}

// startUpdateChecks looks for a new release now and then, and says so once per new version.
func startUpdateChecks(store *settingsStore) {
	announced := ""
	time.Sleep(time.Minute)
	for {
		info, _ := checkForUpdate()
		if info.Available && info.Latest != announced {
			announced = info.Latest
			msg := fmt.Sprintf("⬆️ **Update available**: %s (running %s). Use Update in UPS Monitor to install it.", info.Latest, info.Current)
			log.Println(msg)
			s := store.Get()
			switch {
			case s.isServer():
				sendDiscordWebhook(s.Server.WebhookURL, msg)
			case s.isClient():
				sendDiscordWebhook(s.Client.WebhookURL, msg)
			}
		}
		time.Sleep(6 * time.Hour)
	}
}

// controlListener is the control socket; Windows closes it before starting the new version.
var controlListener interface{ Close() error }

// closeListeners frees the control socket and the client port for a new copy of the program.
func closeListeners() {
	if controlListener != nil {
		controlListener.Close()
	}
	hub.stop()
}

// runSelfUpdate updates this machine and logs how it went.
func runSelfUpdate() {
	tag, err := selfUpdate()
	switch {
	case errors.Is(err, errUpToDate):
		log.Printf("[UPDATE] Already up to date (%s).", version)
	case err != nil:
		log.Printf("[UPDATE] Couldn't update: %v", err)
	default:
		log.Printf("[UPDATE] Installing %s.", tag)
	}
}

var updateWait struct {
	sync.Mutex
	replies chan string
}

// updateReplied notes that a client answered an update request.
func updateReplied(name string) {
	updateWait.Lock()
	defer updateWait.Unlock()
	if updateWait.replies != nil {
		select {
		case updateWait.replies <- name:
		default:
		}
	}
}

// updateEverything asks every connected client to update, waits for their answers, and
// then updates this server (which briefly drops the clients; they reconnect by themselves).
func updateEverything(s Settings) {
	var asked []string
	for _, m := range s.Server.Machines {
		if m.Type == MachineClient && requestClientUpdate(m.Name) == nil {
			asked = append(asked, m.Name)
		}
	}
	log.Printf("[UPDATE] Asked %d connected client(s) to update: %s", len(asked), strings.Join(asked, ", "))
	if len(asked) > 0 {
		replies := make(chan string, len(asked))
		updateWait.Lock()
		updateWait.replies = replies
		updateWait.Unlock()
		timeout := time.After(90 * time.Second)
	wait:
		for got := 0; got < len(asked); got++ {
			select {
			case <-replies:
			case <-timeout:
				log.Println("[UPDATE] Not every client answered within 90 seconds; updating the server anyway.")
				break wait
			}
		}
		updateWait.Lock()
		updateWait.replies = nil
		updateWait.Unlock()
	}
	runSelfUpdate()
}

// runUpdateCommand is "ups-monitor -update": update the running monitor (or, if none is
// running, the program file) and report how it went. It returns the exit code.
func runUpdateCommand() int {
	fmt.Printf("ups-monitor %s: looking for a newer release...\n", version)
	_, err := ctlCall(ctlRequest{Cmd: "ping"})
	switch {
	case err == nil:
		return updateRunningMonitor()
	case errors.Is(err, os.ErrPermission) || strings.Contains(strings.ToLower(err.Error()), "permission denied") ||
		strings.Contains(strings.ToLower(err.Error()), "access is denied"):
		if runtime.GOOS == "windows" {
			fmt.Println("The monitor runs under another account. Run this again from an Administrator terminal.")
		} else {
			fmt.Println("The monitor runs as another user (probably root). Run this again with sudo.")
		}
		return 1
	}

	exe, err := os.Executable()
	if err != nil {
		fmt.Println("Couldn't update:", err)
		return 1
	}
	exe, _ = filepath.EvalSymlinks(exe)
	tag, err := updateFileOnly(exe)
	switch {
	case errors.Is(err, errUpToDate):
		fmt.Printf("Already up to date (%s).\n", version)
		return 0
	case err != nil:
		fmt.Println("Couldn't update:", err)
		return 1
	}
	fmt.Printf("Updated %s to %s. No monitor was running here; start it again to use the new version.\n", exe, tag)
	return 0
}

// updateRunningMonitor asks the running monitor to update and waits for it to come back.
func updateRunningMonitor() int {
	resp, err := ctlCall(ctlRequest{Cmd: "update_now"})
	if err != nil {
		fmt.Println("Couldn't update:", err)
		return 1
	}
	fmt.Println(resp.Info)
	if resp.Update == nil || !resp.Update.Available {
		return 0
	}
	want := resp.Update.Latest
	fmt.Print("Waiting for the monitor to restart")
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		fmt.Print(".")
		if r, err := ctlCall(ctlRequest{Cmd: "status"}); err == nil && r.Status != nil && r.Status.Version == want {
			fmt.Printf("\nDone: the monitor is now running %s.\n", want)
			return 0
		}
	}
	fmt.Printf("\nThe monitor hasn't come back on %s yet. Check its log: %s\n", want, filepath.Join(appDir, "activity.log"))
	return 1
}
