package main

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
)

// sqlQuery is one statement the viewer asked jacad to run, and where its result goes.
type sqlQuery struct {
	sql  string
	done func(dbResultSet, error)
}

// testSQLViewer is testCloudViewer with captured data (so a query may run) and its queries
// recorded instead of sent.
func testSQLViewer(t *testing.T) (*cloudViewer, *[]cloudCall, *[]sqlQuery) {
	t.Helper()
	v, calls := testCloudViewer(t)
	queries := &[]sqlQuery{}
	v.query = func(sql string, done func(dbResultSet, error)) { *queries = append(*queries, sqlQuery{sql, done}) }
	v.setStream(cloudStreamState{HasData: true, HasMoreOlder: true})
	return v, calls, queries
}

// idRows is a result with an insert_id column holding these ids.
func idRows(ids ...string) dbResultSet {
	rows := make([][]any, len(ids))
	for i, id := range ids {
		rows[i] = []any{id}
	}
	return dbResultSet{Columns: []string{"insert_id"}, Rows: sqlRows(rows...)}
}

func listedMessages(v *cloudViewer) []string {
	out := []string{}
	for _, e := range v.listed() {
		out = append(out, e.Message)
	}
	return out
}

// answer hands a result to the oldest query still waiting and forgets it.
func answer(t *testing.T, queries *[]sqlQuery, result dbResultSet, err error) {
	t.Helper()
	if len(*queries) == 0 {
		t.Fatal("no query is in flight")
	}
	q := (*queries)[0]
	*queries = (*queries)[1:]
	q.done(result, err)
}

// The switch turns SQL mode on and off, and the list follows: the feed in Logs mode, the
// query's rows in SQL mode, the feed again after it.
func TestCloudSQLModeSwitch(t *testing.T) {
	v, _, queries := testSQLViewer(t)
	feedEntries(v, 1, 5)
	if got := len(v.listed()); got != 5 || v.mode != cloudModeLogs {
		t.Fatalf("Logs mode lists %d entries in mode %d", got, v.mode)
	}
	v.handleKey([]byte("Q"))
	if v.mode != cloudModeSQL || len(*queries) != 1 {
		t.Fatalf("Q: mode %d, %d queries", v.mode, len(*queries))
	}
	if (*queries)[0].sql != generatedCloudSQL(v.cfg) || v.sql.text.String() != generatedCloudSQL(v.cfg) {
		t.Errorf("the first run was of %q", (*queries)[0].sql)
	}
	if !v.sql.running {
		t.Error("a run the user started doesn't read Running…")
	}
	answer(t, queries, idRows("id-4", "id-2"), nil)
	if got := listedMessages(v); !reflect.DeepEqual(got, []string{"line 4", "line 2"}) {
		t.Errorf("SQL mode lists %v", got)
	}
	if v.sql.running || v.sql.err != "" || v.sql.count != 2 {
		t.Errorf("after the result: running %v, error %q, count %d", v.sql.running, v.sql.err, v.sql.count)
	}
	// The status bar and the editor bar count the query's rows.
	setTermSize(t, 30, 120)
	frame := stripSGR(strings.Join(v.frame(30, 120), "\n"))
	for _, want := range []string{"SQL filter · 2 shown · re-runs live", "2 shown  5 total", cloudSQLHint, "Run"} {
		if !strings.Contains(frame, want) {
			t.Errorf("the frame has no %q:\n%s", want, frame)
		}
	}
	// Entries that arrive are counted, and wait for the next run to be listed.
	feedEntries(v, 6, 2)
	if got := len(v.listed()); got != 2 || v.total != 7 {
		t.Errorf("after a live batch: %d listed, %d total", got, v.total)
	}
	// Older logs aren't asked for in SQL mode.
	calls := 0
	v.call = func(string, map[string]any) { calls++ }
	v.loadOlder()
	if calls != 0 {
		t.Error("SQL mode asked for older logs")
	}

	v.handleKey([]byte("Q"))
	if v.mode != cloudModeLogs || v.sql.result != nil || v.sql.err != "" || v.sql.timer != nil {
		t.Errorf("after leaving: mode %d, result %v, error %q, timer %v", v.mode, v.sql.result, v.sql.err, v.sql.timer)
	}
	if got := len(v.listed()); got != 7 {
		t.Errorf("Logs mode lists %d entries after SQL mode, want the feed's 7", got)
	}
	if frame := stripSGR(strings.Join(v.frame(30, 120), "\n")); strings.Contains(frame, "SQL filter") {
		t.Error("the SQL bar is drawn in Logs mode")
	}
	// A result that lands after the mode was left changes nothing.
	v.setMode(cloudModeSQL)
	v.setMode(cloudModeLogs)
	answer(t, queries, idRows("id-1"), nil)
	if got := len(v.listed()); got != 7 || v.sql.inFlight || v.sql.result != nil {
		t.Errorf("a late result: %d listed, in flight %v", got, v.sql.inFlight)
	}
	// Leaving the pane stops the refresh.
	v.setMode(cloudModeSQL)
	gen := v.sql.gen
	v.leave(false)
	if v.sql.timer != nil || v.sql.gen == gen {
		t.Error("leaving the pane left the refresh running")
	}
}

