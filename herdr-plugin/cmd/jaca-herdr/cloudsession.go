package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// openLogNameSheet makes the log-names sheet (the app's LogNameSheet, shared with the home) for
// a project. A variable so tests can leave it out.
var openLogNameSheet = func(p *pane, project cloudProject, done func()) cloudSheet {
	return newLogNameSheet(p, project, done)
}

// launchCloudFork opens a session forked from this one. A variable so tests don't open a tab.
var launchCloudFork = launchCloudSession

// openURL opens a URL in the default browser. A variable so tests don't.
var openURL = func(url string) error { return exec.Command("open", url).Run() }

// cloudMode is what drives the list: the live logs, or a SQL query's result.
type cloudMode int

const (
	cloudModeLogs cloudMode = iota
	cloudModeSQL
)

// What the keys act on in the session viewer, when no popup is open.
type cloudFocus int

const (
	cloudList   cloudFocus = iota // the log list
	cloudSearch                   // the search field
	cloudBar                      // the query builder bar
	cloudDetail                   // the detail panel
	cloudSQLBar                   // the SQL editor bar
)

const (
	// cloudCallTimeout bounds a session call. gcloud is slow, and a call queued behind another
	// waits for it.
	cloudCallTimeout = time.Minute
	// cloudMaxEntryRows is LogTextLines.maxPerEntry: the most display rows one entry takes.
	cloudMaxEntryRows = 200
	// cloudOlderZone is how close to the top, in rows, a scroll asks for older logs.
	cloudOlderZone = 4
)

// cloudCommands runs the session's calls one at a time, in the order they were asked for, off
// the pane's loop: a restart is stop, resetScrollback, start, and the daemon must get them in
// that order.
type cloudCommands struct {
	mu      sync.Mutex
	queue   []func()
	running bool
	// stopped is set when the viewer leaves: calls still queued are dropped.
	stopped atomic.Bool
}

func (q *cloudCommands) enqueue(f func()) {
	q.mu.Lock()
	q.queue = append(q.queue, f)
	if !q.running {
		q.running = true
		go q.run()
	}
	q.mu.Unlock()
}

func (q *cloudCommands) run() {
	for {
		q.mu.Lock()
		if len(q.queue) == 0 {
			q.running = false
			q.mu.Unlock()
			return
		}
		f := q.queue[0]
		q.queue = q.queue[1:]
		q.mu.Unlock()
		f()
	}
}

// cloudHit is a clickable span of the toolbar: rows y0..y1 and columns x0..x1 (1-based).
type cloudHit struct {
	y0, y1, x0, x1 int
	act            func()
}

// cloudViewer is one Cloud Logging session (the app's CloudLogSessionView): the toolbar, the
// query builder bar, in SQL mode the SQL editor bar, the log list with the detail panel beside
// it, and the status bar. The stream runs in jacad; this is its view.
type cloudViewer struct {
	p    *pane
	back func() // returns to the home when the session runs in its pane; nil in a tab
	left bool   // the pane moved on; results that land now are dropped

	// call asks jacad for something without waiting: results come back through finishOpen,
	// finishResync, setCloudState and fail. Tests replace it to record what was asked.
	call     func(method string, params map[string]any)
	commands *cloudCommands
	now      func() time.Time

	id   string // the session's id in jacad, an uppercase UUID
	name string // the tab's name, "" for the project's title
	// cfg is what the session streams. Query, TimeRange and RawFilter are edited in place and
	// reach gcloud on apply or on the next start; LogName follows the project's selected one.
	cfg     cloudStreamConfig
	rawMode bool // a raw filter replaces the query (the app's rawFilter != nil), even when empty
	mode    cloudMode

	project     cloudProject
	haveProject bool
	templates   []cloudQueryTemplate

	connecting   bool // the first subscribe is in flight
	openFailed   bool // the first open failed: Start asks for it again
	openSent     bool // cloud.sessions.open was asked for: the session is this pane's to close
	opened       bool
	wantsRunning bool // what the user last asked for; the stream state follows it
	busy         int  // start and stop calls not answered yet
	stream       cloudStreamState
	gotStream    bool   // a state event arrived
	err          string // why the last call failed, as jacad reported it
	saved        string // where the last export went

	feed      *cloudFeed
	visible   []cloudEntry // the entries the search keeps, oldest first
	lines     []int        // the display rows each of them takes
	totalRows int
	total     int // entries received since the last clear
	trimSeen  int // the feed's dropped count the visible entries were last built for
	follow    bool
	offset    int // display rows scrolled up from the tail
	search    textInput

	// The oldest loaded seq when older logs were last asked for. Until it changes, asking again
	// would fetch the same page.
	olderAsked    bool
	olderAskedSeq uint64
	olderAskedAt  time.Time

	flashed      string // what the last call confirmed ("Saved SQL template")
	noticeHidden bool   // as last drawn: the notice gave its row to the SQL bar

	sel      selection
	listTop  int      // the list as last drawn: the screen row of its first row,
	listW    int      // its width (0 when the detail panel takes the pane),
	listRows int      // its height,
	rowSeq   []uint64 // and the entry on each row

	focus  cloudFocus
	help   bool
	menu   *popupMenu
	editor *formatEditor
	sheet  cloudSheet

	// The toolbar as last drawn.
	hits        []cloudHit
	anchors     map[string][2]int // where a menu opened by key appears: x, y
	statusInBar bool              // the status message fit in the toolbar
	barRows     int
	helpX0      int
	helpX1      int
	chooseRow   int // the empty list's Choose a log name button: its screen row (0 when hidden)
	chooseX0    int
	chooseX1    int

	// The query builder bar (cloudsession_bar.go).
	showBar   bool
	showQuery bool
	barStop   cloudStop
	inputs    map[string]*textInput // a condition's value field, by condition id
	raw       textArea
	barTop    int // the bar as last drawn: its first screen row (0 when hidden),
	barHits   []boxHit
	rawFirst  int // the raw filter's first line in the bar (-1 when not showing),
	rawHeight int

	// The detail panel (cloudsession_detail.go).
	detailOn    bool
	detail      cloudEntry
	detailRow   int
	detailTop   int
	detailKeep  bool // the panel scrolls to keep the selected row in view
	showMessage bool
	showRaw     bool
	detailX0    int // the panel as last drawn: its first screen column (0 when hidden),
	detailW     int
	detailFirst int // the screen row of its first content line,
	detailRoom  int
	detailRows  []cloudDetailRow
	detailHits  []boxHit
	closeX0     int
	closeX1     int
	copyX0      int
	copyX1      int

	// SQL mode (cloudsession_sql.go). query runs a statement over the captured entries without
	// waiting and hands the result to done on the loop. Tests replace it.
	query        func(sql string, done func(dbResultSet, error))
	sqlTemplates []cloudSqlTemplate
	sql          cloudSQL
}

// newCloudSession is the viewer for one session, connected to jacad: it subscribes, opens the
// session (started when the spec says so) and streams it until the screen is left.
func newCloudSession(p *pane, spec cloudSessionSpec, back func()) screen {
	v := newCloudViewer(p, spec, back)
	v.commands = &cloudCommands{}
	v.call = v.send
	v.query = v.sendQuery
	v.connect()
	return v
}

// newCloudViewer is the viewer with nothing asked of jacad yet.
func newCloudViewer(p *pane, spec cloudSessionSpec, back func()) *cloudViewer {
	cfg := spec.Config
	cfg.TimeRange = cfg.TimeRange.normalized()
	v := &cloudViewer{
		p: p, back: back, now: time.Now,
		id: newUUID(), name: spec.Name, cfg: cfg, rawMode: cfg.RawFilter != "",
		wantsRunning: spec.AutoStart, follow: true,
		feed: newCloudFeed(0), inputs: map[string]*textInput{}, anchors: map[string][2]int{},
		rawFirst: -1,
	}
	v.raw.set(cfg.RawFilter)
	v.call = func(string, map[string]any) {}
	return v
}

// MARK: the session in jacad

func (v *cloudViewer) topics() []string {
	entries, older, state := cloudTopics(v.id)
	return []string{entries, older, state}
}

// connect subscribes first, so nothing published by the open is missed, and reads the project
// (its selected log name goes into the config the session opens with). begin takes the result.
func (v *cloudViewer) connect() {
	if v.connecting || v.openSent {
		return
	}
	v.connecting, v.err = true, ""
	c, topics := v.p.c, append(v.topics(), cloudStateTopic)
	go func() {
		err := c.Subscribe(topics...)
		var st cloudState
		have := false
		if err == nil {
			have = c.CallTimeout("cloud.state", nil, &st, syncTimeout) == nil
		}
		v.p.post(func() { v.begin(st, have, err) })
	}()
}

