package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// These port the Swift tests that freeze the Cloud Logging logic (CloudLogQueryTests,
// CloudLoggingCoreTests, CloudLabelOrderingTests, CloudConsoleURLTests, CloudSqlMappingTests,
// CloudMigrationTests and the wire cases of DaemonCloudTests), and add the wire payloads the
// daemon sends and the feed's state machine.

const (
	wireCloudState = `{"authState":{"account":"lucas@saltpay.co","state":"authenticated"},"binaryPath":"/opt/homebrew/bin/gcloud","isDetecting":false,"projects":[{"displayName":"Prod","favoriteLabelKeysByLogName":{"projects/acme-prod/logs/stdout":["user_id"]},"labelExampleRulesByLogName":{"projects/acme-prod/logs/stdout":{"tag":{"all":true,"count":1}}},"labelKeysByLogName":{"":["tag"],"projects/acme-prod/logs/stdout":["tag","user_id"]},"logNames":["projects/acme-prod/logs/run.googleapis.com%2Frequests","projects/acme-prod/logs/stdout"],"projectID":"acme-prod","selectedLogName":"projects/acme-prod/logs/stdout"}],"queryTemplates":[{"id":"9B2F6C1E-3D4A-4F7B-8A21-0C5D6E7F8A9B","name":"auth errors","query":{"labelCombineOr":false,"labelConditions":[{"id":"1F0E8D7C-6B5A-4938-8271-605F4E3D2C1B","key":"tag","mode":"exact","scope":"entry","value":"auth"}],"minSeverity":500,"severitySet":[],"textCombineOr":true,"textConditions":[{"id":"7C1D2E3F-4A5B-4C6D-8E7F-9A0B1C2D3E4F","mode":"contains","value":"timeout"}]}}],"sqlTemplates":[{"id":"0A1B2C3D-4E5F-4A6B-8C7D-9E0F1A2B3C4D","name":"errors","sql":"SELECT insert_id, seq, text_payload FROM log_entry WHERE severity >= 500 ORDER BY seq DESC LIMIT 500;"}]}`

	wireCloudEntry = `{"httpRequestSummary":"GET https://api.acme.com/v1/users → 200 · 0.012s","insertId":"6703f1a2000b1c2d","labels":{"tag":"auth","user_id":"42"},"logId":"stdout","logName":"projects/acme-prod/logs/stdout","message":"login ok user=42","payloadKind":"text","raw":"{\n  \"insertId\" : \"6703f1a2000b1c2d\"\n}","receiveTimestamp":1791374400.512,"resourceLabels":{"service_name":"api"},"resourceType":"cloud_run_revision","seq":1099511627776,"severity":200,"spanId":"00f067aa0ba902b7","timestamp":1791374400.123,"trace":"projects/acme-prod/traces/4bf92f35"}`

	wireCloudStreamState = `{"hasData":true,"hasMoreOlder":true,"isLoading":false,"isRunning":true,"olderLoading":false}`

	wireCloudSessionInfo = `{"config":{"logName":"projects/acme-prod/logs/stdout","projectID":"acme-prod","query":{"labelCombineOr":false,"labelConditions":[],"severitySet":[],"textCombineOr":true,"textConditions":[]},"timeRange":{"last":{"minutes":15}}},"existed":false,"id":"5E0C1B7A-9D2F-4C3E-8B6A-1F2E3D4C5B6A","state":{"hasData":false,"hasMoreOlder":true,"isLoading":false,"isRunning":false,"olderLoading":false}}`

	wireCloudSQLResult = `{"columns":["insert_id","seq","time","severity_name","log_id","text_payload"],"rows":[["6703f1a2000b1c2d","1099511627776","2026-10-07 09:00:00","INFO","stdout","login ok user=42"],[null,"2",null,null,null,null]]}`
)

