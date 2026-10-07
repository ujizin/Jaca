package main

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
)

// The Cloud Logging home (the app's CloudLoggingHomeView): gcloud detection and sign-in, the
// GCP projects, and the sheets that add, rename and configure them. Everything it shows comes
// from jacad's retained cloud.state topic.

// cloudAuthCommand is CloudLoggingRegistry.authCommand, shown when gcloud is not signed in.
const cloudAuthCommand = "gcloud auth login"

const (
	// authPollEvery is how often the home re-checks sign-in while it shows "Not signed in".
	authPollEvery = 3 * time.Second
	// The waits for the calls that run gcloud in jacad, a little over the daemon's own limits
	// (20 s for auth, 30 s to validate a project, 45 s to list log names).
	authTimeout     = 25 * time.Second
	addTimeout      = 40 * time.Second
	logNamesTimeout = 50 * time.Second
)

// cloudCalls is the clock and the calls to jacad that the home and its sheets use, replaced in
// tests. after runs f on the pane's loop once d has passed. spawn runs work off the loop and
// what it returns on the loop.
type cloudCalls struct {
	now   func() time.Time
	after func(d time.Duration, f func())
	call  func(method string, params, out any, timeout time.Duration) error
	spawn func(work func() func())
}

func newCloudCalls(p *pane) cloudCalls {
	return cloudCalls{
		now: time.Now,
		after: func(d time.Duration, f func()) {
			go func() {
				time.Sleep(d)
				p.post(f) // nothing to undo when the pane has quit
			}()
		},
		call: func(method string, params, out any, timeout time.Duration) error {
			return p.c.CallTimeout(method, params, out, timeout)
		},
		spawn: func(work func() func()) {
			go func() {
				// A result that arrives after the pane quit is dropped: these calls leave
				// nothing open in jacad.
				p.post(work())
			}()
		},
	}
}

// cloudSheet is a popup over the home. Its methods run on the pane's loop.
type cloudSheet interface {
	key(k []byte)
	mouse(m mouseEvent)
	box(rows, cols int) (box []string, top, left int)
}

// The actions of a project row, in the order they are drawn.
const (
	actionLogNames = iota
	actionRename
	actionRemove
	actionNewSession
	actionCount
)

// cloudHome is the home screen.
type cloudHome struct {
	p *pane
	cloudCalls

	// What leaves the pane, replaced in tests: launch opens a session, openTerminal a terminal
	// for the sign-in command.
	launch       func(spec cloudSessionSpec)
	openTerminal func() error

	state  cloudState
	loaded bool // a cloud.state has arrived

	selected int // the project under the cursor
	action   int // the action of that row Enter presses
	top      int // the first body line drawn
	follow   bool
	moved    bool // an event changed the rows since they were drawn

	armed      map[string]time.Time // the Remove buttons waiting for their second press
	guard      pressGuard           // so a held key doesn't arm and confirm by itself
	quietUntil time.Time            // until then keys are ignored: a sheet closed by itself
	toast      string
	toastUntil time.Time
	err        string // why the last call failed, as jacad or Herdr reported it
	help       bool
	sheet      cloudSheet

	away bool // a session runs in this pane; the home is not showing
	gone bool // the pane left the home for good

	authWatch    bool // a sign-in re-check is scheduled
	authInFlight bool // a cloud.refreshAuth has not answered yet

	// As last drawn, for the mouse.
	shown    map[int]homeLine // by screen row (1-based)
	bodyRows int
}

// runCloudPane is the Cloud Logging pane: the session a new tab was opened for (cloudSessionEnv),
// else the home.
//
// Keys on the home: j/k or arrows move over the projects, Left/Right or Tab choose one of the
// row's actions and Enter presses it; n opens a new session, l the log names, e renames, x or
// Backspace removes (press twice to confirm, as in the app), a adds a project, u starts a
// session from a Logs Explorer URL, r re-checks gcloud, ? shows the keys, q quits.
func runCloudPane() int {
	c, err := dial()
	if err != nil {
		fmt.Fprintln(os.Stderr, "jaca:", err)
		return 1
	}
	defer c.Close()
	go sendRightClicksToPane()
	var spec cloudSessionSpec
	if raw := os.Getenv(cloudSessionEnv); raw != "" && json.Unmarshal([]byte(raw), &spec) == nil && spec.Config.ProjectID != "" {
		return runPane(c, func(p *pane) screen { return newCloudSession(p, spec, nil) })
	}
	return runPane(c, func(p *pane) screen { return newCloudHome(p) })
}

