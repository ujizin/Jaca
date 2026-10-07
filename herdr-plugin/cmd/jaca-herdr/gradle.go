package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"slices"
	"sort"
)

// gradleDaemon mirrors GradleDaemon's wire format (Sources/Core/Gradle/GradleDaemon.swift).
type gradleDaemon struct {
	PID      int32   `json:"pid"`
	Version  string  `json:"version"`
	Uptime   string  `json:"uptime"`
	CPU      float64 `json:"cpu"`
	MemoryMB int     `json:"memoryMB"`
	JDK      *string `json:"jdk"`
	MaxHeap  *string `json:"maxHeap"`
}

func (d gradleDaemon) isBusy() bool { return d.CPU > 20 }

// ramText and cpuText match GradleDaemonRow in Sources/Features/Gradle/GradleDaemonsView.swift.
func (d gradleDaemon) ramText() string {
	if d.MemoryMB >= 1024 {
		return fmt.Sprintf("%.1f GB", float64(d.MemoryMB)/1024)
	}
	return fmt.Sprintf("%d MB", d.MemoryMB)
}

func (d gradleDaemon) cpuText() string { return fmt.Sprintf("%d%%", int(math.Round(d.CPU))) }

// gradleCache mirrors GradleCacheEntry: one folder of ~/.gradle/caches and its size.
type gradleCache struct {
	Name   string `json:"name"`
	SizeMB int    `json:"sizeMB"`
}

const gradleTopic = "gradle.daemons"

// gradleViewer is the app's Gradle area (GradleDaemonsView, GradleDaemonsModel): the cache
// folders with their sizes, then the running daemons. One cursor moves over both.
type gradleViewer struct {
	toolPane

	daemons  []gradleDaemon // sorted by pid
	loaded   bool
	removing map[int32]bool // the daemons being killed, drawn dimmed
	listed   map[int32]bool // the pids in the last list received

	caches       []gradleCache
	cacheLoading bool
	deleting     map[string]bool // the cache folders being deleted, drawn dimmed
}

// runGradlePane shows the cache sizes and the live daemon list from jacad's gradle.daemons topic.
//
// Keys: j/k or arrows move, Enter, x or Backspace kills or deletes (press twice to confirm, as
// in the app), r refreshes, ? shows the keys, q quits.
func runGradlePane() int {
	c, err := dial()
	if err != nil {
		fmt.Fprintln(os.Stderr, "jaca:", err)
		return 1
	}
	defer c.Close()
	return runPane(c, func(p *pane) screen { return newGradleViewer(p, nil) })
}

func newGradleViewer(p *pane, back func()) *gradleViewer {
	v := &gradleViewer{toolPane: newToolPane(p, back), removing: map[int32]bool{}, listed: map[int32]bool{}, deleting: map[string]bool{}}
	v.owner = v
	v.open()
	return v
}

// open subscribes to the daemon list, reads it once (the topic's next poll can be two seconds
// away) and measures the cache, as the app does when the area appears.
func (v *gradleViewer) open() {
	v.spawn(func() func() {
		err := v.call("events.subscribe", map[string]any{"topics": []string{gradleTopic}}, nil, callTimeout)
		return func() { v.failed(err) }
	})
	v.refresh()
}

// leave stops the daemon list when the pane goes back to the picker, so jacad stops polling for
// it. On exit the connection closes, which does the same.
func (v *gradleViewer) leave(wait bool) {
	if wait {
		return
	}
	v.spawn(func() func() {
		_ = v.call("events.unsubscribe", map[string]any{"topics": []string{gradleTopic}}, nil, teardownTimeout)
		return func() {}
	})
}

func (v *gradleViewer) handleEvent(ev event) {
	switch ev.Topic {
	case gradleTopic:
		var list []gradleDaemon
		if json.Unmarshal(ev.Data, &list) == nil {
			v.apply(list)
		}
	case "events.dropped":
		// The skipped update may have been the latest list: fetch it.
		var note droppedNote
		if json.Unmarshal(ev.Data, &note) == nil && note.Topic == gradleTopic {
			v.fetch()
		}
	}
}

// refresh is the app's "Refresh Gradle daemons". It measures the cache again too: the app
// measures it each time the area appears and has no control for that, and a pane stays open.
func (v *gradleViewer) refresh() {
	v.fetch()
	v.measure()
}

func (v *gradleViewer) fetch() {
	v.spawn(func() func() {
		var list []gradleDaemon
		err := v.call("gradle.list", nil, &list, syncTimeout)
		return func() {
			if err != nil {
				v.failed(err)
				return
			}
			v.err = ""
			v.apply(list)
		}
	})
}

