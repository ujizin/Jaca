package main

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The session viewer's SQL mode (the app's CloudSqlEditorBar and the SQL half of
// CloudLogSession): an editor bar above the list, and the list driven by a query over the
// session's captured entries. The query runs in jacad (cloud.sessions.query); its rows are
// mapped back to the loaded entries in result order, and it runs again every two seconds.

// cloudSQLRefreshEvery is how often the query runs again while SQL mode is on.
const cloudSQLRefreshEvery = 2 * time.Second

// cloudSQLEditorRows is the editor's height where the pane has the room (the app's 132 points).
const cloudSQLEditorRows = 6

// The bar's hint line, and the title of the label examples sheet, which also labels the
// control that opens it (the app's is an icon without a label).
const (
	cloudSQLHint            = "⌘↩ to run · keep an `insert_id` column · labels: json_extract(labels_json,'$.key')"
	cloudLabelExamplesTitle = "Label examples for Claude"
)

// cloudSQLColumn is one column of the session's log_entry table (CloudSqlSchema.Column).
type cloudSQLColumn struct{ name, kind, note string }

// cloudSQLColumns is CloudSqlSchema.columns, in the app's order.
var cloudSQLColumns = []cloudSQLColumn{
	{"insert_id", "TEXT", "Cloud Logging insertId (unique) — KEEP in SELECT so matching rows show in the list"},
	{"seq", "INTEGER", "Monotonic id — handy for ORDER BY (insert_id is the stable key to KEEP)"},
	{"ts", "REAL", "Unix epoch seconds — datetime(ts,'unixepoch','localtime')"},
	{"severity", "INTEGER", "100=DEBUG 200=INFO 400=WARNING 500=ERROR 600=CRITICAL"},
	{"severity_name", "TEXT", "ERROR / WARNING / INFO / …"},
	{"text_payload", "TEXT", "The log message (rendered for json/proto payloads)"},
	{"log_id", "TEXT", "Short log name"},
	{"log_name", "TEXT", "Full projects/<id>/logs/<encoded>"},
	{"labels_json", "TEXT", "Entry labels as JSON — json_extract(labels_json,'$.key')"},
	{"resource_type", "TEXT", "e.g. cloud_run_revision"},
	{"resource_labels_json", "TEXT", "Resource labels JSON — json_extract(resource_labels_json,'$.key')"},
	{"trace", "TEXT", "Trace id"},
	{"span_id", "TEXT", "Span id"},
	{"receive_ts", "REAL", "When Logging received it (unix epoch)"},
	{"payload_kind", "TEXT", "text / json / proto / none"},
	{"raw", "TEXT", "Full entry JSON"},
}

// cloudSQLLabelFilter is CloudSqlSchema.labelFilter: a WHERE clause matching an entry label by
// key, its value left empty to fill in.
func cloudSQLLabelFilter(key string) string {
	return "json_extract(labels_json, '$." + key + "') = ''"
}

// cloudLabelCardinalitySQL is CloudSqlAssistant.labelCardinalitySQL: the distinct value count
// of each label key, for the label examples sheet.
const cloudLabelCardinalitySQL = "SELECT scope, k, COUNT(*) AS distinct_values FROM (\n" +
	"  SELECT DISTINCT 'labels' AS scope, je.key AS k, je.value AS v\n" +
	"  FROM log_entry, json_each(log_entry.labels_json) je WHERE json_valid(log_entry.labels_json)\n" +
	"  UNION\n" +
	"  SELECT DISTINCT 'resource', je.key, je.value\n" +
	"  FROM log_entry, json_each(log_entry.resource_labels_json) je WHERE json_valid(log_entry.resource_labels_json)\n" +
	")\n" +
	"GROUP BY scope, k\n" +
	"ORDER BY scope, k;"

// The focus stops of the SQL bar, in drawing order.
const (
	sqlStopExamples  = "examples"
	sqlStopLabels    = "labels"
	sqlStopSchema    = "schema"
	sqlStopTemplates = "templates"
	sqlStopSave      = "save"
	sqlStopRun       = "run"
	sqlStopEditor    = "editor"
)

var sqlStops = []string{sqlStopExamples, sqlStopLabels, sqlStopSchema, sqlStopTemplates, sqlStopSave, sqlStopRun, sqlStopEditor}

// cloudSQL is the viewer's SQL mode state.
type cloudSQL struct {
	text   textArea
	noRoom bool // as last drawn: the pane was too short for the bar
	// generated is the statement last written from the session's config. While the text is
	// still that, entering SQL mode writes it again; once edited the text is the user's.
	generated  string
	customized bool

	// result is the last query result the list is built from, nil when none drives it.
	result *dbResultSet
	count  int    // the rows the last result put in the list
	err    string // shown in place of the hint
	// running is a run the user started being in flight (Run reads Running…). inFlight is any
	// run; one asked for meanwhile sets rerun and runs once when it lands.
	running, inFlight, rerun bool

	// What the list holds, by seq: the dividers, and each row's position. A result is in the
	// query's order, so a row is found by position, not by searching the seqs.
	markers map[uint64]bool
	pos     map[uint64]int

	timer *time.Timer // the live refresh
	gen   int         // changes when the refresh is stopped, so a tick already posted is dropped

	stop    string // the bar's focused stop
	editing bool   // the editor takes the keys; false while Tab moves between the stops

	// The bar as last drawn: its first screen row (0 when hidden), its height, its clickable
	// spans, and the editor's first line in it and height.
	top, height int
	hits        []boxHit
	editFirst   int
	editRows    int
}