// begin opens the session once the pane is subscribed.
func (v *cloudViewer) begin(st cloudState, have bool, err error) {
	v.connecting = false
	if v.left {
		return
	}
	if err != nil {
		v.wantsRunning = false // nothing was opened: Start tries again
		v.fail(err)
		return
	}
	if have {
		v.setCloudState(st)
	}
	v.openSent = true
	// The id is this pane's own, so a signal can close the session even while the open is in
	// flight.
	v.p.setLive("cloud.sessions.close", v.id)
	v.call("cloud.sessions.open", map[string]any{"id": v.id, "config": v.cfg, "autoStart": v.wantsRunning})
}

// send is call for a live pane: the call joins the queue, and what it returns is posted back to
// the loop. The params are encoded here, on the loop, since the config they hold keeps changing.
func (v *cloudViewer) send(method string, params map[string]any) {
	var raw json.RawMessage
	if params != nil {
		data, err := json.Marshal(params)
		if err != nil {
			v.fail(err)
			return
		}
		raw = data
	}
	c, p, q := v.p.c, v.p, v.commands
	q.enqueue(func() {
		if q.stopped.Load() {
			return
		}
		var args any
		if raw != nil {
			args = raw
		}
		switch method {
		case "cloud.sessions.open":
			var info cloudSessionInfo
			err := c.CallTimeout(method, args, &info, cloudCallTimeout)
			// A session opened after the viewer left, or for a pane that quit, has nobody to
			// close it.
			if q.stopped.Load() || !p.post(func() { v.finishOpen(info, err) }) {
				v.closeRemote()
			}
		case "cloud.saveQueryTemplate", "cloud.saveSqlTemplate":
			var saved bool
			err := c.CallTimeout(method, args, &saved, cloudCallTimeout)
			p.post(func() { v.finishSave(method, saved, err) })
		case "cloud.sessions.range":
			var got []cloudEntry
			err := c.CallTimeout(method, args, &got, cloudCallTimeout)
			p.post(func() { v.finishResync(got, err) })
		case "cloud.state":
			var st cloudState
			if c.CallTimeout(method, args, &st, syncTimeout) == nil {
				p.post(func() { v.setCloudState(st) })
			}
		default:
			err := c.CallTimeout(method, args, nil, cloudCallTimeout)
			p.post(func() { v.finishCall(method, err) })
		}
	})
}

// finishCall takes the reply of a call that returns nothing.
func (v *cloudViewer) finishCall(method string, err error) {
	if method == "cloud.sessions.start" || method == "cloud.sessions.stop" {
		v.busy = max(0, v.busy-1)
		if v.busy == 0 {
			// The state event may have come before this reply, when it was still ignored.
			v.wantsRunning = v.stream.IsRunning
		}
	}
	if err != nil && !v.left {
		v.fail(err)
	}
}

// finishSave confirms a saved template as the app does. jacad answers false for a name it
// would not save, and the app says nothing then either.
func (v *cloudViewer) finishSave(method string, saved bool, err error) {
	switch {
	case v.left:
	case err != nil:
		v.fail(err)
	case !saved:
	case method == "cloud.saveSqlTemplate":
		v.flashed = "Saved SQL template"
	default:
		v.flashed = "Saved query template"
	}
}

func (v *cloudViewer) fail(err error) {
	if err != nil {
		v.err = err.Error()
	}
}

// finishOpen takes cloud.sessions.open's result: the first open, or the one made again to read a
// stream state that was dropped.
func (v *cloudViewer) finishOpen(info cloudSessionInfo, err error) {
	if v.left {
		if err == nil {
			go v.closeRemote() // the call may have recreated a session closed meanwhile
		}
		return
	}
	if err != nil {
		if !v.opened {
			// The next Start opens it again. jacad may have made the session all the same (a
			// reply that timed out): then its state events say whether it runs.
			v.wantsRunning, v.openFailed = v.stream.IsRunning, true
		}
		v.fail(err)
		return
	}
	first := !v.opened
	v.opened, v.openFailed = true, false
	if v.feed.opened(info.Existed) {
		// jacad restarted and made the session again: its seqs start over.
		v.resetView()
	}
	if info.Existed && v.busy == 0 {
		v.wantsRunning = info.State.IsRunning
	}
	// A state event that already arrived is newer than the first open's reply.
	if !first || !v.gotStream {
		v.setStream(info.State)
	}
	v.resync()
}

// resync fetches what the daemon's replay holds after the newest entry received. Live batches
// wait in the feed until finishResync.
func (v *cloudViewer) resync() {
	after, has := v.feed.beginResync()
	params := map[string]any{"id": v.id}
	if has {
		params["afterSeq"] = after
	}
	v.call("cloud.sessions.range", params)
}

func (v *cloudViewer) finishResync(got []cloudEntry, err error) {
	if err != nil {
		got = nil
		if !v.left {
			v.fail(err)
		}
	}
	v.appended(v.feed.finishResync(got))
}

func (v *cloudViewer) closeRemote() {
	c := v.p.c
	if c == nil {
		return
	}
	// cloud.state stays subscribed: subscriptions are per connection, and the home shares it.
	_ = c.CallTimeout("events.unsubscribe", map[string]any{"topics": v.topics()}, nil, teardownTimeout)
	_ = c.CallTimeout("cloud.sessions.close", map[string]any{"id": v.id}, nil, teardownTimeout)
}

// leave closes the session: left open it keeps polling gcloud until jacad's orphan reaper runs.
func (v *cloudViewer) leave(wait bool) {
	v.left = true
	v.stopSQLRefresh()
	if v.commands != nil {
		v.commands.stopped.Store(true)
	}
	if !v.openSent {
		return
	}
	v.openSent = false
	v.p.setLive("", "")
	if wait {
		v.closeRemote()
	} else {
		go v.closeRemote()
	}
}

func (v *cloudViewer) handleEvent(ev event) {
	if v.left {
		return
	}
	entries, older, state := cloudTopics(v.id)
	switch ev.Topic {
	case entries:
		v.appended(v.feed.apply(decodeEach[cloudEntry](ev.Data)))
	case older:
		v.prepended(v.feed.prepend(decodeEach[cloudEntry](ev.Data)))
	case state:
		var st cloudStreamState
		if json.Unmarshal(ev.Data, &st) == nil {
			v.gotStream = true
			v.setStream(st)
		}
	case cloudStateTopic:
		var st cloudState
		if json.Unmarshal(ev.Data, &st) == nil {
			v.setCloudState(st)
		}
	case "events.dropped":
		var note droppedNote
		if json.Unmarshal(ev.Data, &note) != nil {
			return
		}
		switch note.Topic {
		case entries:
			if v.openSent {
				v.resync()
			}
		case state:
			// Opening an open session returns its state and changes nothing. A lost older page
			// is asked for again by scrolling.
			if v.openSent {
				v.call("cloud.sessions.open", map[string]any{"id": v.id, "config": v.cfg, "autoStart": v.wantsRunning})
			}
		case cloudStateTopic:
			v.call("cloud.state", nil)
		}
	}
}

// setStream mirrors the stream state. The run state the user asked for follows it, unless a
// start or stop of this pane's own is still on its way.
func (v *cloudViewer) setStream(st cloudStreamState) {
	v.stream = st
	if v.busy == 0 {
		v.wantsRunning = st.IsRunning
	}
}

// setCloudState takes the project and the templates. The log name is the project's, shared by
// its sessions: when it changes a running session is re-targeted.
func (v *cloudViewer) setCloudState(st cloudState) {
	v.templates = st.QueryTemplates
	v.sqlTemplates = st.SqlTemplates
	project, ok := st.project(v.cfg.ProjectID)
	if !ok {
		return
	}
	v.project, v.haveProject = project, true
	if sheet, ok := v.sheet.(*logNameSheet); ok {
		sheet.setProject(project) // the open sheet lists the names as they arrive
	}
	if project.SelectedLogName != v.cfg.LogName {
		v.cfg.LogName = project.SelectedLogName
		v.apply()
	}
}

// active is whether the session is streaming as far as this pane knows. The daemon keeps
// isRunning set after a finite fetch ended or the poll failed, so such a session reads as
// running too, and is stopped before it starts again.
func (v *cloudViewer) active() bool { return v.wantsRunning || v.stream.IsRunning }

func (v *cloudViewer) sessionParams() map[string]any { return map[string]any{"id": v.id} }

func (v *cloudViewer) start() {
	v.wantsRunning = true
	if !v.openSent {
		v.connect() // the open starts it
		return
	}
	if v.openFailed {
		// The first open failed: it is asked again, ahead of the start. jacad may have made the
		// session all the same, and opening one that exists doesn't start it.
		v.openFailed = false
		v.call("cloud.sessions.open", map[string]any{"id": v.id, "config": v.cfg, "autoStart": true})
	}
	v.busy++
	v.call("cloud.sessions.start", map[string]any{"id": v.id, "config": v.cfg})
}

func (v *cloudViewer) stop() {
	v.wantsRunning = false
	if !v.openSent {
		return
	}
	v.busy++
	v.call("cloud.sessions.stop", v.sessionParams())
}

func (v *cloudViewer) toggle() {
	v.err = ""
	if v.active() {
		v.stop()
	} else {
		v.start()
	}
}

