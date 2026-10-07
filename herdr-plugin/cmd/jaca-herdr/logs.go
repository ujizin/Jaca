package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// logState mirrors LogStreamState (Sources/Core/Logs/LogStreamEngine.swift).
type logState struct {
	IsRunning     bool    `json:"isRunning"`
	IsConnecting  bool    `json:"isConnecting"`
	StatusMessage *string `json:"statusMessage"`
	// Every PID the targeted package has had this session; nil = no PID filter.
	PIDs []int32 `json:"pids"`
}

// appEntry mirrors AppEntry (Sources/Core/Devices/InstalledApps.swift).
type appEntry struct {
	ID        string  `json:"id"`
	Name      *string `json:"name"`
	IsUserApp bool    `json:"isUserApp"`
}

func (a appEntry) display() string {
	if a.Name != nil {
		return *a.Name
	}
	return a.ID
}

const maxLines = 20000

// What the keys act on in the log viewer.
type logFocus int

const (
	focusLog     logFocus = iota // the log list
	focusSearch                  // the filter text field
	focusPackage                 // the package id field
	focusApps                    // the installed-apps list
	focusHelp                    // the keys popup
	focusMenu                    // the right-click menu
	focusFormat                  // the Copy format popup
)

// logViewer streams one device's logs with the app log tab's tools (LogSessionView): the level,
// regex and system-log chips, the text filter, the package filter with its installed-apps list,
// clear, and the status bar.
type logViewer struct {
	p      *pane
	device device
	back   func() // returns to the picker when the viewer runs in its pane; nil in a tab
	left   bool   // the pane moved on; a logs.open that lands now is closed

	paused  bool   // the user stopped the stream (p or space), as opposed to it ending by itself
	opening bool   // a logs.open is in flight
	err     string // why the last call failed, as jacad reported it
	session string
	state   logState

	lines   []logLine
	visible []logLine // the lines the filter keeps, in step with lines
	total   int       // lines received since the last clear
	dropped int       // lines trimmed off the front since the last clear
	lastSeq uint64    // seq of the newest line received; valid when hasSeq
	hasSeq  bool
	follow  bool   // pinned to the tail; false once scrolled up
	saved   string // where the last export went, shown under the toolbar
	offset  int    // visible lines scrolled up from the tail when not following

	filter  logFilter
	focus   logFocus
	search  textInput
	pkg     textInput
	applied string // the package jacad was last asked to target

	hits []hit // the toolbar's clickable spans, as last drawn

	sel      selection
	logTop   int       // the log area as last drawn: the screen row of its first line,
	rowLines []logLine // and the lines on screen

	menu   *popupMenu
	editor *formatEditor
	// The status bar's buttons as last drawn: their columns (1-based), 0 while not showing.
	helpX0, helpX1, formatX0, formatX1 int

	apps        []appEntry
	appsLoading bool
	appsLoaded  bool
	appQuery    textInput
	appSelected int
	appTop      int // the apps list as last drawn: the screen row of its first row,
	appStart    int // and that row's index (-1 while the list isn't showing)
	retryRow    int // the screen row of Retry, 0 while it isn't showing
}

// selection is the log lines selected with the mouse: a click selects a line, a drag extends it
// line by line, and letting go copies it. The pane has the mouse (the toolbar is clickable), so
// the terminal can't select here. Lines are held by seq, so the selection stays on them while
// the view scrolls.
type selection struct {
	on        bool // lines are highlighted
	dragging  bool
	held      bool // the view was following the tail and is held still under the selection
	all       bool // every line the filter keeps (Select All), on screen or not
	moves     int  // presses and pointer moves in this drag, to tell when it has come to rest
	edge      int  // while dragging: -1 with the pointer above the log lines, +1 below, else 0
	scrolling bool // an edge scroll is scheduled
	// The lines the drag started and ended on; the selection is every line between them.
	anchor, cursor uint64
}

// has reports whether the line with this seq is selected.
func (s selection) has(seq uint64) bool {
	if !s.on {
		return false
	}
	return s.all || (seq >= min(s.anchor, s.cursor) && seq <= max(s.anchor, s.cursor))
}

// popupMenu is the right-click menu: a box of items at the pointer. An item with no label is a
// separator.
type popupMenu struct {
	x, y     int // where it was opened (1-based cell)
	items    []menuItem
	selected int
	top      int // as last drawn: the screen row of its top border,
	left     int // its first column,
	width    int // and its width
	first    int // the first item drawn, when the pane is too short for them all,
	shown    int // and how many are
}

type menuItem struct {
	label string
	act   func()
}

// move steps the menu's selection over the separators.
func (m *popupMenu) move(by int) {
	for i := m.selected + by; i >= 0 && i < len(m.items); i += by {
		if m.items[i].label != "" && m.items[i].act != nil { // not a separator or a heading
			m.selected = i
			return
		}
	}
}

// box draws the menu, sized to its labels and placed at the pointer, moved as needed to stay
// inside a pane rows by cols.
func (m *popupMenu) box(rows, cols int) []string {
	w := 0
	for _, it := range m.items {
		w = max(w, cellWidth(it.label))
	}
	w = min(w+4, cols)
	if w < 5 || rows < 3 {
		m.shown = 0 // nothing drawn, nothing to click
		return nil
	}
	rule := strings.Repeat("─", w-2)
	box := []string{"╭" + rule + "╮"}
	// A menu taller than the pane shows the items around the selected one.
	m.shown = min(len(m.items), rows-2)
	m.first = max(0, min(m.first, len(m.items)-m.shown))
	if sel := m.selected; sel >= 0 && sel < m.first {
		m.first = sel
	} else if sel < len(m.items) && sel >= m.first+m.shown {
		m.first = sel - m.shown + 1
	}
	for i := m.first; i < m.first+m.shown; i++ {
		it := m.items[i]
		switch {
		case it.label == "":
			box = append(box, "├"+rule+"┤")
		case i == m.selected:
			box = append(box, "│"+sgrRev+" "+fit(it.label, w-4)+" "+sgrReset+"│")
		default:
			box = append(box, "│ "+fit(it.label, w-4)+" │")
		}
	}
	box = append(box, "╰"+rule+"╯")
	m.width = w
	m.left = max(1, min(m.x, cols-w+1))
	m.top = max(1, min(m.y, rows-len(box)+1))
	return box
}

// itemAt is the index of the item drawn at a cell, or -1.
func (m *popupMenu) itemAt(x, y int) int {
	i := y - m.top - 1
	if x < m.left || x >= m.left+m.width || i < 0 || i >= m.shown {
		return -1
	}
	i += m.first
	if i >= len(m.items) || m.items[i].label == "" {
		return -1
	}
	return i
}

