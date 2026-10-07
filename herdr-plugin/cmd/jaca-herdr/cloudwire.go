package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// The Cloud Logging wire format: what the daemon's cloud.* methods take and return and what it
// publishes on cloud.state and the per-session topics (Sources/Core/Daemon/Areas/CloudArea.swift
// and Sources/Core/CloudLogging).
//
// Decoding follows the Swift decoders: a missing or null key takes the Swift default, and what
// Swift requires (a project's projectID, an entry's seq) is required here. Encoding writes the
// keys the app writes, with "" standing for a Swift nil, so a value read from the daemon goes
// back as it came.

const cloudStateTopic = "cloud.state"

// cloudTopics is CloudArea's per-session topics: live entries, older pages and stream state.
// The id is a UUID in the uppercase form the daemon prints.
func cloudTopics(sessionID string) (entries, older, state string) {
	id := strings.ToUpper(sessionID)
	return "cloud.entries." + id, "cloud.older." + id, "cloud.sstate." + id
}

// The states of cloudAuthState.
const (
	cloudAuthUnknown          = "unknown"
	cloudAuthNotInstalled     = "notInstalled"
	cloudAuthNotAuthenticated = "notAuthenticated"
	cloudAuthAuthenticated    = "authenticated"
)

// cloudAuthState mirrors CloudAuthState: gcloud detection and sign-in. Account is set only when
// authenticated. A state from a newer build, and the zero value, read as unknown.
type cloudAuthState struct {
	State, Account string
}

func (a cloudAuthState) MarshalJSON() ([]byte, error) {
	switch a.State {
	case cloudAuthAuthenticated:
		return json.Marshal(struct {
			State   string `json:"state"`
			Account string `json:"account"`
		}{a.State, a.Account})
	case cloudAuthNotInstalled, cloudAuthNotAuthenticated:
		return json.Marshal(struct {
			State string `json:"state"`
		}{a.State})
	}
	return json.Marshal(struct {
		State string `json:"state"`
	}{cloudAuthUnknown})
}