// MARK: the mode

// toggleMode is the Logs/SQL switch by key.
func (v *cloudViewer) toggleMode() {
	if v.mode == cloudModeSQL {
		v.setMode(cloudModeLogs)
	} else {
		v.setMode(cloudModeSQL)
	}
}

// enterSQL is setViewMode(.sql): the generated statement unless the user edited it, a run, and
// the live refresh.
func (v *cloudViewer) enterSQL() {
	s := &v.sql
	if !s.customized {
		v.regenerateSQL()
	}
	if s.stop == "" {
		s.stop = sqlStopEditor
	}
	v.sqlIndex() // the list is still the feed's until a result lands
	v.runSQL(false)
	v.startSQLRefresh()
}

// exitSQL is setViewMode(.logs): the result is dropped and the list shows the feed again.
func (v *cloudViewer) exitSQL() {
	s := &v.sql
	v.stopSQLRefresh()
	s.result, s.running, s.err = nil, false, ""
	s.markers, s.pos = nil, nil
	s.top, s.height, s.hits = 0, 0, nil
	if v.focus == cloudSQLBar {
		v.focus = cloudList
	}
	v.refilter()
}

// regenerateSQL is the app's regenerateSQL: the statement that mirrors the session's config.
func (v *cloudViewer) regenerateSQL() {
	s := &v.sql
	s.generated = generatedCloudSQL(v.cfg)
	if s.text.String() != s.generated {
		s.text.set(s.generated)
	}
	s.customized = false
}

// noteSQLText is noteSqlTextChanged: the text is the user's once it differs from the generated
// statement.
func (v *cloudViewer) noteSQLText() {
	v.sql.customized = v.sql.text.String() != v.sql.generated
}

// applySQLText is the app's applySqlText: a template becomes the statement and runs.
func (v *cloudViewer) applySQLText(text string) {
	if v.sql.text.String() != text {
		v.sql.text.replace(text)
	}
	v.noteSQLText()
	v.runSQL(false)
}

// sqlInsert puts text at the editor's cursor, over its selection, and gives the editor the keys.
func (v *cloudViewer) sqlInsert(text string) {
	s := &v.sql
	if text != "" && s.text.handle([]byte(text), 1) {
		v.noteSQLText()
	}
	v.focus, s.stop, s.editing = cloudSQLBar, sqlStopEditor, true
}

// MARK: running

// sendQuery is query for a live pane. cloud.sessions.query is concurrent in jacad, so it does
// not wait in the session's command queue.
func (v *cloudViewer) sendQuery(sql string, done func(dbResultSet, error)) {
	c, p, params := v.p.c, v.p, map[string]any{"id": v.id, "sql": sql}
	go func() {
		var result dbResultSet
		err := c.CallTimeout("cloud.sessions.query", params, &result, cloudCallTimeout)
		p.post(func() { done(result, err) })
	}()
}

// runSQL is the app's runSQL. A live run (the refresh) is quiet: it shows no Running…, clears
// no error and reports no problem with the statement. Only one query is in flight: a run asked
// for meanwhile happens once, after it.
func (v *cloudViewer) runSQL(live bool) {
	s := &v.sql
	text := s.text.String()
	if problem := sqlRunProblem(text, v.stream.HasData); problem != "" {
		if !live {
			s.err, s.running = problem, false
		}
		return
	}
	if v.query == nil {
		return
	}
	if s.inFlight {
		s.rerun = true
		if !live {
			s.running, s.err = true, ""
		}
		return
	}
	s.inFlight = true
	if !live {
		s.running, s.err = true, ""
	}
	v.query(text, v.finishSQL)
}

// finishSQL takes a query's result, then runs once more if a run was asked for meanwhile. A
// result that lands after SQL mode or the pane was left is dropped.
func (v *cloudViewer) finishSQL(result dbResultSet, err error) {
	s := &v.sql
	active := v.mode == cloudModeSQL && !v.left
	if active {
		if err != nil {
			s.err = err.Error() // the rows of the last result stay
		} else {
			v.applySQLResult(result)
		}
	}
	s.inFlight, s.running = false, false
	if s.rerun {
		s.rerun = false
		if active {
			v.runSQL(true)
		}
	}
}

// applySQLResult is the app's applySqlResult: a result with no insert_id or seq column can't be
// shown and empties the list; any other becomes the list.
func (v *cloudViewer) applySQLResult(result dbResultSet) {
	s := &v.sql
	if cols := findSQLColumns(result.Columns); cols.id < 0 && cols.seq < 0 {
		s.err, s.result, s.count = sqlMissingIDColumn, nil, 0
		v.visible, v.lines, v.totalRows = nil, nil, 0
		s.markers, s.pos = nil, nil
		v.clampOffset()
		return
	}
	s.result, s.err = &result, ""
	v.sqlRebuild()
}

// sqlRebuild is rebuildSqlVisible: the list from the last result, in its order, through the
// instant search. A divider gets a seq counting down from the top of the range, as in the app,
// so it can't collide with an entry's.
func (v *cloudViewer) sqlRebuild() {
	s := &v.sql
	if v.mode != cloudModeSQL || s.result == nil {
		return
	}
	rows, _ := mapSqlRowsMatching(*s.result, v.feed.entries(), v.search.String())
	v.visible, v.lines, v.totalRows = make([]cloudEntry, 0, len(rows)), make([]int, 0, len(rows)), 0
	s.markers = map[uint64]bool{}
	synthetic := uint64(math.MaxUint64)
	for _, row := range rows {
		var e cloudEntry
		if row.Entry != nil && !row.Marker {
			e = *row.Entry
		} else {
			e = cloudEntry{Seq: synthetic, Severity: row.Severity, Message: row.Text, PayloadKind: cloudPayloadNone, Raw: row.Text}
			s.markers[e.Seq] = true
			synthetic--
		}
		n := cloudLineCount(e.Message)
		v.visible, v.lines = append(v.visible, e), append(v.lines, n)
		v.totalRows += n
	}
	s.count = len(v.visible)
	v.sqlIndex()
	v.clampOffset()
}

