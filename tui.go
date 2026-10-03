package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// The TUI is a remote control for the background monitor: every save goes over the
// control socket, is validated and written to settings.json by the monitor, and applies live.

// viewFunc builds a screen: the page and the element to focus.
type viewFunc func() (tview.Primitive, tview.Primitive)

type menuItem struct {
	label  string
	view   viewFunc
	action func()
}

type ui struct {
	app     *tview.Application
	root    *tview.Pages
	header  *tview.TextView
	menu    *tview.List
	items   []menuItem
	content *tview.Pages
	focus   tview.Primitive // focus target of the current content page
	footer  *tview.TextView

	s          Settings
	lastStatus *StatusInfo
	stopped    bool

	statusVisible atomic.Bool
	statusTop     *tview.TextView
	statusLogs    *tview.TextView
	statusFlex    *tview.Flex
}

const helpKeys = "[gray]↑↓ move · Enter/Tab open · Esc back · q quit (monitor keeps running)[-]"

func runTUI() error {
	resp, err := ctlCall(ctlRequest{Cmd: "get_settings"})
	if err != nil {
		return fmt.Errorf("connecting to the monitor: %w", err)
	}
	tview.Styles.PrimitiveBackgroundColor = tcell.ColorDefault

	u := &ui{app: tview.NewApplication(), s: *resp.Settings}
	u.build()
	if u.s.Role == "" {
		u.firstRun()
	}
	go u.refreshLoop()

	if err := u.app.SetRoot(u.root, true).EnableMouse(true).EnablePaste(true).Run(); err != nil {
		return err
	}
	if u.stopped {
		fmt.Println("UPS monitor stopped.")
	} else {
		fmt.Printf("UPS monitor keeps running in the background. Run %s again to view or change settings.\n", filepath.Base(os.Args[0]))
	}
	return nil
}

func (u *ui) build() {
	u.header = tview.NewTextView().SetDynamicColors(true)
	u.footer = tview.NewTextView().SetDynamicColors(true).SetText(helpKeys)

	u.menu = tview.NewList().ShowSecondaryText(false).SetHighlightFullLine(true)
	u.menu.SetBorder(true).SetTitle(" UPS Monitor ")
	u.menu.SetChangedFunc(func(i int, _, _ string, _ rune) {
		if i >= 0 && i < len(u.items) && u.items[i].view != nil {
			u.show(u.items[i].view)
		}
	})
	u.menu.SetSelectedFunc(func(i int, _, _ string, _ rune) {
		if i < 0 || i >= len(u.items) {
			return
		}
		if u.items[i].action != nil {
			u.items[i].action()
			return
		}
		u.app.SetFocus(u.focus)
	})
	u.menu.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		switch {
		case ev.Rune() == 'q':
			u.app.Stop()
			return nil
		case ev.Key() == tcell.KeyTab || ev.Key() == tcell.KeyRight:
			if u.focus != nil {
				u.app.SetFocus(u.focus)
			}
			return nil
		}
		return ev
	})

	u.content = tview.NewPages()
	body := tview.NewFlex().
		AddItem(u.menu, 28, 0, true).
		AddItem(u.content, 0, 1, false)
	main := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(u.header, 1, 0, false).
		AddItem(body, 0, 1, true).
		AddItem(u.footer, 1, 0, false)
	u.root = tview.NewPages().AddPage("main", main, true, true)
	u.buildMenu(0)
	u.renderHeader()
}

// buildMenu lists the screens that apply to the current role.
func (u *ui) buildMenu(selected int) {
	u.items = []menuItem{{label: "Status & Logs", view: u.statusView}, {label: "General", view: u.generalView}}
	switch u.s.Role {
	case "server":
		u.items = append(u.items,
			menuItem{label: "Power Plan", view: u.powerPlanView},
			menuItem{label: "Machines", view: u.machinesView},
			menuItem{label: "UPS Self-Tests", view: u.testsView},
			menuItem{label: "Run Actions", view: u.actionsView})
	case "client":
		u.items = append(u.items,
			menuItem{label: "Server Connection", view: u.serverConnectionView},
			menuItem{label: "Run Actions", view: u.actionsView})
	}
	u.items = append(u.items,
		menuItem{label: "Stop Background Monitor", action: u.confirmStop},
		menuItem{label: "Quit (keep running)", action: u.app.Stop},
	)

	u.menu.Clear()
	for _, it := range u.items {
		u.menu.AddItem(" "+it.label, "", 0, nil)
	}
	if selected >= len(u.items) {
		selected = 0
	}
	u.menu.SetCurrentItem(selected)
	if u.items[selected].view != nil {
		u.show(u.items[selected].view)
	}
}