func newCloudHome(p *pane) *cloudHome {
	h := buildCloudHome(p, newCloudCalls(p))
	h.open()
	return h
}

// buildCloudHome is the home before it asks jacad for anything.
func buildCloudHome(p *pane, calls cloudCalls) *cloudHome {
	h := &cloudHome{p: p, cloudCalls: calls, armed: map[string]time.Time{}, action: actionNewSession, follow: true}
	h.launch = func(spec cloudSessionSpec) {
		launchCloudSession(p, spec, h.back, h.failed)
		// Without Herdr the session took over this pane.
		if p.screen != screen(h) {
			h.away = true
		}
	}
	h.openTerminal = openLoginTerminal
	return h
}

// open subscribes to cloud.state. The topic is retained, so the current state arrives at once.
func (h *cloudHome) open() {
	h.spawn(func() func() {
		err := h.call("events.subscribe", map[string]any{"topics": []string{cloudStateTopic}}, nil, callTimeout)
		return func() { h.failed(err) }
	})
}

// back returns from a session that ran in this pane. The state events went to the session
// meanwhile, so the state is read again.
func (h *cloudHome) back() {
	h.away = false
	h.p.screen = h
	h.fetch()
	h.watchAuth()
}

// leave has nothing to release on exit: the connection closing ends the subscription.
func (h *cloudHome) leave(wait bool) {
	h.gone = true
	if wait {
		return
	}
	h.spawn(func() func() {
		_ = h.call("events.unsubscribe", map[string]any{"topics": []string{cloudStateTopic}}, nil, teardownTimeout)
		return func() {}
	})
}

func (h *cloudHome) handleEvent(ev event) {
	switch ev.Topic {
	case cloudStateTopic:
		var st cloudState
		if json.Unmarshal(ev.Data, &st) == nil {
			h.apply(st)
		}
	case "events.dropped":
		// The skipped update may have been the latest state: fetch it.
		var note droppedNote
		if json.Unmarshal(ev.Data, &note) == nil && note.Topic == cloudStateTopic {
			h.fetch()
		}
	}
}

func (h *cloudHome) fetch() {
	h.spawn(func() func() {
		var st cloudState
		err := h.call("cloud.state", nil, &st, syncTimeout)
		return func() {
			if err != nil {
				h.failed(err)
				return
			}
			h.apply(st)
		}
	})
}

// apply takes a new state. The cursor stays on the project it was on, and an open log-name
// sheet follows its project (it closes when the project is gone).
func (h *cloudHome) apply(st cloudState) {
	at := ""
	if h.selected >= 0 && h.selected < len(h.state.Projects) {
		at = h.state.Projects[h.selected].ProjectID
	}
	// Rows that changed place are drawn at the next tick: until then a click is not for them.
	h.moved = h.moved || !slices.EqualFunc(h.state.Projects, st.Projects, func(a, b cloudProject) bool { return a.ProjectID == b.ProjectID })
	h.state, h.loaded = st, true
	h.selected = clampIndex(h.selected, len(st.Projects))
	for i, p := range st.Projects {
		if p.ProjectID == at {
			h.selected = i
			break
		}
	}
	if sheet, ok := h.sheet.(*logNameSheet); ok {
		if p, found := st.project(sheet.project.ProjectID); found {
			sheet.setProject(p)
		} else {
			h.sheet = nil
			h.hush()
		}
	}
	h.watchAuth()
}

// available is CloudLoggingRegistry.isAvailable: gcloud was found.
func (h *cloudHome) available() bool { return h.state.BinaryPath != "" }