// apply is the app's applyServerQuery: the log name, time range, query and raw filter narrow
// what gcloud returns, so a running session is restarted with them. The daemon ignores a start
// while it runs, hence the stop first. A stopped session keeps the config for its next start.
func (v *cloudViewer) apply() {
	if !v.openSent || !v.active() {
		return
	}
	v.err = ""
	v.stop()
	v.clear()
	v.start()
}

// clear empties the scrollback, here and in the daemon's replay (not in Cloud Logging).
func (v *cloudViewer) clear() {
	v.feed.reset()
	v.resetView()
	if v.openSent && (v.opened || v.gotStream) { // not for a session that failed to open
		v.call("cloud.sessions.resetScrollback", v.sessionParams())
	}
}

// resetView drops what was derived from the entries just cleared.
func (v *cloudViewer) resetView() {
	v.visible, v.lines = nil, nil
	v.totalRows, v.total, v.trimSeen = 0, 0, v.feed.dropped()
	v.clearSelection() // a view the selection held follows the tail again
	v.offset = 0
	v.olderAsked = false
	v.closeDetail()
}

func (v *cloudViewer) setRange(r cloudTimeRange) {
	v.cfg.TimeRange = r
	v.apply()
}

// setMinSeverity is a toolbar severity chip: nil for All. It applies at once.
func (v *cloudViewer) setMinSeverity(sev *int) {
	if v.rawMode {
		return
	}
	v.cfg.Query.SeveritySet, v.cfg.Query.MinSeverity = nil, sev
	v.apply()
}

// setMode switches between the live list and a SQL result (the app's setViewMode).
func (v *cloudViewer) setMode(mode cloudMode) {
	if v.mode == mode {
		return
	}
	v.mode = mode
	v.clearSelection() // a selection names rows of the list it was made in
	if mode == cloudModeSQL {
		v.enterSQL()
	} else {
		v.exitSQL()
	}
}

// listed is the entries the list shows: the one place its rows are chosen. In Logs mode they
// are the loaded entries the search keeps, oldest first. In SQL mode they are the query's rows
// in result order (sqlRebuild puts them here), dividers included.
func (v *cloudViewer) listed() []cloudEntry {
	return v.visible
}

// MARK: entries

// cloudLineCount is LogTextLines.count: the display rows a message takes, one per line, capped.
func cloudLineCount(message string) int {
	n := 1 + strings.Count(message, "\n")
	if strings.HasSuffix(strings.TrimRight(message, "\r"), "\n") {
		n--
	}
	return min(max(n, 1), cloudMaxEntryRows)
}

// cloudDisplayLines is LogTextLines.displayLines: the text of each display row. Past the cap
// the last row says how many lines are left out; a copy still takes the whole message.
func cloudDisplayLines(message string) (lines []string, truncated bool) {
	if strings.Contains(message, "\r") {
		message = strings.ReplaceAll(message, "\r", "")
	}
	parts := strings.Split(message, "\n")
	if len(parts) > 1 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	if len(parts) <= cloudMaxEntryRows {
		return parts, false
	}
	shown := parts[: cloudMaxEntryRows-1 : cloudMaxEntryRows-1]
	hidden := len(parts) - len(shown)
	plural := "s"
	if hidden == 1 {
		plural = ""
	}
	return append(shown, fmt.Sprintf("… %d more line%s — ⌘C copies all", hidden, plural)), true
}

// matches is the instant search over one loaded entry.
func (v *cloudViewer) matches(e cloudEntry) bool {
	return cloudEntryMatches(e, v.search.String())
}

// appended takes the entries the feed just added at its end. A view that isn't following moves
// its offset by the rows added, so it holds still while the stream goes on.
func (v *cloudViewer) appended(added int) {
	if added <= 0 {
		return
	}
	v.total += added
	if v.mode == cloudModeSQL {
		return // the query owns the list; the live refresh rebuilds it
	}
	entries := v.feed.entries()
	added = min(added, len(entries)) // the limit may have trimmed some of them already
	rows := 0
	for _, e := range entries[len(entries)-added:] {
		if v.matches(e) {
			n := cloudLineCount(e.Message)
			v.visible, v.lines = append(v.visible, e), append(v.lines, n)
			rows += n
		}
	}
	v.totalRows += rows
	if !v.follow {
		v.offset += rows
	}
	if v.feed.dropped() != v.trimSeen {
		v.refilter() // the limit trimmed the front
	}
	v.clampOffset()
}

// prepended takes a page of older entries the feed just put at its front. The offset counts
// from the tail, so the view stays on the entries it was showing.
func (v *cloudViewer) prepended(added int) {
	entries := v.feed.entries()
	added = min(added, len(entries))
	if added <= 0 {
		return
	}
	v.total += added
	v.olderAsked = false
	if v.mode == cloudModeSQL {
		return
	}
	var fresh []cloudEntry
	var counts []int
	for _, e := range entries[:added] {
		if v.matches(e) {
			n := cloudLineCount(e.Message)
			fresh, counts = append(fresh, e), append(counts, n)
			v.totalRows += n
		}
	}
	if len(fresh) > 0 {
		v.visible, v.lines = append(fresh, v.visible...), append(counts, v.lines...)
	}
	v.clampOffset()
}

// refilter rebuilds the visible entries after the search or the feed's front changed.
func (v *cloudViewer) refilter() {
	if v.mode == cloudModeSQL {
		v.sqlRebuild()
		return
	}
	v.visible, v.lines, v.totalRows = v.visible[:0], v.lines[:0], 0
	for _, e := range v.feed.entries() {
		if v.matches(e) {
			n := cloudLineCount(e.Message)
			v.visible, v.lines = append(v.visible, e), append(v.lines, n)
			v.totalRows += n
		}
	}
	v.trimSeen = v.feed.dropped()
	v.clampOffset()
}

// cloudOlderRetry is how long a request for older entries that brought nothing blocks the next
// one. A failed read publishes nothing either, so after it scrolling asks again.
const cloudOlderRetry = 5 * time.Second

// loadOlder asks for the page before the oldest loaded entry. It does nothing while a page is
// in flight, when there is nothing older or nothing loaded, and for cloudOlderRetry after a
// request that brought nothing new: a page of entries already loaded publishes nothing and
// leaves hasMoreOlder set, so asking again at once would fetch it again.
func (v *cloudViewer) loadOlder() {
	entries := v.feed.entries()
	if v.mode != cloudModeLogs || !v.openSent || len(entries) == 0 || v.stream.OlderLoading || !v.stream.HasMoreOlder {
		return
	}
	oldest := entries[0].Seq
	now := v.now()
	if v.olderAsked && v.olderAskedSeq == oldest && now.Sub(v.olderAskedAt) < cloudOlderRetry {
		return
	}
	v.olderAsked, v.olderAskedSeq, v.olderAskedAt = true, oldest, now
	v.call("cloud.sessions.loadOlder", v.sessionParams())
}

// MARK: scrolling

// room is the list's height.
func (v *cloudViewer) room() int {
	if v.listRows > 0 {
		return v.listRows
	}
	rows, _ := termSize()
	return max(1, rows-cloudToolbarRows(rows)-1)
}

// clampOffset keeps a scrolled-up view on a full screen.
func (v *cloudViewer) clampOffset() {
	v.offset = max(0, min(v.offset, max(0, v.totalRows-v.room())))
	if v.follow {
		v.offset = 0
	}
}

// topRow is the first display row on screen.
func (v *cloudViewer) topRow() int {
	return max(0, v.totalRows-v.offset-v.room())
}

// scroll moves the view by display rows, up for a positive count. Scrolled up it stops
// following the tail; back at the bottom it follows again, unless entries are selected. Near
// the top it asks for older logs.
func (v *cloudViewer) scroll(by int) {
	v.follow = false
	v.offset += by
	v.clampOffset()
	if v.offset == 0 && !v.sel.on {
		v.follow = true
	}
	if by > 0 && v.topRow() <= cloudOlderZone {
		v.loadOlder()
	}
}

func (v *cloudViewer) followTail() {
	v.clearSelection()
	v.follow, v.offset = true, 0
}

func (v *cloudViewer) toggleFollow() {
	if v.follow {
		v.follow = false
		return
	}
	v.followTail()
}

// cloudRowRef is one display row: an entry's position in the list and a line of its message.
type cloudRowRef struct{ idx, sub int }

// window is the display rows on screen, top first, for a list n rows tall. It walks back from
// the tail, so its cost is the distance scrolled, not the whole list.
func (v *cloudViewer) window(n int) []cloudRowRef {
	bottom := max(0, v.totalRows-v.offset)
	top := max(0, bottom-n)
	refs := make([]cloudRowRef, 0, bottom-top)
	end := v.totalRows
	for i := min(len(v.visible), len(v.lines)) - 1; i >= 0 && end > top; i-- {
		start := end - v.lines[i]
		for r := min(end, bottom) - 1; r >= max(start, top); r-- {
			refs = append(refs, cloudRowRef{i, r - start})
		}
		end = start
	}
	for i, j := 0, len(refs)-1; i < j; i, j = i+1, j-1 {
		refs[i], refs[j] = refs[j], refs[i]
	}
	return refs
}

