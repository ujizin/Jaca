package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// cloudCall is one thing the viewer asked of jacad.
type cloudCall struct {
	method string
	params map[string]any
}

func cloudMethods(calls []cloudCall) []string {
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = c.method
	}
	return out
}

const testLogName = "projects/proj-1/logs/app"

func testCloudState() cloudState {
	template := cloudQueryTemplate{ID: newUUID(), Name: "Errors for a user", Query: newCloudQuery()}
	sev := 500
	template.Query.MinSeverity = &sev
	text := newTextCondition()
	text.Value = "timeout"
	template.Query.TextConditions = []textCondition{text}
	return cloudState{
		Projects: []cloudProject{{
			ProjectID: "proj-1", DisplayName: "Checkout", SelectedLogName: testLogName,
			LabelKeysByLogName:         map[string][]string{testLogName: {"user_id", "tag", "region"}},
			FavoriteLabelKeysByLogName: map[string][]string{testLogName: {"user_id"}},
		}},
		QueryTemplates: []cloudQueryTemplate{template, {ID: newUUID(), Name: "From a URL", Query: newCloudQuery(), RawFilter: `severity>=ERROR`}},
	}
}

// testCloudViewer is a viewer on a pane that has quit (posted results are dropped), with its
// session open and stopped. What it asks of jacad from here on is recorded.
func testCloudViewer(t *testing.T) (*cloudViewer, *[]cloudCall) {
	t.Helper()
	quit := &pane{work: make(chan func()), done: make(chan struct{})}
	close(quit.done)
	v := newCloudViewer(quit, cloudSessionSpec{Config: newCloudStreamConfig("proj-1")}, nil)
	calls := &[]cloudCall{}
	v.call = func(method string, params map[string]any) { *calls = append(*calls, cloudCall{method, params}) }
	v.now = func() time.Time { return time.Date(2026, 10, 7, 12, 0, 0, 0, time.Local) }
	v.begin(testCloudState(), true, nil)
	v.finishOpen(cloudSessionInfo{ID: v.id, State: cloudStreamState{HasMoreOlder: true}}, nil)
	v.finishResync(nil, nil)
	*calls = nil
	return v, calls
}

func cloudTestEntry(seq uint64, severity int, message string) cloudEntry {
	at := time.Date(2026, 10, 7, 12, 0, 1, 250_000_000, time.Local).Add(time.Duration(seq) * time.Second)
	return cloudEntry{
		Seq: seq, InsertID: fmt.Sprint("id-", seq), Timestamp: float64(at.UnixMicro()) / 1e6, Severity: severity,
		LogName: testLogName, LogID: "app", Message: message, PayloadKind: cloudPayloadText,
		Labels: map[string]string{"tag": "auth", "user_id": fmt.Sprint("u", seq)}, Raw: `{"textPayload":"` + message + `"}`,
	}
}

// feedEntries delivers n single-line INFO entries with seqs from..from+n-1 as one live batch.
func feedEntries(v *cloudViewer, from uint64, n int) {
	batch := make([]cloudEntry, n)
	for i := range batch {
		batch[i] = cloudTestEntry(from+uint64(i), 200, fmt.Sprint("line ", from+uint64(i)))
	}
	v.appended(v.feed.apply(batch))
}

func setTermSize(t *testing.T, rows, cols int) {
	t.Helper()
	oldRows, oldCols := termRows, termCols
	t.Cleanup(func() { termRows, termCols = oldRows, oldCols })
	termRows, termCols = rows, cols
}

var cloudSessionSizes = [][2]int{{24, 80}, {30, 100}, {40, 160}, {60, 220}, {12, 50}, {8, 30}, {5, 12}, {50, 61}}

// The session opens the way the daemon needs: the open carries the project's log name, the
// replay is read after it, and gaps reported by events.dropped are filled.
func TestCloudSessionOpensAndResyncs(t *testing.T) {
	quit := &pane{work: make(chan func()), done: make(chan struct{})}
	close(quit.done)
	v := newCloudViewer(quit, cloudSessionSpec{Config: newCloudStreamConfig("proj-1"), AutoStart: true}, nil)
	var calls []cloudCall
	v.call = func(method string, params map[string]any) { calls = append(calls, cloudCall{method, params}) }
	v.begin(testCloudState(), true, nil)
	if got := cloudMethods(calls); !reflect.DeepEqual(got, []string{"cloud.sessions.open"}) {
		t.Fatalf("after subscribing the viewer asked for %v", got)
	}
	open := calls[0].params
	if cfg := open["config"].(cloudStreamConfig); cfg.LogName != testLogName || open["autoStart"] != true || open["id"] != v.id {
		t.Errorf("open params: %+v", open)
	}
	if v.id != strings.ToUpper(v.id) {
		t.Errorf("session id %q isn't uppercase", v.id)
	}
	calls = nil
	v.finishOpen(cloudSessionInfo{ID: v.id, State: cloudStreamState{IsRunning: true, IsLoading: true, HasMoreOlder: true}}, nil)
	if got := cloudMethods(calls); !reflect.DeepEqual(got, []string{"cloud.sessions.range"}) {
		t.Fatalf("after the open the viewer asked for %v", got)
	}
	if _, has := calls[0].params["afterSeq"]; has {
		t.Error("the first backfill passed afterSeq")
	}
	// A live batch that lands during the backfill waits for it.
	entries, _, state := cloudTopics(v.id)
	live, _ := json.Marshal([]cloudEntry{cloudTestEntry(12, 200, "live")})
	v.handleEvent(event{Topic: entries, Data: live})
	if len(v.visible) != 0 {
		t.Error("a batch was shown while the backfill was in flight")
	}
	v.finishResync([]cloudEntry{cloudTestEntry(10, 200, "a"), cloudTestEntry(11, 200, "b")}, nil)
	if got := len(v.visible); got != 3 || v.total != 3 {
		t.Fatalf("%d entries visible, %d total, want 3", got, v.total)
	}

	calls = nil
	note, _ := json.Marshal(droppedNote{Topic: entries, Count: 4})
	v.handleEvent(event{Topic: "events.dropped", Data: note})
	if len(calls) != 1 || calls[0].method != "cloud.sessions.range" || calls[0].params["afterSeq"] != uint64(12) {
		t.Errorf("dropped entries asked for %+v", calls)
	}
	v.finishResync(nil, fmt.Errorf("no reply"))
	calls = nil
	note, _ = json.Marshal(droppedNote{Topic: state, Count: 1})
	v.handleEvent(event{Topic: "events.dropped", Data: note})
	if got := cloudMethods(calls); !reflect.DeepEqual(got, []string{"cloud.sessions.open"}) {
		t.Errorf("a dropped state asked for %v", got)
	}
	// The state topic drives the toolbar and the status bar.
	st, _ := json.Marshal(cloudStreamState{IsRunning: true, StatusMessage: "Permission denied", HasMoreOlder: true})
	v.handleEvent(event{Topic: state, Data: st})
	if !v.stream.IsRunning || v.stream.StatusMessage != "Permission denied" {
		t.Errorf("stream state: %+v", v.stream)
	}
}

// Changing what gcloud is asked restarts a running session (stop first, since the daemon
// ignores a start while it runs) and only keeps the config for a stopped one.
func TestCloudSessionApply(t *testing.T) {
	v, calls := testCloudViewer(t)
	v.setRange(cloudTimeRange{Minutes: 60})
	if len(*calls) != 0 || v.cfg.TimeRange.Minutes != 60 {
		t.Fatalf("a stopped session: asked for %v, range %+v", cloudMethods(*calls), v.cfg.TimeRange)
	}
	v.setStream(cloudStreamState{IsRunning: true, HasMoreOlder: true})
	feedEntries(v, 1, 5)
	v.setRange(cloudTimeRange{Minutes: 5})
	want := []string{"cloud.sessions.stop", "cloud.sessions.resetScrollback", "cloud.sessions.start"}
	if got := cloudMethods(*calls); !reflect.DeepEqual(got, want) {
		t.Fatalf("a running session asked for %v, want %v", got, want)
	}
	if cfg := (*calls)[2].params["config"].(cloudStreamConfig); cfg.TimeRange.Minutes != 5 || cfg.LogName != testLogName {
		t.Errorf("restarted with %+v", cfg)
	}
	if len(v.visible) != 0 || v.total != 0 || len(v.feed.entries()) != 0 {
		t.Error("the restart kept the old entries")
	}
	if !v.active() {
		t.Error("the session reads as stopped after a restart")
	}

	// A session that finished by itself still has isRunning set: it is stopped before it starts.
	v, calls = testCloudViewer(t)
	v.setStream(cloudStreamState{IsRunning: true, StatusMessage: "Permission denied", HasMoreOlder: true})
	v.handleKey([]byte("p"))
	v.finishCall("cloud.sessions.stop", nil)
	v.setStream(cloudStreamState{HasMoreOlder: true})
	v.handleKey([]byte("p"))
	if got := cloudMethods(*calls); !reflect.DeepEqual(got, []string{"cloud.sessions.stop", "cloud.sessions.start"}) {
		t.Errorf("stop then start asked for %v", got)
	}

	// The log name is the project's: when another pane changes it a running session re-targets.
	v, calls = testCloudViewer(t)
	v.setStream(cloudStreamState{IsRunning: true, HasMoreOlder: true})
	st := testCloudState()
	st.Projects[0].SelectedLogName = "projects/proj-1/logs/other"
	v.setCloudState(st)
	if got := cloudMethods(*calls); !reflect.DeepEqual(got, want) || (*calls)[2].params["config"].(cloudStreamConfig).LogName != "projects/proj-1/logs/other" {
		t.Errorf("a new log name asked for %v", got)
	}
	*calls = nil
	v.setCloudState(st)
	if len(*calls) != 0 {
		t.Errorf("an unchanged log name asked for %v", cloudMethods(*calls))
	}

	// Clear empties the view and the daemon's replay, and leaves the stream alone.
	v, calls = testCloudViewer(t)
	feedEntries(v, 1, 3)
	v.handleKey([]byte("c"))
	if got := cloudMethods(*calls); !reflect.DeepEqual(got, []string{"cloud.sessions.resetScrollback"}) || len(v.visible) != 0 {
		t.Errorf("clear asked for %v and left %d entries", got, len(v.visible))
	}
}