// needsLogin reports whether the home is showing the sign-in banner.
func (h *cloudHome) needsLogin() bool {
	return !h.gone && !h.away && h.state.AuthState.State == cloudAuthNotAuthenticated
}

// watchAuth re-checks sign-in every authPollEvery while the home shows "Not signed in", so the
// pane updates by itself once the browser login completes: `gcloud auth login` runs in another
// tab and nothing tells jacad when it has finished. One cloud.refreshAuth is in flight at a
// time, and the checks stop when gcloud is signed in or the pane leaves the home.
func (h *cloudHome) watchAuth() {
	if h.authWatch || !h.needsLogin() {
		return
	}
	h.authWatch = true
	h.after(authPollEvery, h.authTick)
}

func (h *cloudHome) authTick() {
	h.authWatch = false
	if !h.needsLogin() {
		return
	}
	if !h.authInFlight {
		h.authInFlight = true
		h.spawn(func() func() {
			// The answer arrives on cloud.state. A failed check is not reported: the next
			// one runs in three seconds.
			_ = h.call("cloud.refreshAuth", nil, nil, authTimeout)
			return func() { h.authInFlight = false }
		})
	}
	h.watchAuth()
}

// accountLine is the header's second row (CloudLoggingHomeView.accountSummary).
func (h *cloudHome) accountLine() string {
	switch h.state.AuthState.State {
	case cloudAuthNotInstalled:
		return "gcloud CLI not found"
	case cloudAuthNotAuthenticated:
		return "Not signed in"
	case cloudAuthAuthenticated:
		return "Signed in as " + sanitize(h.state.AuthState.Account)
	}
	if h.state.IsDetecting {
		return "Detecting gcloud…"
	}
	return "—"
}

// confirm is one press of the button named key: the first arms it for armWindow and a second
// within that time confirms.
func (h *cloudHome) confirm(key string) bool {
	now := h.now()
	// A second press waits before it gets here (pressGuard): what counts is when it was made.
	if until, ok := h.armed[key]; ok && h.guard.pressed(now).Before(until) {
		delete(h.armed, key)
		return true
	}
	h.armed[key] = now.Add(armWindow)
	h.after(armWindow, h.expire)
	h.after(armWindow+keyRepeat.wait(), h.expire)
	return false
}

func (h *cloudHome) isArmed(key string) bool {
	until, ok := h.armed[key]
	return ok && h.now().Before(until)
}

// flash shows a toast for toastLife. A new one replaces the one showing.
func (h *cloudHome) flash(msg string) {
	h.toast, h.toastUntil = msg, h.now().Add(toastLife)
	h.after(toastLife, h.expire)
}

// expire drops the armed buttons and the toast whose time has passed.
func (h *cloudHome) expire() {
	now := h.now()
	for key, until := range h.armed {
		if !now.Before(until.Add(keyRepeat.wait())) {
			delete(h.armed, key)
		}
	}
	if h.toast != "" && !now.Before(h.toastUntil) {
		h.toast = ""
	}
}

// failed records why a call failed, for the line above the status bar.
func (h *cloudHome) failed(err error) {
	if err != nil {
		h.err = err.Error()
	}
}

// closeSheet closes s if it is still the open sheet.
func (h *cloudHome) closeSheet(s cloudSheet) {
	if h.sheet == s {
		h.sheet = nil
	}
}

// hush is for a sheet that closes by itself, on a result: the keys the user is still typing
// into it don't act on the home.
func (h *cloudHome) hush() { h.quietUntil = h.now().Add(hushTime) }

const hushTime = 700 * time.Millisecond

// detect is Re-check: jacad looks for gcloud again and re-reads the account.
func (h *cloudHome) detect() {
	h.spawn(func() func() {
		err := h.call("cloud.detect", nil, nil, callTimeout)
		return func() { h.failed(err) }
	})
}

func (h *cloudHome) copyCommand() {
	if err := copyToClipboard(cloudAuthCommand); err != nil {
		h.failed(err)
		return
	}
	h.flash("Copied")
}

// login is Open Terminal: a terminal for the sign-in command, opened off the loop.
func (h *cloudHome) login() {
	open := h.openTerminal
	h.spawn(func() func() {
		err := open()
		return func() { h.failed(err) }
	})
}