func (a *cloudAuthState) UnmarshalJSON(data []byte) error {
	var raw struct {
		State   string `json:"state"`
		Account string `json:"account"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	switch raw.State {
	case cloudAuthAuthenticated:
		*a = cloudAuthState{State: raw.State, Account: raw.Account}
	case cloudAuthNotInstalled, cloudAuthNotAuthenticated:
		*a = cloudAuthState{State: raw.State}
	default:
		*a = cloudAuthState{State: cloudAuthUnknown}
	}
	return nil
}

// The results of cloudAddResult.
const (
	cloudAddAdded         = "added"
	cloudAddAlreadyExists = "alreadyExists"
	cloudAddFailure       = "failure"
)

// cloudAddResult mirrors CloudAddProjectResult, what cloud.addProject returns. Message is set
// only for a failure. Anything that isn't added or alreadyExists reads as a failure.
type cloudAddResult struct {
	Result, Message string
}

func (r cloudAddResult) MarshalJSON() ([]byte, error) {
	switch r.Result {
	case cloudAddAdded, cloudAddAlreadyExists:
		return json.Marshal(struct {
			Result string `json:"result"`
		}{r.Result})
	}
	return json.Marshal(struct {
		Result  string `json:"result"`
		Message string `json:"message"`
	}{cloudAddFailure, r.Message})
}

func (r *cloudAddResult) UnmarshalJSON(data []byte) error {
	var raw struct {
		Result  string `json:"result"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	switch raw.Result {
	case cloudAddAdded, cloudAddAlreadyExists:
		*r = cloudAddResult{Result: raw.Result}
	default:
		*r = cloudAddResult{Result: cloudAddFailure, Message: raw.Message}
	}
	return nil
}

// labelExampleRule mirrors LabelExampleRule: how many example values of a label key the SQL
// assistant gets. A missing count is 1, and a count below 1 reads as 1.
type labelExampleRule struct {
	All   bool `json:"all"`
	Count int  `json:"count"`
}

func (r *labelExampleRule) UnmarshalJSON(data []byte) error {
	raw := struct {
		All   bool `json:"all"`
		Count *int `json:"count"`
	}{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	r.All, r.Count = raw.All, 1
	if raw.Count != nil && *raw.Count > 1 {
		r.Count = *raw.Count
	}
	return nil
}

// cloudProject mirrors CloudProject: a configured GCP project and its per-project state. The
// maps are keyed by full log name, with "" for the project-wide bucket used when no log name
// is selected. SelectedLogName is "" when all logs are selected.
type cloudProject struct {
	ProjectID, DisplayName, SelectedLogName string
	LogNames                                []string
	LabelKeysByLogName                      map[string][]string
	FavoriteLabelKeysByLogName              map[string][]string
	LabelExampleRulesByLogName              map[string]map[string]labelExampleRule
}

type cloudProjectWire struct {
	ProjectID                  *string                                `json:"projectID"`
	DisplayName                string                                 `json:"displayName"`
	SelectedLogName            string                                 `json:"selectedLogName,omitempty"`
	LogNames                   []string                               `json:"logNames"`
	LabelKeysByLogName         map[string][]string                    `json:"labelKeysByLogName"`
	FavoriteLabelKeysByLogName map[string][]string                    `json:"favoriteLabelKeysByLogName"`
	LabelExampleRulesByLogName map[string]map[string]labelExampleRule `json:"labelExampleRulesByLogName"`
}

func (p cloudProject) MarshalJSON() ([]byte, error) {
	id := p.ProjectID
	wire := cloudProjectWire{
		ProjectID:                  &id,
		DisplayName:                p.DisplayName,
		SelectedLogName:            p.SelectedLogName,
		LogNames:                   emptyIfNil(p.LogNames),
		LabelKeysByLogName:         p.LabelKeysByLogName,
		FavoriteLabelKeysByLogName: p.FavoriteLabelKeysByLogName,
		LabelExampleRulesByLogName: p.LabelExampleRulesByLogName,
	}
	// The app writes {} for an empty dictionary, never null.
	if wire.LabelKeysByLogName == nil {
		wire.LabelKeysByLogName = map[string][]string{}
	}
	if wire.FavoriteLabelKeysByLogName == nil {
		wire.FavoriteLabelKeysByLogName = map[string][]string{}
	}
	if wire.LabelExampleRulesByLogName == nil {
		wire.LabelExampleRulesByLogName = map[string]map[string]labelExampleRule{}
	}
	return json.Marshal(wire)
}

func (p *cloudProject) UnmarshalJSON(data []byte) error {
	var wire cloudProjectWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if wire.ProjectID == nil {
		return errors.New("cloud project without a projectID")
	}
	*p = cloudProject{
		ProjectID:                  *wire.ProjectID,
		DisplayName:                wire.DisplayName,
		SelectedLogName:            wire.SelectedLogName,
		LogNames:                   wire.LogNames,
		LabelKeysByLogName:         wire.LabelKeysByLogName,
		FavoriteLabelKeysByLogName: wire.FavoriteLabelKeysByLogName,
		LabelExampleRulesByLogName: wire.LabelExampleRulesByLogName,
	}
	return nil
}

// title is CloudProject.title: the display name if set, else the project id.
func (p cloudProject) title() string {
	if p.DisplayName == "" {
		return p.ProjectID
	}
	return p.DisplayName
}

// labelKeys is CloudProject.currentLabelKeys: the keys detected for the selected log name, or
// for the project-wide bucket when none is selected.
func (p cloudProject) labelKeys() []string {
	return p.LabelKeysByLogName[p.SelectedLogName]
}

// favoriteLabelKeys is CloudProject.currentFavoriteLabelKeys.
func (p cloudProject) favoriteLabelKeys() []string {
	return p.FavoriteLabelKeysByLogName[p.SelectedLogName]
}

// The match modes of a condition (CloudMatchMode).
const (
	cloudMatchContains    = "contains"
	cloudMatchExact       = "exact"
	cloudMatchRegex       = "regex"
	cloudMatchNotContains = "notContains"
)

// cloudMatchModes is CloudMatchMode.allCases, in the app's order.
var cloudMatchModes = []string{cloudMatchContains, cloudMatchExact, cloudMatchRegex, cloudMatchNotContains}

// The label namespaces of a label condition (LabelScope): labels.<key> or resource.labels.<key>.
const (
	labelScopeEntry    = "entry"
	labelScopeResource = "resource"
)

func isCloudMatchMode(mode string) bool {
	return containsString(cloudMatchModes, mode)
}

// wireUUID reads a condition's or template's id as Swift's UUID decoder does: a missing id is a
// new one, and text that isn't a UUID is an error. The id is kept uppercase, as the app
// writes it.
func wireUUID(id *string) (string, error) {
	if id == nil {
		return newUUID(), nil
	}
	if !isUUID(*id) {
		return "", fmt.Errorf("%q is not a UUID", *id)
	}
	return strings.ToUpper(*id), nil
}

// textCondition mirrors TextCondition: one textPayload condition. A missing mode is contains.
// A mode this build doesn't know fails the decode, as it does in the app.
type textCondition struct {
	ID, Mode, Value string
}

// newTextCondition is TextCondition(): a new id and the contains mode.
func newTextCondition() textCondition {
	return textCondition{ID: newUUID(), Mode: cloudMatchContains}
}

type textConditionWire struct {
	ID    *string `json:"id"`
	Mode  *string `json:"mode"`
	Value string  `json:"value"`
}

// MarshalJSON writes an id and a mode even for a condition built without them, since the
// daemon rejects a query that has either missing or empty.
func (c textCondition) MarshalJSON() ([]byte, error) {
	id, mode := c.ID, c.Mode
	if id == "" {
		id = newUUID()
	}
	if mode == "" {
		mode = cloudMatchContains
	}
	return json.Marshal(textConditionWire{ID: &id, Mode: &mode, Value: c.Value})
}

func (c *textCondition) UnmarshalJSON(data []byte) error {
	var wire textConditionWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	id, err := wireUUID(wire.ID)
	if err != nil {
		return err
	}
	mode := stringOr(wire.Mode, cloudMatchContains)
	if !isCloudMatchMode(mode) {
		return fmt.Errorf("unknown match mode %q", mode)
	}
	*c = textCondition{ID: id, Mode: mode, Value: wire.Value}
	return nil
}

// labelCondition mirrors LabelCondition: one label condition. A missing scope is entry and a
// missing mode is exact. An unknown scope or mode fails the decode, as it does in the app.
type labelCondition struct {
	ID, Key, Scope, Mode, Value string
}

// newLabelCondition is LabelCondition(): a new id, the entry scope and the exact mode.
func newLabelCondition() labelCondition {
	return labelCondition{ID: newUUID(), Scope: labelScopeEntry, Mode: cloudMatchExact}
}

type labelConditionWire struct {
	ID    *string `json:"id"`
	Key   string  `json:"key"`
	Scope *string `json:"scope"`
	Mode  *string `json:"mode"`
	Value string  `json:"value"`
}

// MarshalJSON writes an id, a scope and a mode even for a condition built without them.
func (c labelCondition) MarshalJSON() ([]byte, error) {
	id, scope, mode := c.ID, c.Scope, c.Mode
	if id == "" {
		id = newUUID()
	}
	if scope == "" {
		scope = labelScopeEntry
	}
	if mode == "" {
		mode = cloudMatchExact
	}
	return json.Marshal(labelConditionWire{ID: &id, Key: c.Key, Scope: &scope, Mode: &mode, Value: c.Value})
}

func (c *labelCondition) UnmarshalJSON(data []byte) error {
	var wire labelConditionWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	id, err := wireUUID(wire.ID)
	if err != nil {
		return err
	}
	scope := stringOr(wire.Scope, labelScopeEntry)
	if scope != labelScopeEntry && scope != labelScopeResource {
		return fmt.Errorf("unknown label scope %q", scope)
	}
	mode := stringOr(wire.Mode, cloudMatchExact)
	if !isCloudMatchMode(mode) {
		return fmt.Errorf("unknown match mode %q", mode)
	}
	*c = labelCondition{ID: id, Key: wire.Key, Scope: scope, Mode: mode, Value: wire.Value}
	return nil
}

// cloudQuery mirrors CloudLogQuery: the structured server-side query of one session.
// TextCombineOr and LabelCombineOr say whether several conditions are OR-ed (true) or AND-ed.
// A non-empty SeveritySet replaces MinSeverity. Severities are CloudSeverity raw values.
//
// The app's empty query has TextCombineOr true, which the zero value doesn't: start from
// newCloudQuery.
type cloudQuery struct {
	TextConditions  []textCondition
	TextCombineOr   bool
	MinSeverity     *int
	SeveritySet     []int
	LabelConditions []labelCondition
	LabelCombineOr  bool
}

// newCloudQuery is CloudLogQuery(): no conditions, text conditions OR-ed, label conditions
// AND-ed.
func newCloudQuery() cloudQuery {
	return cloudQuery{TextCombineOr: true}
}

type cloudQueryWire struct {
	TextConditions  []textCondition  `json:"textConditions"`
	TextCombineOr   *bool            `json:"textCombineOr"`
	MinSeverity     *int             `json:"minSeverity,omitempty"`
	SeveritySet     []int            `json:"severitySet"`
	LabelConditions []labelCondition `json:"labelConditions"`
	LabelCombineOr  bool             `json:"labelCombineOr"`
}

func (q cloudQuery) MarshalJSON() ([]byte, error) {
	or := q.TextCombineOr
	return json.Marshal(cloudQueryWire{
		TextConditions:  emptyIfNil(q.TextConditions),
		TextCombineOr:   &or,
		MinSeverity:     q.MinSeverity,
		SeveritySet:     emptyIfNil(q.SeveritySet),
		LabelConditions: emptyIfNil(q.LabelConditions),
		LabelCombineOr:  q.LabelCombineOr,
	})
}

// UnmarshalJSON fails on a severity that isn't one of CloudSeverity's levels, as the app's
// decoder does: such a query can't be sent back to the daemon.
func (q *cloudQuery) UnmarshalJSON(data []byte) error {
	var wire cloudQueryWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if wire.MinSeverity != nil && !isCloudSeverity(*wire.MinSeverity) {
		return fmt.Errorf("unknown severity %d", *wire.MinSeverity)
	}
	for _, sev := range wire.SeveritySet {
		if !isCloudSeverity(sev) {
			return fmt.Errorf("unknown severity %d", sev)
		}
	}
	*q = cloudQuery{
		TextConditions:  wire.TextConditions,
		TextCombineOr:   wire.TextCombineOr == nil || *wire.TextCombineOr,
		MinSeverity:     wire.MinSeverity,
		SeveritySet:     wire.SeveritySet,
		LabelConditions: wire.LabelConditions,
		LabelCombineOr:  wire.LabelCombineOr,
	}
	return nil
}

// isEmpty is CloudLogQuery.isEmpty: no condition with a value, no severity floor above DEFAULT
// and no severity set.
func (q cloudQuery) isEmpty() bool {
	for _, c := range q.TextConditions {
		if c.Value != "" {
			return false
		}
	}
	if q.MinSeverity != nil && *q.MinSeverity != 0 {
		return false
	}
	if len(q.SeveritySet) > 0 {
		return false
	}
	for _, c := range q.LabelConditions {
		if c.Key != "" && c.Value != "" {
			return false
		}
	}
	return true
}

// cloudDefaultMinutes is the window of a new session: CloudStreamConfig's .last(minutes: 15).
const cloudDefaultMinutes = 15

// cloudTimeRange mirrors CloudTimeRange: Minutes > 0 is the live "last N minutes", otherwise
// the range is the one-shot fetch between Start and End.
//
// The zero value is no range at all. Since the wire needs one, it encodes, and every method
// reads it, as the default last 15 minutes.
type cloudTimeRange struct {
	Minutes    int
	Start, End time.Time
}

// normalized replaces the zero value with the default range.
func (r cloudTimeRange) normalized() cloudTimeRange {
	if r.Minutes <= 0 && r.Start.IsZero() && r.End.IsZero() {
		return cloudTimeRange{Minutes: cloudDefaultMinutes}
	}
	return r
}

// isLive is CloudTimeRange.isLive: a relative range streams, an absolute one is a finite fetch.
func (r cloudTimeRange) isLive() bool {
	return r.normalized().Minutes > 0
}

func (r cloudTimeRange) MarshalJSON() ([]byte, error) {
	r = r.normalized()
	if r.Minutes > 0 {
		return json.Marshal(map[string]any{"last": map[string]int{"minutes": r.Minutes}})
	}
	return json.Marshal(map[string]any{"between": map[string]string{
		"start": wireDate(r.Start), "end": wireDate(r.End),
	}})
}

// UnmarshalJSON reads {"last":{"minutes":N}} or {"between":{"start":…,"end":…}}. A null leaves
// the range as it was. A last of less than a minute, which this type can't hold, reads as one
// minute.
func (r *cloudTimeRange) UnmarshalJSON(data []byte) error {
	if jsonIsNull(data) {
		return nil
	}
	var wire struct {
		Last *struct {
			Minutes *int `json:"minutes"`
		} `json:"last"`
		Between *struct {
			Start json.RawMessage `json:"start"`
			End   json.RawMessage `json:"end"`
		} `json:"between"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	switch {
	case wire.Last != nil && wire.Last.Minutes != nil:
		*r = cloudTimeRange{Minutes: max(1, *wire.Last.Minutes)}
		return nil
	case wire.Between != nil:
		start, err := parseWireDate(wire.Between.Start)
		if err != nil {
			return err
		}
		end, err := parseWireDate(wire.Between.End)
		if err != nil {
			return err
		}
		*r = cloudTimeRange{Start: start, End: end}
		return nil
	}
	return errors.New("time range is neither last nor between")
}

// parseWireDate reads a date as JSONDecoder.daemon does: ISO-8601 text, with or without
// fractional seconds, or a number of seconds since 1970.
func parseWireDate(raw json.RawMessage) (time.Time, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if t, err := time.Parse(time.RFC3339Nano, text); err == nil {
			return t, nil
		}
	}
	var seconds float64
	if len(raw) > 0 && !jsonIsNull(raw) && json.Unmarshal(raw, &seconds) == nil {
		return epochTime(seconds), nil
	}
	return time.Time{}, errors.New("expected an ISO-8601 date")
}

// epochTime is Date(timeIntervalSince1970:), rounded to the microsecond: a double holds today's
// dates to about a quarter of one, so the digits below it are noise that would turn .123 into
// .122999.
func epochTime(seconds float64) time.Time {
	whole, frac := math.Modf(seconds)
	return time.Unix(int64(whole), int64(math.Round(frac*1e6))*1000)
}

func jsonIsNull(data []byte) bool {
	return string(bytes.TrimSpace(data)) == "null"
}

// cloudStreamConfig mirrors CloudStreamConfig: the server-side query a session streams. LogName
// is the full log name, "" for all logs. A non-blank RawFilter (from a Logs Explorer URL)
// replaces LogName and Query.
//
// The daemon requires projectID, query and timeRange, so all three are always written.
// Decoding is more lenient: a missing query or timeRange takes the default of a new session.
type cloudStreamConfig struct {
	ProjectID string         `json:"projectID"`
	Query     cloudQuery     `json:"query"`
	TimeRange cloudTimeRange `json:"timeRange"`
	LogName   string         `json:"logName,omitempty"`
	RawFilter string         `json:"rawFilter,omitempty"`
}

// newCloudStreamConfig is CloudStreamConfig(projectID:): an empty query over the last 15
// minutes of all logs.
func newCloudStreamConfig(projectID string) cloudStreamConfig {
	return cloudStreamConfig{
		ProjectID: projectID,
		Query:     newCloudQuery(),
		TimeRange: cloudTimeRange{Minutes: cloudDefaultMinutes},
	}
}

func (c *cloudStreamConfig) UnmarshalJSON(data []byte) error {
	type plain cloudStreamConfig
	out := plain(newCloudStreamConfig(""))
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	*c = cloudStreamConfig(out)
	return nil
}

// cloudStreamState mirrors CloudStreamState, published on a session's state topic. IsLoading
// is the first query being in flight, HasData the session's SQL database existing (it has been
// started once). A missing hasMoreOlder is true.
type cloudStreamState struct {
	IsRunning     bool   `json:"isRunning"`
	IsLoading     bool   `json:"isLoading"`
	StatusMessage string `json:"statusMessage,omitempty"`
	OlderLoading  bool   `json:"olderLoading"`
	HasMoreOlder  bool   `json:"hasMoreOlder"`
	HasData       bool   `json:"hasData"`
}

func (s *cloudStreamState) UnmarshalJSON(data []byte) error {
	type plain cloudStreamState
	out := plain{HasMoreOlder: true}
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	*s = cloudStreamState(out)
	return nil
}

// The payload kinds of an entry (CloudLogEntry.PayloadKind).
const (
	cloudPayloadText  = "text"
	cloudPayloadJSON  = "json"
	cloudPayloadProto = "proto"
	cloudPayloadNone  = "none"
)

// cloudEntry mirrors CloudLogEntry's wire format. Seq is the session's stamp, which orders
// entries: live ones rise from 1<<40 and older pages sit below. Timestamp and ReceiveTimestamp
// are seconds since 1970. Message is the rendered payload and Raw the whole entry as pretty
// JSON, falling back to Message when the wire omits it.
//
// "" stands for a missing trace, spanId and httpRequestSummary. A missing or unknown severity
// is DEFAULT and a missing or unknown payloadKind is text, as in the app.
type cloudEntry struct {
	Seq                               uint64
	InsertID                          string
	Timestamp                         float64
	ReceiveTimestamp                  *float64
	Severity                          int
	LogName, LogID, Message           string
	PayloadKind                       string
	Labels                            map[string]string
	ResourceType                      string
	ResourceLabels                    map[string]string
	Trace, SpanID, HTTPRequestSummary string
	Raw                               string
}

// time is the entry's timestamp.
func (e cloudEntry) time() time.Time {
	return epochTime(e.Timestamp)
}

// tag is CloudLogEntry.tag: the list's tag column, the value of the "tag" label.
func (e cloudEntry) tag() string {
	return e.Labels["tag"]
}

// MarshalJSON writes what CloudLogEntry.encode writes: the empty and the defaulted fields are
// left out, and raw only when it differs from the message.
func (e cloudEntry) MarshalJSON() ([]byte, error) {
	wire := struct {
		Seq                uint64            `json:"seq"`
		InsertID           string            `json:"insertId,omitempty"`
		Timestamp          float64           `json:"timestamp"`
		ReceiveTimestamp   *float64          `json:"receiveTimestamp,omitempty"`
		Severity           int               `json:"severity"`
		LogName            string            `json:"logName"`
		LogID              string            `json:"logId"`
		Message            string            `json:"message"`
		PayloadKind        string            `json:"payloadKind"`
		Labels             map[string]string `json:"labels,omitempty"`
		ResourceType       string            `json:"resourceType,omitempty"`
		ResourceLabels     map[string]string `json:"resourceLabels,omitempty"`
		Trace              string            `json:"trace,omitempty"`
		SpanID             string            `json:"spanId,omitempty"`
		HTTPRequestSummary string            `json:"httpRequestSummary,omitempty"`
		Raw                string            `json:"raw,omitempty"`
	}{
		Seq: e.Seq, InsertID: e.InsertID, Timestamp: e.Timestamp, ReceiveTimestamp: e.ReceiveTimestamp,
		Severity: knownCloudSeverity(e.Severity), LogName: e.LogName, LogID: e.LogID, Message: e.Message,
		PayloadKind: knownPayloadKind(e.PayloadKind), Labels: e.Labels, ResourceType: e.ResourceType,
		ResourceLabels: e.ResourceLabels, Trace: e.Trace, SpanID: e.SpanID,
		HTTPRequestSummary: e.HTTPRequestSummary,
	}
	if e.Raw != e.Message {
		wire.Raw = e.Raw
	}
	return json.Marshal(wire)
}

func (e *cloudEntry) UnmarshalJSON(data []byte) error {
	var wire struct {
		Seq                *uint64           `json:"seq"`
		InsertID           string            `json:"insertId"`
		Timestamp          float64           `json:"timestamp"`
		ReceiveTimestamp   *float64          `json:"receiveTimestamp"`
		Severity           json.RawMessage   `json:"severity"`
		LogName            string            `json:"logName"`
		LogID              string            `json:"logId"`
		Message            string            `json:"message"`
		PayloadKind        json.RawMessage   `json:"payloadKind"`
		Labels             map[string]string `json:"labels"`
		ResourceType       string            `json:"resourceType"`
		ResourceLabels     map[string]string `json:"resourceLabels"`
		Trace              string            `json:"trace"`
		SpanID             string            `json:"spanId"`
		HTTPRequestSummary string            `json:"httpRequestSummary"`
		Raw                *string           `json:"raw"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if wire.Seq == nil {
		return errors.New("cloud entry without a seq")
	}
	// Severity and payload kind are read leniently: a value this build doesn't know costs the
	// field, not the entry.
	var severity float64
	if len(wire.Severity) > 0 {
		_ = json.Unmarshal(wire.Severity, &severity)
	}
	var kind string
	if len(wire.PayloadKind) > 0 {
		_ = json.Unmarshal(wire.PayloadKind, &kind)
	}
	*e = cloudEntry{
		Seq: *wire.Seq, InsertID: wire.InsertID, Timestamp: wire.Timestamp,
		ReceiveTimestamp: wire.ReceiveTimestamp, Severity: severityFromNumber(severity),
		LogName: wire.LogName, LogID: wire.LogID, Message: wire.Message,
		PayloadKind: knownPayloadKind(kind), Labels: wire.Labels, ResourceType: wire.ResourceType,
		ResourceLabels: wire.ResourceLabels, Trace: wire.Trace, SpanID: wire.SpanID,
		HTTPRequestSummary: wire.HTTPRequestSummary, Raw: stringOr(wire.Raw, wire.Message),
	}
	return nil
}

// knownPayloadKind is the kind, or text for one this build doesn't know.
func knownPayloadKind(kind string) string {
	switch kind {
	case cloudPayloadText, cloudPayloadJSON, cloudPayloadProto, cloudPayloadNone:
		return kind
	}
	return cloudPayloadText
}

// severityFromNumber is the level a wire number names, DEFAULT when it names none.
func severityFromNumber(n float64) int {
	if n != math.Trunc(n) || n < 0 || n > 800 {
		return 0
	}
	return knownCloudSeverity(int(n))
}

// cloudQueryTemplate mirrors CloudQueryTemplate: a saved query, or a raw filter when RawFilter
// isn't "". A missing id is a new one.
type cloudQueryTemplate struct {
	ID, Name  string
	Query     cloudQuery
	RawFilter string
}

type cloudQueryTemplateWire struct {
	ID        *string    `json:"id"`
	Name      string     `json:"name"`
	Query     cloudQuery `json:"query"`
	RawFilter string     `json:"rawFilter,omitempty"`
}

func (t cloudQueryTemplate) MarshalJSON() ([]byte, error) {
	id := t.ID
	if id == "" {
		id = newUUID()
	}
	return json.Marshal(cloudQueryTemplateWire{ID: &id, Name: t.Name, Query: t.Query, RawFilter: t.RawFilter})
}

func (t *cloudQueryTemplate) UnmarshalJSON(data []byte) error {
	wire := cloudQueryTemplateWire{Query: newCloudQuery()}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	id, err := wireUUID(wire.ID)
	if err != nil {
		return err
	}
	*t = cloudQueryTemplate{ID: id, Name: wire.Name, Query: wire.Query, RawFilter: wire.RawFilter}
	return nil
}

// cloudSqlTemplate mirrors CloudSqlTemplate: a saved SQL filter. The built-in templates have
// no id.
type cloudSqlTemplate struct {
	ID, Name, SQL string
}

type cloudSqlTemplateWire struct {
	ID   *string `json:"id"`
	Name string  `json:"name"`
	SQL  string  `json:"sql"`
}

func (t cloudSqlTemplate) MarshalJSON() ([]byte, error) {
	id := t.ID
	if id == "" {
		id = newUUID()
	}
	return json.Marshal(cloudSqlTemplateWire{ID: &id, Name: t.Name, SQL: t.SQL})
}

func (t *cloudSqlTemplate) UnmarshalJSON(data []byte) error {
	var wire cloudSqlTemplateWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	id, err := wireUUID(wire.ID)
	if err != nil {
		return err
	}
	*t = cloudSqlTemplate{ID: id, Name: wire.Name, SQL: wire.SQL}
	return nil
}

// cloudState mirrors CloudState, what cloud.state returns and the cloud.state topic publishes.
// BinaryPath is gcloud's path, "" when it wasn't found.
//
// One project or template that doesn't decode is skipped, and an unreadable authState is
// unknown, so a record from a newer build can't cost the rest.
type cloudState struct {
	BinaryPath     string
	IsDetecting    bool
	AuthState      cloudAuthState
	Projects       []cloudProject
	QueryTemplates []cloudQueryTemplate
	SqlTemplates   []cloudSqlTemplate
}

func (s cloudState) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		BinaryPath     string               `json:"binaryPath,omitempty"`
		IsDetecting    bool                 `json:"isDetecting"`
		AuthState      cloudAuthState       `json:"authState"`
		Projects       []cloudProject       `json:"projects"`
		QueryTemplates []cloudQueryTemplate `json:"queryTemplates"`
		SqlTemplates   []cloudSqlTemplate   `json:"sqlTemplates"`
	}{
		s.BinaryPath, s.IsDetecting, s.AuthState,
		emptyIfNil(s.Projects), emptyIfNil(s.QueryTemplates), emptyIfNil(s.SqlTemplates),
	})
}