// MARK: selection

// indexOf is the position of the entry with this seq in the list.
func (v *cloudViewer) indexOf(seq uint64) (int, bool) {
	if v.mode == cloudModeSQL {
		return v.sqlIndexOf(seq)
	}
	list := v.listed()
	i := sort.Search(len(list), func(i int) bool { return list[i].Seq >= seq })
	return i, i < len(list) && list[i].Seq == seq
}

// selectEntries replaces the selection. The view is held where it is, so the entries under the
// selection stay put while the stream goes on.
func (v *cloudViewer) selectEntries(s selection) {
	s.held = v.sel.held || v.follow
	v.follow = false
	v.sel = s
}

// selectedEntries are the entries the selection covers, on screen or scrolled away.
func (v *cloudViewer) selectedEntries() []cloudEntry {
	if !v.sel.on {
		return nil
	}
	if v.mode == cloudModeSQL {
		return v.sqlSelectedEntries()
	}
	list := v.listed()
	if v.sel.all {
		return list
	}
	lo, hi := min(v.sel.anchor, v.sel.cursor), max(v.sel.anchor, v.sel.cursor)
	from := sort.Search(len(list), func(i int) bool { return list[i].Seq >= lo })
	to := sort.Search(len(list), func(i int) bool { return list[i].Seq > hi })
	return list[from:to]
}

// clearSelection drops the highlight; a view that was following the tail follows it again.
func (v *cloudViewer) clearSelection() {
	if v.sel.held {
		v.follow, v.offset = true, 0
	}
	v.sel = selection{}
}

// reveal scrolls so the entry at a position of the list is on screen.
func (v *cloudViewer) reveal(idx int) {
	if idx < 0 || idx >= len(v.lines) {
		return
	}
	below := 0
	for _, n := range v.lines[idx+1:] {
		below += n
	}
	room := v.room()
	if below < v.offset {
		v.offset = below
	}
	if below+v.lines[idx] > v.offset+room {
		v.offset = below + v.lines[idx] - room
	}
	v.offset = max(0, v.offset)
}

// moveCursor is the arrow keys and the page keys on the list: the selection becomes the entry
// by entries away (older for less than 0), stopping at the ends, and the list scrolls to show
// it. With nothing selected, up starts on the newest entry on screen and down scrolls as
// before; down from the newest entry lets go of the selection and follows the tail again. An
// open detail panel follows, and the keys stay with the list. Near the top it asks for older
// logs.
func (v *cloudViewer) moveCursor(by int) {
	list := v.listed()
	i, found := v.indexOf(v.sel.cursor)
	seq := v.sel.cursor
	if !v.sel.on || v.sel.all || !found {
		if by > 0 {
			v.scroll(-by)
			return
		}
		newest, ok := v.newestShown()
		if !ok {
			return
		}
		seq = newest
		i, _ = v.indexOf(seq)
	} else {
		from := i
		step := 1
		if by < 0 {
			step = -1
		}
		for left, j := by*step, i+step; left > 0 && j >= 0 && j < len(list); j += step {
			if !v.sqlMarker(list[j].Seq) { // dividers are not entries
				i, seq = j, list[j].Seq
				left--
			}
		}
		if by > 0 && i == from {
			v.clearSelection()
			v.followTail()
			return
		}
	}
	v.selectEntries(selection{on: true, anchor: seq, cursor: seq})
	v.reveal(i)
	if v.detailOn {
		focus := v.focus
		v.openDetail(seq)
		v.focus = focus
	}
	if by < 0 && v.topRow() <= cloudOlderZone {
		v.loadOlder()
	}
}

// extend moves the selection's end one entry, older for -1 and newer for +1, starting on the
// newest entry on screen when nothing is selected.
func (v *cloudViewer) extend(by int) {
	list := v.listed()
	if len(list) == 0 {
		return
	}
	i, found := v.indexOf(v.sel.cursor)
	if !v.sel.on || v.sel.all || !found {
		if seq, ok := v.newestShown(); ok {
			v.selectEntries(selection{on: true, anchor: seq, cursor: seq})
		}
		return
	}
	// Dividers are not entries: the cursor steps over them, and stays put at the end.
	for i += by; i >= 0 && i < len(list); i += by {
		if !v.sqlMarker(list[i].Seq) {
			v.sel.cursor = list[i].Seq
			v.reveal(i)
			return
		}
	}
}

// newestShown is the entry Enter and Shift-arrow start from with nothing selected: the lowest
// one on screen, or the last of the list before anything was drawn. Dividers don't count.
func (v *cloudViewer) newestShown() (uint64, bool) {
	for i := len(v.rowSeq) - 1; i >= 0; i-- {
		if !v.sqlMarker(v.rowSeq[i]) {
			return v.rowSeq[i], true
		}
	}
	list := v.listed()
	for i := len(list) - 1; i >= 0; i-- {
		if !v.sqlMarker(list[i].Seq) {
			return list[i].Seq, true
		}
	}
	return 0, false
}

// cloudCopyText is the clipboard text for entries: each in the saved copy format, with
// CloudLogEntry.copyFields' tokens.
func cloudCopyText(entries []cloudEntry, format logCopyFormat) string {
	out := make([]string, len(entries))
	for i, e := range entries {
		date := ""
		if format.DateFormat != "" {
			date = formatSwiftDate(e.time(), format.DateFormat)
		}
		out[i] = substituteTokens(format.Template, map[string]string{
			"date":       date,
			"level":      severityName(e.Severity),
			"levelShort": severityShort(e.Severity),
			"tag":        e.tag(),
			"message":    e.Message,
			"logId":      e.LogID,
			"trace":      e.Trace,
			"insertId":   e.InsertID,
		})
	}
	return strings.Join(out, "\n")
}

// copySelection copies the selected entries whole, in the saved copy format.
func (v *cloudViewer) copySelection() {
	entries := v.selectedEntries()
	if len(entries) == 0 {
		return
	}
	v.copy(cloudCopyText(entries, loadCopyFormat(copyFormatPath())))
}

func (v *cloudViewer) copy(text string) {
	write := copyToClipboard
	go func() {
		if err := write(text); err != nil {
			v.p.post(func() { v.fail(err) })
		}
	}()
}

// dragSelection moves the selection's end to the entry under the pointer and copies the
// selection when the button is released. With the pointer above or below the list the view
// scrolls that way, extending the selection.
func (v *cloudViewer) dragSelection(m mouseEvent, released bool) {
	line := m.y - v.listTop
	switch {
	case len(v.rowSeq) == 0:
		v.sel.edge = 0
	default:
		v.sel.edge = 0
		if line < 0 {
			v.sel.edge, line = -1, 0
		} else if line >= len(v.rowSeq) {
			v.sel.edge, line = 1, len(v.rowSeq)-1
		}
		if !v.sqlMarker(v.rowSeq[line]) { // a divider is not an entry: the cursor stays
			v.sel.cursor = v.rowSeq[line]
		}
	}
	if released {
		v.sel.dragging, v.sel.edge = false, 0
		// A plain click only selects and opens the details, as in the app; dragging over
		// several entries copies them.
		if v.sel.anchor != v.sel.cursor {
			v.copySelection()
		}
		return
	}
	if v.sel.edge != 0 && !v.sel.scrolling {
		v.edgeScroll()
	}
	v.copyAtRest()
}

// edgeScroll scrolls one row toward the pointer and repeats while the drag stays past that
// edge of the list.
func (v *cloudViewer) edgeScroll() {
	v.sel.scrolling = v.sel.dragging && v.sel.edge != 0
	if !v.sel.scrolling {
		return
	}
	v.scroll(-v.sel.edge)
	if refs, list := v.window(v.room()), v.listed(); len(refs) > 0 {
		at := refs[0]
		if v.sel.edge > 0 {
			at = refs[len(refs)-1]
		}
		if at.idx < len(list) && !v.sqlMarker(list[at.idx].Seq) {
			v.sel.cursor = list[at.idx].Seq
		}
	}
	time.AfterFunc(40*time.Millisecond, func() { v.p.post(v.edgeScroll) })
}

// copyAtRest copies the selection once the pointer has rested a moment during a drag, since a
// terminal that doesn't report the button's release would otherwise never copy.
func (v *cloudViewer) copyAtRest() {
	v.sel.moves++
	moves := v.sel.moves
	time.AfterFunc(250*time.Millisecond, func() {
		v.p.post(func() {
			if v.sel.dragging && v.sel.moves == moves && v.sel.edge == 0 && v.sel.anchor != v.sel.cursor {
				v.copySelection()
			}
		})
	})
}

// MARK: actions

// exportName is the file an export is saved as by default: the tab's name, else the project's.
func (v *cloudViewer) exportName() string {
	name := v.name
	if name == "" {
		name = v.projectTitle()
	}
	return strings.NewReplacer("/", "-", ":", "-").Replace(name) + ".log"
}