func (h *cloudHome) openAddSheet() {
	if h.available() {
		h.sheet = newAddProjectSheet(h)
	}
}

func (h *cloudHome) openURLSheet() {
	if h.available() {
		h.sheet = newURLSheet(h)
	}
}

func (h *cloudHome) openLogNames(p cloudProject) {
	var sheet *logNameSheet
	sheet = newLogNameSheetWith(h.cloudCalls, p, func() { h.closeSheet(sheet) })
	sheet.flash = h.flash
	h.sheet = sheet
	sheet.opened()
}

// addProject validates and stores a project (CloudLoggingRegistry.addProject) and hands the
// result to then. A call that failed counts as a failure with the app's fallback text.
func (h *cloudHome) addProject(id, displayName string, then func(cloudAddResult)) {
	h.spawn(func() func() {
		var res cloudAddResult
		err := h.call("cloud.addProject", map[string]any{"id": id, "displayName": displayName}, &res, addTimeout)
		return func() { h.added(id, displayName, res, err, then) }
	})
}

func (h *cloudHome) added(id, displayName string, res cloudAddResult, err error, then func(cloudAddResult)) {
	const fallback = "Couldn't validate the project."
	switch {
	case err != nil || res.Result == "":
		res = cloudAddResult{Result: cloudAddFailure, Message: fallback}
	case res.Result == cloudAddFailure && res.Message == "":
		res.Message = fallback
	case res.Result == cloudAddAdded:
		h.flash("Added " + h.titleOf(id, displayName))
	}
	then(res)
}

// titleOf is the title of the project with this id: the stored one's, else the one a project
// with this display name would have.
func (h *cloudHome) titleOf(id, displayName string) string {
	id = strings.TrimSpace(id)
	if p, ok := h.state.project(id); ok {
		return p.title()
	}
	return cloudProject{ProjectID: id, DisplayName: strings.TrimSpace(displayName)}.title()
}

func (h *cloudHome) rename(id, name string) {
	h.spawn(func() func() {
		err := h.call("cloud.setDisplayName", map[string]any{"id": id, "name": name}, nil, callTimeout)
		return func() {
			h.failed(err)
			if err == nil {
				h.flash("Renamed")
			}
		}
	})
}

func removeKey(id string) string { return "remove:" + id }

// remove is one press of a project's Remove: the second within armWindow removes it.
func (h *cloudHome) remove(p cloudProject) {
	if !h.confirm(removeKey(p.ProjectID)) {
		return
	}
	id, title := p.ProjectID, p.title()
	h.spawn(func() func() {
		err := h.call("cloud.removeProject", map[string]any{"id": id}, nil, callTimeout)
		return func() {
			h.failed(err)
			if err == nil {
				h.flash("Removed " + title)
			}
		}
	})
}

// startSession opens a stopped session on a project, as the app's New session does. The log
// name is the project's selected one; rawFilter is the filter of a pasted URL, else "".
func (h *cloudHome) startSession(projectID, rawFilter string) {
	cfg := newCloudStreamConfig(projectID)
	name := projectID
	if p, ok := h.state.project(projectID); ok {
		cfg.LogName = p.SelectedLogName
		name = p.title()
	}
	cfg.RawFilter = rawFilter
	h.launch(cloudSessionSpec{Config: cfg, Name: name, AutoStart: false})
}

// pressRow is a press of action n on the selected project for the guard to run, now or after a
// wait: it does nothing when the project under the cursor is no longer the one the key was
// pressed on. The row a key presses is brought into view, with its armed button.
func (h *cloudHome) pressRow(n int) func() {
	if h.moved || h.selected < 0 || h.selected >= len(h.state.Projects) {
		return func() {} // also for rows an update moved and the screen doesn't show yet
	}
	id := h.state.Projects[h.selected].ProjectID
	return func() {
		if h.selected >= 0 && h.selected < len(h.state.Projects) && h.state.Projects[h.selected].ProjectID == id {
			h.follow = true
			h.press(h.selected, n)
		}
	}
}