// The editor holds the generated statement until the text is edited; from then on a mode
// switch keeps the user's text.
func TestCloudSQLGeneratedUntilEdited(t *testing.T) {
	v, _, _ := testSQLViewer(t)
	v.setMode(cloudModeSQL)
	first := generatedCloudSQL(v.cfg)
	if v.sql.text.String() != first || v.sql.customized {
		t.Fatalf("the editor holds %q", v.sql.text.String())
	}
	// A filter changed in Logs mode shows in the statement on the way back.
	v.setMode(cloudModeLogs)
	sev := 500
	v.cfg.Query.MinSeverity = &sev
	v.setMode(cloudModeSQL)
	second := generatedCloudSQL(v.cfg)
	if second == first || v.sql.text.String() != second {
		t.Fatalf("the statement didn't follow the filter: %q", v.sql.text.String())
	}
	// Typing makes it the user's.
	v.handleKey([]byte("s"))
	if v.focus != cloudSQLBar || !v.sql.editing {
		t.Fatalf("s: focus %d, editing %v", v.focus, v.sql.editing)
	}
	v.handleKey([]byte("x"))
	edited := v.sql.text.String()
	if !v.sql.customized || edited == second {
		t.Fatalf("typing left the text generated: %q", edited)
	}
	v.setMode(cloudModeLogs)
	v.cfg.Query.MinSeverity = nil
	v.setMode(cloudModeSQL)
	if v.sql.text.String() != edited {
		t.Errorf("a mode switch replaced the edited text with %q", v.sql.text.String())
	}
	// Undoing the edit makes it the generated statement again.
	v.focus, v.sql.stop, v.sql.editing = cloudSQLBar, sqlStopEditor, true
	v.handleKey([]byte{0x1a})
	if v.sql.customized || v.sql.text.String() != second {
		t.Errorf("after undo: customized %v, %q", v.sql.customized, v.sql.text.String())
	}
}

// A run is checked first, with the app's messages, and a result without an id column empties
// the list.
func TestCloudSQLRunValidation(t *testing.T) {
	v, _, queries := testSQLViewer(t)
	feedEntries(v, 1, 3)
	v.setStream(cloudStreamState{})
	v.setMode(cloudModeSQL)
	if v.sql.err != "No data captured yet — start the session first." || len(*queries) != 0 || v.sql.running {
		t.Errorf("with no data: %q, %d queries", v.sql.err, len(*queries))
	}
	v.setStream(cloudStreamState{HasData: true})
	v.sql.text.set("DELETE FROM log_entry")
	v.handleKey([]byte{0x12})
	if v.sql.err != "Only read-only queries are allowed (SELECT / WITH / PRAGMA / EXPLAIN)." || len(*queries) != 0 {
		t.Errorf("a write: %q, %d queries", v.sql.err, len(*queries))
	}
	// The error takes the hint's place.
	setTermSize(t, 30, 100)
	frame := stripSGR(strings.Join(v.frame(30, 100), "\n"))
	if !strings.Contains(frame, "Only read-only queries are allowed") || strings.Contains(frame, "to run") {
		t.Errorf("the error isn't in the hint's place:\n%s", frame)
	}

	v.sql.text.set("SELECT insert_id FROM log_entry")
	v.handleKey([]byte("\x1b[13;9u"))
	if len(*queries) != 1 || v.sql.err != "" || !v.sql.running {
		t.Fatalf("⌘↩: %d queries, error %q, running %v", len(*queries), v.sql.err, v.sql.running)
	}
	answer(t, queries, idRows("id-2"), nil)
	if got := listedMessages(v); !reflect.DeepEqual(got, []string{"line 2"}) {
		t.Fatalf("listed %v", got)
	}
	v.runSQL(false)
	answer(t, queries, dbResultSet{Columns: []string{"time", "text_payload"}, Rows: sqlRows([]any{"t", "x"})}, nil)
	const missing = "Your SQL must SELECT an `insert_id` (or `seq`) column so the matching logs can be shown — e.g. SELECT insert_id, … FROM log_entry …"
	if v.sql.err != missing || len(v.listed()) != 0 || v.sql.count != 0 || v.sql.result != nil {
		t.Errorf("without an id column: %q, %d listed, count %d", v.sql.err, len(v.listed()), v.sql.count)
	}
	// A failed query shows jacad's message.
	v.runSQL(false)
	answer(t, queries, dbResultSet{}, &rpcError{Code: -32000, Message: "no such column: nope"})
	if v.sql.err != "no such column: nope" || v.sql.running {
		t.Errorf("a failed query: %q, running %v", v.sql.err, v.sql.running)
	}
}

