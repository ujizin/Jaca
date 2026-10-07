package main

import (
	"fmt"
	"os"
)

// derivedEntry mirrors DerivedDataEntry's wire format (Sources/Core/Xcode/DerivedDataEntry.swift).
type derivedEntry struct {
	Name          string  `json:"name"`
	Path          string  `json:"path"`
	WorkspacePath *string `json:"workspacePath"`
	SizeMB        int     `json:"sizeMB"`
	Kind          string  `json:"kind"`
}

// kind is live, stale or shared. An unknown kind from a newer daemon reads as shared, as in the
// app, so "clean stale" never takes it.
func (e derivedEntry) kind() string {
	if e.Kind == "live" || e.Kind == "stale" {
		return e.Kind
	}
	return "shared"
}

// subtitle and tag match DerivedDataRow in Sources/Features/Xcode/XcodeAreaView.swift.
func (e derivedEntry) subtitle() string {
	if e.WorkspacePath != nil {
		return *e.WorkspacePath
	}
	return "Shared cache"
}

func (e derivedEntry) tag() cell {
	switch e.kind() {
	case "live":
		return cell{"LIVE", sgrGreen}
	case "stale":
		return cell{"STALE", sgrYellow}
	}
	return cell{"SHARED", ""}
}

// staleKey names the header's clean button among the armed buttons. Rows are named by their path,
// which is absolute, so the two can't collide.
const staleKey = "stale"

// xcodeViewer is the app's Xcode area (XcodeAreaView, DerivedDataModel): the DerivedData
// folders with their sizes, each with a Delete, under a header that cleans the stale ones.
type xcodeViewer struct {
	toolPane

	entries  []derivedEntry  // in jacad's order: stale, shared, live, largest first in each
	loading  bool            // a scan is running
	removing map[string]bool // the paths being deleted, drawn dimmed
	cleaning bool            // "clean stale" is running
}

// runXcodePane lists DerivedData from jacad's xcode.list. There is no topic for it: the pane
// removes the rows it deletes and rescans when asked.
//
// Keys: j/k or arrows move, Enter, x or Backspace deletes (press twice to confirm, as in the
// app), S cleans the stale entries (twice too), r rescans, ? shows the keys, q quits.
func runXcodePane() int {
	c, err := dial()
	if err != nil {
		fmt.Fprintln(os.Stderr, "jaca:", err)
		return 1
	}
	defer c.Close()
	return runPane(c, func(p *pane) screen { return newXcodeViewer(p, nil) })
}

func newXcodeViewer(p *pane, back func()) *xcodeViewer {
	v := &xcodeViewer{toolPane: newToolPane(p, back), removing: map[string]bool{}}
	v.owner = v
	v.refresh()
	return v
}

func (v *xcodeViewer) handleEvent(event) {}

func (v *xcodeViewer) leave(bool) {}

// refresh is the app's "Rescan DerivedData". jacad sizes each folder with du, so it is slow.
func (v *xcodeViewer) refresh() {
	if v.loading {
		return
	}
	v.loading = true
	v.spawn(func() func() {
		var list []derivedEntry
		err := v.call("xcode.list", nil, &list, slowTimeout)
		return func() { v.scanned(list, err) }
	})
}

func (v *xcodeViewer) scanned(list []derivedEntry, err error) {
	v.loading = false
	if err != nil {
		v.failed(err)
		return
	}
	v.err = ""
	v.entries = list
	v.selected = clampIndex(v.selected, len(list))
}

// The header's numbers, computed from the rows held (DerivedDataModel).
func (v *xcodeViewer) totalMB() int {
	total := 0
	for _, e := range v.entries {
		total += e.SizeMB
	}
	return total
}

func (v *xcodeViewer) stale() (count, mb int) {
	for _, e := range v.entries {
		if e.kind() == "stale" {
			count, mb = count+1, mb+e.SizeMB
		}
	}
	return count, mb
}

func (v *xcodeViewer) summary() string {
	return fmt.Sprintf("%s total · %d entries", formatSize(v.totalMB()), len(v.entries))
}

func (v *xcodeViewer) rowCount() int { return len(v.entries) }

// press is one press of row i's Delete.
func (v *xcodeViewer) press(i int) {
	if i < 0 || i >= len(v.entries) {
		return
	}
	e := v.entries[i]
	if !v.removing[e.Path] && v.confirm(e.Path) {
		v.delete(e)
	}
}

func (v *xcodeViewer) key(k byte) {
	if k == 'S' {
		v.pressClean()
	}
}

// pressClean is one press of the header's clean button, which shows while there are stale entries.
func (v *xcodeViewer) pressClean() {
	if count, _ := v.stale(); count > 0 && !v.cleaning && v.confirm(staleKey) {
		v.cleanStale()
	}
}

func (v *xcodeViewer) deleteCall(path string) bool {
	var ok bool
	if err := v.call("xcode.delete", map[string]any{"path": path}, &ok, slowTimeout); err != nil {
		return false
	}
	return ok
}

