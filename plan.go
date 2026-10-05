package main

import (
	"sort"
	"strconv"
)

// planRow is one machine in a UPS's power plan.
type planRow struct {
	Index        int // position in the settings' machine list
	Machine      Machine
	ShutdownRank int // 1 = shut down first; 0 = not shut down
	WakeRank     int // 1 = woken first; 0 = not woken
}

// powerPlan lists the machines on one UPS in the order they shut down, with each machine's
// place in the shutdown order and in the wake order. Both come from the percentages: a
// higher shutdown % goes down earlier, a lower wake % comes back earlier, and equal
// percentages share a place. Machines left running come after those that shut down, then
// wake-only machines.
func powerPlan(machines []Machine, ups string) []planRow {
	var rows []planRow
	shutPcts := map[int]bool{}
	wakePcts := map[int]bool{}
	for i, m := range machines {
		if m.UPS != ups {
			continue
		}
		rows = append(rows, planRow{Index: i, Machine: m})
		if m.autoShutdown() {
			shutPcts[m.ShutdownAt] = true
		}
		if m.Wake != nil {
			wakePcts[m.Wake.AtPercent] = true
		}
	}
	shutRank := denseRanks(shutPcts, true)
	wakeRank := denseRanks(wakePcts, false)
	for i := range rows {
		m := rows[i].Machine
		if m.autoShutdown() {
			rows[i].ShutdownRank = shutRank[m.ShutdownAt]
		}
		if m.Wake != nil {
			rows[i].WakeRank = wakeRank[m.Wake.AtPercent]
		}
	}

	group := func(m Machine) int {
		switch {
		case m.autoShutdown():
			return 0
		case m.shutsDown():
			return 1 // shutdown turned off
		}
		return 2 // wake-only
	}
	sort.SliceStable(rows, func(a, b int) bool {
		ra, rb := rows[a], rows[b]
		if ga, gb := group(ra.Machine), group(rb.Machine); ga != gb {
			return ga < gb
		}
		if ra.ShutdownRank != rb.ShutdownRank {
			return ra.ShutdownRank < rb.ShutdownRank
		}
		if (ra.WakeRank == 0) != (rb.WakeRank == 0) {
			return rb.WakeRank == 0 // machines that are woken first
		}
		if ra.WakeRank != rb.WakeRank {
			return ra.WakeRank < rb.WakeRank
		}
		return ra.Machine.Name < rb.Machine.Name
	})
	return rows
}

// denseRanks numbers the percentages 1, 2, 3... (highest first when desc), equal values sharing a number.
func denseRanks(pcts map[int]bool, desc bool) map[int]int {
	var list []int
	for p := range pcts {
		list = append(list, p)
	}
	sort.Ints(list)
	if desc {
		sort.Sort(sort.Reverse(sort.IntSlice(list)))
	}
	ranks := map[int]int{}
	for i, p := range list {
		ranks[p] = i + 1
	}
	return ranks
}

// ordinal turns 1 into "1st", 2 into "2nd", and so on.
func ordinal(n int) string {
	suffix := "th"
	if n%100 < 11 || n%100 > 13 {
		switch n % 10 {
		case 1:
			suffix = "st"
		case 2:
			suffix = "nd"
		case 3:
			suffix = "rd"
		}
	}
	return strconv.Itoa(n) + suffix
}
