package main

import (
	"strconv"
	"strings"
)

// The client side of one Cloud Logging session, with no I/O: the caller subscribes, makes the
// calls and hands the results in. It ports the gap handling of RemoteCloudFeed
// (Sources/Model/Daemon/RemoteCloudFeed.swift) and the scrollback, instant search and SQL
// mapping of CloudLogSession (Sources/Model/CloudLogSession.swift).

// cloudFeedDefaultLimit is CloudLogSession's ringCap, used when a feed is made without a limit.
const cloudFeedDefaultLimit = 500_000

// cloudFeed holds a session's loaded entries in display order (oldest first, seqs ascending)
// and keeps them free of gaps and duplicates.
//
// How the caller drives it:
//   - cloud.entries.<id> event: apply.
//   - cloud.older.<id> event: prepend.
//   - cloud.sessions.open returned: opened(info.Existed), then a backfill with the pair below.
//   - events.dropped for the entries topic, and the backfill after an open: beginResync, call
//     cloud.sessions.range with the afterSeq it returns (leaving afterSeq out when hasSeq is
//     false), then finishResync with the result, or with nil when the call failed. Every
//     beginResync needs its finishResync, since live batches are held until the last one.
//   - the user cleared the view or restarted the query: reset.
type cloudFeed struct {
	list  []cloudEntry
	limit int
	// lastSeq is the newest live seq delivered; live batches and ranges at or below it are
	// duplicates. hasSeq is false until something was delivered.
	lastSeq uint64
	hasSeq  bool
	// history is whether anything arrived from the daemon's current session, even if the view
	// was cleared since.
	history bool
	// resyncs counts the range calls in flight. While it is above zero live batches wait in
	// held, so lastSeq can't move past a gap before the entries that fill it arrive.
	resyncs int
	held    [][]cloudEntry
	trimmed int
}

// newCloudFeed makes an empty feed that keeps at most limit entries. A limit below 1 is the
// app's 500,000.
func newCloudFeed(limit int) *cloudFeed {
	if limit < 1 {
		limit = cloudFeedDefaultLimit
	}
	return &cloudFeed{limit: limit}
}

// entries is the loaded entries, oldest first. The slice is the feed's own: read it, don't
// change it, and don't keep it across a call that adds entries.
func (f *cloudFeed) entries() []cloudEntry {
	return f.list
}

// apply takes a live batch. Entries at or below the newest seq delivered are skipped. During a
// resync the batch is held and 0 is returned; finishResync delivers it.
func (f *cloudFeed) apply(batch []cloudEntry) (added int) {
	if f.resyncs > 0 {
		f.held = append(f.held, batch)
		return 0
	}
	return f.deliver(batch)
}

// deliver is RemoteCloudFeed.deliver followed by CloudLogSession.append: the entries newer than
// lastSeq go on the end, and what is over the limit comes off the front.
func (f *cloudFeed) deliver(batch []cloudEntry) (added int) {
	for _, e := range batch {
		if f.hasSeq && e.Seq <= f.lastSeq {
			continue
		}
		f.list = append(f.list, e)
		f.lastSeq, f.hasSeq = e.Seq, true
		added++
	}
	if added > 0 {
		f.history = true
	}
	if over := len(f.list) - f.limit; over > 0 {
		f.list = f.list[over:]
		f.trimmed += over
	}
	return added
}

// prepend takes a page of older entries, oldest first, and puts it before everything loaded.
// Entries whose seq is already loaded, or repeated in the page, are skipped.
//
// The limit is not applied here, as in the app: trimming comes off the front, so it would drop
// the page that was just asked for. The next live batch trims.
func (f *cloudFeed) prepend(page []cloudEntry) (added int) {
	if len(page) == 0 {
		return 0
	}
	loaded := make(map[uint64]bool, len(f.list)+len(page))
	for _, e := range f.list {
		loaded[e.Seq] = true
	}
	fresh := make([]cloudEntry, 0, len(page)+len(f.list))
	for _, e := range page {
		if !loaded[e.Seq] {
			loaded[e.Seq] = true
			fresh = append(fresh, e)
		}
	}
	added = len(fresh)
	if added > 0 {
		f.list = append(fresh, f.list...)
	}
	return added
}