func TestCloudSessionSeverityChips(t *testing.T) {
	v, calls := testCloudViewer(t)
	v.setStream(cloudStreamState{IsRunning: true, HasMoreOlder: true})
	v.handleKey([]byte("4")) // All D I W
	if q := v.cfg.Query; q.MinSeverity == nil || *q.MinSeverity != 400 || len(q.SeveritySet) != 0 {
		t.Fatalf("chip 4 set %+v", q)
	}
	if got := cloudMethods(*calls); len(got) != 3 || got[2] != "cloud.sessions.start" {
		t.Errorf("a chip on a running session asked for %v", got)
	}
	v.cfg.Query.SeveritySet = []int{500}
	v.handleKey([]byte("1"))
	if q := v.cfg.Query; q.MinSeverity != nil || len(q.SeveritySet) != 0 {
		t.Errorf("All left %+v", q)
	}
	// A click on the toolbar's chip does the same.
	setTermSize(t, 40, 160)
	v.frame(40, 160)
	plain := stripSGR(v.toolbar(160, true)[4])
	x := cellWidth(plain[:strings.Index(plain, " E ")]) + 2
	v.mouse(mouseEvent{button: 0, x: x, y: 5, press: true})
	if q := v.cfg.Query; q.MinSeverity == nil || *q.MinSeverity != 500 {
		t.Errorf("a click on E set %+v", q)
	}
	// A raw filter carries its own severity: the chips are off.
	*calls = nil
	v.setRaw(true, "severity>=ERROR")
	v.handleKey([]byte("2"))
	if q := v.cfg.Query; *q.MinSeverity != 500 || len(*calls) != 0 {
		t.Errorf("a chip changed the query under a raw filter: %+v", q)
	}
}

// Multi-line messages take one display row per line, capped, with the app's last-row text.
func TestCloudSessionDisplayRows(t *testing.T) {
	for message, want := range map[string]int{"": 1, "one": 1, "a\nb": 2, "a\n": 1, "a\r\nb\r\n": 2, "a\n\n": 2, "\n": 1} {
		lines, _ := cloudDisplayLines(message)
		if got := cloudLineCount(message); got != want || len(lines) != want {
			t.Errorf("%q: %d rows counted, %d drawn, want %d", message, got, len(lines), want)
		}
	}
	long := strings.TrimSuffix(strings.Repeat("x\n", 450), "\n")
	lines, truncated := cloudDisplayLines(long)
	if cloudLineCount(long) != 200 || len(lines) != 200 || !truncated {
		t.Fatalf("450 lines: %d counted, %d drawn, truncated %v", cloudLineCount(long), len(lines), truncated)
	}
	if lines[199] != "… 251 more lines — ⌘C copies all" {
		t.Errorf("last row %q", lines[199])
	}
	if lines, truncated := cloudDisplayLines(strings.Repeat("x\n", 200)); len(lines) != 200 || truncated {
		t.Errorf("200 lines: %d drawn, truncated %v", len(lines), truncated)
	}

	v, _ := testCloudViewer(t)
	setTermSize(t, 30, 120)
	v.appended(v.feed.apply([]cloudEntry{
		cloudTestEntry(1, 200, "first"),
		cloudTestEntry(2, 500, "boom\n\tat main.go:12\n\tat run.go:7"),
		cloudTestEntry(3, 400, long),
	}))
	if v.totalRows != 1+3+200 {
		t.Fatalf("%d display rows, want 204", v.totalRows)
	}
	v.follow, v.offset = false, 198
	list := v.listColumn(120, 6)
	text := func(i int) string { return strings.TrimRight(stripSGR(list[i]), " ") }
	if !strings.HasSuffix(text(0), "first") || !strings.Contains(text(1), " E ") || !strings.HasSuffix(text(1), "boom") {
		t.Errorf("rows 0 and 1: %q, %q", text(0), text(1))
	}
	// A continuation row is only its line, under the message column, its tab four spaces.
	messageX := strings.Index(text(1), "boom")
	if got := text(2); strings.TrimLeft(got, " ") != "at main.go:12" || len(got)-len("at main.go:12") != messageX+4 {
		t.Errorf("continuation row %q, message column %d", got, messageX)
	}
	if got := v.rowSeq; !reflect.DeepEqual(got, []uint64{1, 2, 2, 2, 3, 3}) {
		t.Errorf("rows map to entries %v", got)
	}
	v.offset = 0
	list = v.listColumn(120, 6)
	if got := strings.TrimSpace(stripSGR(list[5])); got != "… 251 more lines — ⌘C copies all" {
		t.Errorf("the capped entry ends with %q", got)
	}
}

// The view follows the tail until it is scrolled up, holds still while off, and follows again
// at the bottom.
func TestCloudSessionFollowTail(t *testing.T) {
	v, _ := testCloudViewer(t)
	setTermSize(t, 30, 100)
	v.frame(30, 100)
	room := v.room()
	feedEntries(v, 1, 100)
	if !v.follow || v.offset != 0 {
		t.Fatalf("a new viewer: follow %v, offset %d", v.follow, v.offset)
	}
	v.handleKey([]byte("k"))
	if v.follow || v.offset != 1 {
		t.Fatalf("after one row up: follow %v, offset %d", v.follow, v.offset)
	}
	feedEntries(v, 101, 10)
	if v.offset != 11 {
		t.Errorf("the view moved while scrolled up: offset %d, want 11", v.offset)
	}
	first := v.window(room)[0]
	v.handleKey([]byte("\x1b[6~"))
	if !v.follow || v.offset != 0 {
		t.Errorf("back at the bottom: follow %v, offset %d", v.follow, v.offset)
	}
	if again := v.window(room)[0]; again.idx <= first.idx {
		t.Errorf("page down didn't move the window: %+v then %+v", first, again)
	}
	// The toolbar's button turns it off in place, and G turns it back on.
	v.handleKey([]byte("f"))
	if v.follow {
		t.Error("f left follow on")
	}
	feedEntries(v, 111, 5)
	if v.offset != 5 {
		t.Errorf("with follow off at the bottom the view moved: offset %d", v.offset)
	}
	v.handleKey([]byte("G"))
	if !v.follow || v.offset != 0 {
		t.Errorf("G: follow %v, offset %d", v.follow, v.offset)
	}
	// Scrolling past the top stops at the first row.
	v.scroll(10_000)
	if v.topRow() != 0 || v.offset != v.totalRows-room {
		t.Errorf("at the top: first row %d, offset %d", v.topRow(), v.offset)
	}
}