// press is one press of action n of project i.
func (h *cloudHome) press(i, n int) {
	if i < 0 || i >= len(h.state.Projects) {
		return
	}
	p := h.state.Projects[i]
	switch n {
	case actionLogNames:
		h.openLogNames(p)
	case actionRename:
		h.sheet = newRenameSheet(h, p)
	case actionRemove:
		h.remove(p)
	case actionNewSession:
		h.startSession(p.ProjectID, "")
	}
}

// move moves the row cursor. The action cursor goes back to New session, so Enter on another
// project never presses a Remove chosen for the one before.
func (h *cloudHome) move(by int) {
	h.selected = clampIndex(h.selected+by, len(h.state.Projects))
	h.action, h.follow = actionNewSession, true
}

func (h *cloudHome) moveAction(by int) {
	h.action = ((h.action+by)%actionCount + actionCount) % actionCount
	h.follow = true
}

func (h *cloudHome) handleKey(k []byte) bool {
	if len(k) == 1 && k[0] == 0x03 {
		return true
	}
	if m, ok := parseMouse(k); ok {
		if m.press && m.button == 0 {
			h.guard.drop()
		}
		h.mouse(m)
		return false
	}
	h.guard.note(k, h.now()) // a sheet's keys too: Enter held past its close is still held
	if h.help {
		if isEsc(k) || isEnter(k) || (len(k) == 1 && (k[0] == 'q' || k[0] == '?')) {
			h.help = false
		}
		return false
	}
	if h.sheet != nil {
		h.sheet.key(k)
		return false
	}
	// Keys still arriving from a sheet that closed on its own (a project validated while the
	// user typed) don't act on the home.
	if h.now().Before(h.quietUntil) {
		return false
	}
	h.err = "" // any key dismisses the last failure
	if nav, ok := decodeNav(k); ok && (nav.dir == "left" || nav.dir == "right") {
		if nav.dir == "left" {
			h.moveAction(-1)
		} else {
			h.moveAction(1)
		}
		return false
	}
	switch {
	case isPageUp(k):
		h.move(-max(1, h.bodyRows/3))
	case isPageDown(k):
		h.move(max(1, h.bodyRows/3))
	case isUp(k):
		h.move(-1)
	case isDown(k):
		h.move(1)
	case isEnter(k):
		if len(h.state.Projects) == 0 {
			h.guard.press(h.after, h.openAddSheet)
		} else {
			h.guard.press(h.after, h.pressRow(h.action))
		}
	case string(k) == "\x1b[Z": // Shift-Tab
		h.moveAction(-1)
	case len(k) != 1:
	case k[0] == '\t':
		h.moveAction(1)
	case k[0] == 'q':
		return true
	case k[0] == 'n':
		h.press(h.selected, actionNewSession)
	case k[0] == 'l':
		h.press(h.selected, actionLogNames)
	case k[0] == 'e':
		h.press(h.selected, actionRename)
	case k[0] == 'x' || k[0] == 0x7f || k[0] == 0x08:
		h.guard.press(h.after, h.pressRow(actionRemove))
	case k[0] == 'a':
		h.openAddSheet()
	case k[0] == 'u':
		h.openURLSheet()
	case k[0] == 'r':
		h.detect()
	case k[0] == '?':
		h.help = true
	}
	return false
}

func (h *cloudHome) mouse(m mouseEvent) {
	const wheelUp, wheelDown, wheelLines = 64, 65, 3
	if !m.press {
		return
	}
	if h.help {
		if m.button == 0 {
			h.help = false
		}
		return
	}
	if h.sheet != nil {
		h.sheet.mouse(m)
		return
	}
	switch {
	case m.button == wheelUp:
		h.top, h.follow = max(0, h.top-wheelLines), false
	case m.button == wheelDown:
		h.top, h.follow = h.top+wheelLines, false
	case m.button == 0:
		h.err = ""
		line, ok := h.shown[m.y]
		if !ok {
			return
		}
		if line.item >= 0 && h.moved {
			return // the row under the pointer is not the one on screen: the click is dropped
		}
		if line.item >= 0 {
			h.selected = clampIndex(line.item, len(h.state.Projects))
		}
		for _, hit := range line.hits {
			if m.x >= hit.x0 && m.x <= hit.x1 {
				hit.act()
				return
			}
		}
	}
}