// apply takes a new daemon list (GradleDaemonsModel.apply). A daemon being killed that the list
// no longer has stays, dimmed, until the kill's result removes it. The cursor stays on the row
// it was on.
func (v *gradleViewer) apply(list []gradleDaemon) {
	at := v.keyAt(v.selected)
	listed := map[int32]bool{}
	next := make([]gradleDaemon, 0, len(list))
	for _, d := range list {
		if !listed[d.PID] {
			listed[d.PID] = true
			next = append(next, d)
		}
	}
	for _, d := range v.daemons {
		if v.removing[d.PID] && !listed[d.PID] {
			next = append(next, d)
		}
	}
	sort.SliceStable(next, func(i, j int) bool { return next[i].PID < next[j].PID })
	// Rows that changed place are drawn at the next tick: until then a click is not for them.
	v.moved = v.moved || !slices.EqualFunc(v.daemons, next, func(a, b gradleDaemon) bool { return a.PID == b.PID })
	v.daemons, v.listed, v.loaded = next, listed, true
	v.reselect(at)
}

// measure reads the cache sizes, which jacad computes with du.
func (v *gradleViewer) measure() {
	if v.cacheLoading {
		return
	}
	v.cacheLoading = true
	v.spawn(func() func() {
		var entries []gradleCache
		err := v.call("gradle.caches", nil, &entries, slowTimeout)
		return func() { v.measured(entries, err) }
	})
}

func (v *gradleViewer) measured(entries []gradleCache, err error) {
	v.cacheLoading = false
	if err != nil {
		v.failed(err)
		return
	}
	at := v.keyAt(v.selected)
	v.caches = entries
	v.reselect(at)
}

func (v *gradleViewer) cacheTotalMB() int {
	total := 0
	for _, c := range v.caches {
		total += c.SizeMB
	}
	return total
}

// showsCache: the section shows while it is measured or has entries (GradleDaemonsView.content).
func (v *gradleViewer) showsCache() bool { return v.cacheLoading || len(v.caches) > 0 }

func (v *gradleViewer) rowCount() int { return len(v.caches) + len(v.daemons) }

func cacheKey(name string) string { return "cache:" + name }
func pidKey(pid int32) string     { return fmt.Sprintf("pid:%d", pid) }

// keyAt names the row at cursor position i ("" for none), so the cursor can follow it.
func (v *gradleViewer) keyAt(i int) string {
	switch {
	case i < 0:
	case i < len(v.caches):
		return cacheKey(v.caches[i].Name)
	case i-len(v.caches) < len(v.daemons):
		return pidKey(v.daemons[i-len(v.caches)].PID)
	}
	return ""
}

func (v *gradleViewer) reselect(key string) {
	for i := 0; key != "" && i < v.rowCount(); i++ {
		if v.keyAt(i) == key {
			v.selected = i
			return
		}
	}
	v.selected = clampIndex(v.selected, v.rowCount())
}

// press is one press of row i's button: Delete for a cache folder, Kill for a daemon.
func (v *gradleViewer) press(i int) {
	switch {
	case i < 0:
	case i < len(v.caches):
		name := v.caches[i].Name
		if !v.deleting[name] && v.confirm(cacheKey(name)) {
			v.deleteCache(name)
		}
	case i-len(v.caches) < len(v.daemons):
		pid := v.daemons[i-len(v.caches)].PID
		if !v.removing[pid] && v.confirm(pidKey(pid)) {
			v.kill(pid)
		}
	}
}

func (v *gradleViewer) rowID(i int) string {
	switch {
	case i < 0:
	case i < len(v.caches):
		return cacheKey(v.caches[i].Name)
	case i-len(v.caches) < len(v.daemons):
		return pidKey(v.daemons[i-len(v.caches)].PID)
	}
	return ""
}

func (v *gradleViewer) key(byte) {}

// kill dims the row at once and asks jacad to kill the daemon (GradleDaemonsModel.kill).
func (v *gradleViewer) kill(pid int32) {
	v.removing[pid] = true
	v.spawn(func() func() {
		var ok bool
		if err := v.call("gradle.kill", map[string]any{"pid": pid}, &ok, callTimeout); err != nil {
			ok = false
		}
		return func() { v.killed(pid, ok) }
	})
}

// killed takes a kill's result: the row goes after its fade, goes at once when the daemon had
// already exited on its own (the last list doesn't have it), or comes back when the kill failed.
func (v *gradleViewer) killed(pid int32, ok bool) {
	switch {
	case ok:
		v.after(fadeTime, func() {
			v.removeDaemon(pid)
			v.flash(fmt.Sprintf("Killed %d", pid))
		})
	case !v.listed[pid]:
		v.removeDaemon(pid)
	default:
		delete(v.removing, pid)
		v.flash(fmt.Sprintf("Couldn't kill %d", pid))
	}
}