func (v *cloudViewer) projectTitle() string {
	if v.haveProject {
		return v.project.title()
	}
	return v.cfg.ProjectID
}

// export is the app's Export: the raw JSON of the entries the list shows, saved where the macOS
// save dialog says.
func (v *cloudViewer) export() {
	text, name := cloudExportText(v.listed()), v.exportName()
	v.err, v.saved = "", ""
	go func() {
		path, err := chooseSavePath(name)
		if err == nil && path != "" {
			err = os.WriteFile(path, []byte(text), 0o644)
		}
		v.p.post(func() {
			if err != nil {
				v.fail(err)
				return
			}
			v.saved = path // empty when the dialog was cancelled
		})
	}()
}

func (v *cloudViewer) openFormatEditor() {
	v.menu = nil
	v.editor = newFormatEditor(loadCopyFormat(copyFormatPath()))
}

// editorDo ends the Copy format popup when a key or click asked to: Save writes the format to
// the file the app reads.
func (v *cloudViewer) editorDo(action editorAction) {
	if action == editorStay {
		return
	}
	if action == editorSave {
		if err := saveCopyFormat(copyFormatPath(), v.editor.format()); err != nil {
			v.fail(err)
		}
	}
	v.editor = nil
}

// openLogNames shows the project's log-names sheet, where one is wired in.
func (v *cloudViewer) openLogNames() {
	if openLogNameSheet == nil || !v.haveProject {
		return
	}
	v.sheet = openLogNameSheet(v.p, v.project, v.closeSheet)
	if sheet, ok := v.sheet.(*logNameSheet); ok {
		sheet.flash = func(message string) { v.fail(errors.New(message)) } // a failed refresh shows on the error line
	}
}

func (v *cloudViewer) closeSheet() { v.sheet = nil }

// openFork opens a new session that keeps this one's log name and time range, running at once.
func (v *cloudViewer) openFork(fork cloudFork) {
	cfg := v.cfg
	cfg.Query, cfg.RawFilter = fork.Query, fork.RawFilter
	spec := cloudSessionSpec{Config: cfg, Name: fork.Name, AutoStart: true}
	if !underHerdr() {
		// With no Herdr to open a tab the new session takes this pane, so this one closes.
		v.leave(false)
	}
	launchCloudFork(v.p, spec, v.back, v.fail, nil)
}

// toggleFavorite pins or unpins a label key for the project's selected log name.
func (v *cloudViewer) toggleFavorite(key string) {
	v.call("cloud.toggleFavoriteLabel", map[string]any{"project": v.cfg.ProjectID, "logName": v.cfg.LogName, "key": key})
}

func (v *cloudViewer) isFavorite(key string) bool {
	return containsString(v.project.favoriteLabelKeys(), key)
}

// MARK: keys

// helpKeys are the viewer's keys for the ? popup. Labels are the app's where it has one.
func (v *cloudViewer) helpKeys() [][2]string {
	return [][2]string{
		{"p  space", "Start / Stop"},
		{"c", "Clear view"},
		{"f  G  Shift+PgDn", "Follow tail"},
		{"l", "Choose log name"},
		{"t", "Time range"},
		{"u", "Copy Logs Explorer URL / Open in browser"},
		{"1-6", "Minimum severity"},
		{"F", "Filters"},
		{"/", "filter loaded logs…"},
		{"j  k", "Scroll"},
		{"↑  ↓  PgUp  PgDn", "Select entries"},
		{"Shift+↑  Shift+↓", "Select entries"},
		{"C", "Copy Entry"},
		{"y", "Copy format"},
		{"S", "Export"},
		{"Enter", "Details / row actions"},
		{"[  ]", "Previous / next entry"},
		{"Tab", "Switch between list and details"},
		{"Q", "Logs / SQL"},
		{"s", "SQL filter"},
		{"Ctrl+R", "Run"},
		{"Esc", "Close / back"},
		{"?", "Help"},
		{"q", "Quit"},
	}
}

func (v *cloudViewer) handleKey(k []byte) bool {
	if len(k) == 1 && k[0] == 0x03 {
		// Ctrl-C quits, except in the SQL editor, where it copies the selection as it does in
		// the override editor, and quitting would drop the query being written.
		if v.focus != cloudSQLBar || !v.sql.editing || v.sql.noRoom {
			return true
		}
		if text := v.sql.text.selectedText(); text != "" {
			v.copy(text)
		}
		return false
	}
	if m, ok := parseMouse(k); ok {
		v.mouse(m)
		return false
	}
	// The export note, the last confirmation and the last failure last until the next key,
	// once they were on screen.
	if !v.noticeHidden {
		v.saved, v.flashed, v.err = "", "", ""
	}
	switch {
	case v.sheet != nil:
		v.sheet.key(k)
		return false
	case v.menu != nil:
		v.menuKey(k)
		return false
	case v.editor != nil:
		v.editorDo(v.editor.key(k))
		return false
	case v.help:
		// Dismissed by Esc, Enter, q or ? again.
		if isEsc(k) || isEnter(k) || (len(k) == 1 && (k[0] == 'q' || k[0] == '?')) {
			v.help = false
		}
		return false
	}
	if v.focus == cloudSQLBar && v.sql.noRoom {
		// The pane got too short for the SQL bar: this key moves to the list and does no more,
		// so nothing is typed into an editor out of sight or taken as a list command.
		v.focus, v.sql.editing = cloudList, false
		return false
	}
	if v.sqlRunKey(k) {
		return false
	}
	switch v.focus {
	case cloudSearch:
		v.editSearch(k)
		return false
	case cloudBar:
		v.barKey(k)
		return false
	case cloudSQLBar:
		v.sqlKey(k)
		return false
	case cloudDetail:
		if v.sqlDetailStep(k) || v.detailKey(k) {
			return false
		}
	}
	nav, isNav := decodeNav(k)
	extends := isNav && nav.shift && (nav.dir == "up" || nav.dir == "down")
	moves := len(k) != 1 && (isUp(k) || isDown(k) || isPageUp(k) || isPageDown(k))
	if v.focus == cloudList && v.sel.on && !extends && !moves && !isEnter(k) && !isEsc(k) && !(len(k) == 1 && k[0] == 'C') {
		v.clearSelection() // any other key drops the highlight
	}
	if len(k) != 1 {
		switch {
		case extends && nav.dir == "up":
			v.extend(-1)
		case extends:
			v.extend(1)
		case isShiftPageDown(k):
			v.followTail()
		case isUp(k):
			v.moveCursor(-1)
		case isDown(k):
			v.moveCursor(1)
		case isPageUp(k):
			v.moveCursor(-v.room())
		case isPageDown(k):
			v.moveCursor(v.room())
		}
		return false
	}
	switch c := k[0]; {
	case c == 'q':
		return true
	case c == 0x1b:
		switch {
		case v.detailOn:
			v.closeDetail()
			v.clearSelection()
		case v.sel.on:
			v.clearSelection()
		case v.back != nil:
			v.leave(false)
			v.back()
		}
	case c == '\r' || c == '\n':
		v.openDetailAtCursor()
	case c == '\t':
		if v.detailOn {
			v.focus = cloudDetail
		}
	case c == 'k':
		v.scroll(1)
	case c == 'j':
		v.scroll(-1)
	case c == 'G':
		v.followTail()
	case c == 'f':
		v.toggleFollow()
	case c == ' ' || c == 'p':
		v.toggle()
	case c == 'c':
		v.clear()
	case c == 'C':
		v.copySelection()
	case c == 'S' || c == 'e':
		v.export()
	case c == 'y':
		v.openFormatEditor()
	case c == '/':
		v.focus = cloudSearch
	case c == 'F':
		v.toggleBar()
	case c == 'Q':
		v.toggleMode()
	case c == 's':
		v.focusSQLBar()
	case c == 't':
		v.openTimeMenu(v.anchor("time"))
	case c == 'u':
		v.openShareMenu(v.anchor("share"))
	case c == 'l':
		v.openLogNames()
	case c >= '1' && c <= '6':
		v.severityChip(int(c - '1'))
	case c == '?':
		v.help = true
	}
	return false
}

// severityChip is the toolbar's chip n: All, then the common severities.
func (v *cloudViewer) severityChip(n int) {
	if n <= 0 || n > len(cloudCommonSeverities) {
		v.setMinSeverity(nil)
		return
	}
	sev := cloudCommonSeverities[n-1]
	v.setMinSeverity(&sev)
}

func (v *cloudViewer) editSearch(k []byte) {
	if isEnter(k) || isEsc(k) {
		v.focus = cloudList
		return
	}
	if v.search.handle(k) {
		v.refilter()
	}
}

// anchor is where a menu opened by key appears: under its toolbar chip as last drawn.
func (v *cloudViewer) anchor(name string) (x, y int) {
	at, ok := v.anchors[name]
	if !ok {
		return 1, 2
	}
	return at[0], at[1]
}