// roundTrip decodes a wire payload and checks that encoding it again loses nothing.
func roundTrip[T any](t *testing.T, wire string) T {
	t.Helper()
	var value T
	if err := json.Unmarshal([]byte(wire), &value); err != nil {
		t.Fatalf("decode: %v", err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !sameJSON(t, encoded, wire) {
		t.Errorf("round trip changed the payload:\n got %s\nwant %s", encoded, wire)
	}
	return value
}

func mustDecode[T any](t *testing.T, wire string) T {
	t.Helper()
	var value T
	if err := json.Unmarshal([]byte(wire), &value); err != nil {
		t.Fatalf("decode %s: %v", wire, err)
	}
	return value
}

func TestCloudStateWire(t *testing.T) {
	s := roundTrip[cloudState](t, wireCloudState)
	if s.BinaryPath != "/opt/homebrew/bin/gcloud" || s.IsDetecting {
		t.Errorf("binary %q detecting %v", s.BinaryPath, s.IsDetecting)
	}
	if want := (cloudAuthState{State: cloudAuthAuthenticated, Account: "lucas@saltpay.co"}); s.AuthState != want {
		t.Errorf("auth = %+v, want %+v", s.AuthState, want)
	}
	p, ok := s.project("acme-prod")
	if !ok {
		t.Fatal("project acme-prod not found")
	}
	if p.title() != "Prod" || p.SelectedLogName != "projects/acme-prod/logs/stdout" || len(p.LogNames) != 2 {
		t.Errorf("project = %+v", p)
	}
	if got := p.labelKeys(); !reflect.DeepEqual(got, []string{"tag", "user_id"}) {
		t.Errorf("labelKeys = %v", got)
	}
	if got := p.favoriteLabelKeys(); !reflect.DeepEqual(got, []string{"user_id"}) {
		t.Errorf("favoriteLabelKeys = %v", got)
	}
	if got := p.LabelExampleRulesByLogName["projects/acme-prod/logs/stdout"]["tag"]; got != (labelExampleRule{All: true, Count: 1}) {
		t.Errorf("rule = %+v", got)
	}
	if _, ok := s.project("other"); ok {
		t.Error("found a project that isn't there")
	}
	q := s.QueryTemplates[0]
	if q.ID != "9B2F6C1E-3D4A-4F7B-8A21-0C5D6E7F8A9B" || q.Name != "auth errors" || q.RawFilter != "" {
		t.Errorf("query template = %+v", q)
	}
	if q.Query.MinSeverity == nil || *q.Query.MinSeverity != 500 || !q.Query.TextCombineOr || q.Query.LabelCombineOr {
		t.Errorf("query = %+v", q.Query)
	}
	if want := (labelCondition{ID: "1F0E8D7C-6B5A-4938-8271-605F4E3D2C1B", Key: "tag", Scope: "entry", Mode: "exact", Value: "auth"}); q.Query.LabelConditions[0] != want {
		t.Errorf("label condition = %+v", q.Query.LabelConditions[0])
	}
	if s.SqlTemplates[0].Name != "errors" || !strings.HasPrefix(s.SqlTemplates[0].SQL, "SELECT insert_id") {
		t.Errorf("sql template = %+v", s.SqlTemplates[0])
	}
}

// TestCloudStateDecodeDefaults is CloudState's tolerant decoder: missing keys take the
// defaults, and one bad record or an unreadable authState can't cost the rest.
func TestCloudStateDecodeDefaults(t *testing.T) {
	s := mustDecode[cloudState](t, `{}`)
	if s.AuthState.State != cloudAuthUnknown || s.BinaryPath != "" || len(s.Projects) != 0 {
		t.Errorf("empty state = %+v", s)
	}
	encoded, _ := json.Marshal(s)
	if want := `{"authState":{"state":"unknown"},"isDetecting":false,"projects":[],"queryTemplates":[],"sqlTemplates":[]}`; !sameJSON(t, encoded, want) {
		t.Errorf("empty state encodes as %s", encoded)
	}

	s = mustDecode[cloudState](t, `{"authState":7,"projects":[{"projectID":"good"},{"displayName":"no id"},{"projectID":"good2"}],
		"queryTemplates":[{"name":"a"},{"name":"b","query":{"minSeverity":123}},{"id":"not-a-uuid"}],
		"sqlTemplates":[{"sql":"SELECT 1"},7]}`)
	if s.AuthState.State != cloudAuthUnknown {
		t.Errorf("auth = %+v", s.AuthState)
	}
	if len(s.Projects) != 2 || s.Projects[0].ProjectID != "good" || s.Projects[1].ProjectID != "good2" {
		t.Errorf("projects = %+v", s.Projects)
	}
	if len(s.QueryTemplates) != 1 || s.QueryTemplates[0].Name != "a" || !isUUID(s.QueryTemplates[0].ID) || !s.QueryTemplates[0].Query.TextCombineOr {
		t.Errorf("query templates = %+v", s.QueryTemplates)
	}
	if len(s.SqlTemplates) != 1 || s.SqlTemplates[0].SQL != "SELECT 1" || !isUUID(s.SqlTemplates[0].ID) {
		t.Errorf("sql templates = %+v", s.SqlTemplates)
	}
}

// TestCloudAuthAndAddResultWire is DaemonCloudTests.test_cloudStateAndAuth_roundTrip.
func TestCloudAuthAndAddResultWire(t *testing.T) {
	authCases := []struct {
		wire string
		want cloudAuthState
	}{
		{`{"state":"later"}`, cloudAuthState{State: "unknown"}},
		{`{}`, cloudAuthState{State: "unknown"}},
		{`{"state":"notInstalled","account":"x"}`, cloudAuthState{State: "notInstalled"}},
		{`{"state":"notAuthenticated"}`, cloudAuthState{State: "notAuthenticated"}},
		{`{"state":"authenticated"}`, cloudAuthState{State: "authenticated"}},
		{`{"state":"authenticated","account":"me@example.com"}`, cloudAuthState{State: "authenticated", Account: "me@example.com"}},
	}
	for _, c := range authCases {
		if got := mustDecode[cloudAuthState](t, c.wire); got != c.want {
			t.Errorf("%s = %+v, want %+v", c.wire, got, c.want)
		}
	}
	encodeCases := []struct {
		value any
		want  string
	}{
		{cloudAuthState{}, `{"state":"unknown"}`},
		{cloudAuthState{State: "notInstalled"}, `{"state":"notInstalled"}`},
		{cloudAuthState{State: "authenticated"}, `{"state":"authenticated","account":""}`},
		{cloudAddResult{Result: "added"}, `{"result":"added"}`},
		{cloudAddResult{Result: "alreadyExists"}, `{"result":"alreadyExists"}`},
		{cloudAddResult{Result: "failure", Message: "x"}, `{"result":"failure","message":"x"}`},
	}
	for _, c := range encodeCases {
		encoded, err := json.Marshal(c.value)
		if err != nil || !sameJSON(t, encoded, c.want) {
			t.Errorf("%+v encodes as %s (%v), want %s", c.value, encoded, err, c.want)
		}
	}
	addCases := []struct {
		wire string
		want cloudAddResult
	}{
		{`{"result":"added"}`, cloudAddResult{Result: "added"}},
		{`{"result":"alreadyExists","message":"ignored"}`, cloudAddResult{Result: "alreadyExists"}},
		{`{"result":"failure","message":"x"}`, cloudAddResult{Result: "failure", Message: "x"}},
		{`{"result":"later"}`, cloudAddResult{Result: "failure"}},
		{`{}`, cloudAddResult{Result: "failure"}},
	}
	for _, c := range addCases {
		if got := mustDecode[cloudAddResult](t, c.wire); got != c.want {
			t.Errorf("%s = %+v, want %+v", c.wire, got, c.want)
		}
	}
}

func TestCloudEntryWire(t *testing.T) {
	e := roundTrip[cloudEntry](t, wireCloudEntry)
	if e.Seq != 1<<40 || e.InsertID != "6703f1a2000b1c2d" || e.Severity != 200 || e.PayloadKind != "text" {
		t.Errorf("entry = %+v", e)
	}
	if e.LogID != "stdout" || e.Message != "login ok user=42" || e.tag() != "auth" || e.ResourceLabels["service_name"] != "api" {
		t.Errorf("entry = %+v", e)
	}
	if e.HTTPRequestSummary != "GET https://api.acme.com/v1/users → 200 · 0.012s" || e.Trace == "" || e.SpanID != "00f067aa0ba902b7" {
		t.Errorf("entry = %+v", e)
	}
	if e.Raw != "{\n  \"insertId\" : \"6703f1a2000b1c2d\"\n}" {
		t.Errorf("raw = %q", e.Raw)
	}
	if e.ReceiveTimestamp == nil || *e.ReceiveTimestamp != 1791374400.512 {
		t.Errorf("receiveTimestamp = %v", e.ReceiveTimestamp)
	}
	if got := e.time().UTC().Format("2006-01-02T15:04:05.000Z"); got != "2026-10-07T12:00:00.123Z" {
		t.Errorf("time = %s", got)
	}
}

// TestCloudEntryDecodeDefaults is CloudLogEntry's tolerant decoder: only seq is required.
func TestCloudEntryDecodeDefaults(t *testing.T) {
	e := mustDecode[cloudEntry](t, `{"seq":42}`)
	if want := (cloudEntry{Seq: 42, PayloadKind: "text"}); !reflect.DeepEqual(e, want) {
		t.Errorf("minimal entry = %+v", e)
	}
	encoded, _ := json.Marshal(e)
	if want := `{"seq":42,"timestamp":0,"severity":0,"logName":"","logId":"","message":"","payloadKind":"text"}`; !sameJSON(t, encoded, want) {
		t.Errorf("minimal entry encodes as %s", encoded)
	}

	cases := []struct {
		name, wire string
		check      func(e cloudEntry) bool
	}{
		{"raw falls back to the message", `{"seq":1,"message":"m"}`, func(e cloudEntry) bool { return e.Raw == "m" }},
		{"raw kept when present", `{"seq":1,"message":"m","raw":"{}"}`, func(e cloudEntry) bool { return e.Raw == "{}" }},
		{"unknown severity is DEFAULT", `{"seq":1,"severity":250}`, func(e cloudEntry) bool { return e.Severity == 0 }},
		{"severity of the wrong type is DEFAULT", `{"seq":1,"severity":"ERROR"}`, func(e cloudEntry) bool { return e.Severity == 0 }},
		{"known severity", `{"seq":1,"severity":800}`, func(e cloudEntry) bool { return e.Severity == 800 }},
		{"unknown payload kind is text", `{"seq":1,"payloadKind":"later"}`, func(e cloudEntry) bool { return e.PayloadKind == "text" }},
		{"payload kind none", `{"seq":1,"payloadKind":"none"}`, func(e cloudEntry) bool { return e.PayloadKind == "none" }},
		{"null fields", `{"seq":1,"trace":null,"labels":null,"receiveTimestamp":null}`,
			func(e cloudEntry) bool { return e.Trace == "" && e.Labels == nil && e.ReceiveTimestamp == nil }},
		{"unknown keys", `{"seq":1,"isSynthetic":false,"later":[1,2]}`, func(e cloudEntry) bool { return e.Seq == 1 }},
	}
	for _, c := range cases {
		if e := mustDecode[cloudEntry](t, c.wire); !c.check(e) {
			t.Errorf("%s: %+v", c.name, e)
		}
	}

	var missing cloudEntry
	if json.Unmarshal([]byte(`{"message":"no seq"}`), &missing) == nil {
		t.Error("an entry without a seq decoded")
	}
	// A batch with one bad entry keeps the rest.
	batch := decodeEach[cloudEntry](json.RawMessage(`[{"seq":1},{"message":"no seq"},{"seq":3}]`))
	if len(batch) != 2 || batch[0].Seq != 1 || batch[1].Seq != 3 {
		t.Errorf("batch = %+v", batch)
	}
}

func TestCloudStreamStateWire(t *testing.T) {
	s := roundTrip[cloudStreamState](t, wireCloudStreamState)
	if want := (cloudStreamState{IsRunning: true, HasMoreOlder: true, HasData: true}); s != want {
		t.Errorf("state = %+v", s)
	}
	if got := mustDecode[cloudStreamState](t, `{}`); got != (cloudStreamState{HasMoreOlder: true}) {
		t.Errorf("empty state = %+v", got)
	}
	withMessage := roundTrip[cloudStreamState](t, `{"hasData":false,"hasMoreOlder":false,"isLoading":false,"isRunning":false,"olderLoading":true,"statusMessage":"gcloud isn't installed."}`)
	if withMessage.StatusMessage != "gcloud isn't installed." || withMessage.HasMoreOlder || !withMessage.OlderLoading {
		t.Errorf("state = %+v", withMessage)
	}
}

func TestCloudSessionInfoWire(t *testing.T) {
	info := roundTrip[cloudSessionInfo](t, wireCloudSessionInfo)
	if info.ID != "5E0C1B7A-9D2F-4C3E-8B6A-1F2E3D4C5B6A" || info.Existed {
		t.Errorf("info = %+v", info)
	}
	cfg := info.Config
	if cfg.ProjectID != "acme-prod" || cfg.LogName != "projects/acme-prod/logs/stdout" || cfg.RawFilter != "" {
		t.Errorf("config = %+v", cfg)
	}
	if cfg.TimeRange != (cloudTimeRange{Minutes: 15}) || !cfg.Query.isEmpty() || !cfg.Query.TextCombineOr {
		t.Errorf("config = %+v", cfg)
	}
	if info.State != (cloudStreamState{HasMoreOlder: true}) {
		t.Errorf("state = %+v", info.State)
	}
	entries, older, state := cloudTopics("5e0c1b7a-9d2f-4c3e-8b6a-1f2e3d4c5b6a")
	if entries != "cloud.entries."+info.ID || older != "cloud.older."+info.ID || state != "cloud.sstate."+info.ID {
		t.Errorf("topics = %s %s %s", entries, older, state)
	}
	// Missing parts take a new session's defaults.
	bare := mustDecode[cloudSessionInfo](t, `{"id":"X","existed":true}`)
	if !bare.Existed || !bare.State.HasMoreOlder || bare.Config.TimeRange.Minutes != 15 || !bare.Config.Query.TextCombineOr {
		t.Errorf("bare info = %+v", bare)
	}
}

// The daemon requires projectID, query and timeRange in a config and decodes logName and
// rawFilter only when present.
func TestCloudConfigEncoding(t *testing.T) {
	emptyQuery := `{"labelCombineOr":false,"labelConditions":[],"severitySet":[],"textCombineOr":true,"textConditions":[]}`
	last15 := `{"last":{"minutes":15}}`
	start := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	end := start.Add(90*time.Minute + 250*time.Millisecond)
	sev := 400
	cases := []struct {
		name string
		cfg  cloudStreamConfig
		want string
	}{
		{"a new session", newCloudStreamConfig("acme-prod"),
			`{"projectID":"acme-prod","query":` + emptyQuery + `,"timeRange":` + last15 + `}`},
		{"the zero value still has all three", cloudStreamConfig{},
			`{"projectID":"","query":{"labelCombineOr":false,"labelConditions":[],"severitySet":[],"textCombineOr":false,"textConditions":[]},"timeRange":` + last15 + `}`},
		{"log name and raw filter", cloudStreamConfig{ProjectID: "p", Query: newCloudQuery(), TimeRange: cloudTimeRange{Minutes: 60},
			LogName: "projects/p/logs/stdout", RawFilter: "severity>=ERROR"},
			`{"projectID":"p","query":` + emptyQuery + `,"timeRange":{"last":{"minutes":60}},"logName":"projects/p/logs/stdout","rawFilter":"severity>=ERROR"}`},
		{"an absolute range", cloudStreamConfig{ProjectID: "p", Query: newCloudQuery(), TimeRange: cloudTimeRange{Start: start, End: end}},
			`{"projectID":"p","query":` + emptyQuery + `,"timeRange":{"between":{"start":"2026-10-07T09:00:00.000Z","end":"2026-10-07T10:30:00.250Z"}}}`},
		{"a query", cloudStreamConfig{ProjectID: "p", TimeRange: cloudTimeRange{Minutes: 5}, Query: cloudQuery{
			TextConditions:  []textCondition{{ID: "7C1D2E3F-4A5B-4C6D-8E7F-9A0B1C2D3E4F", Mode: "regex", Value: "a+"}},
			TextCombineOr:   true,
			MinSeverity:     &sev,
			SeveritySet:     []int{500, 600},
			LabelConditions: []labelCondition{{ID: "1F0E8D7C-6B5A-4938-8271-605F4E3D2C1B", Key: "tag", Scope: "resource", Mode: "notContains", Value: "x"}},
			LabelCombineOr:  true,
		}},
			`{"projectID":"p","timeRange":{"last":{"minutes":5}},"query":{"labelCombineOr":true,"labelConditions":[{"id":"1F0E8D7C-6B5A-4938-8271-605F4E3D2C1B","key":"tag","mode":"notContains","scope":"resource","value":"x"}],"minSeverity":400,"severitySet":[500,600],"textCombineOr":true,"textConditions":[{"id":"7C1D2E3F-4A5B-4C6D-8E7F-9A0B1C2D3E4F","mode":"regex","value":"a+"}]}}`},
	}
	for _, c := range cases {
		encoded, err := json.Marshal(c.cfg)
		if err != nil || !sameJSON(t, encoded, c.want) {
			t.Errorf("%s: encodes as %s (%v), want %s", c.name, encoded, err, c.want)
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &fields); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		for _, key := range []string{"projectID", "query", "timeRange"} {
			if _, ok := fields[key]; !ok {
				t.Errorf("%s: %s is missing from %s", c.name, key, encoded)
			}
		}
		// What was encoded decodes to the same config.
		back := mustDecode[cloudStreamConfig](t, string(encoded))
		again, _ := json.Marshal(back)
		if !sameJSON(t, again, string(encoded)) {
			t.Errorf("%s: decoded and encoded again as %s", c.name, again)
		}
	}

	// Conditions built without an id or a mode still encode what the daemon requires.
	encoded, _ := json.Marshal(cloudQuery{TextConditions: []textCondition{{Value: "x"}}, LabelConditions: []labelCondition{{Key: "k", Value: "v"}}})
	back := mustDecode[cloudQuery](t, string(encoded))
	if tc := back.TextConditions[0]; !isUUID(tc.ID) || tc.Mode != "contains" || tc.Value != "x" {
		t.Errorf("text condition = %+v", tc)
	}
	if lc := back.LabelConditions[0]; !isUUID(lc.ID) || lc.Mode != "exact" || lc.Scope != "entry" || lc.Key != "k" {
		t.Errorf("label condition = %+v", lc)
	}
}

func TestCloudTimeRangeWire(t *testing.T) {
	cases := []struct {
		name, wire string
		want       cloudTimeRange
		fails      bool
	}{
		{"last", `{"last":{"minutes":360}}`, cloudTimeRange{Minutes: 360}, false},
		{"last of no minutes reads as one", `{"last":{"minutes":0}}`, cloudTimeRange{Minutes: 1}, false},
		{"between, with fractions", `{"between":{"start":"2026-10-07T09:00:00.000Z","end":"2026-10-07T10:30:00.250Z"}}`,
			cloudTimeRange{Start: time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC), End: time.Date(2026, 10, 7, 10, 30, 0, 250e6, time.UTC)}, false},
		{"between, plain and numeric", `{"between":{"start":"2026-10-07T09:00:00Z","end":1791374400.5}}`,
			cloudTimeRange{Start: time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC), End: time.Unix(1791374400, 5e8)}, false},
		{"neither", `{}`, cloudTimeRange{}, true},
		{"last without minutes", `{"last":{}}`, cloudTimeRange{}, true},
		{"between without an end", `{"between":{"start":"2026-10-07T09:00:00Z"}}`, cloudTimeRange{}, true},
		{"between with a bad date", `{"between":{"start":"yesterday","end":"2026-10-07T09:00:00Z"}}`, cloudTimeRange{}, true},
	}
	for _, c := range cases {
		var got cloudTimeRange
		err := json.Unmarshal([]byte(c.wire), &got)
		if c.fails {
			if err == nil {
				t.Errorf("%s: decoded as %+v", c.name, got)
			}
			continue
		}
		if err != nil || got.Minutes != c.want.Minutes || !got.Start.Equal(c.want.Start) || !got.End.Equal(c.want.End) {
			t.Errorf("%s: %+v (%v), want %+v", c.name, got, err, c.want)
		}
	}
}