// hit is a clickable span of the toolbar: columns x0..x1 (1-based), on any of its rows.
type hit struct {
	x0, x1 int
	act    func()
}

// runLogsPane is the viewer for the device the picker passed in deviceEnv. Opened without one,
// it shows the picker first and streams in the same pane.
//
// Keys: p or space pauses/resumes, 1-6 set the minimum level (V..F), / edits the filter text, r toggles
// regex, s toggles system logs (not on Android), P edits the package id, a lists the installed
// apps, c clears the view, C clears the device buffer (Android), j/k and PgUp/PgDn scroll, G
// follows the tail again, q quits. In a field, Enter or Esc leaves it and Ctrl-U empties it.
func runLogsPane() int {
	var d device
	if json.Unmarshal([]byte(os.Getenv(deviceEnv)), &d) != nil || d.ID == "" {
		return runPicker(false)
	}
	c, err := dial()
	if err != nil {
		fmt.Fprintln(os.Stderr, "jaca:", err)
		return 1
	}
	defer c.Close()
	go sendRightClicksToPane()
	return runPane(c, func(p *pane) screen { return newLogViewer(p, d, nil) })
}

// sendRightClicksToPane asks Herdr to pass right clicks to this pane, for the log row menu. By
// default Herdr keeps them for its own pane menu (rename, split, close), which this turns off
// for the logs pane; that menu's "Send right-clicks to pane" item turns it back.
func sendRightClicksToPane() {
	if id := os.Getenv("HERDR_PANE_ID"); id != "" {
		_ = exec.Command(envOr("HERDR_BIN_PATH", "herdr"), "pane", "input", "--right-click", "pane", id).Run()
	}
}

func newLogViewer(p *pane, d device, back func()) *logViewer {
	v := &logViewer{p: p, device: d, back: back, follow: true, filter: logFilter{hideSystemLogs: true}}
	v.open()
	return v
}

// open asks jacad for a stream off the loop; finishOpen takes the result.
func (v *logViewer) open() {
	if v.opening || v.session != "" {
		return
	}
	v.opening, v.err = true, ""
	params := map[string]any{"device": v.device, "autoStart": true, "displayName": v.device.displayModel()}
	if v.applied != "" {
		params["package"] = v.applied
	}
	go func() {
		var info struct {
			ID string `json:"id"`
		}
		err := v.p.c.Call("logs.open", params, &info)
		if !v.p.post(func() { v.finishOpen(info.ID, err) }) && err == nil {
			// The pane quit while this was in flight: nobody will stream it.
			v.closeRemote(info.ID)
		}
	}()
}

func (v *logViewer) closeRemote(id string) {
	if id != "" {
		_ = v.p.c.CallTimeout("logs.close", map[string]any{"id": id}, nil, teardownTimeout)
	}
}

func (v *logViewer) topics(id string) []string {
	return []string{"logs.lines." + id, "logs.state." + id}
}

func (v *logViewer) finishOpen(id string, err error) {
	v.opening = false
	if err != nil {
		v.err = err.Error()
		return
	}
	if v.left {
		go v.closeRemote(id)
		return
	}
	v.session = id
	v.p.setLive("logs.close", id)
	_ = v.p.c.CallTimeout("events.subscribe", map[string]any{"topics": v.topics(id)}, nil, syncTimeout)
	// Lines published between logs.open and the subscribe never reach this connection; the
	// daemon's replay has them.
	v.backfill()
}

func (v *logViewer) leave(wait bool) {
	v.left = true
	if v.session == "" {
		return
	}
	id := v.session
	v.session = ""
	v.p.setLive("", "")
	teardown := func() {
		_ = v.p.c.CallTimeout("events.unsubscribe", map[string]any{"topics": v.topics(id)}, nil, teardownTimeout)
		v.closeRemote(id)
	}
	if wait {
		teardown()
	} else {
		go teardown()
	}
}

// call runs a session method off the loop and shows why it failed, if it did.
func (v *logViewer) call(method string, params map[string]any) {
	go func() {
		if err := v.p.c.Call(method, params, nil); err != nil {
			v.p.post(func() { v.err = err.Error() })
		}
	}()
}

// connect (re)starts the stream the way the app's transport button does: checked first, so a
// failure comes back as a status message.
func (v *logViewer) connect() {
	if v.session == "" {
		v.open()
		return
	}
	v.err = ""
	v.call("logs.connect", map[string]any{"id": v.session})
}

func (v *logViewer) handleEvent(ev event) {
	if v.session == "" {
		return
	}
	switch ev.Topic {
	case "logs.lines." + v.session:
		var batch []logLine
		if json.Unmarshal(ev.Data, &batch) == nil {
			v.appendLines(batch)
		}
	case "logs.state." + v.session:
		var st logState
		if json.Unmarshal(ev.Data, &st) == nil {
			v.setState(st)
		}
	case "events.dropped":
		var note droppedNote
		if json.Unmarshal(ev.Data, &note) != nil {
			return
		}
		switch note.Topic {
		case "logs.lines." + v.session:
			v.backfill()
		case "logs.state." + v.session:
			var sessions []struct {
				ID    string   `json:"id"`
				State logState `json:"state"`
			}
			if v.p.c.CallTimeout("logs.list", nil, &sessions, syncTimeout) == nil {
				for _, s := range sessions {
					if s.ID == v.session {
						v.setState(s.State)
					}
				}
			}
		}
	}
}

// setState mirrors the stream state; the package's PIDs become the filter's PID set.
func (v *logViewer) setState(st logState) {
	changed := len(st.PIDs) != len(v.state.PIDs) || (st.PIDs == nil) != (v.state.PIDs == nil)
	for i := 0; !changed && i < len(st.PIDs); i++ {
		changed = st.PIDs[i] != v.state.PIDs[i]
	}
	v.state = st
	if !changed {
		return
	}
	v.filter.pids = nil
	if st.PIDs != nil {
		v.filter.pids = make(map[int32]bool, len(st.PIDs))
		for _, pid := range st.PIDs {
			v.filter.pids[pid] = true
		}
	}
	v.refilter()
}