// beginResync starts filling a gap: live batches are held from now on. It returns the seq to
// pass as cloud.sessions.range's afterSeq; hasSeq is false when nothing was delivered yet, and
// the call then takes the whole replay.
//
// A second drop during a resync begins another one. The held batches are released only when
// each has finished.
func (f *cloudFeed) beginResync() (afterSeq uint64, hasSeq bool) {
	f.resyncs++
	return f.lastSeq, f.hasSeq
}

// finishResync delivers the range a resync fetched (nil when the call failed) and, once no
// other resync is in flight, the batches held meanwhile. Both are de-duplicated by seq, so
// their overlap is harmless. It returns the entries added in all.
func (f *cloudFeed) finishResync(got []cloudEntry) (added int) {
	added = f.deliver(got)
	if f.resyncs > 0 {
		f.resyncs--
	}
	if f.resyncs > 0 {
		return added
	}
	held := f.held
	f.held = nil
	for _, batch := range held {
		added += f.deliver(batch)
	}
	return added
}

// reset empties the view: the user cleared it, or the query changed and the stream restarts.
// lastSeq stays, since live seqs keep rising across a reset and a range racing it must not
// hand back the entries just cleared. The caller also sends cloud.sessions.resetScrollback.
func (f *cloudFeed) reset() {
	f.list = nil
	f.trimmed = 0
}

// restart empties the view and forgets lastSeq: the daemon recreated the session, so its seqs
// start over and would collide with the old ones. Batches held from the old session are
// dropped for the same reason.
func (f *cloudFeed) restart() {
	f.reset()
	f.lastSeq, f.hasSeq = 0, false
	f.history = false
	f.held = nil
}

// opened takes the existed flag of a cloud.sessions.open result. When the open created the
// session and this feed had entries from an earlier one, the daemon restarted: the feed
// restarts and true is returned, so the caller can drop what it derived from the old entries.
func (f *cloudFeed) opened(existed bool) (restarted bool) {
	if existed || !f.history {
		return false
	}
	f.restart()
	return true
}

// dropped is how many entries the limit trimmed off the front since the last reset.
func (f *cloudFeed) dropped() int {
	return f.trimmed
}

// cloudEntryMatches is CloudLogSession.matches: the instant search over one loaded entry. The
// text is looked for, ignoring case, in the message, the log id, the severity's API name and
// the label keys and values. An empty search matches everything.
func cloudEntryMatches(e cloudEntry, search string) bool {
	if search == "" {
		return true
	}
	return entryContainsFold(e, strings.ToLower(search))
}

func entryContainsFold(e cloudEntry, lowered string) bool {
	if containsFold(e.Message, lowered) || containsFold(e.LogID, lowered) || containsFold(severityName(e.Severity), lowered) {
		return true
	}
	for key, value := range e.Labels {
		if containsFold(key, lowered) || containsFold(value, lowered) {
			return true
		}
	}
	return false
}

func containsFold(s, lowered string) bool {
	return strings.Contains(strings.ToLower(s), lowered)
}

// searchCloudEntries is the instant search over the loaded entries: the indexes of the ones
// that match, in order. An empty query matches all of them.
func searchCloudEntries(entries []cloudEntry, query string) []int {
	out := make([]int, 0, len(entries))
	lowered := strings.ToLower(query)
	for i, e := range entries {
		if query == "" || entryContainsFold(e, lowered) {
			out = append(out, i)
		}
	}
	return out
}

// cloudExportText is CloudLogSession.exportText: the raw JSON of each entry, one after another.
func cloudExportText(entries []cloudEntry) string {
	raws := make([]string, len(entries))
	for i, e := range entries {
		raws[i] = e.Raw
	}
	return strings.Join(raws, "\n")
}

// sqlRow is one row of the SQL mode's list, which shows a query's result in result order.
//
// A row that resolved to a loaded entry has Entry set, and Text and Severity are that entry's
// message and severity. Any other row is a marker (the app's synthetic entry, drawn as a dim
// centred divider and not selectable): Entry is nil, Text is the row's text_payload or message
// column ("" without one) and Severity is the level its severity_name column spells, DEFAULT
// without one. Cells is the row as the query returned it, in column order, with "" for NULL.
type sqlRow struct {
	Entry    *cloudEntry
	Marker   bool
	Text     string
	Severity int
	Cells    []string
}