func TestDBResultSetWire(t *testing.T) {
	rs := roundTrip[dbResultSet](t, wireCloudSQLResult)
	if len(rs.Columns) != 6 || len(rs.Rows) != 2 {
		t.Fatalf("result = %+v", rs)
	}
	if rs.Rows[0][0] == nil || *rs.Rows[0][0] != "6703f1a2000b1c2d" || rs.Rows[1][0] != nil || *rs.Rows[1][1] != "2" {
		t.Errorf("rows = %+v", rs.Rows)
	}
}

// TestCloudMigration is CloudMigrationTests: JSON written before a field existed still decodes,
// with the field defaulted.
func TestCloudMigration(t *testing.T) {
	old := decodeEach[cloudProject](json.RawMessage(`[{"projectID":"p","displayName":"Prod","selectedLogName":"projects/p/logs/x",
		"logNames":["projects/p/logs/x"],"labelKeysByLogName":{"projects/p/logs/x":["env","user_id"]}}]`))
	if len(old) != 1 || old[0].ProjectID != "p" || old[0].DisplayName != "Prod" {
		t.Fatalf("old schema = %+v", old)
	}
	if !reflect.DeepEqual(old[0].LabelKeysByLogName["projects/p/logs/x"], []string{"env", "user_id"}) {
		t.Errorf("label keys = %v", old[0].LabelKeysByLogName)
	}
	if len(old[0].FavoriteLabelKeysByLogName) != 0 || len(old[0].LabelExampleRulesByLogName) != 0 {
		t.Errorf("defaults = %+v", old[0])
	}

	minimal := decodeEach[cloudProject](json.RawMessage(`[{"projectID":"p","somethingNew":42}]`))
	if len(minimal) != 1 || minimal[0].ProjectID != "p" || len(minimal[0].LogNames) != 0 || minimal[0].title() != "p" {
		t.Errorf("minimal = %+v", minimal)
	}
	encoded, _ := json.Marshal(minimal[0])
	if want := `{"projectID":"p","displayName":"","logNames":[],"labelKeysByLogName":{},"favoriteLabelKeysByLogName":{},"labelExampleRulesByLogName":{}}`; !sameJSON(t, encoded, want) {
		t.Errorf("minimal project encodes as %s", encoded)
	}

	ruleCases := []struct {
		wire string
		want labelExampleRule
	}{
		{`{"count":5}`, labelExampleRule{Count: 5}},
		{`{}`, labelExampleRule{Count: 1}},
		{`{"all":true,"count":0}`, labelExampleRule{All: true, Count: 1}},
	}
	for _, c := range ruleCases {
		if got := mustDecode[labelExampleRule](t, c.wire); got != c.want {
			t.Errorf("%s = %+v, want %+v", c.wire, got, c.want)
		}
	}

	q := mustDecode[cloudQuery](t, `{"textConditions":[{"value":"boom"}]}`)
	if len(q.TextConditions) != 1 || q.TextConditions[0].Value != "boom" || q.TextConditions[0].Mode != "contains" || !q.TextCombineOr {
		t.Errorf("old query = %+v", q)
	}
	if !isUUID(q.TextConditions[0].ID) {
		t.Errorf("a missing id became %q", q.TextConditions[0].ID)
	}
	if lc := mustDecode[labelCondition](t, `{"key":"env","value":"prod"}`); lc.Scope != "entry" || lc.Mode != "exact" {
		t.Errorf("old label condition = %+v", lc)
	}
	// What the app's decoder rejects is rejected here, so it isn't sent back changed.
	for _, wire := range []string{`{"textConditions":[{"mode":"later"}]}`, `{"labelConditions":[{"scope":"later"}]}`, `{"severitySet":[250]}`, `{"textConditions":[{"id":"x"}]}`} {
		var bad cloudQuery
		if json.Unmarshal([]byte(wire), &bad) == nil {
			t.Errorf("%s decoded as %+v", wire, bad)
		}
	}
}