// sqlIndex records each listed row's position by seq.
func (v *cloudViewer) sqlIndex() {
	pos := make(map[uint64]int, len(v.visible))
	for i, e := range v.visible {
		pos[e.Seq] = i
	}
	v.sql.pos = pos
}

// startSQLRefresh runs the query again every two seconds while SQL mode is on. The timer only
// posts to the loop; the tick runs there.
func (v *cloudViewer) startSQLRefresh() {
	v.stopSQLRefresh()
	gen, p := v.sql.gen, v.p
	v.sql.timer = time.AfterFunc(cloudSQLRefreshEvery, func() {
		p.post(func() { v.sqlTick(gen) })
	})
}

func (v *cloudViewer) stopSQLRefresh() {
	v.sql.gen++
	if v.sql.timer != nil {
		v.sql.timer.Stop()
		v.sql.timer = nil
	}
}

// sqlTick is one live refresh. A tick from a refresh that was stopped does nothing.
func (v *cloudViewer) sqlTick(gen int) {
	if v.left || v.mode != cloudModeSQL || gen != v.sql.gen {
		return
	}
	v.runSQL(true)
	v.startSQLRefresh()
}

// MARK: the list in SQL mode

// sqlMarker is whether the listed row with this seq is a divider.
func (v *cloudViewer) sqlMarker(seq uint64) bool {
	return v.mode == cloudModeSQL && v.sql.markers[seq]
}

// sqlMarkerRow is whether the list row at a screen row shows a divider.
func (v *cloudViewer) sqlMarkerRow(y int) bool {
	i := y - v.listTop
	return i >= 0 && i < len(v.rowSeq) && v.sqlMarker(v.rowSeq[i])
}

// sqlPos is the position of the listed row with this seq.
func (v *cloudViewer) sqlPos(seq uint64) (int, bool) {
	i, ok := v.sql.pos[seq]
	return i, ok && i < len(v.visible) && v.visible[i].Seq == seq
}

// sqlIndexOf is indexOf for a list in result order. A divider has a position but is not an
// entry, so it can't be opened.
func (v *cloudViewer) sqlIndexOf(seq uint64) (int, bool) {
	i, ok := v.sqlPos(seq)
	if !ok {
		return 0, false
	}
	return i, !v.sqlMarker(seq)
}

// selected is whether the listed entry with this seq is in the selection. In SQL mode the
// selection runs between two positions of the list, since its seqs are in the query's order.
func (v *cloudViewer) selected(seq uint64) bool {
	if v.mode != cloudModeSQL {
		return v.sel.has(seq)
	}
	if !v.sel.on || v.sqlMarker(seq) {
		return false
	}
	if v.sel.all {
		return true
	}
	lo, hi, ok := v.sqlSelectedRange()
	i, found := v.sqlPos(seq)
	return ok && found && i >= lo && i <= hi
}

// sqlSelectedRange is the positions the selection covers. It covers nothing when either end
// is no longer in the list.
func (v *cloudViewer) sqlSelectedRange() (lo, hi int, ok bool) {
	a, okA := v.sqlPos(v.sel.anchor)
	c, okC := v.sqlPos(v.sel.cursor)
	return min(a, c), max(a, c), okA && okC
}

// sqlSelectedEntries is selectedEntries in SQL mode: the selected rows without the dividers.
func (v *cloudViewer) sqlSelectedEntries() []cloudEntry {
	lo, hi := 0, len(v.visible)-1
	if !v.sel.all {
		var ok bool
		if lo, hi, ok = v.sqlSelectedRange(); !ok {
			return nil
		}
	}
	var out []cloudEntry
	for i := lo; i <= hi && i < len(v.visible); i++ {
		if e := v.visible[i]; !v.sqlMarker(e.Seq) {
			out = append(out, e)
		}
	}
	return out
}

// sqlDetailStep is the detail panel's previous and next entry in SQL mode, where the step
// passes over the dividers. It reports whether it took the key.
func (v *cloudViewer) sqlDetailStep(k []byte) bool {
	if v.mode != cloudModeSQL || !v.detailOn {
		return false
	}
	nav, isNav := decodeNav(k)
	by := 0
	switch {
	case len(k) == 1 && k[0] == '[', isNav && !nav.shift && nav.dir == "left":
		by = -1
	case len(k) == 1 && k[0] == ']', isNav && !nav.shift && nav.dir == "right":
		by = 1
	default:
		return false
	}
	i, ok := v.sqlPos(v.detail.Seq)
	if !ok { // the open entry left the result: start from the end the step comes from
		i = -1
		if by < 0 {
			i = len(v.visible)
		}
	}
	for i += by; i >= 0 && i < len(v.visible); i += by {
		if seq := v.visible[i].Seq; !v.sqlMarker(seq) {
			v.selectEntries(selection{on: true, anchor: seq, cursor: seq})
			v.reveal(i)
			v.openDetail(seq)
			break
		}
	}
	return true
}