func (u *ui) show(view viewFunc) {
	u.reloadSettings()
	page, focus := view()
	u.statusVisible.Store(u.statusFlex != nil && page == tview.Primitive(u.statusFlex))
	u.content.AddAndSwitchToPage("view", page, true)
	u.focus = focus
}

// open replaces the content with a sub-screen (e.g. an edit form) and focuses it.
func (u *ui) open(page, focus tview.Primitive) {
	u.statusVisible.Store(false)
	u.content.AddAndSwitchToPage("view", page, true)
	u.focus = focus
	u.app.SetFocus(focus)
}

// backTo returns to a screen (e.g. after saving an edit form) and focuses it.
func (u *ui) backTo(ret viewFunc) func() {
	return func() {
		u.show(ret)
		u.app.SetFocus(u.focus)
	}
}

func (u *ui) backToMenu() {
	u.app.SetFocus(u.menu)
}

func (u *ui) reloadSettings() {
	if resp, err := ctlCall(ctlRequest{Cmd: "get_settings"}); err == nil {
		u.s = *resp.Settings
	}
}

func (u *ui) flash(msg string) {
	u.footer.SetText(msg + "   " + helpKeys)
}

// save sends new settings to the monitor, which validates, writes, and applies them.
func (u *ui) save(s Settings, what string) bool {
	resp, err := ctlCall(ctlRequest{Cmd: "set_settings", Settings: &s})
	if err != nil {
		u.alert("Not saved:\n\n" + err.Error())
		return false
	}
	roleChanged := resp.Settings.Role != u.s.Role
	u.s = *resp.Settings
	u.flash("[green]✔ " + what + " saved and applied live[-]")
	u.renderHeader()
	if roleChanged {
		u.buildMenu(1)
		u.app.SetFocus(u.menu)
	}
	return true
}

func (u *ui) alert(text string) {
	prev := u.app.GetFocus()
	modal := tview.NewModal().SetText(text).AddButtons([]string{"OK"}).
		SetDoneFunc(func(int, string) {
			u.root.RemovePage("modal")
			u.app.SetFocus(prev)
		})
	u.root.AddPage("modal", modal, false, true)
	u.app.SetFocus(modal)
}

func (u *ui) confirm(text, yes string, onYes func()) {
	prev := u.app.GetFocus()
	modal := tview.NewModal().SetText(text).AddButtons([]string{yes, "Cancel"}).
		SetDoneFunc(func(i int, _ string) {
			u.root.RemovePage("modal")
			u.app.SetFocus(prev)
			if i == 0 {
				onYes()
			}
		})
	u.root.AddPage("modal", modal, false, true)
	u.app.SetFocus(modal)
}

func (u *ui) firstRun() {
	text := "Welcome! No settings yet.\n\nServer: an always-on box (e.g. a Raspberry Pi) that reads NUT, shuts down your machines in order and wakes them afterwards.\n\nClient: this computer only takes shutdown orders from your server."
	if legacyConfigExists() {
		text += "\n\nYour old config.json is not used by this version (it is left untouched)."
	}
	modal := tview.NewModal().SetText(text).
		AddButtons([]string{"Server", "Client", "Quit"}).
		SetDoneFunc(func(i int, _ string) {
			if i == 2 {
				u.app.Stop()
				return
			}
			u.root.RemovePage("modal")
			role := []string{"server", "client"}[i]
			if u.save(newSettings(role), "Initial setup") {
				next := "set your NUT server details"
				if role == "client" {
					u.menu.SetCurrentItem(2) // Server Connection
					next = "paste the pairing code from your server"
				}
				u.flash("[green]✔ Created " + settingsPath() + ". Next: " + next + ".[-]")
				u.app.SetFocus(u.focus)
			}
		})
	u.root.AddPage("modal", modal, false, true)
	u.app.SetFocus(modal)
}

func (u *ui) confirmStop() {
	u.confirm("Stop the background monitor?\n\nUPS monitoring, shutdowns and Wake-on-LAN stop until it is started again. (If it runs as a service, the service manager may restart it.)", "Stop", func() {
		if _, err := ctlCall(ctlRequest{Cmd: "stop"}); err != nil {
			u.alert("Could not stop the monitor: " + err.Error())
			return
		}
		u.stopped = true
		u.app.Stop()
	})
}