// helpKeys are the home's keys for the ? popup. Labels are the app's buttons where it has
// one; the rest are placeholders that need specified copy.
func (h *cloudHome) helpKeys() [][2]string {
	return [][2]string{
		{"j  k", "Select row"},
		{"←  →  Tab", "Select action"},
		{"Enter", "Press"},
		{"n", "New session"},
		{"l", "Log names"},
		{"e", "Rename"},
		{"x  Backspace", "Remove"},
		{"a", "Add project"},
		{"u", "From URL…"},
		{"r", "Re-check"},
		{"PgUp  PgDn", "Scroll"},
		{"?", "Help"},
		{"q", "Quit"},
	}
}

func (h *cloudHome) draw() {
	rows, cols := termSize()
	frame := h.frame(rows, cols)
	switch {
	case h.help:
		if box := keysBox(h.helpKeys(), rows, cols); len(box) > 0 {
			frame = overlay(frame, box, max(0, (rows-len(box))/2), max(0, (cols-cellWidth(stripSGR(box[0])))/2))
		}
	case h.sheet != nil:
		if box, top, left := h.sheet.box(rows, cols); len(box) > 0 {
			frame = overlay(frame, box, top, left)
		}
	}
	paintRows(frame)
}

// frame is the home without its popup, rows rows of at most cols cells: the header and the
// banner, the projects scrolled to the selected one, and in a pane tall enough for them the
// toast line and the status bar on the last two rows.
func (h *cloudHome) frame(rows, cols int) []string {
	h.moved = false
	h.selected = clampIndex(h.selected, len(h.state.Projects))
	head, body := h.headLines(cols), h.bodyLines(cols)
	footer := rows >= len(head)+3
	room := max(1, rows-len(head))
	if footer {
		room = rows - len(head) - 2
	}
	if h.follow {
		first, last := -1, -1
		for i, line := range body {
			if line.item == h.selected {
				if first < 0 {
					first = i
				}
				last = i
			}
		}
		switch {
		case first < 0:
		case h.selected == 0 && last < room: // the first project shows from the top
			h.top = 0
		case first < h.top:
			h.top = first
		case last >= h.top+room:
			h.top = max(0, min(first, last-room+1))
		}
		h.follow = false
	}
	h.top = max(0, min(h.top, len(body)-room))
	h.bodyRows = room

	lines := append([]homeLine(nil), head...)
	lines = append(lines, body[h.top:min(len(body), h.top+room)]...)
	if footer {
		for len(lines) < rows-2 {
			lines = append(lines, plainLine(""))
		}
		notice := ""
		switch {
		case h.toast != "":
			notice = sgrBold + clip(sanitize(h.toast), cols) + sgrReset
		case h.err != "":
			notice = sgrRed + clip(sanitize(h.err), cols) + sgrReset
		}
		lines = append(lines, plainLine(notice), h.statusBar(cols))
	}
	for len(lines) < rows {
		lines = append(lines, plainLine(""))
	}
	if len(lines) > rows {
		lines = lines[:max(0, rows)]
	}
	h.shown = map[int]homeLine{}
	frame := make([]string, len(lines))
	for i, line := range lines {
		// A row the layout couldn't fit is cut as plain text and loses its buttons.
		if cellWidth(stripSGR(line.text)) > cols {
			line = homeLine{text: clip(stripSGR(line.text), cols), item: line.item}
		}
		frame[i] = line.text
		h.shown[i+1] = line
	}
	return frame
}

// statusBar holds Help at its right, as the other viewers' do.
func (h *cloudHome) statusBar(cols int) homeLine {
	const help = "Help"
	line := plainLine("")
	if cols < len(help) {
		return line
	}
	line.text = strings.Repeat(" ", cols-len(help))
	line.button(sgrUnder+help+sgrReset, func() { h.help = true })
	return line
}