// renderCloudMarker draws one display row of a divider, w cells wide, like the app's synthetic
// row: centred and dim, with no time, badge or tag.
func renderCloudMarker(text string, w int) string {
	if w <= 0 {
		return ""
	}
	row, _, _ := centeredSpan(sanitize(text), sgrDim, w)
	return row
}

// MARK: the bar's keys

// isSQLRunKey is Ctrl+R, or ⌘↩ where the terminal passes it on (the Kitty keyboard form).
func isSQLRunKey(k []byte) bool {
	return (len(k) == 1 && k[0] == 0x12) || string(k) == "\x1b[13;9u"
}

// sqlRunKey runs the statement on the run key, anywhere in SQL mode. It reports whether it
// took the key.
func (v *cloudViewer) sqlRunKey(k []byte) bool {
	if v.mode != cloudModeSQL || !isSQLRunKey(k) {
		return false
	}
	v.runSQL(false)
	return true
}

// focusSQLBar gives the keys to the editor, switching to SQL mode first.
func (v *cloudViewer) focusSQLBar() {
	v.setMode(cloudModeSQL)
	v.focus, v.sql.stop, v.sql.editing = cloudSQLBar, sqlStopEditor, true
}

// sqlActivate is Enter or a click on the focused stop.
func (v *cloudViewer) sqlActivate() {
	switch v.sql.stop {
	case sqlStopExamples:
		v.openLabelExamples()
	case sqlStopLabels:
		v.openSQLLabelsMenu(v.anchor("sqlLabels"))
	case sqlStopSchema:
		v.sheet = &cloudSQLSchema{v: v}
	case sqlStopTemplates:
		v.openSQLTemplatesMenu(v.anchor("sqlTemplates"))
	case sqlStopSave:
		v.openSaveSQLTemplate()
	case sqlStopRun:
		v.runSQL(false)
	case sqlStopEditor:
		v.sql.editing = true
	}
}

// sqlKey: in the editor the keys edit the text (Tab indents) and Esc hands them back to the
// bar, as in the override editor. On the bar Tab, Shift-Tab and the arrows move between the
// stops, Enter or space presses one, and Esc gives the keys back to the list.
func (v *cloudViewer) sqlKey(k []byte) {
	s := &v.sql
	at := len(sqlStops) - 1
	for i, stop := range sqlStops {
		if stop == s.stop {
			at = i
		}
	}
	s.stop = sqlStops[at]
	move := func(by int) {
		s.stop, s.editing = sqlStops[(at+by+len(sqlStops))%len(sqlStops)], false
	}
	if s.stop == sqlStopEditor && s.editing {
		switch {
		case isEsc(k):
			s.editing = false
		case string(k) == "\x1b\t": // Esc and Tab together: out of the text to the next stop
			move(1)
		case string(k) == "\x1b[99;9u": // ⌘C copies the selection
			if text := s.text.selectedText(); text != "" {
				v.copy(text)
			}
		case len(k) == 1 && k[0] == 0x18, string(k) == "\x1b[120;9u": // Ctrl-X or ⌘X cuts it
			if text := s.text.cut(); text != "" {
				v.copy(text)
				v.noteSQLText()
			}
		default:
			if s.text.handle(k, max(1, s.editRows-1)) {
				v.noteSQLText()
			}
		}
		return
	}
	nav, isNav := decodeNav(k)
	switch {
	case isEsc(k):
		v.focus = cloudList
	case string(k) == "\x1b[Z", isArrowUp(k), isNav && nav.dir == "left":
		move(-1)
	case len(k) == 1 && k[0] == '\t', isArrowDown(k), isNav && nav.dir == "right":
		move(1)
	case isEnter(k) || (len(k) == 1 && k[0] == ' '):
		v.sqlActivate()
	}
}

// sqlBarAt is whether a screen row is in the SQL bar.
func (v *cloudViewer) sqlBarAt(y int) bool {
	s := &v.sql
	return v.mode == cloudModeSQL && s.top > 0 && y >= s.top && y < s.top+s.height
}

// sqlClick is a click on the bar at a screen cell.
func (v *cloudViewer) sqlClick(x, y int) {
	s := &v.sql
	row, col := y-s.top, x-1
	v.focus = cloudSQLBar
	if s.editFirst >= 0 && row >= s.editFirst && row < s.editFirst+s.editRows {
		s.stop, s.editing = sqlStopEditor, true
		s.text.click(row-s.editFirst, max(0, col-1))
		return
	}
	for i := len(s.hits) - 1; i >= 0; i-- {
		if h := s.hits[i]; h.row == row && col >= h.x0 && col <= h.x1 {
			h.act(col)
			return
		}
	}
}

// sqlWheel scrolls the editor, by moving its cursor.
func (v *cloudViewer) sqlWheel(by int) {
	t := &v.sql.text
	t.ensure()
	t.row = max(0, min(t.row-by, len(t.lines)-1))
	t.col = min(t.col, len(t.lines[t.row]))
}

// MARK: the bar

// sqlBarReserve is the rows the query builder bar leaves for the SQL bar.
func (v *cloudViewer) sqlBarReserve() int {
	if v.mode == cloudModeSQL {
		return 4
	}
	return 0
}

