package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// logCopyFormat mirrors LogCopyFormat (Sources/Core/Logs/LogCopyFormat.swift): how a copied log
// line reads. The app keeps the chosen one in ~/.jaca/log-copy-format.json and the pane reads
// and writes the same file, so both copy the same way.
type logCopyFormat struct {
	Template   string `json:"template"`
	DateFormat string `json:"dateFormat"`
}

var defaultCopyFormat = logCopyFormat{Template: "{date} {level} {tag}  {message}", DateFormat: "HH:mm:ss.SSS"}

// copyPresets are LogCopyPresets.all, names included.
var copyPresets = []struct {
	name   string
	format logCopyFormat
}{
	{"Time · level · tag · message", logCopyFormat{"{date} {level} {tag}  {message}", "HH:mm:ss.SSS"}},
	{"Message only", logCopyFormat{"{message}", ""}},
	{"[level] message", logCopyFormat{"[{level}] {message}", ""}},
	{"Minutes:seconds · message", logCopyFormat{"{date}  {message}", "mm:ss"}},
	{"Full date · tag · message", logCopyFormat{"{date} {tag}  {message}", "yyyy-MM-dd HH:mm:ss"}},
	{"[level] time · tag · message", logCopyFormat{"[{level}] {date} {tag}  {message}", "HH:mm:ss"}},
}

// levelName matches LogLevel.name.
var levelName = []string{"Verbose", "Debug", "Info", "Warn", "Error", "Fatal"}

func copyFormatPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".jaca", "log-copy-format.json")
}

// loadCopyFormat reads the saved format. A missing or unreadable file, or a missing key, gives
// the default, as in the app.
func loadCopyFormat(path string) logCopyFormat {
	format := defaultCopyFormat
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &format) != nil {
		return defaultCopyFormat
	}
	return format
}

// saveCopyFormat replaces the saved format in one step, so the app never reads half a file.
func saveCopyFormat(path string, format logCopyFormat) error {
	data, err := json.Marshal(format)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// render is one line in this format, with LogLine.copyFields' tokens.
func (f logCopyFormat) render(l logLine) string {
	level, short := "", ""
	if l.Level >= 0 && l.Level < len(levelName) {
		level, short = levelName[l.Level], levelShort[l.Level]
	}
	pid := ""
	if l.PID > 0 {
		pid = fmt.Sprint(l.PID)
	}
	return substituteTokens(f.Template, map[string]string{
		"date":       formatSwiftDate(time.Unix(0, int64(l.Time*1e9)), f.DateFormat),
		"level":      level,
		"levelShort": short,
		"tag":        l.Tag,
		"message":    l.Message,
		"pid":        pid,
		"process":    l.Process,
	})
}

// copyTokens are LogCopyFormat.knownTokens, the ones the app's Copy format sheet lists.
var copyTokens = []string{"date", "level", "levelShort", "tag", "message", "pid", "logId", "trace"}

// renderSample is the format applied to LogCopyPresets.sample, the row the app's sheet uses to
// show what a format produces.
func (f logCopyFormat) renderSample() string {
	at := time.Date(2024, 3, 15, 14, 23, 45, 678_000_000, time.Local)
	return substituteTokens(f.Template, map[string]string{
		"date":       formatSwiftDate(at, f.DateFormat),
		"level":      "ERROR",
		"levelShort": "E",
		"tag":        "AuthService",
		"message":    "Login failed for user 42",
		"pid":        "1234",
		"logId":      "stdout",
		"trace":      "a1b2c3",
	})
}

// renderLines is the clipboard text for lines: each in the saved format, or as LogClipboard's
// "messages only" form, a level prefix and the message.
func renderLines(lines []logLine, format logCopyFormat, messagesOnly bool) string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if !messagesOnly {
			out = append(out, format.render(l))
			continue
		}
		short := "?"
		if l.Level >= 0 && l.Level < len(levelShort) {
			short = levelShort[l.Level]
		}
		out = append(out, "["+short+"] "+l.Message)
	}
	return strings.Join(out, "\n")
}

// substituteTokens is LogCopyFormat.substitute: it fills {token}s, leaves unknown ones literal,
// and when a known token is empty drops one space next to it, so "[{level}] {tag} {message}"
// with no tag is "[E] message".
func substituteTokens(template string, values map[string]string) string {
	chars := []rune(template)
	var out []rune
	for i := 0; i < len(chars); {
		end := -1
		if chars[i] == '{' {
			for j := i + 1; j < len(chars); j++ {
				if chars[j] == '}' {
					end = j
					break
				}
			}
		}
		value, known := "", false
		if end >= 0 {
			value, known = values[string(chars[i+1:end])]
		}
		if !known {
			out = append(out, chars[i])
			i++
			continue
		}
		switch {
		case value != "":
			out = append(out, []rune(value)...)
		case len(out) > 0 && out[len(out)-1] == ' ':
			out = out[:len(out)-1]
		case end+1 < len(chars) && chars[end+1] == ' ':
			i = end + 2
			continue
		}
		i = end + 1
	}
	return string(out)
}

// formatSwiftDate formats t with a DateFormatter pattern, for the fields a log timestamp uses:
// y M d H h m s S a and quoted text. Other pattern letters are written as they are.
func formatSwiftDate(t time.Time, pattern string) string {
	var b strings.Builder
	rs := []rune(pattern)
	for i := 0; i < len(rs); {
		if rs[i] == '\'' { // quoted literal; '' is a quote
			j := i + 1
			for j < len(rs) && rs[j] != '\'' {
				j++
			}
			if j == i+1 {
				b.WriteRune('\'')
			} else {
				b.WriteString(string(rs[i+1 : j]))
			}
			i = min(j+1, len(rs))
			continue
		}
		n := 1
		for i+n < len(rs) && rs[i+n] == rs[i] {
			n++
		}
		pad := func(v int) { fmt.Fprintf(&b, "%0*d", n, v) }
		switch rs[i] {
		case 'y':
			if n == 2 {
				pad(t.Year() % 100)
			} else {
				pad(t.Year())
			}
		case 'M':
			switch {
			case n >= 4:
				b.WriteString(t.Month().String())
			case n == 3:
				b.WriteString(t.Month().String()[:3])
			default:
				pad(int(t.Month()))
			}
		case 'd':
			pad(t.Day())
		case 'H':
			pad(t.Hour())
		case 'h':
			pad((t.Hour()+11)%12 + 1)
		case 'm':
			pad(t.Minute())
		case 's':
			pad(t.Second())
		case 'S': // the first n digits of the fraction
			b.WriteString(fmt.Sprintf("%09d", t.Nanosecond())[:min(n, 9)])
		case 'a':
			if t.Hour() < 12 {
				b.WriteString("AM")
			} else {
				b.WriteString("PM")
			}
		default:
			b.WriteString(string(rs[i : i+n]))
		}
		i += n
	}
	return b.String()
}