// The list is the result's rows in its order: entries drawn as in Logs mode, dividers centred
// and dim, and the instant search over both.
func TestCloudSQLResultMapping(t *testing.T) {
	v, _, queries := testSQLViewer(t)
	feedEntries(v, 1, 6)
	v.setMode(cloudModeSQL)
	result := dbResultSet{
		Columns: []string{"insert_id", "seq", "severity_name", "text_payload", "is_marker"},
		Rows: sqlRows(
			[]any{"id-5", "5", "INFO", "line 5", "0"},
			[]any{"", "3", "NOTICE", "──  window  ──", "1"},
			[]any{"id-2", "2", "INFO", "line 2", "0"},
			[]any{"id-5", "5", "INFO", "line 5", "0"}, // an entry shows once
			[]any{"gone", nil, "ERROR", "count 12", nil},
			[]any{nil, "6", "INFO", "by seq", "0"},
		),
	}
	answer(t, queries, result, nil)
	if got := listedMessages(v); !reflect.DeepEqual(got, []string{"line 5", "──  window  ──", "line 2", "count 12", "line 6"}) {
		t.Fatalf("listed %v", got)
	}
	list := v.listed()
	if list[1].Seq != math.MaxUint64 || list[3].Seq != math.MaxUint64-1 || !v.sqlMarker(list[1].Seq) || v.sqlMarker(list[0].Seq) {
		t.Errorf("divider seqs %d and %d", list[1].Seq, list[3].Seq)
	}

	setTermSize(t, 30, 120)
	frame := v.frame(30, 120)
	rows := frame[v.listTop-1 : v.listTop-1+len(v.rowSeq)]
	if len(rows) != 5 {
		t.Fatalf("%d list rows", len(rows))
	}
	// An entry row is the Logs mode row.
	if want := renderCloudRow(list[0], "line 5", 0, false, 120); rows[0] != want {
		t.Errorf("entry row %q, want %q", rows[0], want)
	}
	divider := stripSGR(rows[1])
	lead := len(divider) - len(strings.TrimLeft(divider, " "))
	trail := len(divider) - len(strings.TrimRight(divider, " "))
	if strings.TrimSpace(divider) != "──  window  ──" || lead < 40 || lead-trail > 1 || trail-lead > 1 || !strings.HasPrefix(strings.TrimLeft(rows[1], " "), sgrDim) {
		t.Errorf("divider row %q", rows[1])
	}
	if strings.Contains(divider, ":") || strings.Contains(stripSGR(rows[3]), " E ") {
		t.Errorf("a divider has a time or a badge: %q / %q", divider, stripSGR(rows[3]))
	}

	// A divider is neither selected nor opened; an entry is, by its place in the result.
	click := func(row int) { v.mouse(mouseEvent{button: 0, x: 60, y: v.listTop + row, press: true}) }
	click(1)
	if v.sel.on || v.detailOn {
		t.Errorf("a click on a divider: selected %v, detail %v", v.sel.on, v.detailOn)
	}
	click(2)
	if !v.detailOn || v.detail.Message != "line 2" || !v.selected(list[2].Seq) || v.selected(list[0].Seq) {
		t.Errorf("a click on an entry: detail %v %q", v.detailOn, v.detail.Message)
	}
	// Stepping in the detail panel passes over the dividers.
	v.handleKey([]byte("]"))
	if v.detail.Message != "line 6" {
		t.Errorf("] went to %q", v.detail.Message)
	}
	v.handleKey([]byte("["))
	v.handleKey([]byte("["))
	if v.detail.Message != "line 5" {
		t.Errorf("[ [ went to %q", v.detail.Message)
	}
	// A selection runs over positions of the result, without the dividers.
	v.closeDetail()
	v.selectEntries(selection{on: true, anchor: list[0].Seq, cursor: list[4].Seq})
	selected := []string{}
	for _, e := range v.selectedEntries() {
		selected = append(selected, e.Message)
	}
	if !reflect.DeepEqual(selected, []string{"line 5", "line 2", "line 6"}) || v.selected(list[1].Seq) {
		t.Errorf("selected %v", selected)
	}
	v.selectEntries(selection{on: true, all: true})
	if got := len(v.selectedEntries()); got != 3 {
		t.Errorf("Select All took %d rows, want the 3 entries", got)
	}
	v.clearSelection()

	// The search narrows entries and dividers alike.
	v.search.set("window")
	v.refilter()
	if got := listedMessages(v); !reflect.DeepEqual(got, []string{"──  window  ──"}) {
		t.Errorf("searching window lists %v", got)
	}
	v.search.set("line 2")
	v.refilter()
	if got := listedMessages(v); !reflect.DeepEqual(got, []string{"line 2"}) || v.sql.count != 1 {
		t.Errorf("searching line 2 lists %v", got)
	}
}

// Only one query is in flight: runs asked for meanwhile happen once, after it, with the text
// as it is then.
func TestCloudSQLCoalescesRuns(t *testing.T) {
	v, _, queries := testSQLViewer(t)
	feedEntries(v, 1, 3)
	v.setMode(cloudModeSQL)
	v.sql.err = "stale"
	v.runSQL(false)
	v.sqlTick(v.sql.gen)
	v.sql.text.set("SELECT insert_id FROM log_entry LIMIT 1")
	v.runSQL(false)
	if len(*queries) != 1 || !v.sql.rerun || !v.sql.running || v.sql.err != "" {
		t.Fatalf("while one runs: %d queries, rerun %v, running %v, error %q", len(*queries), v.sql.rerun, v.sql.running, v.sql.err)
	}
	answer(t, queries, idRows("id-1"), nil)
	if len(*queries) != 1 || (*queries)[0].sql != "SELECT insert_id FROM log_entry LIMIT 1" {
		t.Fatalf("after the first landed: %d queries", len(*queries))
	}
	// The run that follows is quiet, so Run doesn't stay on Running….
	if v.sql.running || !v.sql.inFlight || v.sql.rerun {
		t.Errorf("the follow-up run: running %v, in flight %v, rerun %v", v.sql.running, v.sql.inFlight, v.sql.rerun)
	}
	answer(t, queries, idRows("id-3"), nil)
	if len(*queries) != 0 || v.sql.inFlight {
		t.Errorf("after both landed: %d queries, in flight %v", len(*queries), v.sql.inFlight)
	}
	if got := listedMessages(v); !reflect.DeepEqual(got, []string{"line 3"}) {
		t.Errorf("listed %v", got)
	}
}

