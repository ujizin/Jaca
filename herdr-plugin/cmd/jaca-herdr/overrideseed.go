package main

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Turning a captured request into a rule (OverrideSeeding.swift, OverridesModel.seed), and the
// rule-owned body store both processes read (OverrideRuleStore.swift, OverrideBodyLoader).

// seedPattern is OverrideSeeding.pattern: scheme://host + path, without the port and the query.
// The matcher ignores the query unless the pattern names one, so carrying it over would imply a
// constraint that isn't applied. A URL that doesn't parse is its own pattern.
func seedPattern(t netTransaction) string {
	u, err := url.Parse(t.URL)
	if err != nil || u.Hostname() == "" {
		return t.URL
	}
	scheme, path := u.Scheme, u.Path
	if scheme == "" {
		scheme = "https"
	}
	if path == "" {
		path = "/"
	}
	return scheme + "://" + urlHost(u) + path
}

// seedName is OverrideSeeding.name: the method and the last path segment, or the host for a
// request to the root.
func seedName(t netTransaction) string {
	last := t.Host
	segments := strings.FieldsFunc(decodedURLPath(t.URL), func(r rune) bool { return r == '/' })
	if len(segments) > 0 {
		last = segments[len(segments)-1]
	}
	return t.Method + " " + last
}

// seedHeaders is OverrideSeeding.headers: the response headers worth copying, which is all but
// the framing headers Jaca recomputes and its own markers. With none left the rule gets a JSON
// content type.
func seedHeaders(headers []headerPair) []headerPair {
	var kept []headerPair
	for _, h := range headers {
		switch strings.ToLower(h.Name) {
		case "content-length", "content-encoding", "transfer-encoding", "connection":
			continue
		}
		if !isJacaInternalHeader(h.Name) {
			kept = append(kept, h)
		}
	}
	if len(kept) == 0 {
		return []headerPair{{Name: "Content-Type", Value: "application/json"}}
	}
	return kept
}

// seedRule is OverridesModel.seed + OverrideSeeding: a new rule that answers this request with
// the response it got. responseBody is the fetched body (may be nil); the rule takes its own
// copy, pretty-printed when it is JSON.
//
// The error is only ever a body too large to inline that couldn't be written to the blob
// store. The rule is still returned, with the body makeBodyRef fell back to.
func seedRule(t netTransaction, responseBody []byte) (overrideRule, error) {
	rule := newOverrideRule()
	rule.Name = seedName(t)
	rule.Matcher = ruleMatcher{Pattern: seedPattern(t), Kind: "glob", Methods: []string{strings.ToUpper(t.Method)}}
	rule.RoutedHosts = derivedRoutedHosts(rule.Matcher)

	body := responseBody
	if pretty, ok := prettyJSON(responseBody, t.ResponseContentType); ok {
		body = pretty
	}
	status := 200
	if t.StatusCode != nil {
		status = *t.StatusCode
	}
	ref, err := makeBodyRefBytes(body)
	rule.Action = ruleAction{Kind: "respond", Respond: respondSpec{StatusCode: status, Headers: seedHeaders(t.ResponseHeaders), Body: ref}}
	return rule, err
}

// seedWarning is OverrideSeeding.warning: why a seeded rule may not reproduce the captured
// response, "" when nothing stands in the way.
func seedWarning(t netTransaction, responseBody []byte) string {
	contentType := strings.ToLower(t.ResponseContentType)
	if strings.Contains(contentType, "text/event-stream") || strings.Contains(contentType, "application/grpc") {
		return "This is a streamed response. Jaca can't capture streamed bodies, and an " +
			"override replies with one complete body — the app's stream will end after your payload."
	}
	if len(responseBody) >= 1024*1024 {
		return "The captured body hit the 1 MB cap and is truncated. Sending it as-is would " +
			"return a truncated payload."
	}
	if contentType != "" && !isTextualContentType(contentType) {
		return "Binary responses are captured as text and can't be reproduced byte-for-byte."
	}
	return ""
}

func isTextualContentType(contentType string) bool {
	for _, textual := range []string{"json", "text", "xml", "javascript", "x-www-form-urlencoded"} {
		if strings.Contains(contentType, textual) {
			return true
		}
	}
	return false
}

// blankRule is OverrideEditorSheet.blankRule: a new rule for "create without a captured
// request", pointed at seedHost when there is one.
func blankRule(seedHost string) overrideRule {
	rule := newOverrideRule()
	if seedHost != "" {
		rule.Matcher.Pattern = "https://" + seedHost + "/"
		rule.RoutedHosts = []string{strings.ToLower(seedHost)}
	}
	return rule
}

// overridesDir is OverrideRuleStore.directory: ~/.jaca/network-overrides, or
// $JACA_OVERRIDES_DIR, which tests set so they never touch the user's rules.
func overridesDir() string {
	if dir := os.Getenv("JACA_OVERRIDES_DIR"); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".jaca", "network-overrides")
}

// overrideBodiesDir holds the payloads too large to inline, one <UUID>.bin each.
func overrideBodiesDir() string {
	return filepath.Join(overridesDir(), "bodies")
}

// makeBodyRef is OverrideRuleStore.makeBodyRef for an edited body: none when empty, inline up
// to inlineBodyLimit bytes, otherwise a blob written to bodies/<UUID>.bin. The daemon deletes
// blobs no saved rule refers to after a day, so one written for a rule that is never saved
// doesn't stay.
//
// The error is only ever the blob that couldn't be written. The ref returned with it is the
// app's fallback, the whole text inline, so the body isn't lost.
func makeBodyRef(text string) (bodyRef, error) {
	return makeBodyRefBytes([]byte(text))
}

func makeBodyRefBytes(data []byte) (bodyRef, error) {
	if len(data) == 0 {
		return bodyRef{Kind: "none"}, nil
	}
	isText := utf8.Valid(data)
	if len(data) <= inlineBodyLimit && isText {
		return bodyRef{Kind: "inline", Text: string(data)}, nil
	}
	filename := newUUID() + ".bin"
	if err := writeBodyBlob(filename, data); err != nil {
		if isText {
			return bodyRef{Kind: "inline", Text: string(data)}, err
		}
		return bodyRef{Kind: "none"}, err
	}
	return bodyRef{Kind: "blob", Filename: filename}, nil
}

// writeBodyBlob writes a blob in one step, so the daemon never serves half a body. Captured
// responses can hold credentials, so the file is the user's alone.
func writeBodyBlob(filename string, data []byte) error {
	dir := overrideBodiesDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, filename)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// loadBodyBytes is OverrideBodyLoader.data: the bytes a body ref stands for. A blob must be a
// plain file name inside bodies/, since rules.json is hand-editable.
func loadBodyBytes(ref bodyRef) ([]byte, error) {
	switch ref.Kind {
	case "inline":
		return []byte(ref.Text), nil
	case "blob":
		name := ref.Filename
		if name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
			return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrInvalid}
		}
		return os.ReadFile(filepath.Join(overrideBodiesDir(), name))
	case "file":
		return os.ReadFile(ref.Path)
	}
	return nil, nil
}

// loadBodyText is the body as the editor shows it: "" for none, the blob's or file's text
// otherwise. The error is a blob or file that can't be read. A body that isn't UTF-8 reads as
// "" with no error, as in the app's editor.
func loadBodyText(ref bodyRef) (string, error) {
	data, err := loadBodyBytes(ref)
	if err != nil || !utf8.Valid(data) {
		return "", err
	}
	return string(data), nil
}