// appendLines adds lines newer than the last one received. Seqs grow but skip values (one line
// can split into sub-seqs), so they order lines without revealing gaps; gaps are reported by
// events.dropped and filled by backfill. A scrolled-up view moves its offset by the visible
// lines added, so it holds still while the device keeps logging.
func (v *logViewer) appendLines(batch []logLine) {
	added := 0
	for _, l := range batch {
		if v.hasSeq && l.Seq <= v.lastSeq {
			continue
		}
		v.lines = append(v.lines, l)
		v.lastSeq, v.hasSeq = l.Seq, true
		v.total++
		if v.filter.matches(l) {
			v.visible = append(v.visible, l)
			added++
		}
	}
	if !v.follow {
		v.offset += added
	}
	// Trimmed in chunks so a full buffer isn't copied on every batch.
	if cut := len(v.lines) - maxLines; cut > maxLines/10 {
		v.lines = append([]logLine(nil), v.lines[cut:]...)
		v.dropped += cut
		v.refilter()
	}
	v.clampOffset()
}

// refilter rebuilds the visible lines after the filter or the buffer's front changed.
func (v *logViewer) refilter() {
	v.visible = v.visible[:0]
	for _, l := range v.lines {
		if v.filter.matches(l) {
			v.visible = append(v.visible, l)
		}
	}
	v.clampOffset()
}

// backfill fetches the session's lines after the newest one received (all of the daemon's
// replay when there is none yet). Live batches that overlap it are skipped by appendLines.
func (v *logViewer) backfill() {
	params := map[string]any{"id": v.session, "limit": maxLines}
	if v.hasSeq {
		params["afterSeq"] = v.lastSeq
	}
	var lines []logLine
	if v.p.c.CallTimeout("logs.range", params, &lines, syncTimeout) == nil {
		v.appendLines(lines)
	}
}

// clear empties the scrollback (not the device buffer). lastSeq stays, so a backfill can't
// bring the cleared lines back.
func (v *logViewer) clear() {
	v.lines, v.visible = nil, nil
	v.total, v.dropped = 0, 0
	v.follow, v.offset = true, 0
	if v.session != "" {
		v.call("logs.resetPairing", map[string]any{"id": v.session})
	}
}

// showsPrompt is the app's connect prompt: a stopped stream with nothing to show.
func (v *logViewer) showsPrompt() bool { return !v.state.IsRunning && v.total == 0 }

// notice is the line under the toolbar: a failed call or the stream's status message (red), else
// where the last export was saved.
func (v *logViewer) notice() (text string, failed bool) {
	switch {
	case v.err != "":
		return v.err, true
	case v.state.StatusMessage != nil:
		return *v.state.StatusMessage, true
	}
	return v.saved, false
}

// logRoom is how many log lines fit between the toolbar and the status bar.
func (v *logViewer) logRoom() int {
	rows, _ := termSize()
	room := rows - toolbarRows(rows) - 1
	if text, _ := v.notice(); text != "" {
		room--
	}
	return max(1, room)
}

// clampOffset keeps a scrolled-up view on a full screen: at most the oldest page, never past
// it, so scrolling (or the buffer trimming under a view that holds still) can't leave it blank.
// A view back at the tail follows it again, unless lines are selected: those stay put.
func (v *logViewer) clampOffset() {
	limit := max(0, len(v.visible)-v.logRoom())
	v.offset = max(0, min(v.offset, limit))
	if v.offset == 0 && !v.sel.on {
		v.follow = true
	}
}

// window is the range of visible lines on screen.
func (v *logViewer) window() (start, end int) {
	end = max(0, len(v.visible)-v.offset)
	return max(0, end-v.logRoom()), end
}

func (v *logViewer) scroll(by int) {
	v.follow = false
	v.offset += by
	v.clampOffset()
}

func (v *logViewer) handleKey(k []byte) bool {
	if len(k) == 1 && k[0] == 0x03 {
		return true
	}
	if m, ok := parseMouse(k); ok {
		v.mouse(m)
		return false
	}
	v.saved = "" // the export note lasts until the next key
	if v.focus == focusMenu {
		v.menuKey(k)
		return false
	}
	if v.focus == focusFormat {
		v.editorDo(v.editor.key(k))
		return false
	}
	if v.sel.on {
		// Any key drops the selection; Esc does only that.
		v.clearSelection()
		if isEsc(k) {
			return false
		}
	}
	if v.focus == focusHelp {
		// Dismissed by Esc, Enter, q or ? again.
		if isEsc(k) || isEnter(k) || (len(k) == 1 && (k[0] == 'q' || k[0] == '?')) {
			v.focus = focusLog
		}
		return false
	}
	switch v.focus {
	case focusSearch:
		v.editSearch(k)
		return false
	case focusPackage:
		v.editPackage(k)
		return false
	case focusApps:
		v.appsKey(k)
		return false
	}
	android := v.device.Platform == "android"
	switch {
	case len(k) != 1:
		switch {
		case isUp(k):
			v.scroll(1)
		case isDown(k):
			v.scroll(-1)
		case isShiftPageDown(k):
			v.follow, v.offset = true, 0 // Follow tail; scrolling up leaves it
		case isPageUp(k):
			v.scroll(v.logRoom())
		case isPageDown(k):
			v.scroll(-v.logRoom())
		}
	case k[0] == 'q':
		return true
	case k[0] == 0x1b:
		if v.back != nil {
			v.leave(false)
			v.back()
		}
	case k[0] == 'k':
		v.scroll(1)
	case k[0] == 'j':
		v.scroll(-1)
	case k[0] == 'G':
		v.follow, v.offset = true, 0
	case k[0] == 'S' || k[0] == 'e':
		v.export()
	case k[0] == 'y':
		v.openFormatEditor()
	case k[0] == ' ' || k[0] == 'p':
		if v.state.IsRunning {
			v.paused = true
			v.call("logs.stop", map[string]any{"id": v.session})
		} else {
			v.paused = false
			v.connect()
		}
	case k[0] == '\r' || k[0] == '\n':
		if v.showsPrompt() && !v.opening && !v.state.IsConnecting {
			v.connect()
		}
	case k[0] >= '1' && k[0] <= '6':
		v.setMinLevel(int(k[0] - '1'))
	case k[0] == 'r':
		v.toggleRegex()
	case k[0] == 's' && !android:
		v.toggleSystemLogs()
	case k[0] == '?':
		v.focus = focusHelp
	case k[0] == '/':
		v.focus = focusSearch
	case k[0] == 'P':
		v.focus = focusPackage
	case k[0] == 'a':
		v.openApps()
	case k[0] == 'c':
		v.clear()
	case k[0] == 'C' && android && v.session != "":
		v.clear()
		v.call("logs.clearDeviceBuffer", map[string]any{"id": v.session})
	}
	return false
}

