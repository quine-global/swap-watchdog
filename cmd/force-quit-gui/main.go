// force-quit-gui is a minimal, always-running process killer. It starts
// minimized at login and is revealed by swap-watchdog when memory pressure
// gets critical. Deliberately plain GTK3 (via gotk3) rather than anything
// heavier, so it has the best chance of already being alive and responsive
// exactly when the system is least able to start something new.
package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/gotk3/gotk3/glib"
	"github.com/gotk3/gotk3/gtk"
)

const refreshIntervalMs = 2000

type procInfo struct {
	pid     int
	name    string
	rssKB   int64
	swapKB  int64
	totalKB int64
}

func readProcesses() []procInfo {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var procs []procInfo
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		status, err := os.Open(filepath.Join("/proc", e.Name(), "status"))
		if err != nil {
			continue
		}
		var rss, swap int64
		scanner := bufio.NewScanner(status)
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) < 2 {
				continue
			}
			switch fields[0] {
			case "VmRSS:":
				rss, _ = strconv.ParseInt(fields[1], 10, 64)
			case "VmSwap:":
				swap, _ = strconv.ParseInt(fields[1], 10, 64)
			}
		}
		status.Close()

		if rss == 0 && swap == 0 {
			continue
		}

		name := e.Name()
		if commBytes, err := os.ReadFile(filepath.Join("/proc", e.Name(), "comm")); err == nil {
			name = strings.TrimSpace(string(commBytes))
		}

		procs = append(procs, procInfo{pid: pid, name: name, rssKB: rss, swapKB: swap, totalKB: rss + swap})
	}
	sort.Slice(procs, func(i, j int) bool { return procs[i].totalKB > procs[j].totalKB })
	return procs
}

type memStatus struct {
	ramUsedKB, ramTotalKB   int64
	swapUsedKB, swapTotalKB int64
}

// readMemStatus reports system-wide RAM and swap usage for the accounting
// line under the warning banner, as distinct from readProcesses' per-process
// breakdown.
func readMemStatus() memStatus {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return memStatus{}
	}
	defer f.Close()

	var memTotal, memAvailable, swapTotal, swapFree int64
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		val, _ := strconv.ParseInt(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			memTotal = val
		case "MemAvailable:":
			memAvailable = val
		case "SwapTotal:":
			swapTotal = val
		case "SwapFree:":
			swapFree = val
		}
	}
	return memStatus{
		ramUsedKB:   memTotal - memAvailable,
		ramTotalKB:  memTotal,
		swapUsedKB:  swapTotal - swapFree,
		swapTotalKB: swapTotal,
	}
}

// Thresholds for the headline, as swap-watchdog uses them to reveal this
// window (its SWAP_PCT_THRESHOLD and PSI_THRESHOLD defaults).
const (
	swapFullFreePct = 1.0
	pressureHighPSI = 10.0
)

// memoryPressureFull is the kernel PSI "full avg10" figure for memory, or
// -1 when it can't be read.
func memoryPressureFull() float64 {
	f, err := os.Open("/proc/pressure/memory")
	if err != nil {
		return -1
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "full ") {
			continue
		}
		for _, field := range strings.Fields(line) {
			if val, found := strings.CutPrefix(field, "avg10="); found {
				avg10, err := strconv.ParseFloat(val, 64)
				if err == nil {
					return avg10
				}
			}
		}
	}
	return -1
}

// headline says what's actually wrong, or nothing alarming when nothing
// is: the window is also on screen at times swap is fine (opened by hand,
// or at login before it minimizes). alarm puts the "alarm" style class on
// the headline and the window, which the default style shows in red
// (style.go).
func headline(m memStatus, psiFull float64) (text string, alarm bool) {
	if m.swapTotalKB > 0 {
		freePct := float64(m.swapTotalKB-m.swapUsedKB) / float64(m.swapTotalKB) * 100
		if freePct < swapFullFreePct {
			return "⚠ SWAP IS FULL", true
		}
	}
	if psiFull > pressureHighPSI {
		return "⚠ MEMORY PRESSURE IS HIGH", true
	}
	return "Force Quit Monitor", false
}