// Older pages are asked for near the top, once per page, and a page that is prepended leaves
// the view on the entries it showed.
func TestCloudSessionLoadOlder(t *testing.T) {
	v, calls := testCloudViewer(t)
	setTermSize(t, 30, 100)
	v.frame(30, 100)
	older := func() int {
		n := 0
		for _, c := range *calls {
			if c.method == "cloud.sessions.loadOlder" {
				n++
			}
		}
		return n
	}
	v.scroll(1)
	if older() != 0 {
		t.Fatal("asked for older logs with nothing loaded")
	}
	const base = 1 << 40
	feedEntries(v, base, 100)
	v.scroll(10)
	if older() != 0 {
		t.Fatal("asked for older logs far from the top")
	}
	v.scroll(v.totalRows)
	if older() != 1 {
		t.Fatalf("at the top: %d requests, want 1", older())
	}
	// No progress: the page is still in flight, failed, or held only entries already loaded.
	v.scroll(1)
	v.setStream(cloudStreamState{OlderLoading: true, HasMoreOlder: true})
	v.scroll(1)
	v.setStream(cloudStreamState{HasMoreOlder: true})
	v.scroll(1)
	if older() != 1 {
		t.Fatalf("asked again without progress: %d requests", older())
	}
	// Leaving the top and coming back doesn't ask again; a while later scrolling does (the
	// read may have failed, which publishes nothing).
	v.scroll(-20)
	v.scroll(20)
	if older() != 1 {
		t.Fatalf("coming back to the top asked again: %d requests", older())
	}
	at := v.now()
	v.now = func() time.Time { return at.Add(cloudOlderRetry) }
	v.scroll(1)
	if older() != 2 {
		t.Fatalf("after the retry time: %d requests, want 2", older())
	}
	// A page arrives: the view stays on the same entries, and the next page can be asked for.
	room := v.room()
	before := v.listed()[v.window(room)[0].idx].Seq
	page := make([]cloudEntry, 50)
	for i := range page {
		page[i] = cloudTestEntry(base-50+uint64(i), 200, "older")
	}
	_, olderTopic, _ := cloudTopics(v.id)
	data, _ := json.Marshal(page)
	v.handleEvent(event{Topic: olderTopic, Data: data})
	if len(v.visible) != 150 || v.total != 150 || v.visible[0].Seq != base-50 {
		t.Fatalf("after the page: %d visible, %d total", len(v.visible), v.total)
	}
	if after := v.listed()[v.window(room)[0].idx].Seq; after != before {
		t.Errorf("the view moved from entry %d to %d when the page was prepended", before, after)
	}
	v.scroll(v.totalRows)
	if older() != 3 {
		t.Fatalf("after a page: %d requests, want 3", older())
	}
	// The other guards: a page in flight, and nothing older left.
	v.olderAsked = false
	v.setStream(cloudStreamState{OlderLoading: true, HasMoreOlder: true})
	v.scroll(1)
	v.setStream(cloudStreamState{HasMoreOlder: false})
	v.scroll(1)
	if older() != 3 {
		t.Errorf("asked while loading or with nothing older: %d requests", older())
	}
	if list := v.listColumn(100, 5); !strings.Contains(stripSGR(list[0]), "older") {
		t.Errorf("first row %q", stripSGR(list[0]))
	}
	v.setStream(cloudStreamState{OlderLoading: true, HasMoreOlder: true})
	if list := v.listColumn(100, 5); strings.TrimSpace(stripSGR(list[0])) != "Loading older logs…" {
		t.Errorf("while loading the first row reads %q", stripSGR(list[0]))
	}
}

func TestCloudSessionSearchAndStatusBar(t *testing.T) {
	v, _ := testCloudViewer(t)
	v.appended(v.feed.apply([]cloudEntry{
		cloudTestEntry(1, 200, "user signed in"),
		cloudTestEntry(2, 500, "payment FAILED"),
		cloudTestEntry(3, 200, "user signed out"),
	}))
	bar := func() string { return stripSGR(v.statusBar(100)) }
	if got := bar(); !strings.HasPrefix(got, "● 3 shown  3 total ") || strings.Contains(got, "dropped") || !strings.HasSuffix(got, "Help   Checkout") {
		t.Errorf("status bar %q", got)
	}
	v.handleKey([]byte("/"))
	for _, r := range "failed" {
		v.handleKey([]byte(string(r)))
	}
	if len(v.visible) != 1 || v.visible[0].Seq != 2 {
		t.Fatalf("search kept %d entries", len(v.visible))
	}
	// Entries that arrive later are filtered too, and count toward the total.
	v.appended(v.feed.apply([]cloudEntry{cloudTestEntry(4, 200, "ok"), cloudTestEntry(5, 500, "upload failed")}))
	if got := bar(); !strings.HasPrefix(got, "● 2 shown  5 total ") {
		t.Errorf("status bar %q", got)
	}
	// The search also reads the severity's name and the labels.
	v.search.set("u3")
	v.refilter()
	if len(v.visible) != 1 || v.visible[0].Seq != 3 {
		t.Errorf("search by label value kept %d entries", len(v.visible))
	}
	v.handleKey([]byte{'\r'})
	if v.focus != cloudList {
		t.Error("Enter left the search field focused")
	}
	// Entries the feed trims off its front are counted as dropped.
	small, _ := testCloudViewer(t)
	small.feed = newCloudFeed(10)
	feedEntries(small, 1, 25)
	if got := stripSGR(small.statusBar(100)); !strings.HasPrefix(got, "● 10 shown  25 total  15 dropped ") {
		t.Errorf("status bar after a trim %q", got)
	}
	if small.visible[0].Seq != 16 || small.totalRows != 10 {
		t.Errorf("after a trim the list starts at %d with %d rows", small.visible[0].Seq, small.totalRows)
	}
}

func TestCloudSessionEmptyStates(t *testing.T) {
	v, _ := testCloudViewer(t)
	setTermSize(t, 30, 100)
	shown := func() string { return stripSGR(strings.Join(v.frame(30, 100), "\n")) }
	if got := v.emptyText(); got != "Press Start to stream Cloud Logging." || !strings.Contains(shown(), got) {
		t.Errorf("stopped with nothing received: %q", got)
	}
	if strings.Contains(shown(), "Choose a log name") {
		t.Error("the log names are offered with a log name selected")
	}
	v.cfg.LogName = ""
	if !strings.Contains(shown(), " Choose a log name ") || v.chooseRow == 0 {
		t.Error("no Choose a log name button without a log name")
	}
	// The button and the toolbar's selector open the sheet, once one is wired in.
	opened := 0
	openLogNameSheet = func(_ *pane, project cloudProject, done func()) cloudSheet {
		opened++
		return v.newForm(project.title(), nil, nil, []string{"Cancel"}, 0, 0, nil)
	}
	t.Cleanup(func() { openLogNameSheet = nil })
	v.mouse(mouseEvent{button: 0, x: (v.chooseX0 + v.chooseX1) / 2, y: v.chooseRow, press: true})
	if opened != 1 || v.sheet == nil {
		t.Fatalf("a click on the button opened %d sheets", opened)
	}
	v.handleKey([]byte{0x1b})
	if v.sheet != nil {
		t.Error("Esc left the sheet open")
	}
	openLogNameSheet = nil
	v.handleKey([]byte("l"))
	if v.sheet != nil {
		t.Error("the selector opened a sheet with none wired in")
	}

	v.setStream(cloudStreamState{IsRunning: true, HasMoreOlder: true})
	if got := v.emptyText(); got != "Waiting for logs…" || !strings.Contains(shown(), got) {
		t.Errorf("running: %q", got)
	}
	if strings.Contains(shown(), "Choose a log name") {
		t.Error("the log names are offered while running")
	}
	v.setStream(cloudStreamState{HasMoreOlder: true})
	feedEntries(v, 1, 3)
	v.search.set("nothing has this")
	v.refilter()
	if got := v.emptyText(); got != "No logs match the search." || !strings.Contains(shown(), got) {
		t.Errorf("searched to nothing: %q", got)
	}
}