// The live refresh is quiet: no Running…, no message for a statement that can't run, and an
// error shown stays until a run lands. A refresh that fails keeps the rows, as in the app.
func TestCloudSQLLiveRefresh(t *testing.T) {
	v, _, queries := testSQLViewer(t)
	feedEntries(v, 1, 3)
	v.setMode(cloudModeSQL)
	answer(t, queries, idRows("id-1", "id-2"), nil)

	v.sqlTick(v.sql.gen)
	if len(*queries) != 1 || v.sql.running {
		t.Fatalf("a tick: %d queries, running %v", len(*queries), v.sql.running)
	}
	if v.sql.timer == nil {
		t.Error("a tick didn't schedule the next")
	}
	feedEntries(v, 4, 1)
	answer(t, queries, idRows("id-4", "id-1"), nil)
	if got := listedMessages(v); !reflect.DeepEqual(got, []string{"line 4", "line 1"}) {
		t.Errorf("after a refresh: %v", got)
	}

	// A refresh that fails: the rows stay, and the failure is shown (the app sets it too).
	v.sqlTick(v.sql.gen)
	answer(t, queries, dbResultSet{}, errors.New("database is locked"))
	if got := listedMessages(v); !reflect.DeepEqual(got, []string{"line 4", "line 1"}) || v.sql.err != "database is locked" {
		t.Errorf("after a failed refresh: %v, error %q", got, v.sql.err)
	}

	// A statement that can't run is reported on Run, and a refresh then neither runs it nor
	// clears or replaces what Run reported.
	v.sql.text.set("DROP TABLE log_entry")
	v.runSQL(false)
	shown := v.sql.err
	v.sqlTick(v.sql.gen)
	if len(*queries) != 0 || v.sql.err != shown || shown == "" {
		t.Errorf("a refresh of a write: %d queries, error %q", len(*queries), v.sql.err)
	}
	v.setStream(cloudStreamState{})
	v.sqlTick(v.sql.gen)
	if v.sql.err != shown || len(*queries) != 0 {
		t.Errorf("a refresh with no data: error %q", v.sql.err)
	}
	if got := len(v.listed()); got != 2 {
		t.Errorf("the rows went with the error: %d listed", got)
	}

	// A tick of a refresh that was stopped does nothing.
	v.setStream(cloudStreamState{HasData: true})
	v.sql.text.set("SELECT insert_id FROM log_entry")
	stale := v.sql.gen
	v.stopSQLRefresh()
	v.sqlTick(stale)
	v.setMode(cloudModeLogs)
	v.sqlTick(v.sql.gen)
	if len(*queries) != 0 {
		t.Errorf("a stopped refresh ran %d queries", len(*queries))
	}
}

// boxWords is a popup's text with its borders and line breaks taken out, so wrapped text reads
// as it was written.
func boxWords(box []string) string {
	var words []string
	for _, row := range box {
		words = append(words, strings.Fields(strings.Trim(stripSGR(row), "│╭╮╰╯├┤─"))...)
	}
	return strings.Join(words, " ")
}

func menuLabels(m *popupMenu) []string {
	if m == nil {
		return nil
	}
	out := make([]string, len(m.items))
	for i, item := range m.items {
		out[i] = item.label
	}
	return out
}

// The Templates menu lists the saved templates and the starters; choosing one sets the text
// and runs. Save… sends the text under a name.
func TestCloudSQLTemplates(t *testing.T) {
	v, calls, queries := testSQLViewer(t)
	v.setMode(cloudModeSQL)
	answer(t, queries, idRows(), nil)
	state := testCloudState()
	state.SqlTemplates = []cloudSqlTemplate{{ID: newUUID(), Name: "Slow requests", SQL: "SELECT insert_id FROM log_entry WHERE 1"}}
	v.setCloudState(state)

	v.focus, v.sql.stop = cloudSQLBar, sqlStopTemplates
	v.handleKey([]byte("\r"))
	want := []string{"Saved", "Slow requests", "Starters", "Recent 1000", "Errors only", "One log per unique label", "Flow window (START…END)", "", "From current filter"}
	if got := menuLabels(v.menu); !reflect.DeepEqual(got, want) {
		t.Fatalf("templates menu %q", got)
	}
	if v.menu.selected != 1 {
		t.Errorf("the menu opens on item %d", v.menu.selected)
	}
	v.runMenuItem(0) // a heading does nothing
	if v.menu == nil {
		t.Fatal("a heading closed the menu")
	}
	v.handleKey([]byte("\r"))
	if v.sql.text.String() != "SELECT insert_id FROM log_entry WHERE 1" || !v.sql.customized || len(*queries) != 1 || (*queries)[0].sql != v.sql.text.String() {
		t.Fatalf("a saved template: text %q, %d queries", v.sql.text.String(), len(*queries))
	}
	answer(t, queries, idRows(), nil)

	v.openSQLTemplatesMenu(1, 1)
	v.runMenuItem(4)
	if v.sql.text.String() != cloudSQLErrorsOnly || len(*queries) != 1 || !v.sql.running {
		t.Fatalf("a starter: text %q, %d queries", v.sql.text.String(), len(*queries))
	}
	answer(t, queries, idRows(), nil)

	v.openSQLTemplatesMenu(1, 1)
	v.runMenuItem(8)
	if v.sql.text.String() != generatedCloudSQL(v.cfg) || v.sql.customized || len(*queries) != 1 {
		t.Errorf("From current filter: text %q, customized %v, %d queries", v.sql.text.String(), v.sql.customized, len(*queries))
	}

	// With nothing saved the menu starts at the starters.
	v.sqlTemplates = nil
	v.openSQLTemplatesMenu(1, 1)
	if got := menuLabels(v.menu); got[0] != "Starters" || len(got) != 7 || v.menu.selected != 1 {
		t.Errorf("templates menu with nothing saved %q", got)
	}
	v.menu = nil

	// Save…
	v.sql.stop = sqlStopSave
	v.handleKey([]byte(" "))
	form, ok := v.sheet.(*cloudForm)
	if !ok || form.title != "Save SQL template" || !reflect.DeepEqual(form.labels, []string{"Template name"}) || !reflect.DeepEqual(form.buttons, []string{"Save", "Cancel"}) {
		t.Fatalf("Save… opened %+v", v.sheet)
	}
	for _, c := range "Mine" {
		v.handleKey([]byte(string(c)))
	}
	v.handleKey([]byte("\r"))
	if v.sheet != nil || len(*calls) != 1 || (*calls)[0].method != "cloud.saveSqlTemplate" ||
		!reflect.DeepEqual((*calls)[0].params, map[string]any{"name": "Mine", "sql": v.sql.text.String()}) {
		t.Errorf("Save sent %+v", *calls)
	}
}

