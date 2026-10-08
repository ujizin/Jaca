package main

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// The pure Cloud Logging query logic, ported from Sources/Core/CloudLogging: severities, the
// filter builder, log names, time ranges, label ordering, Logs Explorer URLs and the SQL mode's
// templates. Every user-facing string is the app's.

// cloudSeverities is CloudSeverity.allCases: the API's LogSeverity scale, least severe first.
var cloudSeverities = []int{0, 100, 200, 300, 400, 500, 600, 700, 800}

// cloudCommonSeverities is CloudSeverity.commonLadder: the levels offered as quick picks.
var cloudCommonSeverities = []int{100, 200, 400, 500, 600}

var cloudSeverityNames = map[int]string{
	0: "DEFAULT", 100: "DEBUG", 200: "INFO", 300: "NOTICE", 400: "WARNING",
	500: "ERROR", 600: "CRITICAL", 700: "ALERT", 800: "EMERGENCY",
}

var cloudSeverityShorts = map[int]string{
	0: "·", 100: "D", 200: "I", 300: "N", 400: "W", 500: "E", 600: "C", 700: "A", 800: "!",
}

func isCloudSeverity(sev int) bool {
	_, ok := cloudSeverityNames[sev]
	return ok
}

// knownCloudSeverity is the level, or DEFAULT for a number that isn't one. The app can't hold
// such a number: its decoder reads an entry's unknown severity as DEFAULT.
func knownCloudSeverity(sev int) int {
	if isCloudSeverity(sev) {
		return sev
	}
	return 0
}

// severityName is CloudSeverity.apiName: the API's name, as filters and the SQL severity_name
// column spell it.
func severityName(sev int) string {
	return cloudSeverityNames[knownCloudSeverity(sev)]
}

// severityShort is CloudSeverity.short: the single-glyph badge of the dense log list.
func severityShort(sev int) string {
	return cloudSeverityShorts[knownCloudSeverity(sev)]
}

// severityTitle is CloudSeverity.name: the API name with only its first letter capital
// ("Error"), which the app uses to name a tab forked on a severity.
func severityTitle(sev int) string {
	name := severityName(sev)
	return name[:1] + strings.ToLower(name[1:])
}

// severityFromName is CloudSeverity(apiValue:): the level an API name spells, in any case.
// A missing or unknown name is DEFAULT.
func severityFromName(name string) int {
	upper := strings.ToUpper(name)
	for _, sev := range cloudSeverities {
		if sev != 0 && cloudSeverityNames[sev] == upper {
			return sev
		}
	}
	return 0
}

// matchModeLabel is CloudMatchMode.label: how the app names a match mode.
func matchModeLabel(mode string) string {
	switch mode {
	case cloudMatchExact:
		return "is exactly"
	case cloudMatchRegex:
		return "matches regex"
	case cloudMatchNotContains:
		return "doesn't contain"
	}
	return "contains"
}

// matchTerm is CloudMatchMode.term: one filter term for a field (a valid filter left-hand side
// such as textPayload or labels.foo) and the user's raw value.
func matchTerm(mode, field, value string) string {
	switch mode {
	case cloudMatchExact:
		return field + "=" + quoteFilterValue(value)
	case cloudMatchRegex:
		return field + "=~" + quoteFilterRegex(value)
	case cloudMatchNotContains:
		return "NOT " + field + ":" + quoteFilterValue(value)
	}
	return field + ":" + quoteFilterValue(value)
}