func (v *gradleViewer) removeDaemon(pid int32) {
	at := v.keyAt(v.selected)
	delete(v.removing, pid)
	kept := v.daemons[:0:0]
	for _, d := range v.daemons {
		if d.PID != pid {
			kept = append(kept, d)
		}
	}
	v.daemons = kept
	v.reselect(at)
}

// deleteCache deletes a folder of ~/.gradle/caches. Its row is dimmed meanwhile and goes only
// once jacad reports it deleted (GradleDaemonsModel.deleteCache).
func (v *gradleViewer) deleteCache(name string) {
	v.deleting[name] = true
	v.spawn(func() func() {
		var ok bool
		if err := v.call("gradle.deleteCache", map[string]any{"name": name}, &ok, slowTimeout); err != nil {
			ok = false
		}
		return func() { v.cacheDeleted(name, ok) }
	})
}

func (v *gradleViewer) cacheDeleted(name string, ok bool) {
	delete(v.deleting, name)
	if !ok {
		v.flash("Couldn't delete " + name)
		return
	}
	at := v.keyAt(v.selected)
	kept := v.caches[:0:0]
	for _, c := range v.caches {
		if c.Name != name {
			kept = append(kept, c)
		}
	}
	v.caches = kept
	v.reselect(at)
	v.flash("Deleted cache " + name)
}

// helpKeys are the pane's keys for the ? popup. The action reads as the selected row's button.
func (v *gradleViewer) helpKeys() [][2]string {
	action := "Kill"
	if v.selected < len(v.caches) {
		action = "Delete"
	}
	return [][2]string{
		{"j  k", "Select row"},
		{"Enter  x  Backspace", action},
		{"r", "Refresh Gradle daemons"},
		{"PgUp  PgDn", "Scroll"},
		{"?", "Help"},
		{"q", "Quit"},
	}
}

// The columns of a cache row (name, size, button) and of a daemon row, laid out like
// GradleDaemonRow: name and PID, the tags, uptime and CPU, and the button. In a narrow pane the
// name is cut first, then columns go, the tags before the rest.
var (
	cacheTable  = rowTable{left: 1, min: 8, right: []int{1}, drop: []int{1, 2}, button: 2}
	daemonTable = rowTable{left: 2, min: 8, right: []int{6, 7}, drop: []int{3, 4, 5, 7, 6, 1, 2, 8}, button: 8}
)

func (v *gradleViewer) lines(cols int) (head, body []toolLine) {
	w := cols - 2 // the selection marker takes two cells
	if v.showsCache() {
		total := "Calculating…"
		if len(v.caches) > 0 {
			total = formatSize(v.cacheTotalMB()) + " total"
		}
		body = append(body, heading(headingRow("GRADLE CACHE", total, cols)))
		if len(v.caches) == 0 {
			body = append(body, heading(clip("Measuring cache size…", cols)))
		} else {
			rows := make([][]cell, len(v.caches))
			for i, c := range v.caches {
				rows[i] = []cell{{sanitize(c.Name), sgrBold}, {formatSize(c.SizeMB), ""},
					rowButton("Delete", "Confirm?", v.isArmed(cacheKey(c.Name)))}
			}
			body = append(body, tableLines(cacheTable.layout(rows, w), 0, v.selected,
				func(i int) bool { return v.deleting[v.caches[i].Name] }, v.press)...)
		}
		body = append(body, heading(""))
	}

	body = append(body, heading(sgrDim+clip("DAEMONS", cols)+sgrReset))
	switch {
	case !v.loaded:
		// Nothing received yet: show nothing rather than a guess.
	case len(v.daemons) == 0:
		body = append(body, heading(clip("No Gradle daemons running", cols)))
	default:
		rows := make([][]cell, len(v.daemons))
		for i, d := range v.daemons {
			state := cell{"IDLE", ""}
			if d.isBusy() {
				state = cell{"BUSY", sgrGreen}
			}
			var jdk, heap cell
			if d.JDK != nil {
				jdk.text = sanitize("JDK " + *d.JDK)
			}
			if d.MaxHeap != nil {
				heap.text = sanitize(*d.MaxHeap)
			}
			// Fields come from process command lines: strip control bytes before they reach the terminal.
			rows[i] = []cell{{sanitize("Gradle " + d.Version), sgrBold}, {fmt.Sprintf("PID %d", d.PID), sgrDim},
				state, jdk, heap, {d.ramText(), ""}, {sanitize(d.Uptime), ""}, {d.cpuText(), sgrDim},
				rowButton("Kill", "Confirm kill?", v.isArmed(pidKey(d.PID)))}
		}
		body = append(body, tableLines(daemonTable.layout(rows, w), len(v.caches), v.selected,
			func(i int) bool { return v.removing[v.daemons[i].PID] }, v.press)...)
	}
	return nil, body
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