// The filter cases of CloudLogQueryTests.
func TestCloudFilterTerms(t *testing.T) {
	cases := []struct{ got, want string }{
		{matchTerm("contains", "textPayload", "foo"), `textPayload:"foo"`},
		{matchTerm("exact", "textPayload", "foo"), `textPayload="foo"`},
		{matchTerm("regex", "textPayload", "fo+"), `textPayload=~"fo+"`},
		{matchTerm("notContains", "textPayload", "foo"), `NOT textPayload:"foo"`},
		{quoteFilterValue(`a"b\c`), `"a\"b\\c"`},
		// A regex passes backslashes to RE2 as they are; only the quote is escaped.
		{matchTerm("regex", "labels.x", `^202606(2[7-9]|[3-9]\d)\d{2}$`), `labels.x=~"^202606(2[7-9]|[3-9]\d)\d{2}$"`},
		{quoteFilterRegex(`say "hi"\d`), `"say \"hi\"\d"`},
		{matchTerm("exact", "labels.x", `a\b`), `labels.x="a\\b"`},
		{quoteLabelKey("env"), "env"},
		{quoteLabelKey("run.googleapis.com/x"), `"run.googleapis.com/x"`},
		{quoteLabelKey(""), `""`},
		{labelField("entry", "env"), "labels.env"},
		{labelField("resource", "a.b"), `resource.labels."a.b"`},
		{matchModeLabel("contains"), "contains"},
		{matchModeLabel("exact"), "is exactly"},
		{matchModeLabel("regex"), "matches regex"},
		{matchModeLabel("notContains"), "doesn't contain"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("got %s, want %s", c.got, c.want)
		}
	}
}

func text(mode, value string) textCondition { return textCondition{Mode: mode, Value: value} }

func label(key, scope, mode, value string) labelCondition {
	return labelCondition{Key: key, Scope: scope, Mode: mode, Value: value}
}

func TestCloudQueryClauses(t *testing.T) {
	sev := func(n int) *int { return &n }
	cases := []struct {
		name  string
		query cloudQuery
		want  []string
	}{
		{"one text condition, no parentheses", cloudQuery{TextConditions: []textCondition{text("contains", "boom")}, TextCombineOr: true},
			[]string{`textPayload:"boom"`}},
		{"text conditions OR-ed", cloudQuery{TextConditions: []textCondition{text("contains", "a"), text("contains", "b")}, TextCombineOr: true},
			[]string{`(textPayload:"a" OR textPayload:"b")`}},
		{"text conditions AND-ed", cloudQuery{TextConditions: []textCondition{text("contains", "a"), text("exact", "b")}},
			[]string{`(textPayload:"a" AND textPayload="b")`}},
		{"an empty text condition is skipped", cloudQuery{TextConditions: []textCondition{text("contains", "")}}, nil},
		{"a text condition without a mode contains", cloudQuery{TextConditions: []textCondition{text("", "x")}}, []string{`textPayload:"x"`}},
		{"min severity", cloudQuery{MinSeverity: sev(400)}, []string{"severity>=WARNING"}},
		{"min severity DEFAULT is no clause", cloudQuery{MinSeverity: sev(0)}, nil},
		{"one severity", cloudQuery{SeveritySet: []int{500}}, []string{"severity=ERROR"}},
		{"several severities", cloudQuery{SeveritySet: []int{500, 600}}, []string{"severity=(ERROR OR CRITICAL)"}},
		{"the severity set wins over the floor", cloudQuery{MinSeverity: sev(200), SeveritySet: []int{500}}, []string{"severity=ERROR"}},
		{"entry label, exact", cloudQuery{LabelConditions: []labelCondition{label("env", "entry", "exact", "prod")}}, []string{`labels.env="prod"`}},
		{"resource label, contains", cloudQuery{LabelConditions: []labelCondition{label("zone", "resource", "contains", "us")}}, []string{`resource.labels.zone:"us"`}},
		{"a label key with dots is quoted", cloudQuery{LabelConditions: []labelCondition{label("a.b", "entry", "exact", "x")}}, []string{`labels."a.b"="x"`}},
		{"a label condition without a mode or scope is an exact entry label", cloudQuery{LabelConditions: []labelCondition{label("env", "", "", "prod")}}, []string{`labels.env="prod"`}},
		{"labels OR-ed", cloudQuery{LabelConditions: []labelCondition{label("env", "entry", "exact", "prod"), label("env", "entry", "exact", "staging")}, LabelCombineOr: true},
			[]string{`(labels.env="prod" OR labels.env="staging")`}},
		{"a label without a key or a value is skipped", cloudQuery{LabelConditions: []labelCondition{label("", "entry", "exact", "x"), label("k", "entry", "exact", "")}}, nil},
		{"text, severity and labels, in that order", cloudQuery{TextConditions: []textCondition{text("contains", "x")}, MinSeverity: sev(500),
			LabelConditions: []labelCondition{label("env", "entry", "exact", "prod")}},
			[]string{`textPayload:"x"`, "severity>=ERROR", `labels.env="prod"`}},
	}
	for _, c := range cases {
		if got := c.query.clauses(); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
		if empty := c.query.isEmpty(); empty != (len(c.want) == 0) {
			t.Errorf("%s: isEmpty = %v", c.name, empty)
		}
	}
	if !newCloudQuery().isEmpty() || !(cloudQuery{}).isEmpty() {
		t.Error("an empty query isn't empty")
	}
}

func TestBuildCloudFilter(t *testing.T) {
	sev := 500
	q := cloudQuery{TextConditions: []textCondition{text("contains", "x")}, TextCombineOr: true, MinSeverity: &sev}
	timeClause := `timestamp>="2024-06-26T10:00:00.000Z"`
	cases := []struct{ name, got, want string }{
		{"logName, time and query", buildFilter("projects/p/logs/stdout", timeClause, q, ""),
			`logName="projects/p/logs/stdout" AND timestamp>="2024-06-26T10:00:00.000Z" AND textPayload:"x" AND severity>=ERROR`},
		{"nothing", buildFilter("", "", newCloudQuery(), ""), ""},
		{"only a logName", buildFilter("projects/p/logs/stdout", "", newCloudQuery(), ""), `logName="projects/p/logs/stdout"`},
		{"a raw filter replaces logName and query", buildFilter("projects/p/logs/stdout", timeClause, q, `resource.type="k8s_container" OR severity>=ERROR`),
			`(resource.type="k8s_container" OR severity>=ERROR) AND timestamp>="2024-06-26T10:00:00.000Z"`},
		{"a raw filter without a time", buildFilter("", "", q, "severity>=ERROR"), "(severity>=ERROR)"},
		{"a blank raw filter is none", buildFilter("", "", q, " \n "), `textPayload:"x" AND severity>=ERROR`},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: %s, want %s", c.name, c.got, c.want)
		}
	}

	now := time.Unix(1_000_000, 0)
	cfg := cloudStreamConfig{ProjectID: "p", LogName: "projects/p/logs/stdout", Query: q, TimeRange: cloudTimeRange{Minutes: 15}}
	if got, want := buildCloudFilter(cfg, now), `logName="projects/p/logs/stdout" AND timestamp>="1970-01-12T13:31:40.000Z" AND textPayload:"x" AND severity>=ERROR`; got != want {
		t.Errorf("live filter = %s, want %s", got, want)
	}
	cfg.TimeRange = cloudTimeRange{Start: time.Unix(1_000_000, 0), End: time.Unix(1_000_100, 5e8)}
	cfg.RawFilter = "severity>=ERROR"
	if got, want := buildCloudFilter(cfg, now), `(severity>=ERROR) AND timestamp>="1970-01-12T13:46:40.000Z" AND timestamp<="1970-01-12T13:48:20.500Z"`; got != want {
		t.Errorf("absolute raw filter = %s, want %s", got, want)
	}
	if got, want := cloudFilterWithoutTime(cfg), "(severity>=ERROR)"; got != want {
		t.Errorf("filter without time = %s, want %s", got, want)
	}
	if got, want := cloudConsoleURL(cfg), "https://console.cloud.google.com/logs/query;query=%28severity%3E%3DERROR%29?project=p"; got != want {
		t.Errorf("console URL = %s, want %s", got, want)
	}
}