// export is the app's Export: the lines the filter keeps, as the device printed them, saved where
// the macOS save dialog says, under the device's name by default.
func (v *logViewer) export() {
	raw := make([]string, len(v.visible))
	for i, l := range v.visible {
		raw[i] = l.Raw
		if raw[i] == "" {
			raw[i] = l.Message
		}
	}
	text := strings.Join(raw, "\n")
	name := strings.NewReplacer("/", "-", ":", "-").Replace(v.device.displayModel()) + ".log"
	v.err, v.saved = "", ""
	go func() {
		path, err := chooseSavePath(name)
		if err == nil && path != "" {
			err = os.WriteFile(path, []byte(text), 0o644)
		}
		v.p.post(func() {
			if err != nil {
				v.err = err.Error()
				return
			}
			v.saved = path // empty when the dialog was cancelled
		})
	}()
}

// chooseSavePath shows the macOS save dialog, starting in Downloads with the given file name, and
// returns the chosen path: empty when the user cancels. The dialog asks before replacing a file.
func chooseSavePath(name string) (string, error) {
	out, err := exec.Command("osascript",
		"-e", "on run argv",
		"-e", "tell current application to activate",
		"-e", "set f to choose file name default name (item 1 of argv) default location (path to downloads folder)",
		"-e", "return POSIX path of f",
		"-e", "end run", name).CombinedOutput()
	text := strings.TrimSpace(string(out))
	switch {
	case err == nil:
		return text, nil
	case strings.Contains(text, "-128"): // userCanceledErr
		return "", nil
	case text != "":
		return "", errors.New(text)
	}
	return "", err
}

func (v *logViewer) setMinLevel(level int) {
	v.filter.minLevel = level
	v.refilter()
}

func (v *logViewer) toggleRegex() {
	v.filter.isRegex = !v.filter.isRegex
	v.filter.compile()
	v.refilter()
}

func (v *logViewer) toggleSystemLogs() {
	v.filter.hideSystemLogs = !v.filter.hideSystemLogs
	v.refilter()
}

// blur leaves a text field the way clicking elsewhere does in the app: what was typed applies.
func (v *logViewer) blur() {
	if v.focus == focusPackage {
		v.setPackage(strings.TrimSpace(v.pkg.String()))
	}
	v.focus = focusLog
}

// mouse handles a click on the toolbar's chips and fields or on the apps list, and the wheel.
func (v *logViewer) mouse(m mouseEvent) {
	const wheelUp, wheelDown, wheelLines = 64, 65, 3
	// A release is the report's final byte in SGR mode; some terminals send button 3 instead.
	released := !m.press || (m.button&mouseDrag == 0 && m.button&3 == 3)
	if v.sel.dragging && (m.button&mouseDrag != 0 || released) {
		v.dragSelection(m, released)
		return
	}
	if !m.press {
		return
	}
	if v.focus == focusHelp {
		if m.button == 0 { // a click anywhere dismisses the popup
			v.focus = focusLog
		}
		return
	}
	if v.focus == focusFormat {
		if m.button == 0 {
			v.editorDo(v.editor.click(m.x, m.y))
		}
		return
	}
	if v.focus == focusMenu {
		// A click on an item runs it; any other press closes the menu and keeps the selection.
		menu := v.menu
		v.closeMenu()
		if i := menu.itemAt(m.x, m.y); m.button == 0 && i >= 0 {
			menu.items[i].act()
		}
		return
	}
	if v.focus == focusApps {
		rows := len(v.filteredApps()) + 1
		switch {
		case m.button == wheelUp:
			v.appSelected = clampIndex(v.appSelected-1, rows)
		case m.button == wheelDown:
			v.appSelected = clampIndex(v.appSelected+1, rows)
		case m.button != 0:
		case v.retryRow != 0 && m.y == v.retryRow:
			v.loadApps()
		case v.appStart >= 0 && m.y >= v.appTop && v.appStart+m.y-v.appTop < rows:
			v.focus = focusLog
			v.setPackage(v.appValue(v.appStart + m.y - v.appTop))
		}
		return
	}
	switch m.button {
	case wheelUp:
		v.clearSelection()
		v.scroll(wheelLines)
	case wheelDown:
		v.clearSelection()
		v.scroll(-wheelLines)
	case 0:
		v.blur()
		rows, _ := termSize()
		line := m.y - v.logTop
		switch {
		case m.y <= toolbarRows(rows):
			v.clearSelection()
			for i := len(v.hits) - 1; i >= 0; i-- { // a hit inside another was added after it
				if h := v.hits[i]; m.x >= h.x0 && m.x <= h.x1 {
					h.act()
					return
				}
			}
		case m.y == rows && v.helpX0 > 0 && m.x >= v.helpX0 && m.x <= v.helpX1:
			v.focus = focusHelp
		case m.y == rows && v.formatX0 > 0 && m.x >= v.formatX0 && m.x <= v.formatX1:
			v.openFormatEditor()
		case line < 0 || line >= len(v.rowLines):
			v.clearSelection()
		default:
			// A press selects the line under it; dragging from here extends the selection.
			seq := v.rowLines[line].Seq
			v.selectLines(selection{on: true, dragging: true, anchor: seq, cursor: seq})
			v.copyAtRest()
		}
	case 2:
		// Like the app's table: a right click on a line outside the selection selects that line,
		// then the menu acts on the selection.
		line := m.y - v.logTop
		if line < 0 || line >= len(v.rowLines) {
			return
		}
		if seq := v.rowLines[line].Seq; !v.sel.has(seq) {
			v.selectLines(selection{on: true, anchor: seq, cursor: seq})
		}
		v.sel.dragging = false
		v.openRowMenu(m.x, m.y)
	}
}

// selectLines replaces the selection. The view is held where it is, so the lines under the
// selection stay put while the device keeps logging.
func (v *logViewer) selectLines(s selection) {
	s.held = v.sel.held || v.follow
	v.follow = false
	v.sel = s
}

// selectedLines are the log lines the selection covers, on screen or scrolled away.
func (v *logViewer) selectedLines() []logLine {
	if !v.sel.on {
		return nil
	}
	if v.sel.all {
		return v.visible
	}
	lo, hi := min(v.sel.anchor, v.sel.cursor), max(v.sel.anchor, v.sel.cursor)
	from := sort.Search(len(v.visible), func(i int) bool { return v.visible[i].Seq >= lo })
	to := sort.Search(len(v.visible), func(i int) bool { return v.visible[i].Seq > hi })
	return v.visible[from:to]
}

// copyLines copies the selected lines whole (not cut at the pane's edge): in the saved copy
// format, or as messages only.
func (v *logViewer) copyLines(messagesOnly bool) {
	lines := v.selectedLines()
	if len(lines) == 0 {
		return
	}
	text := renderLines(append([]logLine(nil), lines...), loadCopyFormat(copyFormatPath()), messagesOnly)
	v.copy(text)
}