// The Labels menu and the Schema popup insert at the editor's cursor.
func TestCloudSQLInsertsAtCursor(t *testing.T) {
	v, _, queries := testSQLViewer(t)
	v.setMode(cloudModeSQL)
	answer(t, queries, idRows(), nil)
	v.sql.text.set("SELECT insert_id FROM log_entry WHERE ;")
	v.sql.text.row, v.sql.text.col = 0, len("SELECT insert_id FROM log_entry WHERE ")
	v.noteSQLText()

	v.focus, v.sql.stop = cloudSQLBar, sqlStopLabels
	v.handleKey([]byte("\r"))
	if got := menuLabels(v.menu); !reflect.DeepEqual(got, []string{"Insert a label filter", "user_id", "tag", "region"}) {
		t.Fatalf("labels menu %q", got)
	}
	v.handleKey([]byte("\x1b[B"))
	v.handleKey([]byte("\r"))
	if got := v.sql.text.String(); got != "SELECT insert_id FROM log_entry WHERE json_extract(labels_json, '$.tag') = '';" {
		t.Errorf("after a label: %q", got)
	}
	if v.focus != cloudSQLBar || !v.sql.editing || v.sql.stop != sqlStopEditor {
		t.Errorf("after a label the keys are with focus %d, stop %q", v.focus, v.sql.stop)
	}
	// One undo takes the snippet out again.
	v.handleKey([]byte{0x1a})
	if got := v.sql.text.String(); got != "SELECT insert_id FROM log_entry WHERE ;" {
		t.Errorf("after undo: %q", got)
	}

	// The schema popup: Esc from the text, Shift-Tab back to Schema, Enter.
	v.handleKey([]byte{0x1b})
	if v.focus != cloudSQLBar || v.sql.editing {
		t.Fatalf("Esc in the editor: focus %d, editing %v", v.focus, v.sql.editing)
	}
	for v.sql.stop != sqlStopSchema {
		v.handleKey([]byte("\x1b[Z"))
	}
	v.handleKey([]byte("\r"))
	schema, ok := v.sheet.(*cloudSQLSchema)
	if !ok {
		t.Fatalf("Schema opened %+v", v.sheet)
	}
	setTermSize(t, 40, 120)
	box, _, _ := schema.box(40, 120)
	if !strings.Contains(stripSGR(strings.Join(box, "\n")), "TABLE  log_entry  —  click a column to insert it") {
		t.Errorf("the schema popup's heading: %q", stripSGR(box[1]))
	}
	text := boxWords(box)
	for _, want := range []string{
		"insert_id TEXT Cloud Logging insertId (unique) — KEEP in SELECT so matching rows show in the list",
		"seq INTEGER Monotonic id — handy for ORDER BY (insert_id is the stable key to KEEP)",
		"ts REAL Unix epoch seconds — datetime(ts,'unixepoch','localtime')",
		"severity INTEGER 100=DEBUG 200=INFO 400=WARNING 500=ERROR 600=CRITICAL",
		"severity_name TEXT ERROR / WARNING / INFO / …",
		"labels_json TEXT Entry labels as JSON — json_extract(labels_json,'$.key')",
		"resource_labels_json TEXT Resource labels JSON — json_extract(resource_labels_json,'$.key')",
		"raw TEXT Full entry JSON",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the schema popup has no %q in %q", want, text)
		}
	}
	if len(cloudSQLColumns) != 16 {
		t.Errorf("%d columns", len(cloudSQLColumns))
	}
	for i := 0; i < 5; i++ {
		v.handleKey([]byte("\x1b[B"))
	}
	v.handleKey([]byte("\r"))
	if got := v.sql.text.String(); v.sheet != nil || got != "SELECT insert_id FROM log_entry WHERE text_payload;" {
		t.Errorf("after a column: %q, sheet %v", got, v.sheet)
	}
	// A click on a column inserts it too.
	v.sheet = &cloudSQLSchema{v: v}
	_, top, left := v.popup(40, 120)
	v.mouse(mouseEvent{button: 0, x: left + 4, y: top + 4, press: true})
	if got := v.sql.text.String(); v.sheet != nil || got != "SELECT insert_id FROM log_entry WHERE text_payloadinsert_id;" {
		t.Errorf("after a click on the first column: %q", got)
	}
	// A second Esc gives the keys back to the list.
	v.handleKey([]byte{0x1b})
	v.handleKey([]byte{0x1b})
	if v.focus != cloudList || v.mode != cloudModeSQL {
		t.Errorf("Esc Esc: focus %d, mode %d", v.focus, v.mode)
	}

	// Without label keys the menu says so.
	v.project.LabelKeysByLogName = nil
	v.openSQLLabelsMenu(1, 1)
	if got := menuLabels(v.menu); !reflect.DeepEqual(got, []string{"No labels detected yet — run a session once"}) {
		t.Errorf("labels menu without keys %q", got)
	}
	v.handleKey([]byte("\r"))
	v.menu = nil
	if cloudSQLLabelFilter("user_id") != "json_extract(labels_json, '$.user_id') = ''" {
		t.Errorf("label filter %q", cloudSQLLabelFilter("user_id"))
	}
}