// The detail panel's label and severity actions edit the query through the core helpers and
// restart, or open a new session with the fork's name.
func TestCloudSessionDetailActions(t *testing.T) {
	copied := captureClipboard(t)
	t.Setenv("HERDR_ENV", "1") // a fork opens a tab; this session stays
	var forks []cloudSessionSpec
	real := launchCloudFork
	launchCloudFork = func(_ *pane, spec cloudSessionSpec, _ func(), _ func(error), _ func()) { forks = append(forks, spec) }
	t.Cleanup(func() { launchCloudFork = real })

	v, calls := testCloudViewer(t)
	setTermSize(t, 40, 160)
	e := cloudTestEntry(7, 500, "payment failed\nretrying")
	e.ResourceType, e.ResourceLabels = "k8s_container", map[string]string{"cluster": "prod"}
	e.Trace, e.HTTPRequestSummary = "trace-1", "GET /pay 500"
	v.appended(v.feed.apply([]cloudEntry{cloudTestEntry(6, 200, "before"), e}))
	v.setStream(cloudStreamState{IsRunning: true, HasMoreOlder: true})
	v.frame(40, 160)
	v.handleKey([]byte{'\r'})
	if !v.detailOn || v.detail.Seq != 7 || v.focus != cloudDetail {
		t.Fatalf("Enter opened entry %d (open %v)", v.detail.Seq, v.detailOn)
	}
	v.frame(40, 160)
	lines, rows, _ := v.detailContent(80)
	text := stripSGR(strings.Join(lines, "\n"))
	order := []string{"MESSAGE", "payment failed", "SEVERITY", "ERROR", "TIME", "LABELS", "user_id", "RESOURCE LABELS", "cluster", "LOG", "RESOURCE TYPE", "HTTP REQUEST", "TRACE", "INSERT ID", "RAW JSON"}
	at := 0
	for _, part := range order {
		next := strings.Index(text[at:], part)
		if next < 0 {
			t.Fatalf("the panel has no %q after offset %d:\n%s", part, at, text)
		}
		at += next
	}
	if strings.Contains(text, "SPAN") || strings.Contains(text, "RECEIVED") || strings.Contains(text, "retrying") {
		t.Errorf("the panel shows a missing field or the folded message:\n%s", text)
	}
	// The favorite key is first, with its star.
	if star, tag := strings.Index(text, "★ user_id"), strings.Index(text, "tag "); star < 0 || star > tag {
		t.Errorf("favorite label not pinned:\n%s", text)
	}
	find := func(label string) cloudDetailRow {
		t.Helper()
		for _, row := range rows {
			if strings.Contains(stripSGR(lines[row.first]), label) {
				return row
			}
		}
		t.Fatalf("no row with %q", label)
		return cloudDetailRow{}
	}
	labels := func(row cloudDetailRow) []string {
		var out []string
		for _, item := range row.menu() {
			out = append(out, item.label)
		}
		return out
	}
	run := func(row cloudDetailRow, label string) {
		t.Helper()
		for _, item := range row.menu() {
			if item.label == label {
				item.act()
				return
			}
		}
		t.Fatalf("no item %q in %q", label, labels(row))
	}
	user := find("user_id")
	want := []string{"Copy value", "Copy “user_id=u7”", "", "Filter by this value", "Add this value (OR)", "Open in new session", "", "Unfavorite “user_id”"}
	if got := labels(user); !reflect.DeepEqual(got, want) {
		t.Errorf("label menu %q", got)
	}
	if got := labels(find("tag ")); got[len(got)-1] != "Favorite “tag” (pin to top)" {
		t.Errorf("a key that isn't a favorite offers %q", got[len(got)-1])
	}

	restart := []string{"cloud.sessions.stop", "cloud.sessions.resetScrollback", "cloud.sessions.start"}
	run(user, "Filter by this value")
	if got := buildFilter("", "", v.cfg.Query, ""); got != `labels.user_id="u7"` {
		t.Errorf("Filter by this value built %q", got)
	}
	if got := cloudMethods(*calls); !reflect.DeepEqual(got, restart) {
		t.Errorf("Filter by this value asked for %v", got)
	}
	if v.detailOn {
		t.Error("the panel stayed open on an entry the restart cleared")
	}
	run(user, "Add this value (OR)")
	run(find("cluster"), "Add this value (OR)")
	if got := buildFilter("", "", v.cfg.Query, ""); got != `(labels.user_id="u7" OR labels.user_id="u7" OR resource.labels.cluster="prod")` {
		t.Errorf("Add this value (OR) built %q", got)
	}
	run(user, "Filter by this value")
	if got := len(v.cfg.Query.LabelConditions); got != 2 {
		t.Errorf("Filter by this value left %d label conditions, want 2", got)
	}

	*calls = nil
	run(user, "Unfavorite “user_id”")
	if c := (*calls)[0]; c.method != "cloud.toggleFavoriteLabel" || c.params["project"] != "proj-1" || c.params["logName"] != testLogName || c.params["key"] != "user_id" {
		t.Errorf("toggling a favorite asked for %+v", c)
	}

	severity := find("ERROR")
	if got := labels(severity); !reflect.DeepEqual(got, []string{"Copy", "Filter by Error", "Open in new session (Error)"}) {
		t.Errorf("severity menu %q", got)
	}
	run(severity, "Filter by Error")
	if !reflect.DeepEqual(v.cfg.Query.SeveritySet, []int{500}) {
		t.Errorf("Filter by Error set %v", v.cfg.Query.SeveritySet)
	}

	// A fork keeps this session's filter, adds the value, and starts at once under its name.
	v.cfg.Query = newCloudQuery()
	run(user, "Open in new session")
	run(severity, "Open in new session (Error)")
	if len(forks) != 2 {
		t.Fatalf("%d forks opened, want 2", len(forks))
	}
	if f := forks[0]; f.Name != "user_id=u7" || !f.AutoStart || f.Config.ProjectID != "proj-1" || f.Config.LogName != testLogName ||
		buildFilter("", "", f.Config.Query, "") != `labels.user_id="u7"` {
		t.Errorf("label fork %+v", f)
	}
	if f := forks[1]; f.Name != "Error" || !reflect.DeepEqual(f.Config.Query.SeveritySet, []int{500}) {
		t.Errorf("severity fork %+v", f)
	}
	if len(v.cfg.Query.LabelConditions) != 0 {
		t.Error("a fork changed this session's query")
	}

	run(user, "Copy “user_id=u7”")
	select {
	case got := <-copied:
		if got != "user_id=u7" {
			t.Errorf("copied %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("nothing copied")
	}
}

// The panel is driven by the keys: rows, their menu, the disclosures, the neighboring entries.
func TestCloudSessionDetailKeys(t *testing.T) {
	captureClipboard(t) // a click on an entry copies it
	v, _ := testCloudViewer(t)
	setTermSize(t, 40, 160)
	v.appended(v.feed.apply([]cloudEntry{cloudTestEntry(1, 200, "one"), cloudTestEntry(2, 400, "two\nmore"), cloudTestEntry(3, 500, "three")}))
	v.frame(40, 160)
	v.handleKey([]byte{'\r'})
	v.frame(40, 160)
	if v.detail.Seq != 3 {
		t.Fatalf("Enter opened entry %d, want the newest", v.detail.Seq)
	}
	v.handleKey([]byte("["))
	if v.detail.Seq != 2 || !v.sel.has(2) || v.sel.has(3) {
		t.Fatalf("[ moved to entry %d", v.detail.Seq)
	}
	v.frame(40, 160)
	// Enter on MESSAGE opens it; the content row below then offers Copy.
	v.handleKey([]byte{'\r'})
	if !v.showMessage {
		t.Fatal("Enter on MESSAGE didn't open it")
	}
	if text := stripSGR(strings.Join(v.frame(40, 160), "\n")); !strings.Contains(text, "more") {
		t.Error("the opened message doesn't show its second line")
	}
	v.handleKey([]byte("j"))
	v.frame(40, 160)
	v.handleKey([]byte{'\r'})
	if v.menu == nil || v.menu.items[0].label != "Copy" {
		t.Fatalf("Enter on the message opened %+v", v.menu)
	}
	if box := v.menu.box(40, 160); len(box) == 0 || v.menu.left < v.detailX0 {
		t.Errorf("the menu opened at column %d, left of the panel at %d", v.menu.left, v.detailX0)
	}
	v.handleKey([]byte{0x1b})
	if v.menu != nil || !v.detailOn {
		t.Error("Esc on the menu didn't just close it")
	}
	v.handleKey([]byte("]"))
	if v.detail.Seq != 3 || v.showMessage {
		t.Errorf("] moved to entry %d (message open %v)", v.detail.Seq, v.showMessage)
	}
	// Tab hands the keys to the list and back; Esc closes the panel and drops the selection.
	v.handleKey([]byte{'\t'})
	if v.focus != cloudList {
		t.Error("Tab left the keys on the panel")
	}
	v.handleKey([]byte{'\t'})
	v.handleKey([]byte{0x1b})
	if v.detailOn || v.sel.on || v.focus != cloudList {
		t.Errorf("Esc: panel open %v, selection %v", v.detailOn, v.sel.on)
	}
	// A click on an entry opens it; a click on the header's close button closes it.
	v.frame(40, 160)
	v.mouse(mouseEvent{button: 0, x: 5, y: v.listTop + 1, press: true})
	v.mouse(mouseEvent{button: 0, x: 5, y: v.listTop + 1, press: false})
	if !v.detailOn || v.detail.Seq != 2 {
		t.Fatalf("a click opened entry %d (open %v)", v.detail.Seq, v.detailOn)
	}
	v.frame(40, 160)
	v.mouse(mouseEvent{button: 2, x: v.detailX0 + 6, y: v.detailFirst + v.detailRows[2].first, press: true})
	if v.menu == nil || v.menu.items[1].label != "Filter by Warning" {
		t.Fatalf("a right click on the severity opened %+v", v.menu)
	}
	v.handleKey([]byte{0x1b})
	v.mouse(mouseEvent{button: 0, x: v.closeX0 + 2, y: v.listTop, press: true})
	if v.detailOn {
		t.Error("the close button left the panel open")
	}
}

// A click selects an entry, a drag or Shift+arrows extends, and the copy is in the app-wide
// copy format with the cloud entry's fields.
func TestCloudSessionSelectionCopies(t *testing.T) {
	copied := captureClipboard(t)
	t.Setenv("HOME", t.TempDir()) // no saved copy format: the default applies
	v, _ := testCloudViewer(t)
	setTermSize(t, 30, 120)
	v.appended(v.feed.apply([]cloudEntry{
		cloudTestEntry(1, 200, "first"),
		cloudTestEntry(2, 500, "boom\n\tat main.go:12"),
		cloudTestEntry(3, 400, "third"),
	}))
	v.frame(30, 120)
	next := func() string {
		t.Helper()
		select {
		case text := <-copied:
			return text
		case <-time.After(2 * time.Second):
			t.Fatal("nothing copied")
			return ""
		}
	}
	// The continuation row belongs to its entry. A click selects it and opens its details
	// without touching the clipboard, as in the app; C copies it.
	v.mouse(mouseEvent{button: 0, x: 40, y: v.listTop + 2, press: true})
	v.mouse(mouseEvent{button: 0, x: 40, y: v.listTop + 2, press: false})
	select {
	case text := <-copied:
		t.Fatalf("a click copied %q", text)
	default:
	}
	v.copySelection()
	if got := next(); got != "12:00:03.250 ERROR auth  boom\n\tat main.go:12" {
		t.Errorf("the selected entry copied as %q", got)
	}
	if v.follow {
		t.Error("the view kept following under a selection")
	}
	v.frame(30, 120)
	v.mouse(mouseEvent{button: 0, x: 3, y: v.listTop, press: true})
	v.mouse(mouseEvent{button: mouseDrag, x: 3, y: v.listTop + 3, press: true})
	v.mouse(mouseEvent{button: 0, x: 3, y: v.listTop + 3, press: false})
	if got := next(); got != "12:00:02.250 INFO auth  first\n12:00:03.250 ERROR auth  boom\n\tat main.go:12\n12:00:04.250 WARNING auth  third" {
		t.Errorf("a drag copied %q", got)
	}
	v.frame(30, 120)
	v.mouse(mouseEvent{button: 2, x: 3, y: v.listTop + 1, press: true})
	if v.menu == nil {
		t.Fatal("a right click opened no menu")
	}
	var labels []string
	for _, item := range v.menu.items {
		labels = append(labels, item.label)
	}
	if !reflect.DeepEqual(labels, []string{"Copy 3 Entries", "Copy Format…", "", "Select All"}) {
		t.Errorf("row menu %q", labels)
	}
	v.handleKey([]byte{0x1b})
	v.handleKey([]byte{0x1b}) // the panel
	if v.sel.on || !v.follow {
		t.Errorf("Esc left the selection (%v) or the view held (%v)", v.sel.on, !v.follow)
	}
	// A right click on an entry outside the selection selects it alone.
	v.frame(30, 120)
	v.mouse(mouseEvent{button: 2, x: 3, y: v.listTop, press: true})
	if v.menu.items[0].label != "Copy Entry" {
		t.Errorf("row menu for one entry starts with %q", v.menu.items[0].label)
	}
	v.runMenuItem(3)
	if got := len(v.selectedEntries()); got != 3 {
		t.Errorf("Select All selected %d entries", got)
	}
	v.handleKey([]byte{0x1b})

	// Shift+Up starts on the newest entry on screen and extends upward; C copies.
	v.frame(30, 120)
	v.handleKey([]byte("\x1b[1;2A"))
	v.handleKey([]byte("\x1b[1;2A"))
	if got := v.selectedEntries(); len(got) != 2 || got[0].Seq != 2 {
		t.Fatalf("Shift+Up twice selected %d entries", len(got))
	}
	v.handleKey([]byte("C"))
	if got := next(); !strings.HasPrefix(got, "12:00:03.250 ERROR") || !strings.HasSuffix(got, "WARNING auth  third") {
		t.Errorf("C copied %q", got)
	}
	// The cloud entry's own tokens, and one it doesn't have.
	e := cloudTestEntry(9, 300, "msg")
	e.Trace, e.Labels = "t-1", nil
	if got := cloudCopyText([]cloudEntry{e}, logCopyFormat{Template: "[{levelShort}] {tag} {logId} {trace} {insertId} {pid} {message}"}); got != "[N] app t-1 id-9 {pid} msg" {
		t.Errorf("custom format gave %q", got)
	}
}

// The builder bar edits the query in place and applies on Apply.
func TestCloudSessionQueryBar(t *testing.T) {
	v, calls := testCloudViewer(t)
	setTermSize(t, 40, 120)
	v.handleKey([]byte("F"))
	if !v.showBar || v.focus != cloudBar || v.barStop.kind != "tadd" {
		t.Fatalf("F: bar %v, stop %+v", v.showBar, v.barStop)
	}
	text := func() string { return stripSGR(strings.Join(v.barLines(120, 30), "\n")) }
	for _, part := range []string{"TEXT PAYLOAD", "Add a text filter", "MIN SEVERITY", "Any   Debug   Info   Warning   Error   Critical", "LABELS", " Apply ", "Reset", "Templates", "Show query"} {
		if !strings.Contains(text(), part) {
			t.Errorf("the bar has no %q:\n%s", part, text())
		}
	}
	typeText := func(s string) {
		for _, r := range s {
			v.handleKey([]byte(string(r)))
		}
	}
	v.handleKey([]byte{'\r'}) // Add a text filter: the new value field takes the keys
	typeText("time out")
	v.handleKey([]byte("\x1b[Z")) // back to its match mode
	v.handleKey([]byte{'\r'})
	v.handleKey([]byte{'\r'})
	if c := v.cfg.Query.TextConditions; len(c) != 1 || c[0].Value != "time out" || c[0].Mode != cloudMatchRegex {
		t.Fatalf("text conditions %+v", c)
	}
	if got := text(); !strings.Contains(got, "matches regex") || !strings.Contains(got, "time out") || !strings.Contains(got, "Add another (OR)") {
		t.Errorf("the bar after one condition:\n%s", got)
	}
	// A second condition shows the combiner between them.
	v.barStop = cloudStop{"tadd", 0}
	v.barActivate()
	typeText("refused")
	if got := text(); !strings.Contains(got, " OR  match any of these") {
		t.Errorf("no combiner between two conditions:\n%s", got)
	}
	v.barStop = cloudStop{"tcomb", 0}
	v.barActivate()
	if got := text(); !strings.Contains(got, " AND  match all of these") || !strings.Contains(got, "Add another (AND)") {
		t.Errorf("after toggling the combiner:\n%s", got)
	}
	// The bar's severity waits for Apply; the footer says what Apply does.
	v.setStream(cloudStreamState{IsRunning: true, HasMoreOlder: true})
	v.barStop = cloudStop{"sev", 4}
	v.barActivate()
	if q := v.cfg.Query; q.MinSeverity == nil || *q.MinSeverity != 500 || len(*calls) != 0 {
		t.Fatalf("the bar's severity chip: %+v, asked for %v", q.MinSeverity, cloudMethods(*calls))
	}
	if !strings.Contains(text(), " Apply (restart) ") {
		t.Error("Apply doesn't say it restarts a running session")
	}
	// A label condition starts on the first detected key, exact; its key comes from the picker.
	v.barStop = cloudStop{"ladd", 0}
	v.barActivate()
	typeText("u1")
	if c := v.cfg.Query.LabelConditions; len(c) != 1 || c[0].Key != "user_id" || c[0].Mode != cloudMatchExact || c[0].Value != "u1" {
		t.Fatalf("label conditions %+v", c)
	}
	v.barStop = cloudStop{"lkey", 0}
	v.barActivate()
	picker, ok := v.sheet.(*cloudKeyPicker)
	if !ok {
		t.Fatalf("the key button opened %T", v.sheet)
	}
	if got := picker.keys(); !reflect.DeepEqual(got, []string{"user_id", "region", "tag"}) {
		t.Errorf("picker keys %v, want the favorite first", got)
	}
	v.handleKey([]byte("r"))
	v.handleKey([]byte("e"))
	if got := picker.keys(); !reflect.DeepEqual(got, []string{"region"}) {
		t.Errorf("searching the picker kept %v", got)
	}
	v.handleKey([]byte{'\t'})
	if c := (*calls)[len(*calls)-1]; c.method != "cloud.toggleFavoriteLabel" || c.params["key"] != "region" {
		t.Errorf("Tab in the picker asked for %+v", c)
	}
	v.handleKey([]byte{'\r'})
	if v.sheet != nil || v.cfg.Query.LabelConditions[0].Key != "region" {
		t.Errorf("Enter in the picker left key %q", v.cfg.Query.LabelConditions[0].Key)
	}
	v.openKeyPicker(v.cfg.Query.LabelConditions[0].ID)
	for _, r := range "zzz" {
		v.handleKey([]byte(string(r)))
	}
	if box, _, _ := v.sheet.box(40, 120); !strings.Contains(stripSGR(strings.Join(box, "\n")), "No labels match “zzz”.") {
		t.Errorf("the picker with no match:\n%s", stripSGR(strings.Join(box, "\n")))
	}
	v.handleKey([]byte{0x1b})

	// Show query is the filter the first query would run, time clause included.
	*calls = nil
	v.barStop = cloudStop{"query", 0}
	v.barActivate()
	shown := strings.Join(strings.Fields(text()), "")
	want := strings.Join(strings.Fields(buildCloudFilter(v.cfg, v.now())), "")
	if !strings.Contains(shown, want) || !strings.Contains(want, "timestamp>=") || !strings.Contains(text(), "Hide query") {
		t.Errorf("Show query shows\n%s\nwant %s", text(), want)
	}
	v.barStop = cloudStop{"apply", 0}
	v.barActivate()
	if got := cloudMethods(*calls); len(got) != 3 || got[0] != "cloud.sessions.stop" || got[2] != "cloud.sessions.start" {
		t.Fatalf("Apply asked for %v", got)
	}
	if cfg := (*calls)[2].params["config"].(cloudStreamConfig); len(cfg.Query.TextConditions) != 2 || *cfg.Query.MinSeverity != 500 {
		t.Errorf("Apply started with %+v", cfg.Query)
	}

	// Templates: a saved one replaces the query (or sets a raw filter) and applies.
	v.barStop = cloudStop{"templates", 0}
	v.barActivate()
	var labels []string
	for _, item := range v.menu.items {
		labels = append(labels, item.label)
	}
	if !reflect.DeepEqual(labels, []string{"Saved", "Errors for a user", "From a URL", "", "Save current as template…"}) || v.menu.selected != 1 {
		t.Fatalf("templates menu %q, selected %d", labels, v.menu.selected)
	}
	v.runMenuItem(0) // the heading does nothing
	if v.menu == nil {
		t.Fatal("the heading closed the menu")
	}
	v.handleKey([]byte{'\r'})
	if q := v.cfg.Query; len(q.TextConditions) != 1 || q.TextConditions[0].Value != "timeout" || len(q.LabelConditions) != 0 || v.rawMode {
		t.Errorf("after a template: %+v", q)
	}
	v.cfg.Query.TextConditions[0].Value = "edited"
	if v.templates[0].Query.TextConditions[0].Value != "timeout" {
		t.Error("editing the query changed the saved template")
	}
	v.applyTemplate(v.templates[1])
	if !v.rawMode || v.cfg.RawFilter != "severity>=ERROR" || v.raw.String() != "severity>=ERROR" {
		t.Errorf("after a raw template: raw %v, filter %q", v.rawMode, v.cfg.RawFilter)
	}
	if got := text(); !strings.Contains(got, "RAW FILTER (from URL)") || !strings.Contains(got, "Use builder instead") || strings.Contains(got, "TEXT PAYLOAD") {
		t.Errorf("the bar under a raw filter:\n%s", got)
	}
	if plain := stripSGR(v.toolbar(120, true)[4]); !strings.Contains(plain, "URL filter") {
		t.Errorf("the toolbar under a raw filter: %q", plain)
	}
	// Typing in the raw filter edits what the next start sends.
	v.barStop = cloudStop{"raw", 0}
	typeText(" AND x")
	if !strings.Contains(v.cfg.RawFilter, " AND x") {
		t.Errorf("the raw filter after typing: %q", v.cfg.RawFilter)
	}

	// Save current as template…
	*calls = nil
	v.openSaveTemplate()
	typeText("Mine")
	v.handleKey([]byte{'\r'})
	if c := (*calls)[0]; v.sheet != nil || c.method != "cloud.saveQueryTemplate" || c.params["name"] != "Mine" || c.params["rawFilter"] != v.cfg.RawFilter {
		t.Errorf("Save asked for %+v", c)
	}
	// Reset empties both and applies.
	*calls = nil
	v.barStop = cloudStop{"reset", 0}
	v.barActivate()
	if v.rawMode || v.cfg.RawFilter != "" || !v.cfg.Query.isEmpty() || len(*calls) != 3 {
		t.Errorf("Reset left raw %v, query %+v, asked for %v", v.rawMode, v.cfg.Query, cloudMethods(*calls))
	}
	if got := text(); strings.Contains(got, "severity") || strings.Contains(got, "textPayload") || !strings.Contains(got, "timestamp>=") {
		t.Errorf("the query view after Reset:\n%s", text())
	}
	v.handleKey([]byte{0x1b})
	if v.focus != cloudList || !v.showBar {
		t.Error("Esc didn't hand the keys back to the list with the bar open")
	}
	// Without detected labels the bar says so, and the add control does nothing.
	v.project.LabelKeysByLogName = nil
	if got := strings.Join(strings.Fields(text()), " "); !strings.Contains(got, "No labels detected yet — run a session once so Jaca can auto-detect this log's label keys.") {
		t.Errorf("the bar without label keys:\n%s", got)
	}
	v.openKeyPicker("")
	box, _, _ := v.sheet.box(40, 120)
	if got := strings.Join(strings.Fields(strings.ReplaceAll(stripSGR(strings.Join(box, " ")), "│", "")), " "); !strings.Contains(got, "No labels detected yet — run a session once.") {
		t.Error("the picker without label keys")
	}
}

func TestCloudSessionTimeRange(t *testing.T) {
	v, calls := testCloudViewer(t)
	setTermSize(t, 40, 120)
	v.frame(40, 120)
	v.handleKey([]byte("t"))
	var labels []string
	for _, item := range v.menu.items {
		labels = append(labels, item.label)
	}
	if !reflect.DeepEqual(labels, []string{"Last 5m", "Last 15m", "Last 1h", "Last 6h", "Last 24h", "Last 7d", "", "Custom minutes…", "Absolute range…"}) {
		t.Fatalf("time menu %q", labels)
	}
	v.runMenuItem(3)
	if v.cfg.TimeRange.Minutes != 360 || !strings.Contains(stripSGR(v.toolbar(120, true)[1]), "Last 6h") {
		t.Errorf("after Last 6h: %+v", v.cfg.TimeRange)
	}
	typeText := func(s string) {
		for _, r := range s {
			v.handleKey([]byte(string(r)))
		}
	}
	// Custom minutes: prefilled, and anything but a positive whole number is ignored.
	for input, want := range map[string]int{"": 15, "0": 360, "abc": 360, "-4": 360, "90": 90} {
		v.cfg.TimeRange = cloudTimeRange{Minutes: 360}
		v.handleKey([]byte("t"))
		v.runMenuItem(7)
		form, ok := v.sheet.(*cloudForm)
		if !ok || form.value(0) != "15" {
			t.Fatalf("Custom minutes opened %T", v.sheet)
		}
		if input != "" {
			v.handleKey([]byte{0x15})
			typeText(input)
		}
		v.handleKey([]byte{'\r'})
		if v.sheet != nil || v.cfg.TimeRange.Minutes != want {
			t.Errorf("minutes %q: range %+v, want %d", input, v.cfg.TimeRange, want)
		}
	}
	// Absolute range: local times, defaulting to the last hour; a bad one keeps the form open.
	v.cfg.TimeRange = cloudTimeRange{Minutes: 90}
	v.openAbsoluteRange()
	form := v.sheet.(*cloudForm)
	if form.value(0) != "2026-10-07 11:00" || form.value(1) != "2026-10-07 12:00" {
		t.Errorf("absolute range defaults %q, %q", form.value(0), form.value(1))
	}
	v.handleKey([]byte{0x15})
	typeText("yesterday")
	v.handleKey([]byte{'\r'})
	if v.sheet == nil || !form.bad[0] || v.cfg.TimeRange.Minutes != 90 {
		t.Fatal("a start that isn't a time was applied")
	}
	v.handleKey([]byte{0x15})
	typeText("2026-10-06 08:30")
	v.setStream(cloudStreamState{IsRunning: true, HasMoreOlder: true})
	*calls = nil
	v.handleKey([]byte{'\r'})
	r := v.cfg.TimeRange
	if v.sheet != nil || r.isLive() || !r.Start.Equal(time.Date(2026, 10, 6, 8, 30, 0, 0, time.Local)) || !r.End.Equal(time.Date(2026, 10, 7, 12, 0, 0, 0, time.Local)) {
		t.Errorf("absolute range %+v", r)
	}
	if got := cloudMethods(*calls); len(got) != 3 {
		t.Errorf("an absolute range on a running session asked for %v", got)
	}
	if !strings.Contains(stripSGR(v.toolbar(120, true)[1]), "Custom range") {
		t.Error("the toolbar doesn't name the absolute range")
	}
	// Cancel changes nothing.
	v.openAbsoluteRange()
	v.handleKey([]byte{0x1b})
	if v.sheet != nil || v.cfg.TimeRange != r {
		t.Error("Esc on the form changed the range")
	}
}

func TestCloudSessionToolbarAndShare(t *testing.T) {
	copied := captureClipboard(t)
	opened := make(chan string, 1)
	real := openURL
	openURL = func(url string) error { opened <- url; return nil }
	t.Cleanup(func() { openURL = real })

	v, calls := testCloudViewer(t)
	setTermSize(t, 40, 200)
	v.setStream(cloudStreamState{IsRunning: true, IsLoading: true, StatusMessage: "Permission denied", HasMoreOlder: true})
	v.frame(40, 200)
	rows := v.toolbar(200, true)
	top, second := stripSGR(rows[1]), stripSGR(rows[4])
	for _, part := range []string{cloudGlyphStop, "Clear view", "Follow tail", "app ▼", "Last 15m ▼ " + cloudGlyphLoading, "Permission denied", " Logs  SQL "} {
		if !strings.Contains(top, part) {
			t.Errorf("toolbar row 1 has no %q: %q", part, top)
		}
	}
	for _, part := range []string{"All", " D ", " I ", " W ", " E ", " C ", "Filters", "filter loaded logs…", "Copy format", "Export"} {
		if !strings.Contains(second, part) {
			t.Errorf("toolbar row 2 has no %q: %q", part, second)
		}
	}
	if text, _ := v.notice(); text != "" {
		t.Errorf("the status message is in the toolbar and under it: %q", text)
	}
	// A narrow pane keeps the controls as glyphs and moves the status under the toolbar.
	narrow := stripSGR(v.toolbar(56, false)[0])
	if !strings.Contains(narrow, cloudGlyphClear) || !strings.Contains(narrow, cloudGlyphShare) || !strings.Contains(narrow, "Logs") || strings.Contains(narrow, "Clear view") {
		t.Errorf("narrow toolbar %q", narrow)
	}
	if text, failed := v.notice(); text != "Permission denied" || !failed {
		t.Errorf("the status message is nowhere in a narrow pane: %q", text)
	}
	v.frame(40, 200)

	click := func(row string, label string, y int) {
		t.Helper()
		at := strings.Index(row, label)
		if at < 0 {
			t.Fatalf("no %q to click in %q", label, row)
		}
		v.mouse(mouseEvent{button: 0, x: cellWidth(row[:at]) + 1, y: y, press: true})
	}
	click(top, cloudGlyphStop, 2)
	if got := cloudMethods(*calls); !reflect.DeepEqual(got, []string{"cloud.sessions.stop"}) {
		t.Errorf("a click on stop asked for %v", got)
	}
	click(top, "Follow tail", 1)
	if v.follow {
		t.Error("a click on Follow tail left it on")
	}
	click(top, "SQL", 2)
	if v.mode != cloudModeSQL {
		t.Error("a click on SQL left the list in Logs mode")
	}
	click(top, "Logs", 2)
	if v.mode != cloudModeLogs {
		t.Error("a click on Logs left the list in SQL mode")
	}
	click(second, "Filters", 5)
	if !v.showBar || v.focus != cloudBar {
		t.Error("a click on Filters didn't open the bar")
	}
	click(second, "Filters", 5)
	click(second, "filter loaded", 5)
	if v.showBar || v.focus != cloudSearch {
		t.Errorf("after Filters again and the field: bar %v, focus %d", v.showBar, v.focus)
	}
	v.handleKey([]byte{0x1b})
	click(second, "Copy format", 6)
	if v.editor == nil {
		t.Fatal("a click on Copy format opened nothing")
	}
	v.handleKey([]byte{0x1b})
	click(top, cloudGlyphShare, 2)
	if v.menu == nil || v.menu.items[0].label != "Copy Logs Explorer URL" || v.menu.items[1].label != "Open in browser" {
		t.Fatalf("share menu %+v", v.menu)
	}
	want := cloudConsoleURL(v.cfg)
	v.runMenuItem(0)
	v.handleKey([]byte("u"))
	v.runMenuItem(1)
	for name, ch := range map[string]<-chan string{"copied": copied, "opened": opened} {
		select {
		case got := <-ch:
			if got != want || !strings.Contains(got, "project=proj-1") {
				t.Errorf("%s %q, want %q", name, got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("nothing %s", name)
		}
	}
	v.handleKey([]byte("?"))
	if box, _, _ := v.popup(40, 200); len(box) == 0 || !strings.Contains(stripSGR(strings.Join(box, "\n")), "Follow tail") {
		t.Error("? opened no help")
	}
	v.handleKey([]byte("?"))
	if v.help {
		t.Error("? again left the help open")
	}
	if v.exportName() != "Checkout.log" {
		t.Errorf("export name %q", v.exportName())
	}
	if !v.handleKey([]byte("q")) {
		t.Error("q didn't quit")
	}
}

// Every row of the frame fits the pane, whatever is open, at any size.
func TestCloudSessionFrameFits(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	v, _ := testCloudViewer(t)
	long := strings.Repeat("0123456789 ", 40)
	e := cloudTestEntry(4, 500, "multi\n"+long+"\n\tindented 日本語 text")
	e.ResourceLabels = map[string]string{"cluster": long, "": ""}
	e.Trace, e.SpanID, e.HTTPRequestSummary, e.ResourceType = long, "span", "GET / 200", "gce_instance"
	received := e.Timestamp + 1
	e.ReceiveTimestamp = &received
	e.Raw = "{\n  \"textPayload\": \"" + long + "\"\n}"
	feedEntries(v, 1, 3)
	v.appended(v.feed.apply([]cloudEntry{e}))
	feedEntries(v, 5, 60)
	text := newTextCondition()
	text.Value = long
	label := newLabelCondition()
	label.Key, label.Value = "user_id", long
	other := newLabelCondition()

	check := func(name string) {
		t.Helper()
		for _, size := range cloudSessionSizes {
			rows, cols := size[0], size[1]
			setTermSize(t, rows, cols)
			frame := v.frame(rows, cols)
			if len(frame) > rows {
				t.Errorf("%s at %dx%d: %d rows", name, rows, cols, len(frame))
			}
			for i, row := range frame {
				if got := plainWidth(row); got > cols {
					t.Errorf("%s at %dx%d: row %d is %d cells: %q", name, rows, cols, i, got, stripSGR(row))
				}
			}
			if rows >= 12 && cols >= 50 && plainWidth(frame[len(frame)-1]) != cols {
				t.Errorf("%s at %dx%d: the status bar is %d cells", name, rows, cols, plainWidth(frame[len(frame)-1]))
			}
			box, top, left := v.popup(rows, cols)
			if len(box) > rows || top < 0 || left < 0 {
				t.Errorf("%s at %dx%d: a popup of %d rows at %d,%d", name, rows, cols, len(box), top, left)
			}
			for i, row := range box {
				if got := plainWidth(row); got != plainWidth(box[0]) || left+got > cols {
					t.Errorf("%s at %dx%d: popup row %d is %d cells at column %d, the border %d", name, rows, cols, i, got, left, plainWidth(box[0]))
				}
			}
		}
	}
	check("list")
	v.setStream(cloudStreamState{IsRunning: true, IsLoading: true, OlderLoading: true, HasMoreOlder: true,
		StatusMessage: "PERMISSION_DENIED: " + long})
	check("running with a status")
	v.err = long
	check("failed call")
	v.err = ""

	v.selectEntries(selection{on: true, anchor: 4, cursor: 4})
	v.openDetail(4)
	check("detail")
	v.showMessage, v.showRaw = true, true
	for row := 0; row < 14; row++ {
		v.detailRow, v.detailKeep = row, true
		check(fmt.Sprint("detail row ", row))
	}
	v.detailActivate()
	check("detail menu")
	v.menu = nil
	v.menu = &popupMenu{x: 500, y: 500, items: v.labelMenu(labelScopeResource, "cluster", long)}
	check("label menu")
	v.menu = nil

	v.showBar, v.showQuery, v.focus = true, true, cloudBar
	check("empty bar")
	v.cfg.Query.TextConditions = []textCondition{text, newTextCondition()}
	v.cfg.Query.LabelConditions = []labelCondition{label, other}
	for _, stop := range v.stops() {
		v.barStop = stop
		check("bar on " + stop.kind)
	}
	v.project.LabelKeysByLogName = nil
	check("bar without label keys")
	v.project = testCloudState().Projects[0]
	v.setRaw(true, "resource.type=\"k8s_container\"\n"+long+"\nseverity>=ERROR\na\nb\nc\nd\ne")
	for _, stop := range v.stops() {
		v.barStop = stop
		check("raw bar on " + stop.kind)
	}
	v.setRaw(true, "")
	v.focus = cloudList
	check("empty raw bar")
	v.closeDetail()
	check("bar without detail")

	// The popups.
	v.openCustomMinutes()
	check("custom minutes")
	v.openAbsoluteRange()
	v.sheet.(*cloudForm).bad[0] = true
	check("absolute range")
	v.openSaveTemplate()
	v.sheet.(*cloudForm).fields[0].set(long)
	check("save template")
	v.openKeyPicker(label.ID)
	check("key picker")
	v.sheet.(*cloudKeyPicker).query.set(long)
	check("key picker with no match")
	v.project.LabelKeysByLogName[testLogName] = strings.Fields(long + " " + strings.Repeat("key ", 30))
	v.sheet.(*cloudKeyPicker).query.set("")
	v.sheet.(*cloudKeyPicker).selected = 25
	check("long key picker")
	v.sheet = nil
	v.openTimeMenu(3, 4)
	check("time menu")
	v.openShareMenu(900, 2)
	check("share menu")
	v.templates = append(v.templates, cloudQueryTemplate{Name: long, Query: newCloudQuery()})
	v.openTemplatesMenu(v.anchor("templates"))
	check("templates menu")
	v.selectEntries(selection{on: true, all: true})
	v.openRowMenu(40, 10)
	check("row menu")
	v.menu = nil
	v.openFormatEditor()
	check("copy format")
	v.editor = nil
	v.help = true
	check("help")
	v.help = false

	// The columns fill their width exactly, so the two sit side by side.
	v.selectEntries(selection{on: true, anchor: 4, cursor: 4})
	v.openDetail(4)
	for w := 1; w <= 170; w += 3 {
		for i, row := range v.listColumn(w, 12) {
			if got := plainWidth(row); got != w {
				t.Fatalf("list row %d is %d cells in a %d-cell column", i, got, w)
			}
		}
		for i, row := range v.detailColumn(w, 30) {
			if got := plainWidth(row); got != w {
				t.Fatalf("detail row %d is %d cells in a %d-cell column", i, got, w)
			}
		}
		for _, raw := range []bool{false, true} {
			v.rawMode = raw
			for i, row := range v.barLines(w, 40) {
				if got := plainWidth(row); got != w {
					t.Fatalf("bar row %d (raw %v) is %d cells in a %d-cell pane: %q", i, raw, got, w, stripSGR(row))
				}
			}
		}
		for _, tall := range []bool{false, true} {
			for i, row := range v.toolbar(w, tall) {
				if got := plainWidth(row); got > w {
					t.Fatalf("toolbar row %d is %d cells in a %d-cell pane", i, got, w)
				}
			}
		}
		if got := plainWidth(v.statusBar(w)); got > w {
			t.Fatalf("status bar is %d cells in a %d-cell pane", got, w)
		}
	}
	// A bar taller than its budget is cut to it, around the focused stop.
	v.rawMode, v.focus, v.barStop = false, cloudBar, cloudStop{"query", 0}
	if lines := v.barLines(100, 4); len(lines) != 4 || !strings.Contains(stripSGR(strings.Join(lines, "\n")), "Hide query") {
		t.Errorf("a bar cut to 4 rows: %d rows, footer shown %v", len(lines), strings.Contains(stripSGR(strings.Join(lines, "\n")), "Hide query"))
	}
}

// A first open whose reply failed is asked again by Start, and Stop still reaches a session
// jacad made all the same.
func TestCloudSessionOpenFailsThenRecovers(t *testing.T) {
	fresh := func(autoStart bool) (*cloudViewer, *[]cloudCall) {
		quit := &pane{work: make(chan func()), done: make(chan struct{})}
		close(quit.done)
		v := newCloudViewer(quit, cloudSessionSpec{Config: newCloudStreamConfig("proj-1"), AutoStart: autoStart}, nil)
		calls := &[]cloudCall{}
		v.call = func(method string, params map[string]any) { *calls = append(*calls, cloudCall{method, params}) }
		v.now = time.Now
		v.begin(testCloudState(), true, nil)
		v.finishOpen(cloudSessionInfo{}, errors.New("cloud.sessions.open: no reply from jacad after 1m0s"))
		*calls = nil
		return v, calls
	}

	// Nothing was made: one press of Start opens it again, started.
	v, calls := fresh(true)
	if v.active() {
		t.Fatal("a session that failed to open reads as running")
	}
	v.toggle()
	if got := cloudMethods(*calls); len(got) != 2 || got[0] != "cloud.sessions.open" || got[1] != "cloud.sessions.start" || (*calls)[0].params["autoStart"] != true {
		t.Fatalf("Start after a failed open asked for %v", *calls)
	}
	v.finishOpen(cloudSessionInfo{ID: v.id, State: cloudStreamState{IsRunning: true}}, nil)
	if !v.active() || len(*calls) < 1 || cloudMethods(*calls)[0] != "cloud.sessions.open" {
		t.Fatalf("after the second open: active %v, calls %v", v.active(), cloudMethods(*calls))
	}

	// jacad had made it, stopped: opening it again doesn't start it, so Start is sent.
	v, calls = fresh(false)
	v.toggle()
	v.finishOpen(cloudSessionInfo{ID: v.id, Existed: true}, nil)
	started := 0
	for _, m := range cloudMethods(*calls) {
		if m == "cloud.sessions.start" {
			started++
		}
	}
	if started != 1 || !v.active() {
		t.Fatalf("an existing stopped session: %v, active %v", cloudMethods(*calls), v.active())
	}

	// jacad had made it and it runs: its state says so, and Stop reaches it.
	v, calls = fresh(true)
	v.setStream(cloudStreamState{IsRunning: true})
	if !v.active() {
		t.Fatal("a running session reads as stopped")
	}
	v.toggle()
	if got := cloudMethods(*calls); len(got) != 1 || got[0] != "cloud.sessions.stop" {
		t.Fatalf("Stop asked for %v", got)
	}

	// A subscribe that failed leaves nothing wanted.
	quit := &pane{work: make(chan func()), done: make(chan struct{})}
	close(quit.done)
	v = newCloudViewer(quit, cloudSessionSpec{Config: newCloudStreamConfig("proj-1"), AutoStart: true}, nil)
	v.begin(cloudState{}, false, errors.New("no socket"))
	if v.active() || v.openSent {
		t.Fatalf("after a failed subscribe: active %v, openSent %v", v.active(), v.openSent)
	}
}

// The arrow and page keys move a one-entry selection over the list; an open detail panel
// follows it and the keys stay with the list.
func TestCloudSessionArrowKeysSelect(t *testing.T) {
	v, _ := testCloudViewer(t)
	setTermSize(t, 30, 100)
	v.frame(30, 100)
	feedEntries(v, 1, 100)
	v.frame(30, 100)
	up, down, pageUp := []byte("\x1b[A"), []byte("\x1b[B"), []byte("\x1b[5~")

	v.handleKey(up)
	if !v.sel.on || v.sel.cursor != 100 || v.sel.anchor != 100 || v.follow {
		t.Fatalf("the first Up selected %+v, follow %v", v.sel, v.follow)
	}
	v.handleKey(up)
	v.handleKey(up)
	if v.sel.cursor != 98 || v.sel.anchor != 98 {
		t.Fatalf("two more Ups: %+v", v.sel)
	}
	v.openDetail(98)
	v.focus = cloudList
	v.handleKey(pageUp)
	want := uint64(98 - v.room())
	if v.sel.cursor != want || v.detail.Seq != want || v.focus != cloudList {
		t.Fatalf("PgUp: cursor %d, detail %d, focus %v; want entry %d and the list", v.sel.cursor, v.detail.Seq, v.focus, want)
	}
	v.frame(30, 100)
	shown := false
	for _, seq := range v.rowSeq {
		shown = shown || seq == want
	}
	if !shown {
		t.Errorf("entry %d is selected and not on screen: %v", want, v.rowSeq)
	}
	// Down from the newest entry lets go and follows the tail again.
	for i := 0; i < 200 && v.sel.on; i++ {
		v.handleKey(down)
	}
	if v.sel.on || !v.follow || v.offset != 0 {
		t.Errorf("past the newest entry: selection %+v, follow %v, offset %d", v.sel, v.follow, v.offset)
	}
	// At the oldest entry Up stays.
	for i := 0; i < 200; i++ {
		v.handleKey(up)
	}
	if v.sel.cursor != 1 {
		t.Errorf("Up past the oldest entry: %+v", v.sel)
	}
}