func TestCloudLogNames(t *testing.T) {
	fullCases := []struct{ project, short, want string }{
		{"p", "stdout", "projects/p/logs/stdout"},
		{"p", "run.googleapis.com/stdout", "projects/p/logs/run.googleapis.com%2Fstdout"},
		{"p", "projects/other/logs/x", "projects/other/logs/x"},
		{"p", "a b~c_d-é", "projects/p/logs/a%20b~c_d-%C3%A9"},
	}
	for _, c := range fullCases {
		if got := cloudLogNameFull(c.project, c.short); got != c.want {
			t.Errorf("full(%q, %q) = %q, want %q", c.project, c.short, got, c.want)
		}
	}
	idCases := []struct{ full, want string }{
		{"projects/p/logs/run.googleapis.com%2Fstdout", "run.googleapis.com/stdout"},
		{"stdout", "stdout"},
		{"projects/p/logs/bad%zz", "bad%zz"},
		{"projects/p/logs/", ""},
	}
	for _, c := range idCases {
		if got := cloudLogID(c.full); got != c.want {
			t.Errorf("id(%q) = %q, want %q", c.full, got, c.want)
		}
	}
}

// The severity, time range and label cases of CloudLoggingCoreTests.
func TestCloudSeverity(t *testing.T) {
	cases := []struct {
		sev                int
		name, short, title string
	}{
		{0, "DEFAULT", "·", "Default"},
		{100, "DEBUG", "D", "Debug"},
		{200, "INFO", "I", "Info"},
		{300, "NOTICE", "N", "Notice"},
		{400, "WARNING", "W", "Warning"},
		{500, "ERROR", "E", "Error"},
		{600, "CRITICAL", "C", "Critical"},
		{700, "ALERT", "A", "Alert"},
		{800, "EMERGENCY", "!", "Emergency"},
		{250, "DEFAULT", "·", "Default"},
		{-1, "DEFAULT", "·", "Default"},
	}
	for _, c := range cases {
		if severityName(c.sev) != c.name || severityShort(c.sev) != c.short || severityTitle(c.sev) != c.title {
			t.Errorf("%d: %s %s %s", c.sev, severityName(c.sev), severityShort(c.sev), severityTitle(c.sev))
		}
	}
	if !reflect.DeepEqual(cloudSeverities, []int{0, 100, 200, 300, 400, 500, 600, 700, 800}) {
		t.Errorf("cloudSeverities = %v", cloudSeverities)
	}
	parse := []struct {
		name string
		want int
	}{{"ERROR", 500}, {"warning", 400}, {"", 0}, {"bogus", 0}, {"DEFAULT", 0}, {"Emergency", 800}}
	for _, c := range parse {
		if got := severityFromName(c.name); got != c.want {
			t.Errorf("severityFromName(%q) = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestCloudTimeRange(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	last := cloudTimeRange{Minutes: 15}
	between := cloudTimeRange{Start: time.Unix(1_000_000, 0), End: time.Unix(1_000_100, 0)}
	if !last.isLive() || between.isLive() {
		t.Error("isLive is wrong")
	}
	if got := last.start(now).Unix(); got != 1_000_000-900 {
		t.Errorf("start = %d", got)
	}
	if got := between.start(now); !got.Equal(between.Start) {
		t.Errorf("start of an absolute range = %v", got)
	}
	if got, want := last.clause(now), `timestamp>="1970-01-12T13:31:40.000Z"`; got != want {
		t.Errorf("clause = %s, want %s", got, want)
	}
	if got, want := between.clause(now), `timestamp>="1970-01-12T13:46:40.000Z" AND timestamp<="1970-01-12T13:48:20.000Z"`; got != want {
		t.Errorf("clause = %s, want %s", got, want)
	}
	labels := []struct {
		r    cloudTimeRange
		want string
	}{
		{cloudTimeRange{Minutes: 5}, "Last 5m"},
		{cloudTimeRange{Minutes: 15}, "Last 15m"},
		{cloudTimeRange{Minutes: 60}, "Last 1h"},
		{cloudTimeRange{Minutes: 120}, "Last 2h"},
		{cloudTimeRange{Minutes: 360}, "Last 6h"},
		{cloudTimeRange{Minutes: 1440}, "Last 24h"},
		{cloudTimeRange{Minutes: 2880}, "Last 2d"},
		{cloudTimeRange{Minutes: 10080}, "Last 7d"},
		{cloudTimeRange{Minutes: 90}, "Last 90m"},
		{between, "Custom range"},
		{cloudTimeRange{}, "Last 15m"},
	}
	for _, c := range labels {
		if got := c.r.label(); got != c.want {
			t.Errorf("label(%+v) = %q, want %q", c.r, got, c.want)
		}
	}
}

func TestLabelKeys(t *testing.T) {
	entries := []cloudEntry{
		{Labels: map[string]string{"env": "prod", "v": "1"}},
		{Labels: map[string]string{"env": "prod", "region": "eu"}},
	}
	if got := detectLabelKeys(entries); !reflect.DeepEqual(got, []string{"env", "region", "v"}) {
		t.Errorf("detected = %v", got)
	}
	merged, changed := mergeLabelKeys([]string{"env"}, []string{"env", "region"})
	if !changed || !reflect.DeepEqual(merged, []string{"env", "region"}) {
		t.Errorf("merge = %v %v", merged, changed)
	}
	merged, changed = mergeLabelKeys([]string{"env", "region"}, []string{"env"})
	if changed || !reflect.DeepEqual(merged, []string{"env", "region"}) {
		t.Errorf("merge = %v %v", merged, changed)
	}

	// CloudLabelOrderingTests.
	cases := []struct {
		name                  string
		keys, favorites, want []string
	}{
		{"favorites first, both sorted", []string{"region", "env", "user_id", "version"}, []string{"user_id", "env"}, []string{"env", "user_id", "region", "version"}},
		{"no favorites", []string{"b", "a", "c"}, nil, []string{"a", "b", "c"}},
		{"a stale favorite is dropped", []string{"env"}, []string{"env", "gone"}, []string{"env"}},
		{"all favorites", []string{"z", "a"}, []string{"z", "a"}, []string{"a", "z"}},
		{"nothing detected", nil, []string{"a"}, nil},
	}
	for _, c := range cases {
		if got := orderedLabelKeys(c.keys, c.favorites); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// TestConsoleURL is CloudConsoleURLTests.
func TestConsoleURL(t *testing.T) {
	built := buildConsoleURL("my-proj", `textPayload:"foo" AND severity>=ERROR`)
	if want := "https://console.cloud.google.com/logs/query;query=textPayload%3A%22foo%22%20AND%20severity%3E%3DERROR?project=my-proj"; built != want {
		t.Errorf("built %s, want %s", built, want)
	}
	if got, want := buildConsoleURL("p", ""), "https://console.cloud.google.com/logs/query?project=p"; got != want {
		t.Errorf("empty filter: %s, want %s", got, want)
	}
	if got, want := buildConsoleURL("a b", " \n"), "https://console.cloud.google.com/logs/query?project=a%20b"; got != want {
		t.Errorf("blank filter: %s, want %s", got, want)
	}

	filter := `logName="projects/p/logs/stdout" AND textPayload:"boom" AND severity>=ERROR`
	if project, query := parseConsoleURL(buildConsoleURL("p", filter)); project != "p" || query != filter {
		t.Errorf("round trip = %q %q", project, query)
	}

	cases := []struct{ name, url, project, query string }{
		{"a realistic URL", "https://console.cloud.google.com/logs/query;query=severity%3E%3DERROR;timeRange=PT1H?project=demo-prod-42&hl=en", "demo-prod-42", "severity>=ERROR"},
		{"no query", "https://console.cloud.google.com/logs/query?project=p", "p", ""},
		{"an ordinary query parameter", "https://console.cloud.google.com/logs/query?query=severity%3DERROR&project=p", "p", "severity=ERROR"},
		{"the first ordinary query parameter wins", "https://x/logs?query=a&query=b", "", "a"},
		{"the matrix parameter wins", "https://console.cloud.google.com/logs/query;query=a?query=b&project=p", "p", "a"},
		{"no project", "https://console.cloud.google.com/logs/query;query=a", "", "a"},
		{"a value that can't be decoded", "https://console.cloud.google.com/logs/query?project=%zz", "", ""},
		{"empty values", "https://console.cloud.google.com/logs/query;query=?project=&x", "", ""},
		{"not a URL", "", "", ""},
	}
	for _, c := range cases {
		if project, query := parseConsoleURL(c.url); project != c.project || query != c.query {
			t.Errorf("%s: %q %q, want %q %q", c.name, project, query, c.project, c.query)
		}
	}
	if !looksLikeConsoleURL("https://console.cloud.google.com/logs/query;query=x") || looksLikeConsoleURL("https://example.com") {
		t.Error("looksLikeConsoleURL is wrong")
	}
}

func TestCloudSubtitleAndForks(t *testing.T) {
	sev := 500
	base := newCloudStreamConfig("p")
	withLog := base
	withLog.LogName = "projects/p/logs/run.googleapis.com%2Fstdout"
	filtered := withLog
	filtered.Query.MinSeverity = &sev
	raw := filtered
	raw.RawFilter = "severity>=ERROR"
	subtitles := []struct {
		name string
		cfg  cloudStreamConfig
		sql  bool
		want string
	}{
		{"all logs", base, false, "Prod · Last 15m"},
		{"a log name", withLog, false, "Prod · run.googleapis.com/stdout · Last 15m"},
		{"a query", filtered, false, "Prod · run.googleapis.com/stdout · Last 15m · filtered"},
		{"SQL mode", filtered, true, "Prod · run.googleapis.com/stdout · Last 15m · SQL"},
		{"a raw filter", raw, false, "Prod · URL filter · Last 15m"},
	}
	for _, c := range subtitles {
		if got := cloudSubtitle("Prod", c.cfg, c.sql); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}

	q := newCloudQuery()
	q.LabelConditions = []labelCondition{label("user_id", "entry", "contains", "4"), label("zone", "resource", "exact", "us")}
	one := q.withLabelFilter("entry", "user_id", "42")
	if got := one.clauses(); !reflect.DeepEqual(got, []string{`(resource.labels.zone="us" AND labels.user_id="42")`}) {
		t.Errorf("withLabelFilter = %q", got)
	}
	or := one.orLabelFilter("entry", "user_id", "43")
	if got := or.clauses(); !reflect.DeepEqual(got, []string{`(resource.labels.zone="us" OR labels.user_id="42" OR labels.user_id="43")`}) {
		t.Errorf("orLabelFilter = %q", got)
	}
	if len(q.LabelConditions) != 2 || len(one.LabelConditions) != 2 || one.LabelCombineOr {
		t.Error("a derived query changed the one it came from")
	}
	if got := q.withSeverityFilter(600).clauses()[0]; got != "severity=CRITICAL" {
		t.Errorf("withSeverityFilter = %q", got)
	}

	fork := forkAddingLabel(filtered, "entry", "user_id", "42")
	if fork.Name != "user_id=42" || fork.RawFilter != "" || !reflect.DeepEqual(fork.Query.clauses(), []string{"severity>=ERROR", `labels.user_id="42"`}) {
		t.Errorf("fork = %+v", fork)
	}
	fork = forkAddingLabel(raw, "resource", "a.b", "x")
	if fork.RawFilter != `(severity>=ERROR) AND resource.labels."a.b"="x"` || !fork.Query.isEmpty() || !fork.Query.TextCombineOr {
		t.Errorf("fork of a raw filter = %+v", fork)
	}
	fork = forkAddingLabel(base, "entry", "request_identifier", "0123456789abcdef")
	if fork.Name != "request_identifier=012345678…" {
		t.Errorf("long fork name = %q", fork.Name)
	}
	fork = forkAddingSeverity(filtered, 400)
	if fork.Name != "Warning" || !reflect.DeepEqual(fork.Query.SeveritySet, []int{400}) {
		t.Errorf("severity fork = %+v", fork)
	}
	fork = forkAddingSeverity(raw, 400)
	if fork.RawFilter != "(severity>=ERROR) AND severity=WARNING" || fork.Name != "Warning" {
		t.Errorf("severity fork of a raw filter = %+v", fork)
	}
}

func TestCloudSQLTexts(t *testing.T) {
	names := []string{"Recent 1000", "Errors only", "One log per unique label", "Flow window (START…END)"}
	if len(builtinSqlTemplates) != len(names) {
		t.Fatalf("%d built-in templates", len(builtinSqlTemplates))
	}
	for i, tpl := range builtinSqlTemplates {
		if tpl.Name != names[i] || tpl.ID != "" {
			t.Errorf("template %d = %q (id %q)", i, tpl.Name, tpl.ID)
		}
		if !sqlIsReadOnly(tpl.SQL) || !strings.HasSuffix(tpl.SQL, ";") || !strings.Contains(tpl.SQL, "insert_id") {
			t.Errorf("template %q is not a runnable list query", tpl.Name)
		}
	}
	if !strings.Contains(cloudSQLFlowWindow, "SELECT '', last_start, NULL, 'NOTICE', '──────────  window  ──────────', 1\n") {
		t.Error("the flow window's divider row changed")
	}

	cfg := newCloudStreamConfig("p")
	want := "-- Level 1 (Cloud Logging filter, fetched live): (all logs)\n" +
		"-- Level 2 (SQL filter): keep `insert_id`; the matching rows show in the list, in this order.\n" +
		cloudSQLRecent
	if got := generatedCloudSQL(cfg); got != want {
		t.Errorf("generated SQL:\n%s\nwant:\n%s", got, want)
	}
	cfg.LogName = "projects/p/logs/stdout"
	if got := generatedCloudSQL(cfg); !strings.HasPrefix(got, "-- Level 1 (Cloud Logging filter, fetched live): logName=\"projects/p/logs/stdout\"\n") {
		t.Errorf("generated SQL:\n%s", got)
	}

	readOnly := []struct {
		sql  string
		want bool
	}{
		{"SELECT 1", true},
		{"  \n with x as (select 1) select * from x", true},
		{"PRAGMA table_info(log_entry)", true},
		{"explain select 1", true},
		{"-- a comment\n/* and a block */ SELECT 1", true},
		{"DELETE FROM log_entry", false},
		{"-- SELECT\nDROP TABLE log_entry", false},
		{"-- only a comment", false},
		{"/* never closed SELECT 1", false},
		{"", false},
	}
	for _, c := range readOnly {
		if got := sqlIsReadOnly(c.sql); got != c.want {
			t.Errorf("sqlIsReadOnly(%q) = %v", c.sql, got)
		}
	}
	problems := []struct {
		sql     string
		hasData bool
		want    string
	}{
		{"SELECT 1", false, "No data captured yet — start the session first."},
		{"DELETE FROM log_entry", true, "Only read-only queries are allowed (SELECT / WITH / PRAGMA / EXPLAIN)."},
		{"SELECT 1", true, ""},
	}
	for _, c := range problems {
		if got := sqlRunProblem(c.sql, c.hasData); got != c.want {
			t.Errorf("sqlRunProblem(%q, %v) = %q", c.sql, c.hasData, got)
		}
	}
}

func seqEntries(seqs ...uint64) []cloudEntry {
	out := make([]cloudEntry, len(seqs))
	for i, seq := range seqs {
		out[i] = cloudEntry{Seq: seq}
	}
	return out
}

func feedSeqs(f *cloudFeed) []uint64 {
	out := make([]uint64, 0, len(f.entries()))
	for _, e := range f.entries() {
		out = append(out, e.Seq)
	}
	return out
}

func wantSeqs(t *testing.T, f *cloudFeed, want ...uint64) {
	t.Helper()
	if got := feedSeqs(f); !reflect.DeepEqual(got, append([]uint64{}, want...)) {
		t.Errorf("entries = %v, want %v", got, want)
	}
}

func TestCloudFeedSeqDedupe(t *testing.T) {
	f := newCloudFeed(100)
	if got := f.apply(nil); got != 0 {
		t.Errorf("an empty batch added %d", got)
	}
	if got := f.apply(seqEntries(10, 18)); got != 2 {
		t.Errorf("added %d", got)
	}
	// A batch overlapping what was delivered adds only the newer entries.
	if got := f.apply(seqEntries(10, 18, 26)); got != 1 {
		t.Errorf("added %d", got)
	}
	if got := f.apply(seqEntries(18, 26)); got != 0 {
		t.Errorf("a replayed batch added %d", got)
	}
	// Within one batch too.
	if got := f.apply(seqEntries(34, 34, 30, 42)); got != 2 {
		t.Errorf("added %d", got)
	}
	wantSeqs(t, f, 10, 18, 26, 34, 42)
}

func TestCloudFeedResync(t *testing.T) {
	f := newCloudFeed(100)
	f.apply(seqEntries(1, 2))

	// events.dropped: the batch that arrives before the range is held and delivered once, after.
	after, has := f.beginResync()
	if after != 2 || !has {
		t.Errorf("beginResync = %d %v", after, has)
	}
	if got := f.apply(seqEntries(5, 6)); got != 0 {
		t.Errorf("a held batch added %d", got)
	}
	wantSeqs(t, f, 1, 2)
	// The range overlaps the held batch.
	if got := f.finishResync(seqEntries(3, 4, 5)); got != 4 {
		t.Errorf("finishResync added %d", got)
	}
	wantSeqs(t, f, 1, 2, 3, 4, 5, 6)
	// Live batches flow again.
	if got := f.apply(seqEntries(7)); got != 1 {
		t.Errorf("added %d", got)
	}

	// A failed range still releases what was held.
	f.beginResync()
	f.apply(seqEntries(8))
	if got := f.finishResync(nil); got != 1 {
		t.Errorf("finishResync after a failed call added %d", got)
	}
	wantSeqs(t, f, 1, 2, 3, 4, 5, 6, 7, 8)
}

func TestCloudFeedSecondDropDuringResync(t *testing.T) {
	f := newCloudFeed(100)
	f.apply(seqEntries(1))
	f.beginResync()
	f.apply(seqEntries(4))
	// A second drop: batches stay held until both ranges are in.
	if after, has := f.beginResync(); after != 1 || !has {
		t.Errorf("second beginResync = %d %v", after, has)
	}
	f.apply(seqEntries(7))
	if got := f.finishResync(seqEntries(2, 3)); got != 2 {
		t.Errorf("first finishResync added %d", got)
	}
	wantSeqs(t, f, 1, 2, 3)
	if got := f.apply(seqEntries(8)); got != 0 {
		t.Errorf("a batch between the two ranges added %d", got)
	}
	// The second range covers the first one's entries and the second gap.
	if got := f.finishResync(seqEntries(2, 3, 4, 5, 6)); got != 5 {
		t.Errorf("second finishResync added %d", got)
	}
	wantSeqs(t, f, 1, 2, 3, 4, 5, 6, 7, 8)
	if got := f.apply(seqEntries(9)); got != 1 {
		t.Errorf("added %d after the resync", got)
	}
}

// The backfill after an open is the same pair: with nothing delivered yet it asks for the
// whole replay, and a live batch that races it is not lost and doesn't hide the replay.
func TestCloudFeedFirstBackfill(t *testing.T) {
	f := newCloudFeed(100)
	if f.opened(false) {
		t.Error("a first open restarted the feed")
	}
	if after, has := f.beginResync(); has || after != 0 {
		t.Errorf("beginResync = %d %v", after, has)
	}
	f.apply(seqEntries(30))
	if got := f.finishResync(seqEntries(10, 20, 30)); got != 3 {
		t.Errorf("added %d", got)
	}
	wantSeqs(t, f, 10, 20, 30)
	// finishResync without a beginResync just delivers.
	if got := f.finishResync(seqEntries(30, 40)); got != 1 {
		t.Errorf("added %d", got)
	}
	if got := f.apply(seqEntries(50)); got != 1 {
		t.Errorf("added %d", got)
	}
}

func TestCloudFeedPrepend(t *testing.T) {
	f := newCloudFeed(100)
	f.apply(seqEntries(100, 108))
	if got := f.prepend(seqEntries(76, 84, 92)); got != 3 {
		t.Errorf("prepended %d", got)
	}
	wantSeqs(t, f, 76, 84, 92, 100, 108)
	// A page that repeats loaded entries, or itself, adds only the new ones.
	if got := f.prepend(seqEntries(60, 68, 68, 76, 84)); got != 2 {
		t.Errorf("prepended %d", got)
	}
	wantSeqs(t, f, 60, 68, 76, 84, 92, 100, 108)
	if got := f.prepend(seqEntries(60, 68)); got != 0 {
		t.Errorf("a repeated page prepended %d", got)
	}
	if got := f.prepend(nil); got != 0 {
		t.Errorf("an empty page prepended %d", got)
	}
	// An older page doesn't move lastSeq: the next live entry is still accepted, an old one not.
	if got := f.apply(seqEntries(92, 116)); got != 1 {
		t.Errorf("added %d", got)
	}
	// An older page is not held by a resync.
	f.beginResync()
	if got := f.prepend(seqEntries(52)); got != 1 {
		t.Errorf("prepended %d during a resync", got)
	}
	f.finishResync(nil)
	wantSeqs(t, f, 52, 60, 68, 76, 84, 92, 100, 108, 116)
}

func TestCloudFeedResetAndRestart(t *testing.T) {
	f := newCloudFeed(3)
	f.apply(seqEntries(1, 2, 3, 4))
	if f.dropped() != 1 {
		t.Errorf("dropped = %d", f.dropped())
	}

	// reset keeps lastSeq: a range that races the clear can't bring the cleared entries back.
	f.reset()
	if len(f.entries()) != 0 || f.dropped() != 0 {
		t.Errorf("after reset: %v, dropped %d", feedSeqs(f), f.dropped())
	}
	if after, has := f.beginResync(); after != 4 || !has {
		t.Errorf("beginResync after reset = %d %v", after, has)
	}
	if got := f.finishResync(seqEntries(2, 3, 4)); got != 0 {
		t.Errorf("a stale range added %d", got)
	}
	if got := f.apply(seqEntries(4, 5)); got != 1 {
		t.Errorf("added %d", got)
	}
	wantSeqs(t, f, 5)

	// An open that attached to the session changes nothing.
	if f.opened(true) {
		t.Error("an attach restarted the feed")
	}
	wantSeqs(t, f, 5)

	// The daemon recreated the session: seqs start over, so lastSeq is forgotten.
	f.beginResync()
	f.apply(seqEntries(6))
	if !f.opened(false) {
		t.Error("a recreated session didn't restart the feed")
	}
	if len(f.entries()) != 0 {
		t.Errorf("after restart: %v", feedSeqs(f))
	}
	if after, has := f.beginResync(); has || after != 0 {
		t.Errorf("beginResync after restart = %d %v", after, has)
	}
	f.finishResync(nil)
	// The batch held from the old session is gone; the first resync is still to finish.
	if got := f.finishResync(seqEntries(1, 2)); got != 2 {
		t.Errorf("added %d", got)
	}
	wantSeqs(t, f, 1, 2)
	// A second open of the new session, with nothing lost, is not a restart.
	if f.opened(true) {
		t.Error("restarted twice")
	}

	// A view cleared before the daemon restarted still restarts: its lastSeq is from the old
	// session.
	f.reset()
	if !f.opened(false) {
		t.Error("a cleared feed kept the old session's lastSeq")
	}
	if got := f.apply(seqEntries(1)); got != 1 {
		t.Errorf("added %d", got)
	}

	g := newCloudFeed(10)
	g.apply(seqEntries(5))
	g.restart()
	if got := g.apply(seqEntries(1)); got != 1 {
		t.Errorf("after restart added %d", got)
	}
}

func TestCloudFeedCap(t *testing.T) {
	f := newCloudFeed(3)
	if got := f.apply(seqEntries(1, 2)); got != 2 || f.dropped() != 0 {
		t.Errorf("added %d, dropped %d", got, f.dropped())
	}
	if got := f.apply(seqEntries(3, 4, 5)); got != 3 {
		t.Errorf("added %d", got)
	}
	wantSeqs(t, f, 3, 4, 5)
	if f.dropped() != 2 {
		t.Errorf("dropped = %d", f.dropped())
	}
	// A batch larger than the limit keeps its newest entries.
	f.apply(seqEntries(6, 7, 8, 9, 10))
	wantSeqs(t, f, 8, 9, 10)
	if f.dropped() != 7 {
		t.Errorf("dropped = %d", f.dropped())
	}
	// An older page is kept whole until the next live batch trims the front.
	f.prepend(seqEntries(6, 7))
	wantSeqs(t, f, 6, 7, 8, 9, 10)
	if f.dropped() != 7 {
		t.Errorf("dropped = %d", f.dropped())
	}
	f.apply(seqEntries(11))
	wantSeqs(t, f, 9, 10, 11)
	if f.dropped() != 10 {
		t.Errorf("dropped = %d", f.dropped())
	}
	// A range delivered by a resync is trimmed the same way.
	f.beginResync()
	f.finishResync(seqEntries(12, 13))
	wantSeqs(t, f, 11, 12, 13)
	f.reset()
	if f.dropped() != 0 {
		t.Errorf("dropped after reset = %d", f.dropped())
	}
	if newCloudFeed(0).limit != cloudFeedDefaultLimit {
		t.Error("a feed without a limit doesn't take the app's")
	}
}

func TestSearchCloudEntries(t *testing.T) {
	entries := []cloudEntry{
		{Seq: 1, Message: "Login OK user=42", LogID: "stdout", Severity: 200},
		{Seq: 2, Message: "boom", LogID: "run.googleapis.com/requests", Severity: 500},
		{Seq: 3, Message: "quiet", LogID: "stderr", Severity: 0, Labels: map[string]string{"User_ID": "Alice"}},
		{Seq: 4, Message: "raw only", Raw: "needle", ResourceLabels: map[string]string{"zone": "needle"}, Trace: "needle"},
	}
	cases := []struct {
		query string
		want  []int
	}{
		{"", []int{0, 1, 2, 3}},
		{"login", []int{0}},
		{"LOGIN ok", []int{0}},
		{"REQUESTS", []int{1}},
		{"std", []int{0, 2}},
		{"error", []int{1}},
		{"default", []int{2, 3}},
		{"user_id", []int{2}},
		{"alice", []int{2}},
		// Raw JSON, resource labels and the trace are not searched.
		{"needle", []int{}},
		{"nothing matches this", []int{}},
	}
	for _, c := range cases {
		if got := searchCloudEntries(entries, c.query); !reflect.DeepEqual(got, c.want) {
			t.Errorf("search %q = %v, want %v", c.query, got, c.want)
		}
		for i, e := range entries {
			if got := cloudEntryMatches(e, c.query); got != containsInt(c.want, i) {
				t.Errorf("cloudEntryMatches(%d, %q) = %v", i, c.query, got)
			}
		}
	}
	if got := cloudExportText([]cloudEntry{{Raw: "{a}"}, {Raw: "{b}"}}); got != "{a}\n{b}" {
		t.Errorf("export = %q", got)
	}
}

func containsInt(list []int, n int) bool {
	for _, item := range list {
		if item == n {
			return true
		}
	}
	return false
}

func sqlRows(rows ...[]any) [][]*string {
	out := make([][]*string, len(rows))
	for i, row := range rows {
		out[i] = make([]*string, len(row))
		for j, cell := range row {
			if s, ok := cell.(string); ok {
				out[i][j] = &s
			}
		}
	}
	return out
}

func ringEntry(seq uint64, insertID string) cloudEntry {
	return cloudEntry{Seq: seq, InsertID: insertID, Severity: 200, LogID: "x", Message: "m"}
}

// TestMapSqlRows is CloudSqlMappingTests.
func TestMapSqlRows(t *testing.T) {
	columns := []string{"insert_id", "seq", "time", "severity_name", "text_payload", "is_marker"}

	cols := findSQLColumns(columns)
	if cols != (sqlColumns{id: 0, seq: 1, message: 4, severity: 3, marker: 5}) {
		t.Errorf("columns = %+v", cols)
	}
	if !cols.isMarker(sqlRows([]any{"", "2", nil, "NOTICE", "x", "1"})[0]) || cols.isMarker(sqlRows([]any{"b", "2", nil, "INFO", "x", "0"})[0]) {
		t.Error("isMarker is wrong")
	}
	if got := findSQLColumns([]string{"Message", "SEQ", "Marker"}); got != (sqlColumns{id: -1, seq: 1, message: 0, severity: -1, marker: 2}) {
		t.Errorf("columns by other names = %+v", got)
	}

	// Two flow windows with a divider before each.
	ring := []cloudEntry{ringEntry(2, "b"), ringEntry(3, "c"), ringEntry(4, "d"), ringEntry(7, "g"), ringEntry(8, "h")}
	result := dbResultSet{Columns: columns, Rows: sqlRows(
		[]any{"", "2", nil, "NOTICE", "----- window -----", "1"},
		[]any{"b", "2", nil, "INFO", "flow START checkout", "0"},
		[]any{"c", "3", nil, "INFO", "validating cart", "0"},
		[]any{"d", "4", nil, "INFO", "charging card", "0"},
		[]any{"", "7", nil, "NOTICE", "----- window -----", "1"},
		[]any{"g", "7", nil, "INFO", "flow START login", "0"},
		[]any{"h", "8", nil, "INFO", "check password", "0"},
	)}
	rows, problem := mapSqlRows(result, ring)
	if problem != "" || len(rows) != 7 {
		t.Fatalf("%d rows, problem %q", len(rows), problem)
	}
	if !rows[0].Marker || rows[0].Entry != nil || rows[0].Text != "----- window -----" || rows[0].Severity != 300 {
		t.Errorf("row 0 = %+v", rows[0])
	}
	if rows[1].Marker || rows[1].Entry == nil || rows[1].Entry.InsertID != "b" || rows[1].Text != "m" || rows[1].Severity != 200 {
		t.Errorf("row 1 = %+v", rows[1])
	}
	if !rows[4].Marker || rows[5].Marker || rows[5].Entry.InsertID != "g" {
		t.Errorf("rows 4, 5 = %+v %+v", rows[4], rows[5])
	}
	if want := []string{"", "2", "", "NOTICE", "----- window -----", "1"}; !reflect.DeepEqual(rows[0].Cells, want) {
		t.Errorf("cells = %q", rows[0].Cells)
	}

	// A LIMIT is honoured exactly, in result order.
	var many []cloudEntry
	for i := uint64(1); i <= 25; i++ {
		many = append(many, ringEntry(i, "id"+string(rune('a'+i))))
	}
	rows, _ = mapSqlRows(dbResultSet{Columns: columns, Rows: sqlRows(
		[]any{many[24].InsertID, "25", nil, "INFO", "m", "0"},
		[]any{many[23].InsertID, "24", nil, "INFO", "m", "0"},
	)}, many)
	if len(rows) != 2 || rows[0].Entry.Seq != 25 || rows[1].Entry.Seq != 24 {
		t.Errorf("limited rows = %+v", rows)
	}

	// A row that maps to nothing is a marker built from its own columns.
	rows, _ = mapSqlRows(dbResultSet{Columns: []string{"insert_id", "seq", "severity_name", "text_payload"},
		Rows: sqlRows([]any{"", "999", "WARNING", "orphan line"})}, nil)
	if len(rows) != 1 || !rows[0].Marker || rows[0].Text != "orphan line" || rows[0].Severity != 400 {
		t.Errorf("orphan row = %+v", rows)
	}

	cases := []struct {
		name    string
		columns []string
		rows    [][]*string
		search  string
		want    []string // "e<seq>" for an entry row, "m:<text>" for a marker
	}{
		{"insert_id wins over seq", []string{"insert_id", "seq"}, sqlRows([]any{"c", "2"}), "", []string{"e3"}},
		{"seq when the insert_id is unknown", []string{"insert_id", "seq"}, sqlRows([]any{"gone", "4"}), "", []string{"e4"}},
		{"seq alone", []string{"seq"}, sqlRows([]any{"8"}, []any{"x"}, []any{nil}), "", []string{"e8", "m:", "m:"}},
		{"insert_id alone", []string{"INSERT_ID"}, sqlRows([]any{"h"}, []any{nil}), "", []string{"e8", "m:"}},
		{"an entry shows once", []string{"insert_id"}, sqlRows([]any{"b"}, []any{"b"}, []any{"c"}), "", []string{"e2", "e3"}},
		{"a flagged row is a marker even when it maps", []string{"insert_id", "message", "marker"}, sqlRows([]any{"b", "div", "YES"}, []any{"b", "x", "no"}), "", []string{"m:div", "e2"}},
		{"a short row reads as missing", []string{"seq", "text_payload", "is_marker"}, sqlRows([]any{"3"}, []any{}), "", []string{"e3", "m:"}},
		{"the search filters entries and markers", []string{"insert_id", "text_payload", "severity_name"},
			sqlRows([]any{"b", "ignored", "ERROR"}, []any{"", "window", "NOTICE"}, []any{"", "other", "ERROR"}), "window", []string{"m:window"}},
		{"the search sees a marker's severity", []string{"insert_id", "text_payload", "severity_name"},
			sqlRows([]any{"b", "x", nil}, []any{"", "window", "notice"}), "NOTICE", []string{"m:window"}},
	}
	for _, c := range cases {
		rows, problem := mapSqlRowsMatching(dbResultSet{Columns: c.columns, Rows: c.rows}, ring, c.search)
		var got []string
		for _, row := range rows {
			if row.Marker {
				got = append(got, "m:"+row.Text)
			} else {
				got = append(got, "e"+string(rune('0'+row.Entry.Seq)))
			}
		}
		if problem != "" || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %v (problem %q), want %v", c.name, got, problem, c.want)
		}
	}

	// A result with neither column can't drive the list.
	rows, problem = mapSqlRows(dbResultSet{Columns: []string{"time", "text_payload"}, Rows: sqlRows([]any{"t", "x"})}, ring)
	if rows != nil || problem != "Your SQL must SELECT an `insert_id` (or `seq`) column so the matching logs can be shown — e.g. SELECT insert_id, … FROM log_entry …" {
		t.Errorf("unmappable result: %v, %q", rows, problem)
	}

	// The daemon's result for the wire entry: one captured row and one that maps to nothing.
	wire := mustDecode[dbResultSet](t, wireCloudSQLResult)
	entry := mustDecode[cloudEntry](t, wireCloudEntry)
	rows, problem = mapSqlRows(wire, []cloudEntry{entry})
	if problem != "" || len(rows) != 2 || rows[0].Entry == nil || rows[0].Entry.Seq != 1<<40 || rows[0].Text != "login ok user=42" {
		t.Fatalf("wire result: %+v, %q", rows, problem)
	}
	if !rows[1].Marker || rows[1].Text != "" || rows[1].Severity != 0 || !reflect.DeepEqual(rows[1].Cells, []string{"", "2", "", "", "", ""}) {
		t.Errorf("wire marker = %+v", rows[1])
	}
}