// The bar's keys and clicks: Tab walks the controls, Run reads Running… while the user's run
// is in flight, and a click in the editor puts the cursor there.
func TestCloudSQLBarKeysAndClicks(t *testing.T) {
	v, _, queries := testSQLViewer(t)
	v.setMode(cloudModeSQL)
	setTermSize(t, 40, 160)
	frame := v.frame(40, 160)
	if !strings.Contains(stripSGR(strings.Join(frame, "\n")), "Running…") {
		t.Error("Run doesn't read Running… while the run is in flight")
	}
	answer(t, queries, idRows(), nil)
	frame = v.frame(40, 160)
	head := stripSGR(frame[v.sql.top-1])
	order := []string{"SQL filter · 0 shown · re-runs live", cloudLabelExamplesTitle, "Labels", "Schema", "Templates", "Save…", "Run"}
	at := 0
	for _, label := range order {
		i := strings.Index(head[at:], label)
		if i < 0 {
			t.Fatalf("no %q after column %d in %q", label, at, head)
		}
		at += i + len(label)
	}
	if strings.Contains(head, "Ask Claude") {
		t.Error("the bar offers Ask Claude")
	}

	click := func(label string) {
		t.Helper()
		row := stripSGR(v.frame(40, 160)[v.sql.top-1])
		i := strings.Index(row, label)
		if i < 0 {
			t.Fatalf("no %q to click in %q", label, row)
		}
		v.mouse(mouseEvent{button: 0, x: cellWidth(row[:i]) + 1, y: v.sql.top, press: true})
	}
	click("Run")
	if len(*queries) != 1 || v.focus != cloudSQLBar || v.sql.stop != sqlStopRun {
		t.Fatalf("a click on Run: %d queries, stop %q", len(*queries), v.sql.stop)
	}
	answer(t, queries, idRows(), nil)
	click("Schema")
	if _, ok := v.sheet.(*cloudSQLSchema); !ok {
		t.Errorf("a click on Schema opened %+v", v.sheet)
	}
	v.handleKey([]byte{0x1b})
	click("Templates")
	if v.menu == nil || v.menu.y != v.sql.top+1 {
		t.Errorf("a click on Templates opened %+v", v.menu)
	}
	v.handleKey([]byte{0x1b})
	click(cloudLabelExamplesTitle)
	if _, ok := v.sheet.(*cloudLabelExamples); !ok {
		t.Errorf("a click on %s opened %+v", cloudLabelExamplesTitle, v.sheet)
	}
	answer(t, queries, dbResultSet{}, nil)
	v.handleKey([]byte{0x1b})

	// Tab walks every stop and comes back; on the editor it moves on until Enter edits.
	v.sql.stop, v.sql.editing = sqlStopExamples, false
	seen := []string{}
	for range sqlStops {
		seen = append(seen, v.sql.stop)
		v.handleKey([]byte("\t"))
	}
	if !reflect.DeepEqual(seen, sqlStops) || v.sql.stop != sqlStopExamples {
		t.Errorf("Tab went over %v", seen)
	}
	v.handleKey([]byte("\x1b[Z"))
	v.handleKey([]byte("\r"))
	if v.sql.stop != sqlStopEditor || !v.sql.editing {
		t.Fatalf("Shift-Tab Enter: stop %q, editing %v", v.sql.stop, v.sql.editing)
	}
	// In the text Tab indents, and Ctrl+R runs without leaving it.
	v.sql.text.set("x")
	v.handleKey([]byte("\t"))
	if v.sql.text.String() != "  x" || v.sql.stop != sqlStopEditor {
		t.Errorf("Tab in the text: %q", v.sql.text.String())
	}
	v.sql.text.set("SELECT seq FROM log_entry\nLIMIT 3")
	v.handleKey([]byte{0x12})
	if len(*queries) != 1 || (*queries)[0].sql != "SELECT seq FROM log_entry\nLIMIT 3" || !v.sql.editing {
		t.Errorf("Ctrl+R in the text: %d queries", len(*queries))
	}
	answer(t, queries, idRows(), nil)
	// A click on the second line of the text.
	v.focus = cloudList
	v.frame(40, 160)
	v.mouse(mouseEvent{button: 0, x: 5, y: v.sql.top + v.sql.editFirst + 1, press: true})
	if v.focus != cloudSQLBar || !v.sql.editing || v.sql.text.row != 1 || v.sql.text.col != 3 {
		t.Errorf("a click in the text: focus %d, cursor %d:%d", v.focus, v.sql.text.row, v.sql.text.col)
	}
	// Ctrl+R runs from the list as well, and not in Logs mode.
	v.focus = cloudList
	v.handleKey([]byte{0x12})
	if len(*queries) != 1 {
		t.Errorf("Ctrl+R on the list ran %d queries", len(*queries))
	}
	answer(t, queries, idRows(), nil)
	v.setMode(cloudModeLogs)
	v.handleKey([]byte{0x12})
	if len(*queries) != 0 {
		t.Error("Ctrl+R ran a query in Logs mode")
	}
	// The builder bar and the SQL bar show together, the SQL bar under it.
	v.setMode(cloudModeSQL)
	v.showBar = true
	v.frame(40, 160)
	if v.barTop == 0 || v.sql.top <= v.barTop || v.listTop != v.sql.top+v.sql.height {
		t.Errorf("builder bar at %d, SQL bar at %d+%d, list at %d", v.barTop, v.sql.top, v.sql.height, v.listTop)
	}
	if v.sql.editRows != cloudSQLEditorRows {
		t.Errorf("the editor is %d rows in a tall pane", v.sql.editRows)
	}
	v.frame(16, 160)
	if v.sql.top == 0 || v.sql.editRows < 1 || v.sql.editRows >= cloudSQLEditorRows {
		t.Errorf("in a short pane the SQL bar is at %d with %d editor rows", v.sql.top, v.sql.editRows)
	}
}