func (v *cloudViewer) openTimeMenu(x, y int) {
	items := make([]menuItem, 0, len(cloudTimePresets)+3)
	for _, preset := range cloudTimePresets {
		items = append(items, menuItem{preset.Label, func() { v.setRange(cloudTimeRange{Minutes: preset.Minutes}) }})
	}
	items = append(items, menuItem{},
		menuItem{"Custom minutes…", v.openCustomMinutes},
		menuItem{"Absolute range…", v.openAbsoluteRange})
	v.menu = &popupMenu{x: x, y: y, items: items}
}

func (v *cloudViewer) openShareMenu(x, y int) {
	v.menu = &popupMenu{x: x, y: y, items: []menuItem{
		{"Copy Logs Explorer URL", func() { v.copy(cloudConsoleURL(v.cfg)) }},
		{"Open in browser", func() {
			url := cloudConsoleURL(v.cfg)
			go func() {
				if err := openURL(url); err != nil {
					v.p.post(func() { v.fail(err) })
				}
			}()
		}},
	}}
}

// openRowMenu shows the app's log row menu (CloudLogNSTableView.menu) at a cell.
func (v *cloudViewer) openRowMenu(x, y int) {
	label := "Copy Entry"
	if n := len(v.selectedEntries()); n > 1 {
		label = fmt.Sprintf("Copy %d Entries", n)
	}
	v.menu = &popupMenu{x: x, y: y, items: []menuItem{
		{label, v.copySelection},
		{"Copy Format…", v.openFormatEditor},
		{},
		{"Select All", func() { v.selectEntries(selection{on: true, all: true}) }},
	}}
}

// runMenuItem closes the menu and runs an item. A heading has nothing to run.
func (v *cloudViewer) runMenuItem(i int) {
	menu := v.menu
	if menu == nil || i < 0 || i >= len(menu.items) || menu.items[i].act == nil {
		return
	}
	v.menu = nil
	menu.items[i].act()
}

// menuKey: arrows (or j/k) move, Enter runs the item, Esc closes the menu.
func (v *cloudViewer) menuKey(k []byte) {
	switch {
	case isEsc(k) || (len(k) == 1 && k[0] == 'q'):
		v.menu = nil
	case isUp(k):
		v.menu.move(-1)
	case isDown(k):
		v.menu.move(1)
	case isEnter(k):
		v.runMenuItem(v.menu.selected)
	}
}

// MARK: mouse

func (v *cloudViewer) mouse(m mouseEvent) {
	const wheelUp, wheelDown, wheelLines = 64, 65, 3
	if v.sheet != nil {
		v.sheet.mouse(m)
		return
	}
	// A release is the report's final byte in SGR mode; some terminals send button 3 instead.
	released := !m.press || (m.button&mouseDrag == 0 && m.button&3 == 3)
	if v.sel.dragging && (m.button&mouseDrag != 0 || released) {
		v.dragSelection(m, released)
		return
	}
	if !m.press {
		return
	}
	switch {
	case v.help:
		if m.button == 0 { // a click anywhere dismisses the popup
			v.help = false
		}
		return
	case v.editor != nil:
		if m.button == 0 {
			v.editorDo(v.editor.click(m.x, m.y))
		}
		return
	case v.menu != nil:
		// A click on an item runs it; any other press closes the menu.
		if i := v.menu.itemAt(m.x, m.y); m.button == 0 && i >= 0 {
			v.runMenuItem(i)
		} else {
			v.menu = nil
		}
		return
	}
	rows, _ := termSize()
	inList := m.y >= v.listTop && m.y < v.listTop+v.listRows
	inDetail := inList && v.detailX0 > 0 && m.x >= v.detailX0
	inSQL := v.sqlBarAt(m.y)
	inBar := !inSQL && v.barTop > 0 && m.y >= v.barTop && m.y < v.listTop
	switch m.button {
	case wheelUp, wheelDown:
		by := wheelLines
		if m.button == wheelDown {
			by = -wheelLines
		}
		if inDetail {
			v.detailTop, v.detailKeep = max(0, v.detailTop-by), false
		} else if inSQL {
			v.sqlWheel(by)
		} else if !inBar {
			v.scroll(by)
		}
	case 0:
		switch {
		case m.y <= v.barRows:
			v.focus = cloudList
			for i := len(v.hits) - 1; i >= 0; i-- {
				if h := v.hits[i]; m.y >= h.y0 && m.y <= h.y1 && m.x >= h.x0 && m.x <= h.x1 {
					h.act()
					return
				}
			}
		case inSQL:
			v.sqlClick(m.x, m.y)
		case inBar:
			v.barClick(m.x, m.y)
		case inDetail:
			v.detailClick(m, false)
		case m.y == rows && v.helpX0 > 0 && m.x >= v.helpX0 && m.x <= v.helpX1:
			v.help = true
		case v.chooseRow != 0 && m.y == v.chooseRow && m.x >= v.chooseX0 && m.x <= v.chooseX1:
			v.openLogNames()
		case !inList || m.y-v.listTop >= len(v.rowSeq) || v.sqlMarkerRow(m.y):
			v.focus = cloudList
			v.clearSelection()
		default:
			// A press selects the entry under it and shows it in the detail panel; dragging
			// from here extends the selection.
			seq := v.rowSeq[m.y-v.listTop]
			v.selectEntries(selection{on: true, dragging: true, anchor: seq, cursor: seq})
			v.openDetail(seq)
		}
	case 2:
		switch {
		case inDetail:
			v.detailClick(m, true)
		case inList && m.y-v.listTop < len(v.rowSeq) && !v.sqlMarkerRow(m.y):
			// Like the app's table: a right click on an entry outside the selection selects it,
			// then the menu acts on the selection.
			if seq := v.rowSeq[m.y-v.listTop]; !v.selected(seq) {
				v.selectEntries(selection{on: true, anchor: seq, cursor: seq})
			}
			v.sel.dragging = false
			v.openRowMenu(m.x, m.y)
		}
	}
}

// MARK: drawing

func (v *cloudViewer) draw() {
	rows, cols := termSize()
	frame := v.frame(rows, cols)
	if box, top, left := v.popup(rows, cols); len(box) > 0 {
		frame = overlay(frame, box, top, left)
	}
	paintRows(frame)
}

// popup is the open popup's box and where it goes (0-based), nil when none is open.
func (v *cloudViewer) popup(rows, cols int) (box []string, top, left int) {
	switch {
	case v.sheet != nil:
		return v.sheet.box(rows, cols)
	case v.menu != nil:
		box = v.menu.box(rows, cols)
		return box, v.menu.top - 1, v.menu.left - 1
	case v.editor != nil:
		box = v.editor.box(rows, cols)
		return box, v.editor.top - 1, v.editor.left - 1
	case v.help:
		box = keysBox(v.helpKeys(), rows, cols)
		if len(box) == 0 {
			v.help = false // no room to draw it: it is closed, not left holding the keys unseen
			return nil, 0, 0
		}
		return box, max(0, (rows-len(box))/2), max(0, (cols-cellWidth(stripSGR(box[0])))/2)
	}
	return nil, 0, 0
}

// notice is the line under the toolbar: a failed call or a status message the toolbar had no
// room for (red), else where the last export was saved.
func (v *cloudViewer) notice() (text string, failed bool) {
	switch {
	case v.err != "":
		return v.err, true
	case v.stream.StatusMessage != "" && !v.statusInBar:
		return v.stream.StatusMessage, true
	}
	if v.saved != "" {
		return "↓ " + v.saved, false
	}
	return v.flashed, false
}

// frame draws the pane without its popups: every row at most cols cells.
func (v *cloudViewer) frame(rows, cols int) []string {
	if cols < 1 || rows < 1 {
		return nil
	}
	var frame []string
	frame = append(frame, v.toolbar(cols, cloudBarTall(rows))...)
	v.barRows = len(frame)
	// While the SQL editor has the keys the notice gives way when its row is the one the bar
	// needs, so a message doesn't take the editor from under the cursor.
	text, failed := v.notice()
	v.noticeHidden = false
	if text != "" && v.mode == cloudModeSQL && v.focus == cloudSQLBar && rows-len(frame)-1-3-1 < 3 && rows-len(frame)-1-3 >= 3 {
		text, v.noticeHidden = "", true
	}
	switch {
	case text == "":
	case failed:
		frame = append(frame, sgrRed+fit(sanitize(text), cols)+sgrReset)
	default:
		frame = append(frame, sgrGreen+fit(sanitize(text), cols)+sgrReset)
	}

	// The builder bar takes what the list can spare.
	v.barTop, v.barHits = 0, nil
	if budget := rows - len(frame) - 1 - 3 - v.sqlBarReserve(); v.showBar && budget >= 3 {
		v.barTop = len(frame) + 1
		frame = append(frame, v.barLines(cols, budget)...)
	}
	// The SQL editor bar, under it as in the app.
	frame = append(frame, v.sqlBarLines(cols, rows-len(frame)-1-3, len(frame)+1)...)

	// The list, with the detail panel beside it when an entry is open.
	v.listTop = len(frame) + 1
	v.listRows = max(1, rows-len(frame)-1)
	v.listW, v.detailX0 = cols, 0
	if v.detailOn {
		v.listW = max(40, cols*9/20)
		if cols-v.listW < 30 { // too narrow to split: the panel takes the pane
			v.listW = 0
		}
	}
	if !v.detailOn {
		frame = append(frame, v.listColumn(cols, v.listRows)...)
	} else if v.listW == 0 {
		v.detailX0 = 1
		v.rowSeq = v.rowSeq[:0]
		frame = append(frame, v.detailColumn(cols, v.listRows)...)
	} else {
		list := v.listColumn(v.listW, v.listRows)
		v.detailX0 = v.listW + 2
		detail := v.detailColumn(cols-v.listW-1, v.listRows)
		for i := range detail {
			frame = append(frame, list[i]+borderStyle+"│"+sgrReset+detail[i])
		}
	}
	frame = append(frame, v.statusBar(cols))
	if len(frame) > rows {
		frame = frame[:rows]
	}
	return frame
}