func (v *logViewer) copy(text string) {
	write := copyToClipboard
	go func() {
		if err := write(text); err != nil {
			v.p.post(func() { v.err = err.Error() })
		}
	}()
}

// openRowMenu shows the app's log row menu (LogNSTableView.menu) at a cell.
func (v *logViewer) openRowMenu(x, y int) {
	n := len(v.selectedLines())
	lines, messages := "Copy Line", "Copy Message only"
	if n > 1 {
		lines, messages = fmt.Sprintf("Copy %d Lines", n), fmt.Sprintf("Copy %d Messages only", n)
	}
	v.menu = &popupMenu{x: x, y: y, items: []menuItem{
		{lines, func() { v.copyLines(false) }},
		{messages, func() { v.copyLines(true) }},
		{"Copy Format…", v.openFormatEditor},
		{},
		{"Select All", func() { v.selectLines(selection{on: true, all: true}) }},
	}}
	v.focus = focusMenu
}

// openFormatEditor shows the app's Copy format sheet on the saved format.
func (v *logViewer) openFormatEditor() {
	v.menu = nil
	v.editor, v.focus = newFormatEditor(loadCopyFormat(copyFormatPath())), focusFormat
}

// editorDo ends the Copy format popup when a key or click asked to: Save writes the format to
// the file the app reads, so the app and the pane copy the same way.
func (v *logViewer) editorDo(action editorAction) {
	if action == editorStay {
		return
	}
	if action == editorSave {
		if err := saveCopyFormat(copyFormatPath(), v.editor.format()); err != nil {
			v.err = err.Error()
		}
	}
	v.editor, v.focus = nil, focusLog
}

func (v *logViewer) closeMenu() {
	v.menu, v.focus = nil, focusLog
}

// menuKey: arrows (or j/k) move, Enter runs the item, Esc closes the menu.
func (v *logViewer) menuKey(k []byte) {
	switch {
	case isEsc(k) || (len(k) == 1 && k[0] == 'q'):
		v.closeMenu()
	case isUp(k):
		v.menu.move(-1)
	case isDown(k):
		v.menu.move(1)
	case isEnter(k):
		menu := v.menu
		v.closeMenu()
		menu.items[menu.selected].act()
	}
}

// dragSelection moves the selection's end to the line under the pointer and copies the selection
// when the button is released. With the pointer above or below the log lines the view scrolls
// that way, extending the selection, until the pointer comes back or the button is released.
func (v *logViewer) dragSelection(m mouseEvent, released bool) {
	line := m.y - v.logTop
	switch {
	case len(v.rowLines) == 0:
		v.sel.edge = 0
	case line < 0:
		v.sel.edge, v.sel.cursor = -1, v.rowLines[0].Seq
	case line >= len(v.rowLines):
		v.sel.edge, v.sel.cursor = 1, v.rowLines[len(v.rowLines)-1].Seq
	default:
		v.sel.edge, v.sel.cursor = 0, v.rowLines[line].Seq
	}
	if released {
		v.sel.dragging, v.sel.edge = false, 0
		v.copyLines(false)
		return
	}
	if v.sel.edge != 0 && !v.sel.scrolling {
		v.edgeScroll()
	}
	v.copyAtRest()
}

// edgeScroll scrolls one line toward the pointer and repeats while the drag stays past that
// edge of the log lines.
func (v *logViewer) edgeScroll() {
	v.sel.scrolling = v.sel.dragging && v.sel.edge != 0
	if !v.sel.scrolling {
		return
	}
	v.scroll(-v.sel.edge)
	if start, end := v.window(); end > start {
		if v.sel.edge < 0 {
			v.sel.cursor = v.visible[start].Seq
		} else {
			v.sel.cursor = v.visible[end-1].Seq
		}
	}
	time.AfterFunc(40*time.Millisecond, func() { v.p.post(v.edgeScroll) })
}

// copyAtRest copies the selection once the pointer has rested a moment during a drag, since a
// terminal that doesn't report the button's release would otherwise never copy.
func (v *logViewer) copyAtRest() {
	v.sel.moves++
	moves := v.sel.moves
	time.AfterFunc(250*time.Millisecond, func() {
		v.p.post(func() {
			if v.sel.dragging && v.sel.moves == moves && v.sel.edge == 0 {
				v.copyLines(false)
			}
		})
	})
}

// clearSelection drops the highlight; a view that was following the tail follows it again.
func (v *logViewer) clearSelection() {
	if v.sel.held {
		v.follow, v.offset = true, 0
	}
	v.sel = selection{}
}

// copyToClipboard puts text on the macOS pasteboard. A variable so tests don't touch it.
var copyToClipboard = func(text string) error {
	cmd := exec.Command("pbcopy")
	cmd.Stdin = strings.NewReader(text)
	return cmd.Run()
}

func (v *logViewer) editSearch(k []byte) {
	if isEnter(k) || isEsc(k) {
		v.focus = focusLog
		return
	}
	if v.search.handle(k) {
		v.filter.query = v.search.String()
		v.filter.compile()
		v.refilter()
	}
}

// editPackage applies the field on Enter; Esc puts back the package in effect.
func (v *logViewer) editPackage(k []byte) {
	switch {
	case isEnter(k):
		v.focus = focusLog
		v.setPackage(strings.TrimSpace(v.pkg.String()))
	case isEsc(k):
		v.focus = focusLog
		v.pkg.set(v.applied)
	default:
		v.pkg.handle(k)
	}
}

// setPackage targets an app (empty = the whole device): jacad tracks its PIDs and reports them
// in the stream state, which narrows the filter.
func (v *logViewer) setPackage(value string) {
	v.pkg.set(value)
	if value == v.applied {
		return
	}
	v.applied, v.err = value, ""
	if v.session != "" {
		v.call("logs.setPackage", map[string]any{"id": v.session, "package": value})
	}
}

func (v *logViewer) openApps() {
	v.focus = focusApps
	v.appQuery.set("")
	v.appSelected = 0
	// Fetched again while there is nothing to show, so reopening retries a failed listing.
	if !v.appsLoaded {
		v.loadApps()
	}
}

func (v *logViewer) loadApps() {
	if v.appsLoading {
		return
	}
	v.appsLoading = true
	id := v.device.ID
	go func() {
		var list []appEntry
		_ = v.p.c.Call("devices.apps", map[string]any{"deviceID": id}, &list)
		v.p.post(func() {
			v.appsLoading = false
			if len(list) > 0 { // keep the last list when a refresh comes back empty
				v.apps = list
			}
			v.appsLoaded = len(v.apps) > 0
		})
	}()
}