// sqlMissingIDColumn is the app's message for a result that can't be mapped to the list.
const sqlMissingIDColumn = "Your SQL must SELECT an `insert_id` (or `seq`) column so the matching logs can be shown — e.g. SELECT insert_id, … FROM log_entry …"

// sqlColumns is SqlResultColumns: the columns the list needs, found by name ignoring case.
// An index is -1 when the result has no such column.
type sqlColumns struct {
	id, seq, message, severity, marker int
}

func findSQLColumns(columns []string) sqlColumns {
	find := func(names ...string) int {
		for i, column := range columns {
			if containsString(names, strings.ToLower(column)) {
				return i
			}
		}
		return -1
	}
	return sqlColumns{
		id:       find("insert_id"),
		seq:      find("seq"),
		message:  find("text_payload", "message"),
		severity: find("severity_name"),
		marker:   find("is_marker", "is_synthetic", "marker"),
	}
}

// sqlCell reads a column of a row. A missing column, a short row and NULL all read as absent.
func sqlCell(row []*string, col int) (string, bool) {
	if col < 0 || col >= len(row) || row[col] == nil {
		return "", false
	}
	return *row[col], true
}

// isMarker is SqlResultColumns.isMarker: the is_marker-style column is 1, true or yes.
func (c sqlColumns) isMarker(row []*string) bool {
	v, _ := sqlCell(row, c.marker)
	switch strings.ToLower(v) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// mapSqlRows is the SQL mode's list for a query result: mapSqlRowsMatching with no search.
func mapSqlRows(result dbResultSet, entries []cloudEntry) (rows []sqlRow, problem string) {
	return mapSqlRowsMatching(result, entries, "")
}

// mapSqlRowsMatching is CloudLogSession.mapSqlRows with the check applySqlResult makes first.
// Each result row, in order, resolves to a loaded entry by insert_id and then by seq (insert_id
// first because it survives a restarted stream, where seqs are stamped again). A row flagged by
// an is_marker column, and a row that resolves to nothing, becomes a marker built from its own
// columns, so a query can inject dividers. An entry shows once, at its first row. Rows that
// don't match the instant search are left out.
//
// problem is the app's message when the result has neither an insert_id nor a seq column, and
// rows is then nil.
func mapSqlRowsMatching(result dbResultSet, entries []cloudEntry, search string) (rows []sqlRow, problem string) {
	cols := findSQLColumns(result.Columns)
	if cols.id < 0 && cols.seq < 0 {
		return nil, sqlMissingIDColumn
	}
	byID := make(map[string]int, len(entries))
	bySeq := make(map[uint64]int, len(entries))
	for i, e := range entries {
		if e.InsertID != "" {
			byID[e.InsertID] = i
		}
		bySeq[e.Seq] = i
	}
	rows = make([]sqlRow, 0, len(result.Rows))
	seen := make(map[uint64]bool, len(result.Rows))
	for _, row := range result.Rows {
		index, found := 0, false
		if !cols.isMarker(row) {
			if id, ok := sqlCell(row, cols.id); ok && id != "" {
				index, found = lookup(byID, id)
			}
			if !found {
				if text, ok := sqlCell(row, cols.seq); ok {
					if seq, err := strconv.ParseUint(text, 10, 64); err == nil {
						index, found = lookup(bySeq, seq)
					}
				}
			}
		}
		cells := make([]string, len(row))
		for i, cell := range row {
			if cell != nil {
				cells[i] = *cell
			}
		}
		if found {
			e := &entries[index]
			if seen[e.Seq] || !cloudEntryMatches(*e, search) {
				continue
			}
			seen[e.Seq] = true
			rows = append(rows, sqlRow{Entry: e, Text: e.Message, Severity: e.Severity, Cells: cells})
			continue
		}
		text, _ := sqlCell(row, cols.message)
		name, _ := sqlCell(row, cols.severity)
		marker := sqlRow{Marker: true, Text: text, Severity: severityFromName(name), Cells: cells}
		// The search sees a marker as the app's synthetic entry: its message and severity only.
		if !cloudEntryMatches(cloudEntry{Message: marker.Text, Severity: marker.Severity}, search) {
			continue
		}
		rows = append(rows, marker)
	}
	return rows, ""
}

func lookup[K comparable](m map[K]int, key K) (int, bool) {
	i, ok := m[key]
	return i, ok
}