// sqlBarLines draws the SQL bar w cells wide in at most budget rows, its first row on screen
// row top: the status and the controls, the editor, then the hint or the error. A pane with no
// room for three rows shows no bar. When the controls take more rows than there are, the rows
// around the focused one are shown.
func (v *cloudViewer) sqlBarLines(w, budget, top int) []string {
	s := &v.sql
	s.top, s.height, s.hits, s.editFirst, s.editRows = 0, 0, nil, -1, 0
	if v.mode != cloudModeSQL || budget < 3 || w < 1 {
		s.noRoom = v.mode == cloudModeSQL
		return nil
	}
	s.noRoom = false
	const (
		plain   = sgrUnder
		focused = sgrBold + sgrRev
	)
	c := &formColumn{w: w}
	focusLine := -1
	anchorLine := map[string][2]int{} // a menu control's line and column
	on := func(kind string) bool { return v.focus == cloudSQLBar && s.stop == kind }
	type control struct{ kind, label, style, focusStyle string }
	run := "Run"
	if s.running {
		run = "Running…"
	}
	controls := []control{
		{sqlStopExamples, cloudLabelExamplesTitle, plain, focused},
		{sqlStopLabels, "Labels " + cloudGlyphChevron, plain, focused},
		{sqlStopSchema, "Schema", plain, focused},
		{sqlStopTemplates, "Templates " + cloudGlyphChevron, plain, focused},
		{sqlStopSave, "Save…", plain, focused},
		{sqlStopRun, " " + run + " ", sgrGreen + sgrUnder, badgeStyle(colorGreen)},
	}
	button := func(row *string, ct control) {
		style := ct.style
		if on(ct.kind) {
			style, focusLine = ct.focusStyle, len(c.lines)
		}
		anchorLine[ct.kind] = [2]int{len(c.lines), cellWidth(stripSGR(*row))}
		c.button(row, ct.label, style, func() {
			v.focus, s.stop, s.editing = cloudSQLBar, ct.kind, false
			v.sqlActivate()
		})
	}

	// The status at the left and the controls at the right, on one row where they fit. Else
	// the controls go under the status, on as many rows as they need.
	status := fmt.Sprintf("SQL filter · %d shown · re-runs live", s.count)
	total := 2 * (len(controls) - 1)
	for _, ct := range controls {
		total += cellWidth(ct.label)
	}
	wrapped := 1+cellWidth(status)+2+total+1 > w
	if !wrapped {
		row := " " + sgrDim + status + sgrReset + strings.Repeat(" ", w-2-cellWidth(status)-total)
		for i, ct := range controls {
			if i > 0 {
				row += "  "
			}
			button(&row, ct)
		}
		c.add(row)
	} else {
		c.add(" " + sgrDim + clip(status, w-2) + sgrReset)
		row := " "
		for _, ct := range controls {
			if used := cellWidth(stripSGR(row)); used > 1 && used+2+cellWidth(ct.label) > w-1 {
				c.add(row)
				row = " "
			}
			if cellWidth(stripSGR(row)) > 1 {
				row += "  "
			}
			button(&row, ct)
		}
		c.add(row)
	}

	// The error in place of the hint, on at most two rows.
	hint := []string{sgrDim + clip(cloudSQLHint, w-2) + sgrReset}
	if parts := wrapWords(sanitize(s.err), w-2); len(parts) > 0 {
		if len(parts) > 2 {
			parts = []string{parts[0], clip(strings.Join(parts[1:], " "), w-2)}
		}
		hint = hint[:0]
		for _, part := range parts {
			hint = append(hint, sgrRed+part+sgrReset)
		}
	}

	// The editor takes what the rest leaves, up to its height. Short of a row for it the
	// error keeps one row, then the head gives up rows.
	head, start := len(c.lines), 0
	if budget-head-len(hint) < 1 {
		hint = hint[:1]
	}
	if keep := max(0, budget-1-len(hint)); head > keep {
		if wrapped {
			start = 1 // the status goes first
		}
		if focusLine >= 0 {
			start = max(min(start, focusLine), focusLine-keep+1)
		}
		start = max(0, min(start, head-keep))
		head = keep
	}
	rows := max(1, min(cloudSQLEditorRows, budget-head-len(hint)))

	out := &formColumn{w: w}
	out.lines = append(out.lines, c.lines[start:start+head]...)
	gutter := sgrDim
	if on(sqlStopEditor) {
		gutter = sgrBold
	}
	s.editFirst, s.editRows = head, rows
	for _, line := range s.text.view(max(1, w-2), rows, on(sqlStopEditor) && s.editing) {
		out.add(gutter + "│" + sgrReset + line)
	}
	for _, line := range hint {
		out.add(" " + line)
	}
	for _, h := range c.hits {
		if h.row -= start; h.row >= 0 && h.row < head {
			s.hits = append(s.hits, h)
		}
	}
	for name, kind := range map[string]string{"sqlLabels": sqlStopLabels, "sqlTemplates": sqlStopTemplates} {
		at := anchorLine[kind]
		v.anchors[name] = [2]int{at[1] + 1, top + at[0] - start + 1}
	}
	s.top, s.height = top, len(out.lines)
	return out.lines
}

// MARK: the menus

// openSQLLabelsMenu is the bar's Labels menu: the project's label keys, each inserting a
// filter on that label at the cursor.
func (v *cloudViewer) openSQLLabelsMenu(x, y int) {
	keys := v.project.labelKeys()
	if len(keys) == 0 {
		v.menu = &popupMenu{x: x, y: y, selected: -1, items: []menuItem{{label: "No labels detected yet — run a session once"}}}
		return
	}
	items := []menuItem{{label: "Insert a label filter"}}
	for _, key := range keys {
		items = append(items, menuItem{sanitize(key), func() { v.sqlInsert(cloudSQLLabelFilter(key)) }})
	}
	v.menu = &popupMenu{x: x, y: y, items: items, selected: 1}
}