// quoteFilterValue is CloudFilter.quote: the value in double quotes, with \ and " escaped.
func quoteFilterValue(value string) string {
	escaped := strings.ReplaceAll(value, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `"` + escaped + `"`
}

// quoteFilterRegex is CloudFilter.quoteRegex: an RE2 pattern in double quotes. Backslashes are
// not doubled, since Cloud Logging hands them to RE2 as they are and a doubled \d would match a
// literal backslash. Only the quote is escaped.
func quoteFilterRegex(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
}

// quoteLabelKey is CloudFilter.quoteKeyIfNeeded: a key is quoted unless it is a bare identifier
// of letters, digits and underscores.
func quoteLabelKey(key string) string {
	if key == "" {
		return quoteFilterValue(key)
	}
	for _, r := range key {
		if !unicode.IsLetter(r) && !unicode.IsNumber(r) && r != '_' {
			return quoteFilterValue(key)
		}
	}
	return key
}

// labelField is LabelScope.field: the filter field of a label key in a scope.
func labelField(scope, key string) string {
	if scope == labelScopeResource {
		return "resource.labels." + quoteLabelKey(key)
	}
	return "labels." + quoteLabelKey(key)
}

// groupTerms is CloudFilter.group: terms joined with OR or AND, in parentheses only when there
// is more than one. "" when there are none.
func groupTerms(terms []string, or bool) string {
	switch len(terms) {
	case 0:
		return ""
	case 1:
		return terms[0]
	}
	sep := " AND "
	if or {
		sep = " OR "
	}
	return "(" + strings.Join(terms, sep) + ")"
}

// clauses is CloudLogQuery.clauses: the top-level filter clauses the query contributes, each
// already grouped. Conditions without a value (or, for a label, without a key) are skipped.
func (q cloudQuery) clauses() []string {
	var out []string

	var textTerms []string
	for _, c := range q.TextConditions {
		if c.Value != "" {
			textTerms = append(textTerms, matchTerm(orDefault(c.Mode, cloudMatchContains), "textPayload", c.Value))
		}
	}
	if group := groupTerms(textTerms, q.TextCombineOr); group != "" {
		out = append(out, group)
	}

	if len(q.SeveritySet) > 0 {
		names := make([]string, len(q.SeveritySet))
		for i, sev := range q.SeveritySet {
			names[i] = severityName(sev)
		}
		if len(names) == 1 {
			out = append(out, "severity="+names[0])
		} else {
			out = append(out, "severity=("+strings.Join(names, " OR ")+")")
		}
	} else if q.MinSeverity != nil && knownCloudSeverity(*q.MinSeverity) != 0 {
		out = append(out, "severity>="+severityName(*q.MinSeverity))
	}

	var labelTerms []string
	for _, c := range q.LabelConditions {
		if c.Key != "" && c.Value != "" {
			labelTerms = append(labelTerms, matchTerm(orDefault(c.Mode, cloudMatchExact), labelField(c.Scope, c.Key), c.Value))
		}
	}
	if group := groupTerms(labelTerms, q.LabelCombineOr); group != "" {
		out = append(out, group)
	}
	return out
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// buildFilter is CloudFilter.build: the AND of the logName clause, the time clause and the
// query's clauses, any of which may be empty. A non-blank rawFilter is the whole user portion,
// used as it is in parentheses, with only the time clause AND-ed on.
func buildFilter(logName, timeClause string, query cloudQuery, rawFilter string) string {
	var clauses []string
	if strings.TrimSpace(rawFilter) != "" {
		clauses = append(clauses, "("+rawFilter+")")
		if timeClause != "" {
			clauses = append(clauses, timeClause)
		}
		return strings.Join(clauses, " AND ")
	}
	if logName != "" {
		clauses = append(clauses, "logName="+quoteFilterValue(logName))
	}
	if timeClause != "" {
		clauses = append(clauses, timeClause)
	}
	clauses = append(clauses, query.clauses()...)
	return strings.Join(clauses, " AND ")
}

// buildCloudFilter is the filter the daemon's first query runs for cfg when started at now
// (CloudLogPoller's backfill filter). A live range then polls forward with a moving timestamp
// clause in place of this one.
func buildCloudFilter(cfg cloudStreamConfig, now time.Time) string {
	return buildFilter(cfg.LogName, cfg.TimeRange.clause(now), cfg.Query, cfg.RawFilter)
}

// cloudFilterWithoutTime is the session's filter with no time clause: what the app shares as a
// Logs Explorer URL and documents in the generated SQL.
func cloudFilterWithoutTime(cfg cloudStreamConfig) string {
	return buildFilter(cfg.LogName, "", cfg.Query, cfg.RawFilter)
}

// cloudTimestamp is CloudTimestamp.format: RFC 3339 in UTC with milliseconds.
func cloudTimestamp(t time.Time) string {
	return wireDate(t)
}

// start is CloudTimeRange.start: the first instant of the window.
func (r cloudTimeRange) start(now time.Time) time.Time {
	r = r.normalized()
	if r.Minutes > 0 {
		return now.Add(-time.Duration(r.Minutes) * time.Minute)
	}
	return r.Start
}

// clause is CloudTimeRange.clause: the timestamp filter of the first query.
func (r cloudTimeRange) clause(now time.Time) string {
	r = r.normalized()
	if r.Minutes > 0 {
		return `timestamp>="` + cloudTimestamp(r.start(now)) + `"`
	}
	return `timestamp>="` + cloudTimestamp(r.Start) + `" AND timestamp<="` + cloudTimestamp(r.End) + `"`
}

// cloudTimePreset is one of the relative ranges the app's toolbar offers.
type cloudTimePreset struct {
	Label   string
	Minutes int
}

// cloudTimePresets is CloudTimeRange.presets.
var cloudTimePresets = []cloudTimePreset{
	{"Last 5m", 5},
	{"Last 15m", 15},
	{"Last 1h", 60},
	{"Last 6h", 360},
	{"Last 24h", 1440},
	{"Last 7d", 10080},
}

// label is CloudTimeRange.label: the preset's label, else whole days, hours or minutes.
func (r cloudTimeRange) label() string {
	r = r.normalized()
	if r.Minutes <= 0 {
		return "Custom range"
	}
	for _, preset := range cloudTimePresets {
		if preset.Minutes == r.Minutes {
			return preset.Label
		}
	}
	switch {
	case r.Minutes%1440 == 0:
		return fmt.Sprintf("Last %dd", r.Minutes/1440)
	case r.Minutes%60 == 0:
		return fmt.Sprintf("Last %dh", r.Minutes/60)
	}
	return fmt.Sprintf("Last %dm", r.Minutes)
}

// percentEncodeUnreserved is addingPercentEncoding with only the RFC 3986 unreserved
// characters (ASCII letters and digits and "-._~") left as they are. Every other byte of the
// UTF-8 text becomes %XX.
func percentEncodeUnreserved(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// cloudLogNameFull is CloudLogName.full: projects/<id>/logs/<encoded> from a project id and a
// log id. A name that is already full is returned unchanged.
func cloudLogNameFull(projectID, short string) string {
	if strings.HasPrefix(short, "projects/") {
		return short
	}
	return "projects/" + projectID + "/logs/" + percentEncodeUnreserved(short)
}

// cloudLogID is CloudLogName.shortId: the percent-decoded log id after "/logs/" of a full name.
// A name without "/logs/" is returned unchanged.
func cloudLogID(fullName string) string {
	_, id, found := strings.Cut(fullName, "/logs/")
	if !found {
		return fullName
	}
	return percentDecoded(id)
}

// orderedLabelKeys is CloudLabelOrdering.ordered: the detected keys with the favorites first,
// both groups sorted. A favorite that is no longer detected is left out.
func orderedLabelKeys(detected, favorites []string) []string {
	favorite := make(map[string]bool, len(favorites))
	for _, key := range favorites {
		favorite[key] = true
	}
	var pinned, rest []string
	for _, key := range detected {
		if favorite[key] {
			pinned = append(pinned, key)
		} else {
			rest = append(rest, key)
		}
	}
	sort.Strings(pinned)
	sort.Strings(rest)
	return append(pinned, rest...)
}

// detectLabelKeys is LabelDetector.keys: the distinct entry-label keys of a batch, sorted.
func detectLabelKeys(entries []cloudEntry) []string {
	seen := map[string]bool{}
	var keys []string
	for _, e := range entries {
		for key := range e.Labels {
			if !seen[key] {
				seen[key] = true
				keys = append(keys, key)
			}
		}
	}
	sort.Strings(keys)
	return keys
}

// mergeLabelKeys is LabelDetector.merge: newly seen keys merged into a known list. When
// nothing is new the list comes back as it was, otherwise sorted and without duplicates.
func mergeLabelKeys(known, seen []string) (merged []string, changed bool) {
	have := make(map[string]bool, len(known))
	for _, key := range known {
		have[key] = true
	}
	for _, key := range seen {
		if !have[key] {
			changed = true
		}
	}
	if !changed {
		return known, false
	}
	for _, key := range seen {
		have[key] = true
	}
	merged = make([]string, 0, len(have))
	for key := range have {
		merged = append(merged, key)
	}
	sort.Strings(merged)
	return merged, true
}

const consoleURLBase = "https://console.cloud.google.com/logs/query"

// buildConsoleURL is CloudConsoleURL.build: a shareable Logs Explorer URL for a filter in a
// project, https://console.cloud.google.com/logs/query;query=<encoded>?project=<id>. A blank
// filter gives a project link with no query.
func buildConsoleURL(project, filter string) string {
	trimmed := strings.TrimSpace(filter)
	if trimmed == "" {
		return consoleURLBase + "?project=" + percentEncodeUnreserved(project)
	}
	return consoleURLBase + ";query=" + percentEncodeUnreserved(trimmed) + "?project=" + percentEncodeUnreserved(project)
}

// parseConsoleURL is CloudConsoleURL.parse: the project and the percent-decoded filter of a
// pasted Logs Explorer URL, each "" when the URL has none. The filter is read from the matrix
// parameter (…/logs/query;query=ENC;other=…), else from an ordinary query parameter.
func parseConsoleURL(text string) (project, query string) {
	pathPart, queryPart, _ := strings.Cut(text, "?")
	for _, kv := range strings.Split(queryPart, "&") {
		kv = strings.TrimLeft(kv, "=")
		name, value, found := strings.Cut(kv, "=")
		if !found || value == "" {
			continue
		}
		switch name {
		case "project":
			project = percentDecodedOrEmpty(value)
		case "query":
			if query == "" {
				query = percentDecodedOrEmpty(value)
			}
		}
	}
	if _, after, found := strings.Cut(pathPart, "query="); found {
		raw, _, _ := strings.Cut(after, ";")
		if decoded := percentDecodedOrEmpty(raw); decoded != "" {
			query = decoded
		}
	}
	return project, query
}

// percentDecodedOrEmpty is String.removingPercentEncoding with "" for text that can't be
// decoded, where Swift gives nil.
func percentDecodedOrEmpty(s string) string {
	decoded, err := url.PathUnescape(s)
	if err != nil || !utf8.ValidString(decoded) {
		return ""
	}
	return decoded
}

// looksLikeConsoleURL is CloudConsoleURL.looksLikeConsoleURL.
func looksLikeConsoleURL(s string) bool {
	return strings.Contains(s, "console.cloud.google.com/logs")
}

// cloudConsoleURL is CloudLogSession.consoleURL: the Logs Explorer URL of a session's filter.
func cloudConsoleURL(cfg cloudStreamConfig) string {
	return buildConsoleURL(cfg.ProjectID, cloudFilterWithoutTime(cfg))
}

// cloudSubtitle is CloudLogSession.subtitle: the project's title, the log id (or "URL filter"
// for a raw filter), the time range, and "SQL" while a SQL result drives the list or "filtered"
// when the structured query narrows the stream.
func cloudSubtitle(projectTitle string, cfg cloudStreamConfig, sqlActive bool) string {
	parts := []string{projectTitle}
	if cfg.RawFilter != "" {
		parts = append(parts, "URL filter")
	} else if cfg.LogName != "" {
		parts = append(parts, cloudLogID(cfg.LogName))
	}
	parts = append(parts, cfg.TimeRange.label())
	if sqlActive {
		parts = append(parts, "SQL")
	} else if cfg.RawFilter == "" && !cfg.Query.isEmpty() {
		parts = append(parts, "filtered")
	}
	return strings.Join(parts, " · ")
}

// withLabelFilter is CloudLogSession.filterByLabel: the query with any condition on this label
// replaced by one exact match.
func (q cloudQuery) withLabelFilter(scope, key, value string) cloudQuery {
	var kept []labelCondition
	for _, c := range q.LabelConditions {
		if c.Scope != scope || c.Key != key {
			kept = append(kept, c)
		}
	}
	c := newLabelCondition()
	c.Scope, c.Key, c.Value = scope, key, value
	q.LabelConditions = append(kept, c)
	return q
}

// orLabelFilter is CloudLogSession.orLabel: the query with one more exact match on this label,
// and its label conditions OR-ed.
func (q cloudQuery) orLabelFilter(scope, key, value string) cloudQuery {
	c := newLabelCondition()
	c.Scope, c.Key, c.Value = scope, key, value
	q.LabelConditions = append(append([]labelCondition(nil), q.LabelConditions...), c)
	q.LabelCombineOr = true
	return q
}

// withSeverityFilter is CloudLogSession.filterBySeverity: the query matching only this level.
func (q cloudQuery) withSeverityFilter(sev int) cloudQuery {
	q.SeveritySet = []int{sev}
	return q
}

// cloudFork mirrors CloudSessionFork: the starting query (or raw filter) and the suggested tab
// name of a session forked from another.
type cloudFork struct {
	Query     cloudQuery
	RawFilter string
	Name      string
}

// forkAddingLabel is CloudLogSession.forkAddingLabel: a new session that keeps cfg's filter and
// adds an exact label match. A raw filter gets the term AND-ed on; a structured query gets the
// condition, replacing any on the same label.
func forkAddingLabel(cfg cloudStreamConfig, scope, key, value string) cloudFork {
	name := shortForkName(key + "=" + value)
	if strings.Trim(cfg.RawFilter, " \t") != "" {
		term := labelField(scope, key) + "=" + quoteFilterValue(value)
		return cloudFork{Query: newCloudQuery(), RawFilter: "(" + cfg.RawFilter + ") AND " + term, Name: name}
	}
	return cloudFork{Query: cfg.Query.withLabelFilter(scope, key, value), Name: name}
}

// forkAddingSeverity is CloudLogSession.forkAddingSeverity.
func forkAddingSeverity(cfg cloudStreamConfig, sev int) cloudFork {
	name := severityTitle(sev)
	if strings.Trim(cfg.RawFilter, " \t") != "" {
		return cloudFork{
			Query:     newCloudQuery(),
			RawFilter: "(" + cfg.RawFilter + ") AND severity=" + severityName(sev),
			Name:      name,
		}
	}
	return cloudFork{Query: cfg.Query.withSeverityFilter(sev), Name: name}
}

// shortForkName is CloudLogSession.shortName: a tab name cut to 28 characters and an ellipsis.
func shortForkName(s string) string {
	rs := []rune(s)
	if len(rs) > 28 {
		return string(rs[:28]) + "…"
	}
	return s
}

// The SQL mode's built-in snippets (CloudSqlTemplates).
const (
	cloudSQLRecent = "" +
		"SELECT insert_id, seq, datetime(ts, 'unixepoch', 'localtime') AS time, severity_name, log_id, text_payload\n" +
		"FROM log_entry\n" +
		"ORDER BY seq DESC\n" +
		"LIMIT 1000;"

	cloudSQLErrorsOnly = "" +
		"SELECT insert_id, seq, datetime(ts, 'unixepoch', 'localtime') AS time, severity_name, text_payload\n" +
		"FROM log_entry\n" +
		"WHERE severity >= 500          -- ERROR and above\n" +
		"ORDER BY seq DESC\n" +
		"LIMIT 500;"

	cloudSQLUniquePerLabel = "" +
		"-- One log per unique label: a single (latest) row for each distinct value of a label —\n" +
		"-- e.g. one line per user_id. Replace `user_id` with your label key (see the Labels menu).\n" +
		"-- MAX(seq) makes the shown row the most recent one for that value.\n" +
		"SELECT insert_id, MAX(seq) AS seq, datetime(ts, 'unixepoch', 'localtime') AS time,\n" +
		"       severity_name, log_id, text_payload,\n" +
		"       json_extract(labels_json, '$.user_id') AS label_value\n" +
		"FROM log_entry\n" +
		"GROUP BY label_value\n" +
		"HAVING label_value IS NOT NULL          -- drop rows that don't carry this label\n" +
		"ORDER BY label_value;"

	cloudSQLFlowWindow = "" +
		"-- Flow window (req 14): keep only rows inside a START…END bracket, with a divider per window.\n" +
		"-- Edit the two LIKE patterns to your flow's start/end markers, then Run.\n" +
		"WITH marked AS (\n" +
		"  SELECT *,\n" +
		"    MAX(CASE WHEN text_payload LIKE '%START%' THEN seq END) OVER (ORDER BY seq) AS last_start,\n" +
		"    MAX(CASE WHEN text_payload LIKE '%END%'   THEN seq END) OVER (ORDER BY seq) AS last_end\n" +
		"  FROM log_entry\n" +
		"),\n" +
		"flow AS (\n" +
		"  SELECT * FROM marked\n" +
		"  WHERE last_start IS NOT NULL\n" +
		"    AND (last_end IS NULL OR last_end < last_start)   -- inside an open START…END bracket\n" +
		")\n" +
		"SELECT insert_id, seq, datetime(ts, 'unixepoch', 'localtime') AS time, severity_name, text_payload, 0 AS is_marker\n" +
		"FROM flow\n" +
		"UNION ALL\n" +
		"-- one synthetic divider per window (is_marker = 1 → drawn as a divider, not a log line)\n" +
		"SELECT '', last_start, NULL, 'NOTICE', '──────────  window  ──────────', 1\n" +
		"FROM flow\n" +
		"GROUP BY last_start\n" +
		"ORDER BY seq, is_marker DESC;"
)

// builtinSqlTemplates is CloudSqlTemplates.all, in the app's order. They have no id: they are
// not saved templates and can't be deleted.
var builtinSqlTemplates = []cloudSqlTemplate{
	{Name: "Recent 1000", SQL: cloudSQLRecent},
	{Name: "Errors only", SQL: cloudSQLErrorsOnly},
	{Name: "One log per unique label", SQL: cloudSQLUniquePerLabel},
	{Name: "Flow window (START…END)", SQL: cloudSQLFlowWindow},
}

// generatedCloudSQL is CloudLogSession.generatedSQL: the SQL the app starts the SQL mode with,
// which selects the captured rows and documents the session's filter in its first comment.
func generatedCloudSQL(cfg cloudStreamConfig) string {
	serverFilter := cloudFilterWithoutTime(cfg)
	if serverFilter == "" {
		serverFilter = "(all logs)"
	}
	return strings.Join([]string{
		"-- Level 1 (Cloud Logging filter, fetched live): " + serverFilter,
		"-- Level 2 (SQL filter): keep `insert_id`; the matching rows show in the list, in this order.",
		"SELECT insert_id, seq, datetime(ts, 'unixepoch', 'localtime') AS time, severity_name, log_id, text_payload",
		"FROM log_entry",
		"ORDER BY seq DESC",
		"LIMIT 1000;",
	}, "\n")
}

// sqlIsReadOnly is DatabaseService.isReadOnly: whether the statement, after its leading
// comments, starts with SELECT, WITH, PRAGMA or EXPLAIN. The daemon checks again.
func sqlIsReadOnly(sql string) bool {
	t := strings.ToLower(stripLeadingSQLComments(sql))
	for _, keyword := range []string{"select", "with", "pragma", "explain"} {
		if strings.HasPrefix(t, keyword) {
			return true
		}
	}
	return false
}

// stripLeadingSQLComments is DatabaseService.stripLeadingComments: the statement without its
// leading whitespace, "-- line" comments and "/* block */" comments.
func stripLeadingSQLComments(sql string) string {
	s := sql
	for {
		before := s
		s = strings.TrimLeft(s, " \t\n\r")
		if strings.HasPrefix(s, "--") {
			_, s, _ = strings.Cut(s, "\n")
		} else if strings.HasPrefix(s, "/*") {
			_, s, _ = strings.Cut(s, "*/")
		}
		if s == before {
			return s
		}
	}
}

// sqlRunProblem is the check CloudLogSession.runSQL makes before it sends a query: the app's
// message when the session has captured nothing yet or the statement isn't read-only, ""
// when the query can run. hasData is the stream state's HasData.
func sqlRunProblem(sql string, hasData bool) string {
	if !hasData {
		return "No data captured yet — start the session first."
	}
	if !sqlIsReadOnly(sql) {
		return "Only read-only queries are allowed (SELECT / WITH / PRAGMA / EXPLAIN)."
	}
	return ""
}