func (v *logViewer) filteredApps() []appEntry {
	q := strings.ToLower(v.appQuery.String())
	if q == "" {
		return v.apps
	}
	var out []appEntry
	for _, a := range v.apps {
		if strings.Contains(strings.ToLower(a.ID), q) || strings.Contains(strings.ToLower(a.display()), q) {
			out = append(out, a)
		}
	}
	return out
}

func (v *logViewer) appsKey(k []byte) {
	// Row 0 is "All processes"; the filtered apps follow.
	rows := len(v.filteredApps()) + 1
	switch {
	case isEsc(k):
		v.focus = focusLog
	case isArrowUp(k):
		v.appSelected = clampIndex(v.appSelected-1, rows)
	case isArrowDown(k):
		v.appSelected = clampIndex(v.appSelected+1, rows)
	case isEnter(k):
		switch {
		case v.appsLoading:
		case len(v.apps) == 0:
			v.loadApps()
		default:
			v.focus = focusLog
			v.setPackage(v.appValue(v.appSelected))
		}
	default:
		if v.appQuery.handle(k) {
			v.appSelected = 0
		}
	}
}

// appValue is what selecting a row targets. Android and the simulator resolve the package or
// bundle id to PIDs; a physical iOS device scopes by process name, which is the display name.
func (v *logViewer) appValue(row int) string {
	apps := v.filteredApps()
	if row <= 0 || row > len(apps) {
		return ""
	}
	if v.device.Platform == "iosDevice" {
		return apps[row-1].display()
	}
	return apps[row-1].ID
}

func (v *logViewer) draw() {
	rows, cols := termSize()
	var frame []string
	line := func(s string) { frame = append(frame, s) }

	if v.focus == focusApps {
		v.drawApps(line, rows, cols)
		paintRows(frame)
		return
	}

	for _, row := range v.toolbar(cols, toolbarRows(rows)) {
		line(row)
	}
	if text, failed := v.notice(); failed {
		line(sgrRed + fit(sanitize(text), cols) + sgrReset)
	} else if text != "" {
		line(sgrGreen + fit("↓ "+sanitize(text), cols) + sgrReset)
	}
	v.clampOffset() // the notice line or the pane size may have changed
	room := v.logRoom()
	drawn := 0
	v.logTop, v.rowLines = len(frame)+1, v.rowLines[:0]
	if v.showsPrompt() {
		action := sgrRev + " Try to Connect " + sgrReset
		if v.opening || v.state.IsConnecting {
			action = "Connecting…"
		}
		prompt := []string{"", sgrBold + clip(sanitize(v.device.displayModel()), cols) + sgrReset,
			sgrDim + "Disconnected" + sgrReset, "", action}
		for _, l := range prompt[:min(len(prompt), room)] {
			line(l)
		}
		drawn = min(len(prompt), room)
	} else {
		start, end := v.window()
		for _, l := range v.visible[start:end] {
			row := renderLogLine(l, cols)
			v.rowLines = append(v.rowLines, l)
			// A selected line is drawn plain and reversed.
			if v.sel.has(l.Seq) {
				row = sgrRev + stripSGR(row) + sgrReset
			}
			line(row)
		}
		drawn = end - start
	}
	for ; drawn < room; drawn++ {
		line("")
	}
	line(v.statusBar(cols))
	switch {
	case v.focus == focusHelp:
		box := v.helpBox(rows, cols)
		if len(box) > 0 { // centered
			frame = overlay(frame, box, max(0, (rows-len(box))/2), max(0, (cols-cellWidth(stripSGR(box[0])))/2))
		}
	case v.focus == focusFormat && v.editor != nil:
		box := v.editor.box(rows, cols)
		frame = overlay(frame, box, v.editor.top-1, v.editor.left-1)
	case v.focus == focusMenu && v.menu != nil:
		box := v.menu.box(rows, cols)
		frame = overlay(frame, box, v.menu.top-1, v.menu.left-1)
	}
	paintRows(frame)
}

// levelChip is a selected level chip's style: the level's badge color as its background.
var levelChip = []string{"\x1b[1;7m", levelBadge[1], levelBadge[2], levelBadge[3], levelBadge[4], levelBadge[5]}

// boxedField is a text field in a three-row box w cells wide, its border bold while focused.
// button, when not empty, is a one-cell glyph in its own compartment at the field's right end
// (w at least 9 then, else 5).
func boxedField(t *textInput, placeholder string, focused bool, w int, button string) [3]string {
	border := sgrDim
	if focused {
		border = sgrBold
	}
	if button == "" {
		rule := strings.Repeat("─", w-2)
		return [3]string{
			border + "╭" + rule + "╮" + sgrReset,
			border + "│" + sgrReset + " " + renderField(t, placeholder, focused, w-4, "") + " " + border + "│" + sgrReset,
			border + "╰" + rule + "╯" + sgrReset,
		}
	}
	// The compartment is a divider, a space, the glyph, a space.
	rule := strings.Repeat("─", w-2-buttonCells)
	return [3]string{
		border + "╭" + rule + "┬───╮" + sgrReset,
		border + "│" + sgrReset + " " + renderField(t, placeholder, focused, w-4-buttonCells, "") + " " +
			border + "│" + sgrReset + " " + sgrBold + button + sgrReset + " " + border + "│" + sgrReset,
		border + "╰" + rule + "┴───╯" + sgrReset,
	}
}

// buttonCells is the width a boxed field's button compartment adds: its divider and three cells.
const buttonCells = 4

// helpKeys are the viewer's keys for the ? popup. Labels are the app's where it has one (the
// toolbar's chips, placeholders and tooltips); the rest are placeholders that need specified copy.
func (v *logViewer) helpKeys() [][2]string {
	keys := [][2]string{
		{"1-6", "Minimum level"},
		{"/", "Filter text"},
		{"r", "Regex"},
	}
	if v.device.Platform != "android" {
		keys = append(keys, [2]string{"s", "System logs"})
	}
	keys = append(keys,
		[2]string{"P", "Package id"},
		[2]string{"a", "Choose app / package"},
		[2]string{"p  space", "Pause / unpause"},
		[2]string{"c", "Clear view"})
	if v.device.Platform == "android" {
		keys = append(keys, [2]string{"C", "Clear device buffer"})
	}
	return append(keys,
		[2]string{"j  k  PgUp  PgDn", "Scroll"},
		[2]string{"Shift+PgDn  G", "Follow tail"},
		[2]string{"S", "Export"},
		[2]string{"y", "Copy format"},
		[2]string{"?", "Help"},
		[2]string{"q", "Quit"})
}