// openSQLTemplatesMenu is the bar's Templates menu: the saved templates and the app's starters
// under their headings, then the statement generated from the session's filter.
func (v *cloudViewer) openSQLTemplatesMenu(x, y int) {
	var items []menuItem
	add := func(heading string, templates []cloudSqlTemplate) {
		if len(templates) == 0 {
			return
		}
		items = append(items, menuItem{label: heading})
		for _, t := range templates {
			items = append(items, menuItem{sanitize(t.Name), func() { v.applySQLText(t.SQL) }})
		}
	}
	add("Saved", v.sqlTemplates)
	add("Starters", builtinSqlTemplates)
	items = append(items, menuItem{}, menuItem{"From current filter", func() {
		v.regenerateSQL()
		v.runSQL(false)
	}})
	menu := &popupMenu{x: x, y: y, items: items}
	for i, item := range items { // start on the first template, past its heading
		if item.act != nil {
			menu.selected = i
			break
		}
	}
	v.menu = menu
}

// MARK: the schema popup

// cloudSQLSchema is the app's Schema popover: the columns of log_entry with their type and
// note. Enter or a click inserts a column's name at the cursor.
type cloudSQLSchema struct {
	v        *cloudViewer
	selected int
	scroll   int // the first line of the list drawn

	// As last drawn (0-based).
	top, left, width, height int
	hits                     []boxHit
}

func (p *cloudSQLSchema) pick(i int) {
	p.v.closeSheet()
	if i >= 0 && i < len(cloudSQLColumns) {
		p.v.sqlInsert(cloudSQLColumns[i].name)
	}
}

func (p *cloudSQLSchema) move(by int) {
	p.selected = clampIndex(p.selected+by, len(cloudSQLColumns))
}

func (p *cloudSQLSchema) key(k []byte) {
	switch {
	case isEsc(k), len(k) == 1 && k[0] == 'q':
		p.v.closeSheet()
	case isUp(k):
		p.move(-1)
	case isDown(k):
		p.move(1)
	case isPageUp(k):
		p.move(-5)
	case isPageDown(k):
		p.move(5)
	case isEnter(k):
		p.pick(p.selected)
	}
}

func (p *cloudSQLSchema) mouse(m mouseEvent) {
	const wheelUp, wheelDown = 64, 65
	if !m.press {
		return
	}
	switch m.button {
	case wheelUp:
		p.move(-1)
	case wheelDown:
		p.move(1)
	case 0:
		row, col := m.y-1-p.top, m.x-1-p.left
		if row < 0 || row >= p.height || col < 0 || col >= p.width {
			p.v.closeSheet() // a click outside closes the popover
			return
		}
		for i := len(p.hits) - 1; i >= 0; i-- {
			if h := p.hits[i]; h.row == row && col >= h.x0 && col <= h.x1 {
				h.act(col)
				return
			}
		}
	}
}

// box draws the popup centered in a pane rows by cols: the heading, then each column's name
// with its type and note beside it, or under it in a narrow pane. A pane too small for it
// shows nothing, and Esc still closes it.
func (p *cloudSQLSchema) box(rows, cols int) (box []string, top, left int) {
	p.hits, p.width, p.height = nil, 0, 0
	w := min(cols, 84)
	if w < 30 || rows < 6 {
		return nil, 0, 0
	}
	b := &popupBox{w: w, inner: w - 4, rows: []string{"╭" + strings.Repeat("─", w-2) + "╮"}}
	b.add(sgrDim + clip("TABLE  log_entry  —  click a column to insert it", b.inner) + sgrReset)
	b.rows = append(b.rows, "├"+strings.Repeat("─", w-2)+"┤")

	nameW := 0
	for _, col := range cloudSQLColumns {
		nameW = max(nameW, cellWidth(col.name))
	}
	beside := b.inner-nameW-2 >= 24
	// Each column's lines, plain, and the column each line belongs to.
	var lines []string
	var owner, first []int
	for i, col := range cloudSQLColumns {
		first = append(first, len(lines))
		indent, noteW := strings.Repeat(" ", nameW+2), b.inner-nameW-2
		if !beside {
			indent, noteW = "  ", b.inner-2
		}
		lines = append(lines, fit(col.name, nameW)+"  "+col.kind)
		for _, part := range wrapWords(col.note, noteW) {
			lines = append(lines, indent+part)
		}
		for len(owner) < len(lines) {
			owner = append(owner, i)
		}
	}
	p.selected = clampIndex(p.selected, len(cloudSQLColumns))
	room := max(1, min(len(lines), rows-4))
	// Keep the selected column's lines in view, its name first.
	from := first[p.selected]
	to := len(lines)
	if p.selected+1 < len(first) {
		to = first[p.selected+1]
	}
	if to > p.scroll+room {
		p.scroll = to - room
	}
	p.scroll = max(0, min(min(p.scroll, from), len(lines)-room))
	for i := p.scroll; i < p.scroll+room && i < len(lines); i++ {
		col := owner[i]
		b.hit(0, b.inner-1, func(int) { p.pick(col) })
		line, name := clip(lines[i], b.inner), cloudSQLColumns[col].name
		switch {
		case col == p.selected:
			b.add(sgrRev + sgrBold + fit(line, b.inner) + sgrReset)
		case i == first[col] && strings.HasPrefix(line, name):
			// The name in its color, the type dim after it.
			b.add(sgrCyan + sgrBold + name + sgrReset + sgrDim + strings.TrimPrefix(line, name) + sgrReset)
		default:
			b.add(line)
		}
	}
	box = b.close()
	if len(box) > rows { // keep the borders, drop the rows that don't fit
		box = append(box[:rows-1], box[len(box)-1])
	}
	for _, h := range b.hits {
		if h.row < len(box)-1 {
			p.hits = append(p.hits, h)
		}
	}
	p.width, p.height = w, len(box)
	p.top, p.left = max(0, (rows-len(box))/2), max(0, (cols-w)/2)
	return box, p.top, p.left
}