// The label examples sheet: the keys of both scopes merged by bare key with the larger count,
// and Save sending only the rules that differ from the default.
func TestCloudSQLLabelExamples(t *testing.T) {
	merged := mergeLabelCardinalities(sqlRows(
		[]any{"labels", "user_id", "40"},
		[]any{"labels", "tag", "3"},
		[]any{"resource", "tag", "5"},
		[]any{"resource", "user_id", "2"},
		[]any{"resource", "zone", "1"},
		[]any{"labels", "bad", "many"},
		[]any{"labels", nil, "2"},
		[]any{"short"},
	))
	want := []labelCardinality{{"resource", "tag", 5}, {"labels", "user_id", 40}, {"resource", "zone", 1}}
	if !reflect.DeepEqual(merged, want) {
		t.Fatalf("merged %+v", merged)
	}
	if !strings.HasPrefix(cloudLabelCardinalitySQL, "SELECT scope, k, COUNT(*) AS distinct_values FROM (\n  SELECT DISTINCT 'labels' AS scope") ||
		!strings.HasSuffix(cloudLabelCardinalitySQL, ")\nGROUP BY scope, k\nORDER BY scope, k;") {
		t.Errorf("cardinality SQL %q", cloudLabelCardinalitySQL)
	}

	v, calls, queries := testSQLViewer(t)
	v.project.LabelExampleRulesByLogName = map[string]map[string]labelExampleRule{
		testLogName: {"tag": {All: true, Count: 1}, "old": {Count: 4}, "plain": {Count: 1}},
	}
	v.setMode(cloudModeSQL)
	answer(t, queries, idRows(), nil)
	v.focus, v.sql.stop = cloudSQLBar, sqlStopExamples
	v.handleKey([]byte("\r"))
	sheet, ok := v.sheet.(*cloudLabelExamples)
	if !ok || !sheet.loading || len(*queries) != 1 || (*queries)[0].sql != cloudLabelCardinalitySQL {
		t.Fatalf("the sheet: %+v, %d queries", v.sheet, len(*queries))
	}
	setTermSize(t, 40, 120)
	// Save does nothing while the keys load.
	sheet.save()
	if v.sheet == nil || len(*calls) != 0 {
		t.Fatal("Save went through while loading")
	}
	answer(t, queries, dbResultSet{Columns: []string{"scope", "k", "distinct_values"}, Rows: sqlRows(
		[]any{"labels", "tag", "3"}, []any{"labels", "user_id", "40"}, []any{"resource", "zone", "1"})}, nil)
	box, _, _ := sheet.box(40, 120)
	text := boxWords(box)
	for _, want := range []string{
		"Label examples for Claude How many example values of each label to send to Claude, so it learns the format. Default is 1. " +
			"Set a low-cardinality label (e.g. tag) to All; keep a high-cardinality one (e.g. user_id) at 1. tag ● All − all +",
		"labels · 3 values user_id ○ All − 1 + labels · 40 values zone ○ All − 1 + resource · 1 value Cancel Save",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the sheet has no %q in %q", want, text)
		}
	}

	// tag is All: its count doesn't step. Turning All off lets it, up to its 3 values.
	sheet.focus = 0
	v.handleKey([]byte("+"))
	if got := sheet.rule("tag"); !got.All || got.Count != 1 {
		t.Errorf("stepping a key set to All: %+v", got)
	}
	v.handleKey([]byte(" "))
	for i := 0; i < 5; i++ {
		v.handleKey([]byte("\x1b[C"))
	}
	if got := sheet.rule("tag"); got.All || got.Count != 3 {
		t.Errorf("tag after All off and five steps: %+v", got)
	}
	// user_id: up one and back, which leaves the default. zone has one value and can't step.
	v.handleKey([]byte("\x1b[B"))
	v.handleKey([]byte("+"))
	v.handleKey([]byte("-"))
	v.handleKey([]byte("-"))
	v.handleKey([]byte("\x1b[B"))
	v.handleKey([]byte("+"))
	if got := sheet.rule("user_id"); got.All || got.Count != 1 {
		t.Errorf("user_id %+v", got)
	}
	if got := sheet.rule("zone"); got.All || got.Count != 1 {
		t.Errorf("zone stepped past its one value: %+v", got)
	}
	v.handleKey([]byte("\r"))
	if got := sheet.rule("zone"); !got.All {
		t.Errorf("Enter on zone: %+v", got)
	}
	// Down to Cancel, right to Save, Enter.
	v.handleKey([]byte("\x1b[B"))
	v.handleKey([]byte("\x1b[C"))
	v.handleKey([]byte("\r"))
	if v.sheet != nil || len(*calls) != 1 || (*calls)[0].method != "cloud.setLabelExampleRules" {
		t.Fatalf("Save sent %+v", *calls)
	}
	wantParams := map[string]any{"project": "proj-1", "logName": testLogName, "rules": map[string]labelExampleRule{
		"tag": {Count: 3}, "old": {Count: 4}, "zone": {All: true, Count: 1},
	}}
	if !reflect.DeepEqual((*calls)[0].params, wantParams) {
		t.Errorf("Save sent %+v, want %+v", (*calls)[0].params, wantParams)
	}

	// With nothing captured the sheet asks nothing and shows the app's notice; Esc cancels.
	*calls = nil
	v.setStream(cloudStreamState{})
	v.openLabelExamples()
	sheet = v.sheet.(*cloudLabelExamples)
	box, _, _ = sheet.box(40, 120)
	if sheet.loading || len(*queries) != 0 || !strings.Contains(stripSGR(strings.Join(box, "\n")), "No labels detected yet — capture some logs first.") {
		t.Errorf("with no data: loading %v, %d queries", sheet.loading, len(*queries))
	}
	v.handleKey([]byte{0x1b})
	if v.sheet != nil || len(*calls) != 0 {
		t.Errorf("Esc: sheet %v, %d calls", v.sheet, len(*calls))
	}
	// A failed query leaves the sheet empty, and saving then sends no rules as an object.
	v.setStream(cloudStreamState{HasData: true})
	v.project.LabelExampleRulesByLogName = nil
	v.openLabelExamples()
	answer(t, queries, dbResultSet{}, errors.New("no such table"))
	sheet = v.sheet.(*cloudLabelExamples)
	if sheet.loading || len(sheet.rows) != 0 {
		t.Errorf("after a failed query: loading %v, %d rows", sheet.loading, len(sheet.rows))
	}
	sheet.save()
	if rules, ok := (*calls)[0].params["rules"].(map[string]labelExampleRule); !ok || rules == nil || len(rules) != 0 {
		t.Errorf("saving nothing sent %#v", (*calls)[0].params["rules"])
	}
}