// frame puts a title and short explanation around a screen.
func frame(title, help string, body tview.Primitive) *tview.Flex {
	fl := tview.NewFlex().SetDirection(tview.FlexRow)
	if help != "" {
		tv := tview.NewTextView().SetDynamicColors(true).SetWrap(true).SetText("[gray]" + help + "[-]")
		// Leave room for lines that wrap on narrower terminals.
		height := 1
		for _, line := range strings.Split(help, "\n") {
			height += 1 + len([]rune(line))/90
		}
		fl.AddItem(tv, height, 0, false)
	}
	fl.AddItem(body, 0, 1, true)
	fl.SetBorder(true).SetTitle(" "+title+" ").SetBorderPadding(0, 0, 1, 1)
	return fl
}

func newForm(onCancel func()) *tview.Form {
	f := tview.NewForm().SetItemPadding(0)
	f.SetCancelFunc(onCancel)
	return f
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

func indexOf(options []string, v string) int {
	for i, o := range options {
		if o == v {
			return i
		}
	}
	return 0
}

// ---------- Status ----------

func (u *ui) statusView() (tview.Primitive, tview.Primitive) {
	u.statusTop = tview.NewTextView().SetDynamicColors(true).SetWrap(false)
	u.statusLogs = tview.NewTextView().SetDynamicColors(true).SetScrollable(true)
	u.statusLogs.SetBorder(true).SetTitle(" Recent activity ")
	u.statusLogs.SetDoneFunc(func(tcell.Key) { u.backToMenu() })
	u.statusFlex = tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(u.statusTop, 8, 0, false).
		AddItem(u.statusLogs, 0, 1, true)
	u.statusFlex.SetBorder(true).SetTitle(" Status ").SetBorderPadding(0, 0, 1, 1)
	u.renderStatus()
	return u.statusFlex, u.statusLogs
}

func (u *ui) refreshLoop() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for range ticker.C {
		resp, err := ctlCall(ctlRequest{Cmd: "status"})
		u.app.QueueUpdateDraw(func() {
			if err != nil {
				u.lastStatus = nil
				u.header.SetText("[red::b] ⚡ UPS Monitor │ lost connection to the background monitor: " + tview.Escape(err.Error()) + "[-:-:-]")
				return
			}
			u.lastStatus = resp.Status
			u.renderHeader()
			if u.statusVisible.Load() {
				u.renderStatus()
			}
		})
	}
}

func (u *ui) renderHeader() {
	role := strings.ToUpper(u.s.Role)
	if role == "" {
		role = "not configured"
	} else {
		role += " mode"
	}
	pid := ""
	if u.lastStatus != nil {
		pid = fmt.Sprintf(" (PID %d)", u.lastStatus.PID)
	}
	u.header.SetText(fmt.Sprintf("[::b] ⚡ UPS Monitor[::-] [gray]%s[-] │ attached to background monitor%s │ %s │ changes apply live", version, pid, role))
}

func chargeColor(c int) string {
	switch {
	case c >= 50:
		return "green"
	case c >= 25:
		return "yellow"
	}
	return "red"
}