func (s *cloudState) UnmarshalJSON(data []byte) error {
	var wire struct {
		BinaryPath     string          `json:"binaryPath"`
		IsDetecting    bool            `json:"isDetecting"`
		AuthState      json.RawMessage `json:"authState"`
		Projects       json.RawMessage `json:"projects"`
		QueryTemplates json.RawMessage `json:"queryTemplates"`
		SqlTemplates   json.RawMessage `json:"sqlTemplates"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	auth := cloudAuthState{State: cloudAuthUnknown}
	if len(wire.AuthState) > 0 && json.Unmarshal(wire.AuthState, &auth) != nil {
		auth = cloudAuthState{State: cloudAuthUnknown}
	}
	*s = cloudState{
		BinaryPath:     wire.BinaryPath,
		IsDetecting:    wire.IsDetecting,
		AuthState:      auth,
		Projects:       decodeEach[cloudProject](wire.Projects),
		QueryTemplates: decodeEach[cloudQueryTemplate](wire.QueryTemplates),
		SqlTemplates:   decodeEach[cloudSqlTemplate](wire.SqlTemplates),
	}
	return nil
}

// project is the project with this id.
func (s cloudState) project(id string) (cloudProject, bool) {
	for _, p := range s.Projects {
		if p.ProjectID == id {
			return p, true
		}
	}
	return cloudProject{}, false
}

// dbResultSet mirrors DBResultSet, what cloud.sessions.query returns. A nil cell is SQL NULL.
type dbResultSet struct {
	Columns []string    `json:"columns"`
	Rows    [][]*string `json:"rows"`
}

// cloudSessionInfo mirrors CloudArea.SessionInfo, what cloud.sessions.open returns. Config and
// State are the daemon's, which win over the caller's when the session already existed.
// Existed is false when this open created the session.
type cloudSessionInfo struct {
	ID      string            `json:"id"`
	Config  cloudStreamConfig `json:"config"`
	State   cloudStreamState  `json:"state"`
	Existed bool              `json:"existed"`
}

func (i *cloudSessionInfo) UnmarshalJSON(data []byte) error {
	type plain cloudSessionInfo
	out := plain{Config: newCloudStreamConfig(""), State: cloudStreamState{HasMoreOlder: true}}
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	*i = cloudSessionInfo(out)
	return nil
}