// MARK: the label examples sheet

// labelCardinality is CloudLabelCardinality: how many distinct values a label key has in the
// captured entries, and the scope it was found in (labels or resource).
type labelCardinality struct {
	scope, key string
	distinct   int
}

// mergeLabelCardinalities is CloudSqlAssistant.cardinalities followed by the sheet's merge: the
// rows of cloudLabelCardinalitySQL as one row per bare key, sorted by key. A key in both scopes
// shares one rule, so it shows once, with the larger count.
func mergeLabelCardinalities(rows [][]*string) []labelCardinality {
	byKey := map[string]labelCardinality{}
	for _, row := range rows {
		scope, okScope := sqlCell(row, 0)
		key, okKey := sqlCell(row, 1)
		count, okCount := sqlCell(row, 2)
		n, err := strconv.Atoi(count)
		if !okScope || !okKey || !okCount || err != nil {
			continue
		}
		if have, ok := byKey[key]; ok && have.distinct >= n {
			continue
		}
		byKey[key] = labelCardinality{scope: scope, key: key, distinct: n}
	}
	out := make([]labelCardinality, 0, len(byKey))
	for _, row := range byKey {
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out
}

// cloudLabelExamples is the app's CloudSqlLabelExamplesSheet: how many example values of each
// label key the app's SQL assistant is sent, kept per log name. Each key has an All toggle and
// a count from 1 to its distinct values.
type cloudLabelExamples struct {
	v       *cloudViewer
	loading bool
	rows    []labelCardinality
	rules   map[string]labelExampleRule
	focus   int // a row, then Cancel and Save

	// As last drawn (0-based).
	top, left int
	hits      []boxHit
}

// openLabelExamples shows the sheet with the saved rules and asks jacad for the label keys.
// As in the app, a session with nothing captured has none.
func (v *cloudViewer) openLabelExamples() {
	sheet := &cloudLabelExamples{v: v, rules: map[string]labelExampleRule{}}
	for key, rule := range v.project.LabelExampleRulesByLogName[v.cfg.LogName] {
		sheet.rules[key] = rule
	}
	v.sheet = sheet
	if !v.stream.HasData || v.query == nil {
		return
	}
	sheet.loading = true
	v.query(cloudLabelCardinalitySQL, func(result dbResultSet, err error) {
		if err != nil {
			result = dbResultSet{}
		}
		sheet.rows, sheet.loading = mergeLabelCardinalities(result.Rows), false
	})
}

// rule is a key's rule, the default (one value) when it has none.
func (p *cloudLabelExamples) rule(key string) labelExampleRule {
	r := p.rules[key]
	r.Count = max(1, r.Count)
	return r
}

func (p *cloudLabelExamples) toggleAll(i int) {
	if i < 0 || i >= len(p.rows) {
		return
	}
	r := p.rule(p.rows[i].key)
	r.All = !r.All
	p.rules[p.rows[i].key] = r
}

// step changes a key's count, within 1 and its distinct values. The app's stepper is disabled
// for a key set to All and for one with a single value.
func (p *cloudLabelExamples) step(i, by int) {
	if i < 0 || i >= len(p.rows) {
		return
	}
	row := p.rows[i]
	r := p.rule(row.key)
	if r.All || row.distinct <= 1 {
		return
	}
	r.Count = min(max(1, r.Count+by), max(1, row.distinct))
	p.rules[row.key] = r
}

// pruned is the rules worth saving: the ones that differ from the default.
func (p *cloudLabelExamples) pruned() map[string]labelExampleRule {
	out := map[string]labelExampleRule{}
	for key := range p.rules {
		if r := p.rule(key); r.All || r.Count != 1 {
			out[key] = r
		}
	}
	return out
}

func (p *cloudLabelExamples) save() {
	if p.loading {
		return
	}
	p.v.call("cloud.setLabelExampleRules", map[string]any{
		"project": p.v.cfg.ProjectID, "logName": p.v.cfg.LogName, "rules": p.pruned(),
	})
	p.v.closeSheet()
}

// key: the arrows, Tab and Shift-Tab move over the rows and the two buttons. On a row space or
// Enter toggles All and left and right (or - and +) change the count. Esc cancels.
func (p *cloudLabelExamples) key(k []byte) {
	n := len(p.rows) + 2
	p.focus = clampIndex(p.focus, n)
	nav, isNav := decodeNav(k)
	left := isNav && nav.dir == "left"
	right := isNav && nav.dir == "right"
	press := isEnter(k) || (len(k) == 1 && k[0] == ' ')
	switch {
	case isEsc(k):
		p.v.closeSheet()
	case string(k) == "\x1b[Z", isArrowUp(k):
		p.focus = (p.focus - 1 + n) % n
	case len(k) == 1 && k[0] == '\t', isArrowDown(k):
		p.focus = (p.focus + 1) % n
	case p.focus < len(p.rows):
		switch {
		case press:
			p.toggleAll(p.focus)
		case left, len(k) == 1 && k[0] == '-':
			p.step(p.focus, -1)
		case right, len(k) == 1 && (k[0] == '+' || k[0] == '='):
			p.step(p.focus, 1)
		}
	case left || right:
		p.focus = len(p.rows) + 1 - (p.focus - len(p.rows))
	case press && p.focus == len(p.rows):
		p.v.closeSheet()
	case press:
		p.save()
	}
}

func (p *cloudLabelExamples) mouse(m mouseEvent) {
	const wheelUp, wheelDown = 64, 65
	if !m.press {
		return
	}
	switch m.button {
	case wheelUp:
		p.focus = clampIndex(p.focus-1, len(p.rows)+2)
	case wheelDown:
		p.focus = clampIndex(p.focus+1, len(p.rows)+2)
	case 0:
		row, col := m.y-1-p.top, m.x-1-p.left
		for i := len(p.hits) - 1; i >= 0; i-- {
			if h := p.hits[i]; h.row == row && col >= h.x0 && col <= h.x1 {
				h.act(col)
				return
			}
		}
	}
}

// box draws the sheet centered in a pane rows by cols: the app's text, then two lines a key
// (its controls beside it, its scope and distinct values under it), then the buttons. A pane
// too small for it shows nothing, and Esc still closes it.
func (p *cloudLabelExamples) box(rows, cols int) (box []string, top, left int) {
	p.hits = nil
	w := min(cols, 64)
	if w < 32 || rows < 8 {
		return nil, 0, 0
	}
	b := newPopupBox(cloudLabelExamplesTitle, w)
	const body = "How many example values of each label to send to Claude, so it learns the " +
		"format. Default is 1. Set a low-cardinality label (e.g. tag) to All; keep a " +
		"high-cardinality one (e.g. user_id) at 1."
	text := wrapWords(body, b.inner)
	// The rows left for the keys, two lines each, once the text, the buttons, the blank rows
	// around the list and the borders have theirs. A short pane keeps the text's first line.
	if over := len(text) + 7 - rows; over > 0 {
		text = text[:max(1, len(text)-over)]
	}
	for _, part := range text {
		b.add(sgrDim + part + sgrReset)
	}
	b.add("")
	n := len(p.rows) + 2
	p.focus = clampIndex(p.focus, n)
	switch {
	case p.loading:
		row, _, _ := centeredSpan(cloudGlyphLoading, sgrDim, b.inner)
		b.add(row)
	case len(p.rows) == 0:
		for _, part := range wrapWords("No labels detected yet — capture some logs first.", b.inner) {
			b.add(sgrDim + part + sgrReset)
		}
	default:
		// The count's cells: the widest it can show, "all" at least.
		valW := 3
		for _, row := range p.rows {
			valW = max(valW, len(strconv.Itoa(max(1, row.distinct))))
		}
		valW = min(valW, 7)
		const toggleW = 5 // "● All"
		keyW := max(1, b.inner-1-toggleW-2-valW-4)
		room := max(1, min(8, (rows-len(text)-6)/2))
		start := listWindow(min(p.focus, len(p.rows)-1), room)
		for i := start; i < len(p.rows) && i < start+room; i++ {
			row := p.rows[i]
			r := p.rule(row.key)
			name := fit(sanitize(row.key), keyW)
			if i == p.focus {
				name = sgrRev + sgrBold + name + sgrReset
			}
			toggle, toggleStyle := "○ All", sgrDim
			if r.All {
				toggle, toggleStyle = "● All", sgrGreen
			}
			value, stepStyle := strconv.Itoa(r.Count), ""
			if r.All {
				value = "all"
			}
			if r.All || row.distinct <= 1 {
				stepStyle = sgrDim
			}
			value = clip(value, valW)
			value = strings.Repeat(" ", valW-cellWidth(value)) + value
			b.hit(0, keyW-1, func(int) { p.focus = i })
			line := name + " "
			b.button(&line, toggle, toggleStyle, func() { p.focus = i; p.toggleAll(i) })
			line += "  "
			b.button(&line, "−", stepStyle, func() { p.focus = i; p.step(i, -1) })
			line += stepStyle + " " + value + " " + sgrReset
			b.button(&line, "+", stepStyle, func() { p.focus = i; p.step(i, 1) })
			b.add(line)
			plural := "s"
			if row.distinct == 1 {
				plural = ""
			}
			caption := fmt.Sprintf("%s · %d value%s", sanitize(row.scope), row.distinct, plural)
			b.add(sgrDim + clip(caption, b.inner) + sgrReset)
		}
	}
	b.add("")
	// The buttons at the right: the header's Cancel, then the footer's Save.
	const cancel, save = " Cancel ", " Save "
	line := strings.Repeat(" ", max(0, b.inner-len(cancel)-2-len(save)))
	style := func(button int, plain string) string {
		if p.focus == len(p.rows)+button {
			return sgrBold + sgrRev
		}
		return plain
	}
	b.button(&line, cancel, style(0, sgrUnder), p.v.closeSheet)
	line += "  "
	saveStyle := sgrUnder
	if p.loading {
		saveStyle = sgrDim
	}
	b.button(&line, save, style(1, saveStyle), p.save)
	b.add(line)
	box = b.close()
	if len(box) > rows { // keep the borders, drop the rows that don't fit
		box = append(box[:rows-1], box[len(box)-1])
	}
	for _, h := range b.hits {
		if h.row < len(box)-1 {
			p.hits = append(p.hits, h)
		}
	}
	p.top, p.left = max(0, (rows-len(box))/2), max(0, (cols-w)/2)
	return box, p.top, p.left
}