// Every row of the SQL bar, the schema popup and the label examples sheet fits the pane.
func TestCloudSQLFrameFits(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	v, _, queries := testSQLViewer(t)
	long := strings.Repeat("0123456789 ", 40)
	feedEntries(v, 1, 40)
	v.setMode(cloudModeSQL)
	rows := [][]any{}
	for i := 1; i <= 40; i++ {
		rows = append(rows, []any{fmt.Sprint("id-", i), "INFO", "x", "0"})
		if i%7 == 0 {
			rows = append(rows, []any{"", "NOTICE", "── " + long + " 日本語 ──\nsecond line", "1"})
		}
	}
	answer(t, queries, dbResultSet{Columns: []string{"insert_id", "severity_name", "text_payload", "is_marker"}, Rows: sqlRows(rows...)}, nil)

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
			if v.sql.top > 0 {
				for i := v.sql.top - 1; i < v.sql.top-1+v.sql.height && i < len(frame); i++ {
					if got := plainWidth(frame[i]); got != cols {
						t.Errorf("%s at %dx%d: SQL bar row %d is %d cells: %q", name, rows, cols, i, got, stripSGR(frame[i]))
					}
				}
				if v.listTop != v.sql.top+v.sql.height || v.sql.editRows < 1 {
					t.Errorf("%s at %dx%d: bar %d+%d, list at %d, %d editor rows", name, rows, cols, v.sql.top, v.sql.height, v.listTop, v.sql.editRows)
				}
			}
			box, top, left := v.popup(rows, cols)
			if len(box) > rows || top < 0 || left < 0 {
				t.Errorf("%s at %dx%d: a popup of %d rows at %d,%d", name, rows, cols, len(box), top, left)
			}
			for i, row := range box {
				if got := plainWidth(row); got != plainWidth(box[0]) || left+got > cols {
					t.Errorf("%s at %dx%d: popup row %d is %d cells at column %d, the border %d: %q", name, rows, cols, i, got, left, plainWidth(box[0]), stripSGR(row))
				}
			}
		}
	}
	check("result with dividers")
	v.scroll(15)
	check("scrolled")
	v.sql.text.set(strings.Repeat(long+"\n", 12))
	for _, stop := range sqlStops {
		v.focus, v.sql.stop, v.sql.editing = cloudSQLBar, stop, stop == sqlStopEditor
		check("bar on " + stop)
	}
	v.sql.running = true
	v.sql.err = "near \"FROM\": syntax error " + long
	check("error")
	v.sql.err = sqlMissingIDColumn
	v.showBar, v.showQuery = true, true
	check("with the builder bar")
	v.selectEntries(selection{on: true, anchor: 4, cursor: 4})
	v.openDetail(4)
	check("with the detail panel")
	v.closeDetail()
	v.showBar = false
	v.focus = cloudList

	// The bar alone, at any width and budget.
	for w := 1; w <= 170; w += 3 {
		for _, budget := range []int{0, 2, 3, 4, 5, 9, 40} {
			lines := v.sqlBarLines(w, budget, 5)
			if len(lines) > budget || (budget >= 3 && len(lines) < 3) {
				t.Fatalf("the bar is %d rows in a budget of %d at %d cells", len(lines), budget, w)
			}
			for i, row := range lines {
				if got := plainWidth(row); got != w {
					t.Fatalf("bar row %d is %d cells in a %d-cell pane: %q", i, got, w, stripSGR(row))
				}
			}
			for _, h := range v.sql.hits {
				if h.row < 0 || h.row >= v.sql.editFirst {
					t.Fatalf("a control's span is on row %d of a bar whose text starts at %d", h.row, v.sql.editFirst)
				}
			}
		}
		for i, row := range v.listColumn(w, 12) {
			if got := plainWidth(row); got != w {
				t.Fatalf("list row %d is %d cells in a %d-cell column", i, got, w)
			}
		}
	}
	// A bar cut short keeps the focused control's row.
	v.focus, v.sql.stop, v.sql.editing = cloudSQLBar, sqlStopRun, false
	if lines := v.sqlBarLines(40, 3, 5); !strings.Contains(stripSGR(strings.Join(lines, "\n")), "Running…") {
		t.Errorf("a bar cut to 3 rows lost the focused control:\n%s", stripSGR(strings.Join(lines, "\n")))
	}
	v.focus = cloudList

	// The popups.
	v.sqlTemplates = []cloudSqlTemplate{{Name: long, SQL: "SELECT 1"}}
	v.openSQLTemplatesMenu(v.anchor("sqlTemplates"))
	check("templates menu")
	v.project.LabelKeysByLogName[testLogName] = strings.Fields(long + " " + strings.Repeat("key ", 30))
	v.openSQLLabelsMenu(900, 900)
	check("labels menu")
	v.menu = nil
	v.openSaveSQLTemplate()
	v.sheet.(*cloudForm).fields[0].set(long)
	check("save template")
	schema := &cloudSQLSchema{v: v}
	v.sheet = schema
	for i := range cloudSQLColumns {
		schema.selected = i
		check(fmt.Sprint("schema on column ", i))
	}
	schema.selected = 0
	check("schema back at the top")

	v.openLabelExamples()
	check("examples loading")
	keys := [][]any{}
	for i := 0; i < 30; i++ {
		keys = append(keys, []any{"labels", fmt.Sprint("key-", i, "-", long[:i*3]), fmt.Sprint(i * 12345)})
	}
	keys = append(keys, []any{"resource " + long, "日本語", "1"})
	answer(t, queries, dbResultSet{Rows: sqlRows(keys...)}, nil)
	examples := v.sheet.(*cloudLabelExamples)
	if len(examples.rows) != 31 {
		t.Fatalf("%d example rows", len(examples.rows))
	}
	for i := 0; i < len(examples.rows)+2; i += 3 {
		examples.focus = i
		examples.toggleAll(i)
		check(fmt.Sprint("examples on ", i))
	}
	examples.focus = len(examples.rows) + 1
	check("examples on Save")
	examples.rows = nil
	check("examples with no labels")
	v.sheet = nil
	v.help = true
	check("help")
}