// setClass adds or removes a style class on w.
func setClass(w gtk.IWidget, class string, on bool) {
	ctx, err := w.ToWidget().GetStyleContext()
	if err != nil {
		return
	}
	if on {
		ctx.AddClass(class)
	} else {
		ctx.RemoveClass(class)
	}
}

func gb(kb int64) float64 {
	return float64(kb) / (1024 * 1024)
}

func accountingText(m memStatus) string {
	var swapPct float64
	if m.swapTotalKB > 0 {
		swapPct = float64(m.swapUsedKB) / float64(m.swapTotalKB) * 100
	}
	return fmt.Sprintf(
		"Swap: %.2f GB used / %.2f GB total (%.0f%% full)    RAM: %.2f GB used / %.2f GB total",
		gb(m.swapUsedKB), gb(m.swapTotalKB), swapPct,
		gb(m.ramUsedKB), gb(m.ramTotalKB),
	)
}

func main() {
	gtk.Init(nil)

	win, err := gtk.WindowNew(gtk.WINDOW_TOPLEVEL)
	if err != nil {
		log.Fatalf("could not create window: %v", err)
	}
	win.SetTitle("Force Quit Monitor")
	win.SetDefaultSize(700, 450)
	win.SetName("force-quit-monitor")
	reloadStyles := setupStyles()

	// Closing the window just hides it — this process must keep running so
	// the watchdog can reveal it again later without spawning anything new.
	win.Connect("delete-event", func() bool {
		win.Iconify()
		return true
	})

	box, _ := gtk.BoxNew(gtk.ORIENTATION_VERTICAL, 8)
	box.SetBorderWidth(8)
	win.Add(box)

	// A stylesheet that doesn't parse says so here (hidden otherwise).
	styleErrorLabel, _ := gtk.LabelNew("")
	styleErrorLabel.SetName("style-error")
	styleErrorLabel.SetHAlign(gtk.ALIGN_START)
	styleErrorLabel.SetLineWrap(true)
	styleErrorLabel.SetNoShowAll(true)
	box.PackStart(styleErrorLabel, false, false, 0)

	warningLabel, _ := gtk.LabelNew("")
	warningLabel.SetName("headline")
	warningLabel.SetHAlign(gtk.ALIGN_START)
	box.PackStart(warningLabel, false, false, 0)

	accountingLabel, _ := gtk.LabelNew(accountingText(readMemStatus()))
	accountingLabel.SetName("accounting")
	accountingLabel.SetHAlign(gtk.ALIGN_START)
	box.PackStart(accountingLabel, false, false, 0)

	label, _ := gtk.LabelNew("Sorted by memory + swap used, highest first. Select a process and force quit it.")
	label.SetName("hint")
	label.SetHAlign(gtk.ALIGN_START)
	box.PackStart(label, false, false, 0)

	// Columns: PID, Name, RSS (MB), Swap (MB), Total (MB) as shown, then
	// RSS, swap and total in KB, hidden, which the MB columns sort by (as
	// text, "100.0" came before "20.0").
	const (
		colPID = iota
		colName
		colRSS
		colSwap
		colTotal
		colRSSKB
		colSwapKB
		colTotalKB
	)
	store, _ := gtk.ListStoreNew(
		glib.TYPE_INT, glib.TYPE_STRING, glib.TYPE_STRING, glib.TYPE_STRING, glib.TYPE_STRING,
		glib.TYPE_INT64, glib.TYPE_INT64, glib.TYPE_INT64,
	)
	// Highest memory + swap first, kept so by the store as rows change, until
	// a column header is clicked.
	store.SetSortColumnId(colTotalKB, gtk.SORT_DESCENDING)

	treeView, _ := gtk.TreeViewNewWithModel(store)
	treeView.SetName("processes")
	addColumn := func(title string, idx, sortIdx int) {
		renderer, _ := gtk.CellRendererTextNew()
		col, _ := gtk.TreeViewColumnNewWithAttribute(title, renderer, "text", idx)
		col.SetSortColumnID(sortIdx)
		treeView.AppendColumn(col)
	}
	addColumn("PID", colPID, colPID)
	addColumn("Name", colName, colName)
	addColumn("RSS (MB)", colRSS, colRSSKB)
	addColumn("Swap (MB)", colSwap, colSwapKB)
	addColumn("Total (MB)", colTotal, colTotalKB)

	scrolled, _ := gtk.ScrolledWindowNew(nil, nil)
	scrolled.SetPolicy(gtk.POLICY_AUTOMATIC, gtk.POLICY_AUTOMATIC)
	scrolled.Add(treeView)
	box.PackStart(scrolled, true, true, 0)

	statusLabel, _ := gtk.LabelNew("")
	statusLabel.SetName("status")
	statusLabel.SetHAlign(gtk.ALIGN_START)
	box.PackStart(statusLabel, false, false, 0)

	killButton, _ := gtk.ButtonNewWithLabel("Force Quit Selected")
	killButton.SetName("force-quit")
	box.PackStart(killButton, false, false, 0)

	// selectedPID reads the PID column of the current selection, if any.
	selectedPID := func() (int, bool) {
		sel, err := treeView.GetSelection()
		if err != nil {
			return 0, false
		}
		model, iter, ok := sel.GetSelected()
		if !ok {
			return 0, false
		}
		val, err := model.(*gtk.TreeModel).GetValue(iter, 0)
		if err != nil {
			return 0, false
		}
		pidVal, err := val.GoValue()
		if err != nil {
			return 0, false
		}
		pid, ok := pidVal.(int)
		return pid, ok
	}

	// The row of each process listed, by PID.
	rows := make(map[int]*gtk.TreeIter)

	refresh := func() {
		if problems := reloadStyles(); len(problems) > 0 {
			styleErrorLabel.SetText("Style: " + strings.Join(problems, "; "))
			styleErrorLabel.Show()
		} else {
			styleErrorLabel.Hide()
		}
		mem := readMemStatus()
		text, alarm := headline(mem, memoryPressureFull())
		warningLabel.SetText(text)
		setClass(warningLabel, "alarm", alarm)
		setClass(win, "alarm", alarm)
		accountingLabel.SetText(accountingText(mem))

		// Each process keeps its row, updated in place: added when it
		// appears, removed when it's gone. The list used to be cleared and
		// refilled, which threw the scrollbar back to the top for a frame on
		// every refresh (and the selection had to be found again); a row
		// that stays put keeps both. GtkListStore's iters last as long as
		// their row.
		seen := make(map[int]bool)
		for _, p := range readProcesses() {
			seen[p.pid] = true
			iter, ok := rows[p.pid]
			if !ok {
				iter = store.Append()
				rows[p.pid] = iter
			}
			store.Set(iter,
				[]int{colPID, colName, colRSS, colSwap, colTotal, colRSSKB, colSwapKB, colTotalKB},
				[]interface{}{
					p.pid,
					p.name,
					fmt.Sprintf("%.1f", float64(p.rssKB)/1024),
					fmt.Sprintf("%.1f", float64(p.swapKB)/1024),
					fmt.Sprintf("%.1f", float64(p.totalKB)/1024),
					p.rssKB,
					p.swapKB,
					p.totalKB,
				},
			)
		}
		for pid, iter := range rows {
			if !seen[pid] {
				store.Remove(iter)
				delete(rows, pid)
			}
		}
	}
	refresh()

	glib.TimeoutAdd(refreshIntervalMs, func() bool {
		refresh()
		return true // keep repeating
	})

	killButton.Connect("clicked", func() {
		pid, ok := selectedPID()
		if !ok {
			statusLabel.SetText("No process selected.")
			return
		}
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			statusLabel.SetText(fmt.Sprintf("Failed to kill PID %d: %v", pid, err))
		} else {
			statusLabel.SetText(fmt.Sprintf("Killed PID %d.", pid))
		}
		refresh()
	})

	// Start minimized: once the window is actually mapped (a minimize
	// before that is ignored), not after a fixed delay. A 200 ms timer lost
	// that race on a slow start, the live session from the ISO, and left the
	// window open at login.
	var mappedOnce bool
	win.Connect("map-event", func() bool {
		if !mappedOnce {
			mappedOnce = true
			glib.IdleAdd(func() bool {
				win.Iconify()
				return false
			})
		}
		return false
	})
	win.ShowAll()

	gtk.Main()
}