// helpBox is the viewer's keys popup.
func (v *logViewer) helpBox(rows, cols int) []string { return keysBox(v.helpKeys(), rows, cols) }

// keysBox is a keys popup: a bordered box sized to its text, cut down to fit a small pane.
func keysBox(keys [][2]string, rows, cols int) []string {
	keyW, textW := 0, 0
	for _, k := range keys {
		keyW, textW = max(keyW, cellWidth(k[0])), max(textW, cellWidth(k[1]))
	}
	// Two cells of padding inside each border, three between the columns.
	if over := keyW + 3 + textW + 6 - cols; over > 0 {
		textW -= over
	}
	if textW < 1 || rows < 3 {
		return nil
	}
	w := keyW + 3 + textW + 6
	rule := strings.Repeat("─", w-2)
	blank := "│" + strings.Repeat(" ", w-2) + "│"
	// Under a blank row, the close button at the right, like Herdr's overlays.
	box := []string{titledBorder("Help", w), blank, blank, blank}
	if room := w - 2 - len(escClose) - 2; room >= 0 {
		box[2] = "│" + strings.Repeat(" ", room) + closeButton(escClose) + "  │"
	}
	for _, k := range keys {
		box = append(box, "│  "+sgrBold+fit(k[0], keyW)+sgrReset+"   "+fit(k[1], textW)+"  │")
	}
	box = append(box, blank, "╰"+rule+"╯")
	if len(box) > rows { // keep the borders, drop the rows that don't fit
		box = append(box[:rows-1], box[len(box)-1])
	}
	return box
}

// overlay draws box, on the panel background, over frame with its corner at row top, column left (0-based): each of its
// rows is appended to the frame row under it, after a jump to the box's column, so it paints
// over what that row drew.
func overlay(frame, box []string, top, left int) []string {
	for i, row := range box {
		if top+i < 0 || top+i >= len(frame) {
			continue
		}
		frame[top+i] += fmt.Sprintf("%s\x1b[%dG%s", sgrReset, left+1, onPanel(framed(row), 0))
	}
	return frame
}

// fieldRoom is the width the toolbar keeps for each text field before it adds an action.
const fieldRoom = 30

// toolbarRows is the toolbar's height: three rows, or one in a pane too short to spare them.
func toolbarRows(rows int) int {
	if rows < 12 {
		return 1
	}
	return 3
}

// toolbar is the app's filter row: level chips, regex, system logs, the filter text and the
// package id. Three rows tall, a selected chip is a block of its color and the fields are boxes;
// one row tall, chips are a single cell high and the fields are underlined. Chips that don't fit
// are left out; the fields share the width that remains.
func (v *logViewer) toolbar(cols, height int) []string {
	tall := height >= 3
	var top, mid, bottom strings.Builder
	used := 0
	v.hits = v.hits[:0]
	add := func(w int, t, m, b string, act func()) {
		gap := strings.Repeat(" ", min(used, 1))
		top.WriteString(gap + t)
		mid.WriteString(gap + m)
		bottom.WriteString(gap + b)
		used += len(gap) + w
		v.hits = append(v.hits, hit{x0: used - w + 1, x1: used, act: act})
	}
	chip := func(label, selected string, on bool, act func()) {
		pad := 2
		if tall {
			pad = 3
		}
		w := cellWidth(label) + 2*pad
		if used+min(used, 1)+w > cols {
			return
		}
		style := ""
		if on {
			style = selected
		}
		blank := style + strings.Repeat(" ", w) + sgrReset
		add(w, blank, style+strings.Repeat(" ", pad)+label+strings.Repeat(" ", pad)+sgrReset, blank, act)
	}
	// field draws a text field w cells wide, borders included when tall.
	// field draws a text field w cells wide, borders included when tall. With open set, a down
	// arrow at its right end runs it (the package field's list of installed apps).
	field := func(t *textInput, placeholder string, target logFocus, w int, open func()) {
		focus := func() { v.focus = target }
		button := ""
		if open != nil {
			button = "▼"
		}
		if !tall && open != nil {
			add(w, "", renderField(t, placeholder, v.focus == target, w-2, sgrUnder)+" "+sgrBold+button+sgrReset, "", focus)
		} else if !tall {
			add(w, "", renderField(t, placeholder, v.focus == target, w, sgrUnder), "", focus)
		} else {
			box := boxedField(t, placeholder, v.focus == target, w, button)
			add(w, box[0], box[1], box[2], focus)
		}
		if open != nil {
			// The arrow's compartment; later hits win over the field's.
			v.hits = append(v.hits, hit{x0: max(used-w+1, used-buttonCells), x1: used, act: open})
		}
	}
	for i, short := range levelShort {
		chip(short, levelChip[i], v.filter.minLevel == i, func() { v.setMinLevel(i) })
	}
	chip(".*", sgrBold+sgrRev, v.filter.isRegex, v.toggleRegex)
	if v.device.Platform != "android" {
		chip("System logs", sgrBold+sgrRev, !v.filter.hideSystemLogs, v.toggleSystemLogs)
	}
	// The app's Export action, where it still leaves the two fields a useful width.
	action := func(label string, on bool, act func()) {
		pad := 2
		if tall {
			pad = 3
		}
		if cols-used-1-cellWidth(label)-2*pad >= 2*fieldRoom {
			chip(label, sgrBold+sgrRev, on, act)
		}
	}
	action("Export", false, v.export)

	// The app's placeholders, capitalized. The filter text takes three fifths of what is left.
	const minField = 12
	placeholder := "Filter text…"
	if v.filter.isRegex {
		placeholder = "Regex…"
	}
	if searchW := (cols - used - 2) * 3 / 5; searchW >= minField {
		field(&v.search, placeholder, focusSearch, searchW, nil)
		if pkgW := cols - used - 1; pkgW >= minField {
			field(&v.pkg, "Package id…", focusPackage, pkgW, v.openApps)
		}
	}
	if !tall {
		return []string{mid.String()}
	}
	return []string{top.String(), mid.String(), bottom.String()}
}

