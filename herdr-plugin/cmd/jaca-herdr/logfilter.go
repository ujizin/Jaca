package main

import (
	"regexp"
	"strings"
)

// logLine mirrors LogLine's wire format (Sources/Core/Logs/LogLineWire.swift).
type logLine struct {
	Seq      uint64  `json:"s"`
	Time     float64 `json:"t"`
	Level    int     `json:"l"`
	Tag      string  `json:"g"`
	PID      int32   `json:"p"`
	Message  string  `json:"m"`
	Raw      string  `json:"r"` // the line as the device printed it; omitted when it equals Message
	Process  string  `json:"n"`
	Marker   bool    `json:"mk"`
	Critical bool    `json:"mc"`
	Console  bool    `json:"co"`
}

// levelShort matches LogLevel.short.
var levelShort = []string{"V", "D", "I", "W", "E", "F"}

// logFilter is the part of LogFilter (Sources/Core/Logs/LogFilter.swift) a pane sets: the same
// fields, defaults and matching order as the app's log tab. Call compile after changing query
// or isRegex.
type logFilter struct {
	minLevel       int
	query          string // free text or regex over tag + message
	isRegex        bool
	hideSystemLogs bool
	pids           map[int32]bool // the targeted package's PIDs; nil = all

	lowered string
	re      *regexp.Regexp // nil when isRegex is off or the pattern is invalid
}

func (f *logFilter) compile() {
	f.lowered = strings.ToLower(f.query)
	f.re = nil
	if f.isRegex && f.query != "" {
		f.re, _ = regexp.Compile("(?i)" + f.query)
	}
}

func (f *logFilter) matches(l logLine) bool {
	if l.Marker { // Jaca markers are never filtered out
		return true
	}
	if f.hideSystemLogs && strings.HasPrefix(l.Tag, "com.apple") {
		return false
	}
	if l.Level < f.minLevel {
		return false
	}
	// Console (stdout/print) lines carry no pid but belong to the targeted app.
	if f.pids != nil && !l.Console && !f.pids[l.PID] {
		return false
	}
	if f.query == "" {
		return true
	}
	if f.isRegex {
		// An invalid pattern matches nothing.
		return f.re != nil && f.re.MatchString(l.Tag+" "+l.Message)
	}
	return strings.Contains(strings.ToLower(l.Message), f.lowered) ||
		strings.Contains(strings.ToLower(l.Tag), f.lowered)
}