// cloudBarTall is whether the toolbar's two rows of controls are three rows tall each.
func cloudBarTall(rows int) bool { return rows >= 24 }

// cloudToolbarRows is the toolbar's height.
func cloudToolbarRows(rows int) int {
	if cloudBarTall(rows) {
		return 6
	}
	return 2
}

// cloudBarRow lays out one row of toolbar controls. Chips are padded by pad cells a side; one
// that doesn't fit is left out.
type cloudBarRow struct {
	barBuilder
	chipPad int
}

func (b *cloudBarRow) fits(w int) bool { return b.used+min(b.used, 1)+w <= b.cols }

// chip adds a clickable label, a block of style when it has one, and reports whether it fit.
// With no act it is drawn but does nothing.
func (b *cloudBarRow) chip(label, style string, act func()) bool {
	w := cellWidth(label) + 2*b.chipPad
	if !b.fits(w) {
		return false
	}
	pad := strings.Repeat(" ", b.chipPad)
	blank := style + strings.Repeat(" ", w) + sgrReset
	b.add(w, blank, style+pad+label+pad+sgrReset, blank, act)
	return true
}

// space adds w cells holding text (already styled, text cells wide) at their right end.
func (b *cloudBarRow) space(w int, text string, cells int) {
	if w <= 0 || !b.fits(w) {
		return
	}
	blank := strings.Repeat(" ", w)
	b.add(w, blank, strings.Repeat(" ", max(0, w-cells))+text, blank, nil)
}

// collect turns the row's spans into toolbar hits on screen rows y0..y1.
func (b *cloudBarRow) collect(v *cloudViewer, y0, y1 int) {
	for _, h := range b.hits {
		v.hits = append(v.hits, cloudHit{y0: y0, y1: y1, x0: h.x0, x1: h.x1, act: h.act})
	}
}

// The glyphs of the toolbar's icon buttons. The app's are icons with a tooltip; the tooltip is
// the chip's label where the pane is wide enough for it.
const (
	cloudGlyphStart   = "▶"
	cloudGlyphStop    = "■"
	cloudGlyphClear   = "⌫"
	cloudGlyphFollow  = "⤓"
	cloudGlyphShare   = "↗"
	cloudGlyphLoading = "⟳"
)

// toolbar is the app's two rows of controls. Row 1: start/stop, clear, follow, the log name, the
// time range, then at the right the loading mark, the status message, share and the Logs/SQL
// switch. Row 2: the severity chips, Filters, the search field, Copy format and Export. The
// labels shrink to glyphs, then the padding goes, as a row runs out of room.
func (v *cloudViewer) toolbar(cols int, tall bool) []string {
	v.hits = v.hits[:0]
	// Each row takes the roomiest layout that holds all of its controls.
	pick := func(build func(cols int, tall bool, level int) (*cloudBarRow, bool)) (b *cloudBarRow) {
		for level := 0; level <= 2; level++ {
			var complete bool
			if b, complete = build(cols, tall, level); complete {
				break
			}
		}
		return b
	}
	top, second := pick(v.toolbarTop), pick(v.toolbarSecond)
	height := 1
	if tall {
		height = 3
	}
	top.collect(v, 1, height)
	second.collect(v, height+1, 2*height)
	return append(top.rows(), second.rows()...)
}

func newCloudBarRow(cols int, tall bool, level int) *cloudBarRow {
	b := &cloudBarRow{barBuilder: barBuilder{cols: cols, tall: tall}}
	b.chipPad = b.pad()
	if level >= 2 {
		b.chipPad = 1
	}
	return b
}

// cloudSeverityBadge is a severity's badge: neutral for DEFAULT and DEBUG, blue for INFO and
// NOTICE, yellow for WARNING, red from ERROR up (CloudSeverityStyle.badgeBackground).
func cloudSeverityBadge(sev int) string {
	switch sev = knownCloudSeverity(sev); {
	case sev >= 500:
		return badgeStyle(colorRed)
	case sev >= 400:
		return badgeStyle(colorYellow)
	case sev >= 200:
		return badgeStyle(colorBlue)
	}
	return sgrBold
}

// cloudSeverityText is the color of a message of this severity. DEFAULT, DEBUG and INFO keep the
// default text, as the device log list does for its low levels.
func cloudSeverityText(sev int) string {
	switch sev = knownCloudSeverity(sev); {
	case sev >= 500:
		return sgrRed
	case sev >= 400:
		return sgrYellow
	case sev >= 300:
		return "\x1b[" + colorBlue + "m"
	}
	return ""
}

func (v *cloudViewer) toolbarTop(cols int, tall bool, level int) (b *cloudBarRow, complete bool) {
	b = newCloudBarRow(cols, tall, level)
	complete = true
	chip := func(label, style string, act func()) {
		complete = b.chip(label, style, act) && complete
	}
	y := 2
	if tall {
		y = 4
	}
	start, stop, clear, follow := cloudGlyphStart+" Start", cloudGlyphStop+" Stop", "Clear view", "Follow tail"
	if level >= 1 {
		start, stop, clear, follow = cloudGlyphStart, cloudGlyphStop, cloudGlyphClear, cloudGlyphFollow
	}
	if v.active() {
		chip(stop, badgeStyle(colorRed), v.toggle)
	} else {
		chip(start, badgeStyle(colorGreen), v.toggle)
	}
	chip(clear, "", v.clear)
	followStyle := ""
	if v.follow {
		followStyle = sgrBold + sgrRev
	}
	chip(follow, followStyle, v.toggleFollow)

	// What follows the log name, so the name takes the width that is left.
	const logs, sql = " Logs ", " SQL "
	timeLabel := v.cfg.TimeRange.label() + " ▼"
	if v.stream.IsLoading {
		// The loading mark rides on the time range, which is always drawn: the room at the
		// right is gone in an 80 or 120 column pane.
		timeLabel += " " + cloudGlyphLoading
	}
	timeW := cellWidth(timeLabel) + 2*b.chipPad
	shareW := 1 + 2*b.chipPad
	modeW := len(logs) + len(sql)
	name, nameStyle := "Choose log name", ""
	if v.cfg.LogName != "" {
		name, nameStyle = sanitize(cloudLogID(v.cfg.LogName)), sgrBold
	}
	nameRoom := cols - b.used - 1 - 2*b.chipPad - 2 - (1 + timeW) - (1 + shareW) - (1 + modeW)
	if cellWidth(name) > nameRoom {
		// A name cut to a few cells can't be told from its neighbours (run.googleapis.com/stderr
		// and /stdout): a tighter layout that shows more of it is preferred.
		complete = complete && nameRoom >= min(cellWidth(name), 28)
		name = clip(name, max(4, nameRoom))
	}
	chip(name+" ▼", nameStyle, v.openLogNames)
	v.anchors["time"] = [2]int{b.used + 2, y}
	chip(timeLabel, "", func() { v.openTimeMenu(v.anchor("time")) })

	// The loading mark and the status message, at the right of the room left before share.
	room := cols - b.used - 1 - (1 + shareW) - (1 + modeW)
	status, cells := "", 0
	v.statusInBar = false
	if msg := sanitize(v.stream.StatusMessage); msg != "" && room-cells-1 >= 12 {
		text := clip(msg, room-cells-1)
		if cells > 0 {
			status, cells = status+" ", cells+1
		}
		status, cells = status+sgrRed+text+sgrReset, cells+cellWidth(text)
		v.statusInBar = true
	}
	b.space(room, status, cells)
	v.anchors["share"] = [2]int{b.used + 2, y}
	chip(cloudGlyphShare, "", func() { v.openShareMenu(v.anchor("share")) })

	// The Logs/SQL switch, one segment with a span each.
	if b.fits(modeW) {
		logsStyle, sqlStyle := sgrBold+sgrRev, sgrDim
		if v.mode == cloudModeSQL {
			logsStyle, sqlStyle = sgrDim, sgrBold+sgrRev
		}
		blank := func(style string, n int) string { return style + strings.Repeat(" ", n) + sgrReset }
		edge := blank(logsStyle, len(logs)) + blank(sqlStyle, len(sql))
		if !tall {
			edge = ""
		}
		b.add(modeW, edge, logsStyle+logs+sgrReset+sqlStyle+sql+sgrReset, edge, nil)
		x := b.used - modeW + 1
		b.hits = append(b.hits,
			hit{x0: x, x1: x + len(logs) - 1, act: func() { v.setMode(cloudModeLogs) }},
			hit{x0: x + len(logs), x1: b.used, act: func() { v.setMode(cloudModeSQL) }})
	} else {
		complete = false
	}
	return b, complete
}