func (u *ui) renderStatus() {
	st := u.lastStatus
	if st == nil {
		u.statusTop.SetText("Waiting for the monitor...")
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[::b]Monitor[::-]  PID %d · running %s · %s\n", st.PID, time.Since(st.Started).Round(time.Second), tview.Escape(st.SettingsPath))
	if st.Role == "" {
		b.WriteString("[yellow]Not configured yet. Open General to get started.[-]\n")
	} else {
		conn := "[green]● connected[-]"
		if !st.NutConnected {
			conn = "[red]● not connected[-]"
			if st.NutError != "" {
				conn += " [gray](" + tview.Escape(st.NutError) + ")[-]"
			}
		}
		if st.Source == "server" {
			fmt.Fprintf(&b, "[::b]Server[::-]   %s %s\n", tview.Escape(st.NutHost), conn)
		} else {
			fmt.Fprintf(&b, "[::b]NUT[::-]      %s %s · last poll %s\n", tview.Escape(st.NutHost), conn, formatAgo(st.LastPoll))
		}
	}

	sort.Slice(st.UPS, func(i, j int) bool { return st.UPS[i].Name < st.UPS[j].Name })
	for _, x := range st.UPS {
		charge := "[gray]?[-]"
		if x.ChargeKnown {
			charge = fmt.Sprintf("[%s]%d%%[-]", chargeColor(x.Charge), x.Charge)
		}
		power := "[green]on mains[-]"
		if isOnBatteryStatus(x.Status) {
			power = "[yellow::b]ON BATTERY[-::-]"
		}
		if strings.Contains(x.Status, "LB") {
			power = "[red::b]LOW BATTERY[-::-]"
		}
		fmt.Fprintf(&b, "[::b]UPS[::-]      %-10s %s  %s  [gray]%s[-]\n", tview.Escape(x.Name), charge, power, tview.Escape(x.Status))
	}
	for _, m := range st.Machines {
		line := fmt.Sprintf("  %-14s %-9s %-6s", tview.Escape(m.Name), m.Type, tview.Escape(m.UPS))
		if m.ShutdownState != "" {
			line += fmt.Sprintf("  off ≤%3d%%: %-28s", m.ShutdownAt, tview.Escape(m.ShutdownState))
		}
		if m.WakeState != "" {
			line += fmt.Sprintf("  on ≥%3d%%: %s", m.WakeAt, tview.Escape(m.WakeState))
		}
		b.WriteString(line + "\n")
	}
	text := strings.TrimRight(b.String(), "\n")
	u.statusTop.SetText(text)
	u.statusFlex.ResizeItem(u.statusTop, strings.Count(text, "\n")+2, 0)

	logs := make([]string, len(st.Logs))
	for i, l := range st.Logs {
		logs[i] = tview.Escape(l)
	}
	u.statusLogs.SetText(strings.Join(logs, "\n"))
	if !u.statusLogs.HasFocus() {
		u.statusLogs.ScrollToEnd()
	}
}

// ---------- General ----------

func (u *ui) generalView() (tview.Primitive, tview.Primitive) {
	s := cloneSettings(u.s)
	if s.Role == "" {
		s = newSettings("server")
	}
	normalizeSettings(&s)
	role := s.Role
	f := newForm(u.backToMenu)
	roles := []string{"server", "client"}
	f.AddDropDown("Role", roles, indexOf(roles, role), func(o string, i int) {
		if i >= 0 {
			role = o
		}
	})

	var help string
	if s.Role == "server" {
		srv := s.Server
		f.AddInputField("NUT host", srv.NUT.Host, 30, nil, func(t string) { srv.NUT.Host = strings.TrimSpace(t) })
		f.AddInputField("NUT port", strconv.Itoa(srv.NUT.Port), 6, tview.InputFieldInteger, func(t string) { srv.NUT.Port = atoi(t) })
		f.AddInputField("NUT username (optional)", srv.NUT.Username, 30, nil, func(t string) { srv.NUT.Username = t })
		f.AddPasswordField("NUT password", srv.NUT.Password, 30, '*', func(t string) { srv.NUT.Password = t })
		f.AddInputField("Check UPS every (seconds)", strconv.Itoa(srv.PollSeconds), 6, tview.InputFieldInteger, func(t string) { srv.PollSeconds = atoi(t) })
		f.AddInputField("Port for clients", strconv.Itoa(srv.ListenPort), 6, tview.InputFieldInteger, func(t string) { srv.ListenPort = atoi(t) })
		f.AddCheckbox("Wake machines after this server restarts", srv.WakeAfterRestart, func(b bool) { srv.WakeAfterRestart = b })
		f.AddInputField("Discord webhook URL", srv.WebhookURL, 50, nil, func(t string) { srv.WebhookURL = strings.TrimSpace(t) })
		help = "Server: reads NUT, shuts machines down in order and wakes them afterwards.\nThe NUT login is only needed for self-tests on most setups. The webhook gets every event\n(each machine can override it)."
	} else {
		c := s.Client
		f.AddInputField("Failsafe (seconds, 0 = off)", strconv.Itoa(c.FailsafeSeconds), 6, tview.InputFieldInteger, func(t string) { c.FailsafeSeconds = atoi(t) })
		f.AddInputField("Discord webhook URL", c.WebhookURL, 50, nil, func(t string) { c.WebhookURL = strings.TrimSpace(t) })
		help = "Client: this computer only takes shutdown orders from your server (pair it under Server Connection).\nFailsafe: if the server goes silent while the UPS is on battery, shut down after this long."
	}
	f.AddButton("Save", func() {
		out := cloneSettings(s)
		out.Role = role // the other role's settings are kept, so switching back restores them
		u.save(out, "General settings")
	})
	return frame("General", help, f), f
}

// ---------- Server connection (client) ----------

func (u *ui) serverConnectionView() (tview.Primitive, tview.Primitive) {
	s := cloneSettings(u.s)
	normalizeSettings(&s)
	c := s.Client
	code := ""
	f := newForm(u.backToMenu)
	f.AddInputField("Pairing code", "", 0, nil, func(t string) { code = t })
	f.AddInputField("Server address", c.ServerAddress, 30, nil, func(t string) { c.ServerAddress = strings.TrimSpace(t) })
	f.AddInputField("This client's name", c.Name, 24, nil, func(t string) { c.Name = t })
	f.AddPasswordField("Client key", c.Key, 0, '*', func(t string) { c.Key = t })
	f.AddInputField("Server fingerprint", c.ServerFingerprint, 0, nil, func(t string) { c.ServerFingerprint = t })
	f.AddButton("Save & Connect", func() {
		out := cloneSettings(s)
		if strings.TrimSpace(code) != "" {
			addr, id, key, fp, err := parsePairingCode(code)
			if err != nil {
				u.alert(err.Error())
				return
			}
			out.Client.ServerAddress, out.Client.Name, out.Client.Key, out.Client.ServerFingerprint = addr, id, key, fp
		}
		if out.Client.ServerAddress == "" {
			u.alert("Paste the pairing code from the server (Machines → the client → Pairing Code).")
			return
		}
		if u.save(out, "Server connection") {
			u.menu.SetCurrentItem(0)
			u.app.SetFocus(u.menu)
		}
	})
	help := "Paste the pairing code from the server (or fill the fields by hand).\nThis computer only accepts a shutdown order from that server; it opens no network ports."
	return frame("Server Connection", help, f), f
}

// ---------- Self-tests (server) ----------

func (u *ui) testsView() (tview.Primitive, tview.Primitive) {
	s := cloneSettings(u.s)
	t := &s.Server.SelfTests
	f := newForm(u.backToMenu)
	f.AddCheckbox("Scheduled tests enabled", t.Enabled, func(b bool) { t.Enabled = b })
	types := []string{"quick", "deep"}
	f.AddDropDown("Test type", types, indexOf(types, t.Type), func(o string, i int) {
		if i >= 0 {
			t.Type = o
		}
	})
	f.AddInputField("Every (months)", strconv.Itoa(t.EveryMonths), 4, tview.InputFieldInteger, func(v string) { t.EveryMonths = atoi(v) })
	f.AddInputField("UPS names (comma separated)", strings.Join(t.UPS, ", "), 30, nil, func(v string) {
		t.UPS = nil
		for _, n := range strings.Split(v, ",") {
			if n = strings.TrimSpace(n); n != "" {
				t.UPS = append(t.UPS, n)
			}
		}
	})
	f.AddInputField("Discord webhook URL", t.WebhookURL, 50, nil, func(v string) { t.WebhookURL = strings.TrimSpace(v) })
	f.AddButton("Save", func() { u.save(s, "Self-test settings") })

	var last []string
	if u.lastStatus != nil {
		for _, n := range t.UPS {
			if d, ok := u.lastStatus.LastSelfTest[n]; ok {
				last = append(last, fmt.Sprintf("%s %s", n, d.Format("2006-01-02")))
			}
		}
	}
	if len(last) == 0 {
		last = []string{"none yet"}
	}
	help := "Runs a UPS battery self-test on a schedule and reports the result. Needs a NUT login that may run\ninstant commands. Webhook defaults to the server's. Last tests: " + strings.Join(last, ", ") +
		"\nUPS names: " + u.knownUPS()
	return frame("UPS Self-Tests", help, f), f
}

// ---------- Actions ----------

func (u *ui) actionsView() (tview.Primitive, tview.Primitive) {
	l := tview.NewList().SetHighlightFullLine(true)
	run := func(action, label string) func() {
		return func() {
			if _, err := ctlCall(ctlRequest{Cmd: "action", Action: action}); err != nil {
				u.alert(label + " failed:\n\n" + err.Error())
				return
			}
			u.flash("[green]✔ " + label + " started; results appear in Status & Logs[-]")
			u.menu.SetCurrentItem(0)
			u.app.SetFocus(u.menu)
		}
	}
	if u.s.Role == "server" {
		l.AddItem("Run quick self-test now", "Starts a quick battery test on every UPS in UPS Self-Tests", 0, run("test", "Quick self-test"))
		l.AddItem("Run deep self-test now", "Runs the battery down further; takes longer", 0, run("test-long", "Deep self-test"))
		l.AddItem("Check Proxmox / TrueNAS logins", "Logs in to each one without shutting it down", 0, run("check-targets", "Login check"))
	}
	l.AddItem("Send test webhooks", "Posts a test message to every configured Discord webhook", 0, run("test-webhook", "Webhook test"))
	l.SetDoneFunc(u.backToMenu)
	return frame("Run Actions", "", l), l
}