// statusBar is the app's: a dot for the stream, the counts, and the device on the right.
func (v *logViewer) statusBar(cols int) string {
	if cols < 2 {
		return ""
	}
	dot := sgrDim
	if v.state.IsRunning {
		dot = sgrGreen
	}
	// A stream the user paused says so, in the caution color, where there is room for it.
	const label = " Paused "
	badge, used := "", 2
	if v.paused && !v.state.IsRunning && cols >= used+len(label)+1 {
		badge, used = badgeStyle(colorYellow)+label+sgrReset+" ", used+len(label)+1
	}
	counts := fmt.Sprintf("%d shown  %d total", len(v.visible), v.total)
	if v.dropped > 0 {
		counts += fmt.Sprintf("  %d dropped", v.dropped)
	}
	counts = clip(counts, max(0, cols-used))
	used += cellWidth(counts)
	name := clip(sanitize(v.device.displayModel()), max(0, cols-used-2))
	// Help and Copy format sit left of the device name, each where there is room for it.
	const help, format, gap = "Help", "Copy format", 3
	buttons, buttonsW := "", 0
	v.helpX0, v.helpX1, v.formatX0, v.formatX1 = 0, 0, 0, 0
	right := cols - cellWidth(name) // the last column before the name
	place := func(label string) (x0, x1 int) {
		if cols-used-cellWidth(name)-buttonsW-len(label)-gap < 2 {
			return 0, 0
		}
		buttons = sgrUnder + label + sgrReset + strings.Repeat(" ", gap) + buttons
		buttonsW += len(label) + gap
		return right - buttonsW + 1, right - buttonsW + len(label)
	}
	v.formatX0, v.formatX1 = place(format)
	v.helpX0, v.helpX1 = place(help)
	pad := strings.Repeat(" ", max(0, cols-used-cellWidth(name)-buttonsW))
	return dot + "●" + sgrReset + " " + badge + sgrDim + counts + sgrReset + pad + buttons + sgrDim + name + sgrReset
}

// drawApps is the app's package picker: a search field over "All processes" and the installed
// apps, user apps marked with a dot.
func (v *logViewer) drawApps(line func(string), rows, cols int) {
	// The search field is a box the pane's width, like the toolbar's fields, where there is room.
	head := 2
	if toolbarRows(rows) == 3 && cols >= 12 {
		for _, row := range boxedField(&v.appQuery, "Search apps…", true, cols, "") {
			line(row)
		}
		head = 4
	} else {
		line(renderField(&v.appQuery, "Search apps…", true, min(cols, 40), sgrUnder))
	}
	line("")
	v.appTop, v.appStart, v.retryRow = head+1, -1, 0
	switch {
	case v.appsLoading && len(v.apps) == 0:
		line(sgrDim + "…" + sgrReset)
	case len(v.apps) == 0:
		line(sgrDim + "No apps found on this device." + sgrReset)
		line("")
		line(sgrRev + " Retry " + sgrReset)
		v.retryRow = head + 3
	default:
		apps := v.filteredApps()
		v.appSelected = clampIndex(v.appSelected, len(apps)+1)
		room := max(1, rows-head-1)
		start := listWindow(v.appSelected, room)
		v.appStart = start
		for i := start; i <= len(apps) && i < start+room; i++ {
			if i == 0 {
				line(listRow("  "+clip("All processes", max(0, cols-4)), v.appSelected == 0, cols))
				continue
			}
			a := apps[i-1]
			dot := "  "
			if a.IsUserApp {
				dot = sgrGreen + "•" + sgrReset + " "
			}
			title := clip(sanitize(a.display()), max(0, cols-4))
			subtitle := ""
			if rest := cols - 4 - cellWidth(title) - 2; a.Name != nil && rest > 0 {
				subtitle = "  " + sgrDim + clip(sanitize(a.ID), rest) + sgrReset
			}
			line(listRow(dot+title+subtitle, i == v.appSelected, cols))
		}
	}
}

// Colors per level, on the terminal's palette so the pane follows the Herdr theme. The app's
// LogLevelStyle greys V and D out, but a terminal theme's greys and dimmed text are hard to read
// (most of an Android log is V and D), so here V, D and I keep the default text and the badge
// tells them apart; W is caution yellow, E and F critical red, as in the app.
var (
	levelText  = []string{"", "", "", sgrYellow, sgrRed, sgrRed + sgrBold}
	levelBadge = []string{sgrBold, badgeStyle(colorGreen), badgeStyle(colorBlue), badgeStyle(colorYellow), badgeStyle(colorRed), badgeStyle(colorRed)}
)

// badgeStyle is a bold block of a color: the color reversed.
func badgeStyle(color string) string { return "\x1b[1;7;" + color + "m" }

// Column widths of a log row: the HH:mm:ss.SSS clock, the level badge, and the widest tag.
const (
	clockCells = 12
	badgeCells = 3
	tagCells   = 24
)

// renderLogLine lays a line out like the app's LogRowView: the time, the level badge, the tag
// (with its pid) in a fixed column so messages align, then the message in the level's color.
func renderLogLine(l logLine, cols int) string {
	msg := sanitize(strings.ReplaceAll(l.Message, "\n", " ⏎ "))
	if l.Marker {
		return renderMarker(msg, l.Critical, cols)
	}
	short, text, badge := "?", "", "\x1b[1;7m"
	if l.Level >= 0 && l.Level < len(levelShort) {
		short, text, badge = levelShort[l.Level], levelText[l.Level], levelBadge[l.Level]
	}
	// A pane too narrow for the columns shows the level and the message alone.
	tagW := min(tagCells, cols/4)
	msgW := cols - clockCells - 1 - badgeCells - 1 - tagW - 1
	if tagW < 8 || msgW < 8 {
		return text + fit(short+" "+msg, cols) + sgrReset
	}
	tag := sanitize(l.Tag)
	if tag != "" && l.PID > 0 {
		tag += fmt.Sprintf(" (%d)", l.PID)
	}
	clock := time.Unix(0, int64(l.Time*1e9)).Format("15:04:05.000")
	return sgrDim + clock + sgrReset + " " + badge + " " + short + " " + sgrReset + " " +
		sgrCyan + fit(tag, tagW) + sgrReset + " " + text + fit(msg, msgW) + sgrReset
}

// renderMarker draws a Jaca marker (process death, restart, reconnect) as a rule across the pane
// with the text in the middle: critical red for a crash, else a color no level uses.
func renderMarker(msg string, critical bool, cols int) string {
	color := sgrMagenta
	if critical {
		color = sgrRed
	}
	if cols < 8 {
		return color + sgrBold + fit(msg, cols) + sgrReset
	}
	text := " " + clip(msg, cols-6) + " "
	side := cols - cellWidth(text)
	left := side / 2
	return color + sgrDim + strings.Repeat("─", left) + sgrReset + color + sgrBold + text + sgrReset +
		color + sgrDim + strings.Repeat("─", side-left) + sgrReset
}