func (v *cloudViewer) toolbarSecond(cols int, tall bool, level int) (b *cloudBarRow, complete bool) {
	b = newCloudBarRow(cols, tall, level)
	complete = true
	chip := func(label, style string, act func()) {
		complete = b.chip(label, style, act) && complete
	}
	// The severity chips: one minimum severity, applied at once. A raw filter carries its own.
	q := v.cfg.Query
	style := func(on bool, selected string) string {
		switch {
		case v.rawMode:
			return sgrDim
		case on:
			return selected
		}
		return ""
	}
	var act func()
	if !v.rawMode {
		act = func() { v.severityChip(0) }
	}
	chip("All", style(q.MinSeverity == nil && len(q.SeveritySet) == 0, sgrBold+sgrRev), act)
	for i, sev := range cloudCommonSeverities {
		act = nil
		if !v.rawMode {
			act = func() { v.severityChip(i + 1) }
		}
		on := len(q.SeveritySet) == 0 && q.MinSeverity != nil && *q.MinSeverity == sev
		chip(severityShort(sev), style(on, cloudSeverityBadge(sev)), act)
	}
	filters, filtersStyle := "Filters", ""
	if v.rawMode {
		filters = "URL filter"
	}
	if v.showBar || !q.isEmpty() || v.rawMode {
		filtersStyle = sgrBold + sgrRev
	}
	chip(filters, filtersStyle, v.toggleBar)

	// The search field, then Copy format and Export at the right. The field keeps a useful
	// width before either is added.
	const format, export, minField, maxField = "Copy format", "Export", 12, 40
	wanted := 24 // the field's width before the chips give up their padding
	if level >= 2 {
		wanted = minField
	}
	formatW, exportW := cellWidth(format)+2*b.chipPad, cellWidth(export)+2*b.chipPad
	room := cols - b.used - 1
	showFormat, showExport := room-(1+formatW)-(1+exportW) >= minField, room-(1+exportW) >= minField
	if showFormat {
		room -= 1 + formatW
	}
	if showExport {
		room -= 1 + exportW
	}
	complete = complete && showFormat && showExport && room >= wanted
	if w := min(maxField, room); w >= minField {
		b.field(&v.search, "filter loaded logs…", v.focus == cloudSearch, w, func() { v.focus = cloudSearch })
		room -= w
	}
	if showFormat || showExport {
		b.space(room-1, "", 0)
	}
	if showFormat {
		chip(format, "", v.openFormatEditor)
	}
	if showExport {
		chip(export, "", v.export)
	}
	return b, complete
}

// Column widths of a list row: the HH:mm:ss.SSS clock, the severity badge, and the widest tag.
const cloudTagCells = 24

// renderCloudRow draws one display row of an entry, w cells wide, like the app's CloudCellNSView:
// the first carries the time, the severity badge and the tag; the rest only their line of the
// message, under the message column. indicator marks the row that stands for the lines left out.
func renderCloudRow(e cloudEntry, text string, sub int, indicator bool, w int) string {
	if w <= 0 {
		return ""
	}
	text = sanitize(text)
	color := cloudSeverityText(e.Severity)
	if indicator {
		color = sgrDim
	}
	// A pane too narrow for the columns shows the severity and the message alone.
	tagW := min(cloudTagCells, w/4)
	msgW := w - clockCells - 1 - badgeCells - 1 - tagW - 1
	if tagW < 8 || msgW < 8 {
		lead := "  "
		if sub == 0 {
			lead = severityShort(e.Severity) + " "
		}
		return color + fit(lead+text, w) + sgrReset
	}
	if sub > 0 {
		return strings.Repeat(" ", w-msgW) + color + fit(text, msgW) + sgrReset
	}
	return sgrDim + e.time().Format("15:04:05.000") + sgrReset + " " +
		cloudSeverityBadge(e.Severity) + " " + severityShort(e.Severity) + " " + sgrReset + " " +
		sgrCyan + fit(sanitize(e.tag()), tagW) + sgrReset + " " + color + fit(text, msgW) + sgrReset
}

// emptyText is the app's hint for an empty list.
func (v *cloudViewer) emptyText() string {
	switch {
	case v.stream.IsRunning:
		return "Waiting for logs…"
	case v.total == 0:
		return "Press Start to stream Cloud Logging."
	}
	return "No logs match the search."
}

// showsChoose is whether the empty list offers the log names: none is selected and the session
// is stopped.
func (v *cloudViewer) showsChoose() bool {
	return v.cfg.LogName == "" && !v.stream.IsRunning
}

// centeredSpan puts styled text in the middle of w cells and says which columns it took.
func centeredSpan(text, style string, w int) (row string, x0, x1 int) {
	text = clip(text, w)
	lead := (w - cellWidth(text)) / 2
	return strings.Repeat(" ", lead) + style + text + sgrReset + strings.Repeat(" ", w-lead-cellWidth(text)),
		lead + 1, lead + cellWidth(text)
}

// listColumn draws the list w cells wide and n rows tall, and records the entry on each row.
func (v *cloudViewer) listColumn(w, n int) []string {
	w = max(0, w)
	out := make([]string, 0, n)
	v.rowSeq, v.chooseRow, v.listRows = v.rowSeq[:0], 0, max(1, n)
	blank := strings.Repeat(" ", w)
	list := v.listed()
	if len(list) == 0 {
		hint, _, _ := centeredSpan(v.emptyText(), sgrDim, w)
		lines := []string{hint}
		if v.showsChoose() {
			button, x0, x1 := centeredSpan(" Choose a log name ", sgrRev, w)
			lines = append(lines, blank, button)
			v.chooseX0, v.chooseX1 = x0, x1
		}
		lead := max(0, (n-len(lines))/2)
		for len(out) < lead {
			out = append(out, blank)
		}
		for i, line := range lines {
			if len(out) >= n {
				break
			}
			if i == 2 {
				v.chooseRow = v.listTop + len(out)
			}
			out = append(out, line)
		}
		for len(out) < n {
			out = append(out, blank)
		}
		return out
	}
	v.clampOffset()
	cached, truncated := -1, false
	var display []string
	for _, ref := range v.window(n) {
		if ref.idx >= len(list) {
			continue
		}
		e := list[ref.idx]
		if ref.idx != cached {
			display, truncated = cloudDisplayLines(e.Message)
			cached = ref.idx
		}
		text := ""
		if ref.sub < len(display) {
			text = display[ref.sub]
		}
		row := renderCloudRow(e, text, ref.sub, truncated && ref.sub == len(display)-1, w)
		if v.sqlMarker(e.Seq) { // a SQL divider is never selected
			row = renderCloudMarker(text, w)
		} else if v.selected(e.Seq) { // a selected entry is drawn plain and reversed
			row = sgrRev + stripSGR(row) + sgrReset
		}
		out = append(out, row)
		v.rowSeq = append(v.rowSeq, e.Seq)
	}
	if v.stream.OlderLoading && len(out) > 0 {
		out[0] = sgrDim + fit("Loading older logs…", w) + sgrReset
	}
	for len(out) < n {
		out = append(out, blank)
	}
	return out
}

// statusBar is the app's: a dot for the stream, the counts, and at the right Help and the
// project's title.
func (v *cloudViewer) statusBar(cols int) string {
	if cols < 2 {
		return ""
	}
	dot := sgrDim
	if v.stream.IsRunning {
		dot = sgrGreen
	}
	counts := fmt.Sprintf("%d shown  %d total", len(v.listed()), v.total)
	if dropped := v.feed.dropped(); dropped > 0 {
		counts += fmt.Sprintf("  %d dropped", dropped)
	}
	counts = clip(counts, max(0, cols-2))
	used := 2 + cellWidth(counts)
	name := clip(sanitize(v.projectTitle()), max(0, cols-used-2))
	const help, gap = "Help", 3
	button := ""
	v.helpX0, v.helpX1 = 0, 0
	if cols-used-cellWidth(name)-len(help)-gap >= 2 {
		button = sgrUnder + help + sgrReset + strings.Repeat(" ", gap)
		v.helpX0 = cols - cellWidth(name) - gap - len(help) + 1
		v.helpX1 = v.helpX0 + len(help) - 1
	}
	pad := strings.Repeat(" ", max(0, cols-used-cellWidth(name)-cellWidth(stripSGR(button))))
	return dot + "●" + sgrReset + " " + sgrDim + counts + sgrReset + pad + button + sgrDim + name + sgrReset
}