// delete dims the row at once and asks jacad to delete the folder (DerivedDataModel.delete).
func (v *xcodeViewer) delete(e derivedEntry) {
	v.removing[e.Path] = true
	v.spawn(func() func() {
		ok := v.deleteCall(e.Path)
		return func() { v.deleted(e, ok) }
	})
}

// deleted takes a delete's result: the row goes after its fade, or comes back when it failed.
func (v *xcodeViewer) deleted(e derivedEntry, ok bool) {
	if !ok {
		delete(v.removing, e.Path)
		v.flash("Couldn't delete " + e.Name)
		return
	}
	v.after(fadeTime, func() {
		v.remove(map[string]bool{e.Path: true})
		v.flash("Deleted " + e.Name)
	})
}

// cleanStale deletes every stale entry, one after the other (DerivedDataModel.cleanAllStale).
func (v *xcodeViewer) cleanStale() {
	var paths []string
	for _, e := range v.entries {
		if e.kind() == "stale" {
			paths = append(paths, e.Path)
			v.removing[e.Path] = true
		}
	}
	if len(paths) == 0 {
		return
	}
	v.cleaning = true
	v.spawn(func() func() {
		deleted := 0
		for _, path := range paths {
			if v.deleteCall(path) {
				deleted++
			}
		}
		return func() { v.cleaned(paths, deleted) }
	})
}

// cleaned takes the result of "clean stale". Every stale row goes, the ones that failed too, as
// in the app; the toast counts the folders that were deleted and the next rescan lists the rest.
func (v *xcodeViewer) cleaned(paths []string, deleted int) {
	v.after(fadeTime, func() {
		gone := map[string]bool{}
		for _, path := range paths {
			gone[path] = true
		}
		v.remove(gone)
		v.cleaning = false
		v.flash(fmt.Sprintf("Deleted %d stale", deleted))
	})
}

// remove drops the rows at these paths. The cursor stays on the row it was on when that row is kept.
func (v *xcodeViewer) remove(paths map[string]bool) {
	at := ""
	if v.selected >= 0 && v.selected < len(v.entries) {
		at = v.entries[v.selected].Path
	}
	kept := v.entries[:0:0]
	for _, e := range v.entries {
		if paths[e.Path] {
			delete(v.removing, e.Path)
			continue
		}
		if e.Path == at {
			v.selected = len(kept)
		}
		kept = append(kept, e)
	}
	v.entries = kept
	v.selected = clampIndex(v.selected, len(kept))
}

// helpKeys are the pane's keys for the ? popup. The clean button's key shows while the button does.
func (v *xcodeViewer) helpKeys() [][2]string {
	keys := [][2]string{
		{"j  k", "Select row"},
		{"Enter  x  Backspace", "Delete"},
	}
	if count, _ := v.stale(); count > 0 {
		keys = append(keys, [2]string{"S", fmt.Sprintf("Clean %d stale", count)})
	}
	return append(keys,
		[2]string{"r", "Rescan DerivedData"},
		[2]string{"PgUp  PgDn", "Scroll"},
		[2]string{"?", "Help"},
		[2]string{"q", "Quit"})
}

// entryTable is a row laid out like DerivedDataRow: name and workspace path, the kind, the size
// and the button. The path is cut in its middle to fit, and is the first column to go, once
// less than a short path's worth of it would show.
var entryTable = rowTable{left: 2, flex: 1, middle: true, min: 16, right: []int{3}, drop: []int{1, 2, 3, 4}, button: 4}

func (v *xcodeViewer) lines(cols int) (head, body []toolLine) {
	// The header: the overline with the clean button at its right, and the summary under it.
	title := toolLine{text: sgrDim + clip("DERIVED DATA", cols) + sgrReset, item: -1}
	if count, mb := v.stale(); count > 0 {
		button := rowButton(fmt.Sprintf("Clean %d stale", count), fmt.Sprintf("Confirm? (%s)", formatSize(mb)), v.isArmed(staleKey))
		if w := cellWidth(button.text); cols-w >= len("DERIVED DATA")+2 {
			title.text = sgrDim + fit("DERIVED DATA", cols-w) + sgrReset + button.style + button.text + sgrReset
			title.x0, title.x1, title.press = cols-w+1, cols, v.pressClean
		}
	}
	head = []toolLine{title, heading(clip(v.summary(), cols)), heading("")}

	switch {
	case len(v.entries) > 0:
		rows := make([][]cell, len(v.entries))
		for i, e := range v.entries {
			rows[i] = []cell{{sanitize(e.Name), sgrBold}, {sanitize(e.subtitle()), sgrDim}, e.tag(),
				{formatSize(e.SizeMB), ""}, rowButton("Delete", "Confirm?", v.isArmed(e.Path))}
		}
		body = tableLines(entryTable.layout(rows, cols-2), 0, v.selected,
			func(i int) bool { return v.removing[v.entries[i].Path] }, v.press)
	case v.loading:
		body = []toolLine{heading(clip("Scanning DerivedData…", cols))}
	default:
		body = []toolLine{heading(clip("No DerivedData found", cols))}
	}
	return head, body
}
