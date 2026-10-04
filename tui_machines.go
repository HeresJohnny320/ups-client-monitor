package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

var machineTypeLabels = map[string]string{
	MachineProxmox:  "Proxmox VE",
	MachineTrueNAS:  "TrueNAS",
	MachineClient:   "Client (this app)",
	MachineWakeOnly: "Wake-on-LAN only",
}

func (u *ui) machines() []Machine {
	if u.s.Server == nil {
		return nil
	}
	return u.s.Server.Machines
}

// upsNames lists UPS names seen on NUT plus those already used by machines.
func (u *ui) upsNames() []string {
	set := map[string]bool{}
	if u.lastStatus != nil {
		for _, x := range u.lastStatus.UPS {
			set[x.Name] = true
		}
	}
	for _, m := range u.machines() {
		set[m.UPS] = true
	}
	delete(set, "")
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func (u *ui) knownUPS() string {
	if names := u.upsNames(); len(names) > 0 {
		return strings.Join(names, ", ")
	}
	return "none seen yet"
}

func (u *ui) defaultUPS() string {
	if names := u.upsNames(); len(names) > 0 {
		return names[0]
	}
	return "ups1"
}

// addUPSField adds a UPS name field that suggests known UPS names while typing.
func (u *ui) addUPSField(f *tview.Form, value string, changed func(string)) {
	field := tview.NewInputField().SetLabel("UPS").SetText(value).SetFieldWidth(20)
	field.SetChangedFunc(changed)
	field.SetAutocompleteFunc(func(cur string) []string {
		if cur == "" {
			return nil
		}
		var out []string
		for _, n := range u.upsNames() {
			if n != cur && strings.HasPrefix(strings.ToLower(n), strings.ToLower(cur)) {
				out = append(out, n)
			}
		}
		return out
	})
	field.SetAutocompletedFunc(func(text string, _ int, source int) bool {
		if source != tview.AutocompletedNavigate {
			field.SetText(text)
		}
		return source == tview.AutocompletedEnter || source == tview.AutocompletedClick
	})
	f.AddFormItem(field)
}

func (u *ui) machineState(name string) (string, string) {
	if u.lastStatus != nil {
		for _, m := range u.lastStatus.Machines {
			if m.Name == name {
				return m.ShutdownState, m.WakeState
			}
		}
	}
	return "", ""
}

func newMachine(ups string) Machine {
	return Machine{Type: MachineProxmox, UPS: ups, ShutdownAt: 50}
}

// ---------- Machines list ----------

func (u *ui) machinesView() (tview.Primitive, tview.Primitive) {
	t := tview.NewTable().SetSelectable(true, false).SetFixed(1, 0)
	for col, h := range []string{"Name", "Type", "UPS", "Off at", "On at", "State"} {
		t.SetCell(0, col, tview.NewTableCell(h).SetAttributes(tcell.AttrBold).SetSelectable(false).SetExpansion(1))
	}
	ms := u.machines()
	for i, m := range ms {
		row := i + 1
		off, on := "—", "—"
		if m.shutsDown() {
			off = fmt.Sprintf("≤ %d%%", m.ShutdownAt)
		}
		if m.Wake != nil {
			on = fmt.Sprintf("≥ %d%%", m.Wake.AtPercent)
		}
		state, _ := u.machineState(m.Name)
		t.SetCell(row, 0, tview.NewTableCell(tview.Escape(m.Name)))
		t.SetCell(row, 1, tview.NewTableCell(machineTypeLabels[m.Type]))
		t.SetCell(row, 2, tview.NewTableCell(tview.Escape(m.UPS)))
		t.SetCell(row, 3, tview.NewTableCell(off))
		t.SetCell(row, 4, tview.NewTableCell(on))
		t.SetCell(row, 5, tview.NewTableCell(tview.Escape(state)))
	}
	t.SetCell(len(ms)+1, 0, tview.NewTableCell("[green]+ Add machine[-]"))
	t.Select(1, 0)
	t.SetSelectedFunc(func(row, _ int) {
		idx := row - 1
		if idx >= len(ms) {
			u.editMachine(-1, newMachine(u.defaultUPS()), u.machinesView)
			return
		}
		u.editMachine(idx, ms[idx], u.machinesView)
	})
	t.SetDoneFunc(func(key tcell.Key) {
		if key == tcell.KeyEscape {
			u.backToMenu()
		}
	})
	help := "Everything the server shuts down and/or wakes: Proxmox, TrueNAS, client PCs (this app) and\nWake-on-LAN-only machines. Enter to edit, or pick \"+ Add machine\"."
	return frame("Machines", help, t), t
}

// ---------- Power plan ----------

type planStep struct {
	pct  int
	name string
	note string
	open func()
}

// powerPlanView shows, per UPS, the shutdown order on battery and the wake order once power is back.
func (u *ui) powerPlanView() (tview.Primitive, tview.Primitive) {
	ret := u.powerPlanView
	t := tview.NewTable().SetSelectable(true, false)
	actions := map[int]func(){}
	row := 0
	add := func(text string, open func()) {
		cell := tview.NewTableCell(text).SetExpansion(1)
		if open == nil {
			cell.SetSelectable(false)
		} else {
			actions[row] = open
		}
		t.SetCell(row, 0, cell)
		row++
	}

	ms := u.machines()
	for _, ups := range u.upsNames() {
		state := "[gray]not seen on NUT yet[-]"
		if u.lastStatus != nil {
			for _, x := range u.lastStatus.UPS {
				if x.Name != ups {
					continue
				}
				charge := "?"
				if x.ChargeKnown {
					charge = fmt.Sprintf("%d%%", x.Charge)
				}
				state = "[green]on mains " + charge + "[-]"
				if isOnBatteryStatus(x.Status) {
					state = "[yellow::b]ON BATTERY " + charge + "[-::-]"
				}
			}
		}
		add(fmt.Sprintf("[::b]⚡ %s[::-]   %s", tview.Escape(ups), state), nil)

		var down, up []planStep
		var noWake []planStep
		for i, m := range ms {
			if m.UPS != ups {
				continue
			}
			i, m := i, m
			open := func() { u.editMachine(i, m, ret) }
			if m.shutsDown() {
				step := planStep{m.ShutdownAt, m.Name, machineTypeLabels[m.Type], open}
				down = append(down, step)
				if m.Wake == nil {
					noWake = append(noWake, step)
				}
			}
			if m.Wake != nil {
				up = append(up, planStep{m.Wake.AtPercent, m.Name, m.Wake.MAC, open})
			}
		}
		// Highest % shuts down first; lowest % wakes first.
		sort.SliceStable(down, func(a, b int) bool { return down[a].pct > down[b].pct })
		sort.SliceStable(up, func(a, b int) bool { return up[a].pct < up[b].pct })

		add("  [yellow]On battery, as the charge drops:[-]", nil)
		if len(down) == 0 {
			add("    [gray]nothing is shut down[-]", nil)
		}
		for _, d := range down {
			add(fmt.Sprintf("    ≤ %3d%%   shut down   %-20s [gray]%s[-]", d.pct, tview.Escape(d.name), d.note), d.open)
		}
		add("  [green]Power back, as the battery recharges:[-]", nil)
		if len(up) == 0 {
			add("    [gray]nothing is woken[-]", nil)
		}
		for _, w := range up {
			add(fmt.Sprintf("    ≥ %3d%%   wake        %-20s [gray]%s[-]", w.pct, tview.Escape(w.name), w.note), w.open)
		}
		for _, d := range noWake {
			add(fmt.Sprintf("    [orange]! %s has no Wake-on-LAN MAC, so it stays off after an outage[-]", tview.Escape(d.name)), d.open)
		}
		add("", nil)
	}
	add("[green]+ Add machine[-]", func() { u.editMachine(-1, newMachine(u.defaultUPS()), ret) })

	first := row
	for r := range actions {
		if r < first {
			first = r
		}
	}
	t.Select(first, 0)
	t.SetSelectedFunc(func(r, _ int) {
		if open := actions[r]; open != nil {
			open()
		}
	})
	t.SetDoneFunc(func(key tcell.Key) {
		if key == tcell.KeyEscape {
			u.backToMenu()
		}
	})
	help := "Everything this server does, per UPS. On battery, machines shut down as the charge drops.\nAfter an outage, they are woken as the battery recharges. Enter to edit, or add below."
	return frame("Power Plan", help, t), t
}

// ---------- Machine form ----------

// machineDraft holds a machine being edited, including blocks for types it is not (yet),
// so switching the type back and forth in the form does not lose what was typed.
type machineDraft struct {
	m  Machine
	px ProxmoxAPI
	tn TrueNASAPI
	wk WakeSettings
}

func (u *ui) editMachine(idx int, m Machine, ret viewFunc) {
	d := machineDraft{m: m.clone(), wk: WakeSettings{AtPercent: 80}}
	if m.Proxmox != nil {
		d.px = *m.Proxmox
	}
	if m.TrueNAS != nil {
		d.tn = *m.TrueNAS
	}
	if m.Wake != nil {
		d.wk = *m.Wake
	}
	u.machineForm(idx, d, ret)
}

// build turns the draft into the machine that gets saved.
func (d machineDraft) build() Machine {
	m := d.m.clone()
	m.Proxmox, m.TrueNAS, m.Wake = nil, nil, nil
	switch m.Type {
	case MachineProxmox:
		px := d.px
		m.Proxmox = &px
	case MachineTrueNAS:
		tn := d.tn
		m.TrueNAS = &tn
	case MachineClient:
		if m.ClientKey == "" {
			m.ClientKey = newClientKey()
		}
	}
	if d.wk.MAC != "" || m.Type == MachineWakeOnly {
		wk := d.wk
		m.Wake = &wk
	}
	return m
}

func (u *ui) machineForm(idx int, d machineDraft, ret viewFunc) {
	back := u.backTo(ret)
	f := newForm(back)
	m := &d.m

	labels := make([]string, len(machineTypes))
	for i, t := range machineTypes {
		labels[i] = machineTypeLabels[t]
	}
	f.AddDropDown("Type", labels, indexOf(machineTypes, m.Type), func(_ string, i int) {
		if i >= 0 && machineTypes[i] != m.Type {
			m.Type = machineTypes[i]
			u.machineForm(idx, d, ret) // show the fields for the new type
		}
	})
	f.AddInputField("Name", m.Name, 24, nil, func(s string) { m.Name = strings.TrimSpace(s) })
	u.addUPSField(f, m.UPS, func(s string) { m.UPS = strings.TrimSpace(s) })
	if m.Type != MachineWakeOnly {
		f.AddInputField("Shut down at battery %", strconv.Itoa(m.ShutdownAt), 4, tview.InputFieldInteger, func(s string) { m.ShutdownAt = atoi(s) })
	}

	var help string
	switch m.Type {
	case MachineProxmox:
		f.AddInputField("Host", d.px.Host, 40, nil, func(s string) { d.px.Host = strings.TrimSpace(s) })
		f.AddInputField("Node name", d.px.Node, 24, nil, func(s string) { d.px.Node = strings.TrimSpace(s) })
		f.AddPasswordField("API token", d.px.Token, 60, '*', func(s string) { d.px.Token = strings.TrimSpace(s) })
		f.AddCheckbox("Verify TLS certificate", d.px.VerifyTLS, func(b bool) { d.px.VerifyTLS = b })
		help = "Host: https://IP:8006 · Node: name in the Proxmox sidebar · Token: USER@REALM!TOKENID=SECRET\n(needs Sys.PowerMgmt on /nodes/<node>). Leave TLS verification off for the self-signed certificate."
	case MachineTrueNAS:
		f.AddInputField("Host (https)", d.tn.Host, 40, nil, func(s string) { d.tn.Host = strings.TrimSpace(s) })
		f.AddInputField("Username", d.tn.Username, 24, nil, func(s string) { d.tn.Username = strings.TrimSpace(s) })
		f.AddPasswordField("API key", d.tn.APIKey, 60, '*', func(s string) { d.tn.APIKey = strings.TrimSpace(s) })
		f.AddCheckbox("Verify TLS certificate", d.tn.VerifyTLS, func(b bool) { d.tn.VerifyTLS = b })
		help = "Host must be https:// (TrueNAS revokes API keys sent over http). Username: the key's owner.\nCreate the key under Settings (top-right) → API Keys. Leave TLS verification off for self-signed."
	case MachineClient:
		help = "A computer running this app in client mode. After saving, paste its pairing code into the\nclient's Server Connection screen. Renaming it means pairing it again."
	case MachineWakeOnly:
		help = "Never shut down by this server; only woken after an outage once the battery reaches the set %."
	}
	macLabel := "Wake-on-LAN MAC (optional)"
	if m.Type == MachineWakeOnly {
		macLabel = "Wake-on-LAN MAC"
	}
	f.AddInputField(macLabel, d.wk.MAC, 20, nil, func(s string) { d.wk.MAC = strings.TrimSpace(s) })
	f.AddInputField("Wake at battery %", strconv.Itoa(d.wk.AtPercent), 4, tview.InputFieldInteger, func(s string) { d.wk.AtPercent = atoi(s) })
	f.AddInputField("WOL broadcast (optional)", d.wk.Broadcast, 18, nil, func(s string) { d.wk.Broadcast = strings.TrimSpace(s) })
	f.AddInputField("Discord webhook (optional)", m.WebhookURL, 50, nil, func(s string) { m.WebhookURL = strings.TrimSpace(s) })
	help += "\nWake: fill in the MAC to turn it back on after an outage (enable Wake-on-LAN in its BIOS). Leave the\n" +
		"broadcast empty to send on every local network. UPS names: " + u.knownUPS() + " · Webhook defaults to the server's (General)."

	saveLabel := "Save"
	if m.Type == MachineClient {
		saveLabel = "Save & Show Pairing Code"
	}
	f.AddButton(saveLabel, func() {
		built := d.build()
		s := cloneSettings(u.s)
		if idx >= 0 {
			s.Server.Machines[idx] = built
		} else {
			s.Server.Machines = append(s.Server.Machines, built)
		}
		if u.save(s, built.Name) {
			back()
			if built.Type == MachineClient {
				u.showPairing(built)
			}
		}
	})
	if m.Type == MachineProxmox || m.Type == MachineTrueNAS {
		f.AddButton("Test Connection", func() {
			target := d.build().apiTarget()
			u.flash("[yellow]Testing " + tview.Escape(target.Name) + " (login only, nothing is shut down)...[-]")
			go func() {
				info, err := checkRemoteTarget(target)
				u.app.QueueUpdateDraw(func() {
					if err != nil {
						u.flash("[red]✘ Connection test failed[-]")
						u.alert("Connection test failed:\n\n" + err.Error())
						return
					}
					u.flash("[green]✔ " + tview.Escape(target.Name) + ": " + tview.Escape(info) + "[-]")
				})
			}()
		})
	}
	if idx >= 0 && m.Type == MachineClient && m.ClientKey != "" {
		f.AddButton("Pairing Code", func() { u.showPairing(d.build()) })
		f.AddButton("New Key", func() {
			u.confirm("Give "+m.Name+" a new key? It disconnects now and must be paired again with the new code.", "New Key", func() {
				m.ClientKey = newClientKey()
				built := d.build()
				s := cloneSettings(u.s)
				s.Server.Machines[idx] = built
				if u.save(s, "New key for "+built.Name+";") {
					back()
					u.showPairing(built)
				}
			})
		})
	}
	f.AddButton("Wake Now", func() {
		if d.wk.MAC == "" {
			u.alert("Fill in the Wake-on-LAN MAC first.")
			return
		}
		via, err := wakeNodeVia(d.wk.MAC, d.wk.Broadcast)
		if err != nil {
			u.alert("Wake-on-LAN failed:\n\n" + err.Error())
			return
		}
		u.flash("[green]✔ Magic packet sent to " + tview.Escape(d.wk.MAC) + " via " + tview.Escape(strings.Join(via, ", ")) + "[-]")
	})
	if idx >= 0 {
		f.AddButton("Delete", func() {
			u.confirm("Remove "+m.Name+"? It will no longer be shut down or woken.", "Delete", func() {
				s := cloneSettings(u.s)
				s.Server.Machines = append(s.Server.Machines[:idx], s.Server.Machines[idx+1:]...)
				if u.save(s, "Removed "+m.Name+";") {
					back()
				}
			})
		})
	}
	f.AddButton("Cancel", back)

	title := "Add Machine"
	if idx >= 0 {
		title = "Edit " + m.Name
	}
	u.open(frame(title, help, f), f)
}

// showPairing displays the code to paste into the client's Server Connection screen.
func (u *ui) showPairing(m Machine) {
	resp, err := ctlCall(ctlRequest{Cmd: "pairing", Name: m.Name})
	if err != nil {
		u.alert("Could not get the pairing code: " + err.Error())
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[::b]Pairing code for %s[::-]\n\n", tview.Escape(m.Name))
	b.WriteString("On the client: run ups-monitor, choose Client, open Server Connection and paste this.\n")
	b.WriteString("[gray]Select with Shift + mouse drag to copy. Treat it like a password.[-]\n\n")
	for i, p := range resp.Pairing {
		if len(resp.Pairing) > 1 {
			fmt.Fprintf(&b, "[yellow]Using %s:[-]\n", p.Address)
		}
		b.WriteString(p.Code)
		if i < len(resp.Pairing)-1 {
			b.WriteString("\n\n")
		}
	}
	tv := tview.NewTextView().SetDynamicColors(true).SetWrap(true).SetWordWrap(false).SetText(b.String())
	tv.SetBorder(true).SetTitle(" Pairing Code (Enter/Esc to close) ").SetBorderPadding(1, 1, 2, 2)
	prev := u.app.GetFocus()
	tv.SetDoneFunc(func(tcell.Key) {
		u.root.RemovePage("pairing")
		u.app.SetFocus(prev)
	})
	u.root.AddPage("pairing", tview.NewGrid().SetColumns(0, 100, 0).SetRows(0, 18, 0).AddItem(tv, 1, 1, 1, 1, 0, 0, true), true, true)
	u.app.SetFocus(tv)
}
