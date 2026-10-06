package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

func filterWith(change func(f *logFilter)) *logFilter {
	f := &logFilter{hideSystemLogs: true}
	change(f)
	f.compile()
	return f
}

func TestLogFilterMatches(t *testing.T) {
	line := logLine{Level: 2, Tag: "OkHttp", PID: 10, Message: "GET /Users 200"}
	cases := []struct {
		name   string
		filter *logFilter
		line   logLine
		want   bool
	}{
		{"default keeps a line", filterWith(func(*logFilter) {}), line, true},
		{"below the minimum level", filterWith(func(f *logFilter) { f.minLevel = 3 }), line, false},
		{"at the minimum level", filterWith(func(f *logFilter) { f.minLevel = 2 }), line, true},
		{"text in the message, any case", filterWith(func(f *logFilter) { f.query = "/users" }), line, true},
		{"text in the tag", filterWith(func(f *logFilter) { f.query = "okhttp" }), line, true},
		{"text nowhere", filterWith(func(f *logFilter) { f.query = "POST" }), line, false},
		{"regex over tag and message", filterWith(func(f *logFilter) { f.query, f.isRegex = `^okhttp get .*\d{3}$`, true }), line, true},
		{"regex that doesn't match", filterWith(func(f *logFilter) { f.query, f.isRegex = `5\d\d$`, true }), line, false},
		{"invalid regex matches nothing", filterWith(func(f *logFilter) { f.query, f.isRegex = `(`, true }), line, false},
		{"regex characters as plain text", filterWith(func(f *logFilter) { f.query = "(" }), line, false},
		{"pid in the package", filterWith(func(f *logFilter) { f.pids = map[int32]bool{10: true} }), line, true},
		{"pid outside the package", filterWith(func(f *logFilter) { f.pids = map[int32]bool{11: true} }), line, false},
		{"package with no pid yet", filterWith(func(f *logFilter) { f.pids = map[int32]bool{} }), line, false},
		{"console output ignores the pid filter", filterWith(func(f *logFilter) { f.pids = map[int32]bool{} }),
			logLine{Level: 2, PID: -1, Message: "print", Console: true}, true},
		{"system logs hidden", filterWith(func(*logFilter) {}), logLine{Level: 2, Tag: "com.apple.network"}, false},
		{"system logs shown", filterWith(func(f *logFilter) { f.hideSystemLogs = false }), logLine{Level: 2, Tag: "com.apple.network"}, true},
		{"a marker passes every filter", filterWith(func(f *logFilter) { f.minLevel, f.query = 5, "nothing" }),
			logLine{Marker: true, Message: "Process started"}, true},
	}
	for _, c := range cases {
		if got := c.filter.matches(c.line); got != c.want {
			t.Errorf("%s: matches = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestTextInputEditsAtTheEnd(t *testing.T) {
	var in textInput
	steps := []struct {
		key     []byte
		changed bool
		want    string
	}{
		{[]byte{0x7f}, false, ""},
		{[]byte("a"), true, "a"},
		{[]byte("çé"), true, "açé"},
		{[]byte{0x1b, '[', 'A'}, false, "açé"},
		{[]byte{0x7f}, true, "aç"},
		{[]byte("x\ty"), true, "açxy"},
		{[]byte{0x15}, true, ""},
		{[]byte{0x15}, false, ""},
	}
	for i, s := range steps {
		if changed := in.handle(s.key); changed != s.changed || in.String() != s.want {
			t.Errorf("step %d: changed = %v, text = %q; want %v, %q", i, changed, in.String(), s.changed, s.want)
		}
	}
}

func TestTailCells(t *testing.T) {
	for _, c := range []struct {
		in   string
		w    int
		want string
	}{
		{"abc", 5, "abc"}, {"abcdef", 3, "def"}, {"日本語", 4, "本語"}, {"abc", 0, ""},
	} {
		if got := tailCells(c.in, c.w); got != c.want {
			t.Errorf("tailCells(%q, %d) = %q, want %q", c.in, c.w, got, c.want)
		}
	}
}

var sgr = sgrPattern

// The toolbar and status bar never exceed the pane width, whatever it is: a wider row would
// wrap and scroll the frame.
func TestLogViewerRowsFitThePane(t *testing.T) {
	v := &logViewer{device: device{ID: "x", Platform: "iosSimulator", Model: "iPhone 17 Pro Max"}, follow: true}
	v.search.set("a long filter text that will not fit in a narrow field")
	v.pkg.set("com.example.app")
	v.total, v.dropped, v.paused = 123456, 789, true
	for cols := 1; cols <= 200; cols++ {
		for _, focus := range []logFocus{focusLog, focusSearch, focusPackage} {
			v.focus = focus
			for _, height := range []int{1, 3} {
				for _, row := range v.toolbar(cols, height) {
					if w := cellWidth(sgr.ReplaceAllString(row, "")); w > cols {
						t.Fatalf("a %d-row toolbar is %d cells in a %d-cell pane (focus %d)", height, w, cols, focus)
					}
				}
			}
		}
		if w := cellWidth(sgr.ReplaceAllString(v.statusBar(cols), "")); w > cols {
			t.Fatalf("status bar is %d cells in a %d-cell pane", w, cols)
		}
	}
}

func TestAppValueByPlatform(t *testing.T) {
	name := "Example"
	v := &logViewer{apps: []appEntry{{ID: "com.example", Name: &name}}}
	v.device.Platform = "android"
	if got := v.appValue(1); got != "com.example" {
		t.Errorf("android targets %q, want the package id", got)
	}
	v.device.Platform = "iosDevice"
	if got := v.appValue(1); got != "Example" {
		t.Errorf("a physical iOS device targets %q, want the process name", got)
	}
	if got := v.appValue(0); got != "" {
		t.Errorf("All processes targets %q, want empty", got)
	}
	if got := v.appValue(7); got != "" {
		t.Errorf("a row past the list targets %q, want empty", got)
	}
}

// A log row is exactly the pane's width at any size, markers included, so rows never wrap.
func TestRenderLogLineFillsThePane(t *testing.T) {
	lines := []logLine{
		{Level: 0, Tag: "Tag", PID: 123, Message: "short"},
		{Level: 4, Tag: "a.very.long.tag.name.that.overflows.its.column", PID: 99999, Message: "a long message that runs well past the right edge of any narrow pane, 日本語"},
		{Level: 9, Message: "a level this build doesn't know"},
		{Marker: true, Message: "Process died"},
		{Marker: true, Critical: true, Message: "FATAL EXCEPTION in a process with quite a long name indeed"},
	}
	for cols := 1; cols <= 200; cols++ {
		for i, l := range lines {
			if w := cellWidth(sgr.ReplaceAllString(renderLogLine(l, cols), "")); w != cols {
				t.Fatalf("line %d is %d cells in a %d-cell pane", i, w, cols)
			}
		}
	}
}

func TestSplitInput(t *testing.T) {
	esc := "\x1b"
	cases := []struct {
		name, in string
		keys     []string
		rest     string
	}{
		{"one key", "j", []string{"j"}, ""},
		{"repeated keys count one by one", "jjk", []string{"j", "j", "k"}, ""},
		{"an arrow", esc + "[A", []string{esc + "[A"}, ""},
		{"Esc on its own", esc, []string{esc}, ""},
		{"keys around a sequence", "a" + esc + "[5~b", []string{"a", esc + "[5~", "b"}, ""},
		{"two mouse reports in one read", esc + "[<64;10;5M" + esc + "[<0;3;1M", []string{esc + "[<64;10;5M", esc + "[<0;3;1M"}, ""},
		{"a report cut short waits for the rest", "x" + esc + "[<0;12", []string{"x"}, esc + "[<0;12"},
		{"a character cut short waits for the rest", "a\xc3", []string{"a"}, "\xc3"},
		{"a paste stays whole", "com.example.app", []string{"com.example.app"}, ""},
		{"an arrow in application mode", esc + "OA", []string{esc + "OA"}, ""},
	}
	for _, c := range cases {
		keys, rest := splitInput([]byte(c.in))
		var got []string
		for _, k := range keys {
			got = append(got, string(k))
		}
		if !reflect.DeepEqual(got, c.keys) || string(rest) != c.rest {
			t.Errorf("%s: keys %q rest %q, want %q %q", c.name, got, rest, c.keys, c.rest)
		}
	}
}

func TestParseMouse(t *testing.T) {
	if m, ok := parseMouse([]byte("\x1b[<0;12;3M")); !ok || m != (mouseEvent{button: 0, x: 12, y: 3, press: true}) {
		t.Errorf("press parsed as %+v, %v", m, ok)
	}
	if m, ok := parseMouse([]byte("\x1b[<65;1;40m")); !ok || m.button != 65 || m.press {
		t.Errorf("release parsed as %+v, %v", m, ok)
	}
	for _, bad := range []string{"\x1b[A", "\x1b[<0;1M", "\x1b[<a;1;1M", "j"} {
		if _, ok := parseMouse([]byte(bad)); ok {
			t.Errorf("%q parsed as a mouse report", bad)
		}
	}
}

// A click lands on what is drawn under it: a level chip sets the level, a field takes the focus.
func TestToolbarClicks(t *testing.T) {
	v := &logViewer{device: device{Platform: "android"}, follow: true}
	rows := v.toolbar(160, 3)
	plain := sgr.ReplaceAllString(rows[1], "")
	column := func(label string) int {
		i := regexp.MustCompile(regexp.QuoteMeta(label)).FindStringIndex(plain)
		if i == nil {
			t.Fatalf("%q is not in the toolbar: %q", label, plain)
		}
		return cellWidth(plain[:i[0]]) + 1
	}
	v.mouse(mouseEvent{button: 0, x: column("W"), y: 1, press: true})
	if v.filter.minLevel != 3 {
		t.Errorf("a click on W set level %d, want 3", v.filter.minLevel)
	}
	v.mouse(mouseEvent{button: 0, x: column(".*"), y: 3, press: true})
	if !v.filter.isRegex {
		t.Error("a click on .* didn't turn regex on")
	}
	v.toolbar(160, 3)
	v.mouse(mouseEvent{button: 0, x: column("Package id…") + 2, y: 2, press: true})
	if v.focus != focusPackage {
		t.Errorf("a click on the package field left focus %d", v.focus)
	}
	v.mouse(mouseEvent{button: 0, x: column("W"), y: 2, press: false})
	if v.focus != focusPackage {
		t.Error("a button release acted as a click")
	}
}

// The keys popup fits any pane, and is dismissed by the same key that opens it or by a click.
func TestHelpPopup(t *testing.T) {
	v := &logViewer{device: device{Platform: "android"}, follow: true}
	for rows := 1; rows <= 40; rows++ {
		for cols := 1; cols <= 120; cols++ {
			box := v.helpBox(rows, cols)
			if len(box) > rows {
				t.Fatalf("the popup is %d rows in a %d-row pane", len(box), rows)
			}
			for _, row := range box {
				if w := cellWidth(sgr.ReplaceAllString(row, "")); w > cols {
					t.Fatalf("a popup row is %d cells in a %d-cell pane", w, cols)
				}
			}
		}
	}
	v.handleKey([]byte("?"))
	if v.focus != focusHelp {
		t.Fatal("? didn't open the popup")
	}
	v.handleKey([]byte("1"))
	if v.focus != focusHelp || v.filter.minLevel != 0 {
		t.Error("a key reached the viewer under the popup")
	}
	v.handleKey([]byte{0x1b})
	if v.focus != focusLog {
		t.Error("Esc didn't dismiss the popup")
	}
	v.handleKey([]byte("?"))
	v.mouse(mouseEvent{button: 0, x: 1, y: 1, press: true})
	if v.focus != focusLog {
		t.Error("a click didn't dismiss the popup")
	}
}

func TestCopyFormat(t *testing.T) {
	at := time.Date(2024, 3, 15, 14, 23, 45, 678_000_000, time.Local)
	line := logLine{Time: float64(at.UnixNano()) / 1e9, Level: 4, Tag: "AuthService", PID: 1234, Message: "Login failed for user 42"}
	for _, c := range []struct {
		format logCopyFormat
		line   logLine
		want   string
	}{
		{defaultCopyFormat, line, "14:23:45.678 Error AuthService  Login failed for user 42"},
		{logCopyFormat{"[{level}] {message}", ""}, line, "[Error] Login failed for user 42"},
		{logCopyFormat{"{date} {tag}  {message}", "yyyy-MM-dd HH:mm:ss"}, line, "2024-03-15 14:23:45 AuthService  Login failed for user 42"},
		{logCopyFormat{"{date}  {message}", "mm:ss"}, line, "23:45  Login failed for user 42"},
		{logCopyFormat{"{levelShort} {pid} {unknown} {message}", ""}, line, "E 1234 {unknown} Login failed for user 42"},
		// An empty token takes one neighbouring space with it.
		{logCopyFormat{"[{level}] {tag} {message}", ""}, logLine{Level: 2, Message: "hi"}, "[Info] hi"},
		{logCopyFormat{"{date} {message}", ""}, line, "Login failed for user 42"},
	} {
		if got := c.format.render(c.line); got != c.want {
			t.Errorf("%q rendered %q, want %q", c.format.Template, got, c.want)
		}
	}
	if got := renderLines([]logLine{line, {Level: 2, Message: "ok"}}, defaultCopyFormat, true); got != "[E] Login failed for user 42\n[I] ok" {
		t.Errorf("messages only rendered %q", got)
	}
	if got := formatSwiftDate(at, "hh:mm a 'at' yy"); got != "02:23 PM at 24" {
		t.Errorf("formatSwiftDate = %q", got)
	}
}

// The saved format survives a round trip, and an old or broken file reads as the default.
func TestCopyFormatFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "log-copy-format.json")
	if got := loadCopyFormat(path); got != defaultCopyFormat {
		t.Errorf("a missing file loaded %+v", got)
	}
	want := copyPresets[4].format
	if err := saveCopyFormat(path, want); err != nil {
		t.Fatal(err)
	}
	if got := loadCopyFormat(path); got != want {
		t.Errorf("loaded %+v, want %+v", got, want)
	}
	os.WriteFile(path, []byte(`{"template":"{message}"}`), 0o644)
	if got := loadCopyFormat(path); got != (logCopyFormat{"{message}", defaultCopyFormat.DateFormat}) {
		t.Errorf("a file missing a key loaded %+v", got)
	}
	os.WriteFile(path, []byte(`not json`), 0o644)
	if got := loadCopyFormat(path); got != defaultCopyFormat {
		t.Errorf("a broken file loaded %+v", got)
	}
}

// captureClipboard replaces the pasteboard for a test and returns what gets copied.
func captureClipboard(t *testing.T) <-chan string {
	copied := make(chan string, 4)
	real := copyToClipboard
	copyToClipboard = func(text string) error { copied <- text; return nil }
	t.Cleanup(func() { copyToClipboard = real })
	return copied
}

// selectionViewer is a viewer showing three of its five lines (seqs 2 to 4), as if drawn.
func selectionViewer() *logViewer {
	// A pane that has quit: results posted to it (the copy after a rest) are dropped.
	quit := &pane{work: make(chan func()), done: make(chan struct{})}
	close(quit.done)
	v := &logViewer{p: quit, device: device{Platform: "android"}}
	for seq := uint64(1); seq <= 5; seq++ {
		v.visible = append(v.visible, logLine{Seq: seq, Level: 2, Tag: "Tag", Message: fmt.Sprint("line ", seq)})
	}
	v.offset, v.logTop, v.rowLines = 1, 4, v.visible[1:4]
	return v
}

func seqs(lines []logLine) []uint64 {
	var out []uint64
	for _, l := range lines {
		out = append(out, l.Seq)
	}
	return out
}

// One click selects the line under it and copies it whole; a drag extends the selection line by
// line in either direction; the toolbar selects nothing.
func TestClickAndDragSelectLines(t *testing.T) {
	copied := captureClipboard(t)
	t.Setenv("HOME", t.TempDir()) // no saved copy format: the default applies
	v := selectionViewer()

	v.mouse(mouseEvent{button: 0, x: 3, y: 2, press: true})
	if v.sel.on {
		t.Error("a press on the toolbar selected a line")
	}
	v.mouse(mouseEvent{button: 0, x: 3, y: 5, press: true})
	v.mouse(mouseEvent{button: 0, x: 3, y: 5, press: false})
	if got := seqs(v.selectedLines()); !reflect.DeepEqual(got, []uint64{3}) {
		t.Fatalf("a click selected lines %v, want [3]", got)
	}
	select {
	case text := <-copied:
		if !strings.HasSuffix(text, "Info Tag  line 3") {
			t.Errorf("a click copied %q", text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a click copied nothing")
	}

	v.mouse(mouseEvent{button: 0, x: 3, y: 6, press: true})
	v.mouse(mouseEvent{button: mouseDrag, x: 9, y: 4, press: true})
	if got := seqs(v.selectedLines()); !reflect.DeepEqual(got, []uint64{2, 3, 4}) {
		t.Errorf("an upward drag selected lines %v, want [2 3 4]", got)
	}
	v.mouse(mouseEvent{button: mouseDrag, x: 9, y: 5, press: false})
	if text := <-copied; strings.Count(text, "\n") != 1 || !strings.HasSuffix(text, "line 4") {
		t.Errorf("the drag copied %q, want lines 3 and 4", text)
	}
	if v.sel.dragging || !v.sel.on {
		t.Error("releasing the button ended the selection, or left the drag going")
	}
	v.handleKey([]byte{0x1b})
	if v.sel.on {
		t.Error("Esc didn't drop the selection")
	}
}

// Dragging past the top or bottom of the log lines scrolls that way and takes the selection
// with it, to the buffer's ends.
func TestDragPastTheEdgeScrolls(t *testing.T) {
	captureClipboard(t)
	v := selectionViewer()
	rows, _ := termSize()
	room := rows - toolbarRows(rows) - 1
	for seq := uint64(6); seq <= uint64(room)+20; seq++ { // more lines than fit on screen
		v.visible = append(v.visible, logLine{Seq: seq})
	}
	v.offset = 5
	start, end := v.window()
	v.rowLines = v.visible[start:end]
	anchor := v.rowLines[2].Seq

	v.mouse(mouseEvent{button: 0, x: 3, y: v.logTop + 2, press: true})
	v.mouse(mouseEvent{button: mouseDrag, x: 3, y: v.logTop - 1, press: true})
	if v.sel.edge != -1 || v.offset != 6 {
		t.Fatalf("a drag above the lines left edge %d, offset %d; want -1, 6", v.sel.edge, v.offset)
	}
	for i := 0; i < 3; i++ { // the scheduled repeats
		v.edgeScroll()
	}
	top, _ := v.window()
	if v.offset != 9 || v.sel.cursor != v.visible[top].Seq || v.sel.anchor != anchor {
		t.Errorf("after scrolling up: offset %d, selection %d..%d, top line %d", v.offset, v.sel.anchor, v.sel.cursor, v.visible[top].Seq)
	}
	v.mouse(mouseEvent{button: mouseDrag, x: 3, y: v.logTop + len(v.rowLines) + 3, press: true})
	for i := 0; i < 50; i++ {
		v.edgeScroll()
	}
	if last := v.visible[len(v.visible)-1].Seq; v.offset != 0 || v.sel.cursor != last {
		t.Errorf("after scrolling down: offset %d, selection ends on %d, want the last line %d", v.offset, v.sel.cursor, last)
	}
	if v.follow {
		t.Error("the view went back to following the tail under a selection")
	}
	v.mouse(mouseEvent{button: mouseDrag, x: 3, y: v.logTop + 1, press: false})
	offset := v.offset
	v.edgeScroll()
	if v.offset != offset || v.sel.scrolling {
		t.Error("the view kept scrolling after the button was released")
	}
}

// A right click opens the app's row menu on the selection.
func TestRowMenu(t *testing.T) {
	captureClipboard(t)
	v := selectionViewer()

	// A right click on a line selects it and offers the single-line labels.
	v.mouse(mouseEvent{button: 2, x: 5, y: 5, press: true})
	if got := seqs(v.selectedLines()); v.focus != focusMenu || !reflect.DeepEqual(got, []uint64{3}) {
		t.Fatalf("a right click selected %v with focus %d", got, v.focus)
	}
	if got := v.menu.items[0].label + "|" + v.menu.items[1].label; got != "Copy Line|Copy Message only" {
		t.Errorf("menu labels %q", got)
	}
	box := v.menu.box(30, 80)
	if v.menu.itemAt(v.menu.left+2, v.menu.top+1) != 0 || v.menu.itemAt(v.menu.left+2, v.menu.top+4) != -1 {
		t.Errorf("menu hit testing is off: %q", box)
	}
	// Down skips the separator; Enter on Select All takes every line, on screen or not.
	for i := 0; i < 3; i++ {
		v.handleKey([]byte("j"))
	}
	if v.menu.selected != 4 {
		t.Errorf("three steps down landed on item %d, want Select All (4)", v.menu.selected)
	}
	v.handleKey([]byte{'\r'})
	if v.focus != focusLog || len(v.selectedLines()) != 5 {
		t.Errorf("Select All selected %d lines, want 5", len(v.selectedLines()))
	}
	// With several lines selected, a right click inside keeps them and counts them.
	v.mouse(mouseEvent{button: 2, x: 5, y: 4, press: true})
	if got := v.menu.items[0].label + "|" + v.menu.items[1].label; got != "Copy 5 Lines|Copy 5 Messages only" {
		t.Errorf("menu labels %q", got)
	}
	v.handleKey([]byte{0x1b})
	if v.focus != focusLog || !v.sel.on {
		t.Error("Esc on the menu didn't close it and keep the selection")
	}
	// A menu that would run off the pane is moved inside it.
	v.openRowMenu(79, 29)
	if box := v.menu.box(30, 80); v.menu.left+v.menu.width-1 > 80 || v.menu.top+len(box)-1 > 30 {
		t.Errorf("the menu runs off the pane: left %d width %d top %d rows %d", v.menu.left, v.menu.width, v.menu.top, len(box))
	}
}

// Shift+PgDn follows the tail again; scrolling up leaves it, scrolling down doesn't.
func TestFollowTailKeys(t *testing.T) {
	v := selectionViewer()
	for seq := uint64(6); seq <= 200; seq++ { // more lines than fit on screen
		v.visible = append(v.visible, logLine{Seq: seq})
	}
	v.offset, v.follow = 0, true
	v.handleKey([]byte("j"))
	if !v.follow {
		t.Error("scrolling down left Follow tail")
	}
	v.handleKey([]byte("k"))
	if v.follow || v.offset != 1 {
		t.Errorf("scrolling up kept Follow tail: follow %v, offset %d", v.follow, v.offset)
	}
	v.handleKey([]byte("\x1b[6;2~"))
	if !v.follow || v.offset != 0 {
		t.Errorf("Shift+PgDn left follow %v, offset %d", v.follow, v.offset)
	}
}

// The Copy format popup: a preset fills the fields, the example follows what is typed, and
// nothing is saved before Save.
func TestFormatEditor(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	v := selectionViewer()
	v.handleKey([]byte("y"))
	if v.focus != focusFormat || v.editor.focus != 0 {
		t.Fatalf("y opened focus %d on stop %d, want the popup on the preset in use", v.focus, v.editor.focus)
	}
	for rows := 1; rows <= 40; rows++ {
		for cols := 1; cols <= 120; cols++ {
			box := v.editor.box(rows, cols)
			if len(box) > rows {
				t.Fatalf("the popup is %d rows in a %d-row pane", len(box), rows)
			}
			for _, row := range box {
				if w := cellWidth(sgr.ReplaceAllString(row, "")); w > cols {
					t.Fatalf("a popup row is %d cells in a %d-cell pane", w, cols)
				}
			}
		}
	}
	// Down to the third preset, Enter takes it.
	v.handleKey([]byte("\x1b[B"))
	v.handleKey([]byte("\x1b[B"))
	v.handleKey([]byte{'\r'})
	if got := v.editor.format(); got != copyPresets[2].format {
		t.Errorf("the third preset set %+v", got)
	}
	if got := v.editor.format().renderSample(); got != "[ERROR] Login failed for user 42" {
		t.Errorf("example %q", got)
	}
	// A click on the FORMAT field focuses it; typing edits the template.
	// In the full layout the fields sit two rows under their labels, FORMAT on the left.
	box := v.editor.box(40, 120)
	var labels int
	for i, row := range box {
		if strings.Contains(row, "DATE FORMAT") {
			labels = i
		}
	}
	v.mouse(mouseEvent{button: 0, x: v.editor.left + 10, y: v.editor.top + labels + 2, press: true})
	if v.editor.focus != len(copyPresets)+editTemplate {
		t.Fatalf("a click on the FORMAT field focused stop %d", v.editor.focus)
	}
	v.handleKey([]byte("!"))
	if got := v.editor.format().Template; got != "[{level}] {message}!" {
		t.Errorf("typing in the field gave %q", got)
	}
	if _, err := os.Stat(filepath.Join(home, ".jaca", "log-copy-format.json")); err == nil {
		t.Error("the format was saved before Save")
	}
	// Esc cancels: nothing saved. Reopened, Tab to Save and Enter saves.
	v.handleKey([]byte{0x1b})
	if v.focus != focusLog || loadCopyFormat(copyFormatPath()) != defaultCopyFormat {
		t.Error("Esc didn't close the popup without saving")
	}
	v.handleKey([]byte("y"))
	v.handleKey([]byte("\x1b[B"))
	v.handleKey([]byte{'\r'}) // the second preset
	for v.editor.focus != len(copyPresets)+editSave {
		v.handleKey([]byte{'\t'})
	}
	v.handleKey([]byte{'\r'})
	if v.focus != focusLog || loadCopyFormat(copyFormatPath()) != copyPresets[1].format {
		t.Errorf("Save left focus %d and saved %+v", v.focus, loadCopyFormat(copyFormatPath()))
	}
}

// Help and Copy format in the status bar are clickable where they are drawn.
func TestStatusBarButtons(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	v := selectionViewer()
	rows, cols := termSize()
	bar := sgr.ReplaceAllString(v.statusBar(cols), "")
	column := func(label string) int { return cellWidth(bar[:strings.Index(bar, label)]) + 1 }
	if v.helpX0 != column("Help") || v.formatX0 != column("Copy format") {
		t.Fatalf("buttons recorded at %d and %d, drawn at %d and %d", v.helpX0, v.formatX0, column("Help"), column("Copy format"))
	}
	v.mouse(mouseEvent{button: 0, x: v.helpX0 + 1, y: rows, press: true})
	if v.focus != focusHelp {
		t.Errorf("a click on Help left focus %d", v.focus)
	}
	v.handleKey([]byte{0x1b})
	v.mouse(mouseEvent{button: 0, x: v.formatX1, y: rows, press: true})
	if v.focus != focusFormat {
		t.Errorf("a click on Copy format left focus %d", v.focus)
	}
}

// The arrow at the right end of the package field opens the installed-apps list; the rest of
// the field takes the focus.
func TestPackageFieldArrow(t *testing.T) {
	for _, height := range []int{1, 3} {
		v := selectionViewer()
		v.appsLoaded = true // nothing to fetch
		rows := v.toolbar(160, height)
		plain := sgr.ReplaceAllString(rows[len(rows)/2], "")
		arrow := cellWidth(plain[:strings.Index(plain, "▼")]) + 1
		v.mouse(mouseEvent{button: 0, x: arrow, y: 1, press: true})
		if v.focus != focusApps {
			t.Errorf("height %d: a click on the arrow left focus %d", height, v.focus)
		}
		v.focus = focusLog
		v.mouse(mouseEvent{button: 0, x: arrow - 8, y: 1, press: true})
		if v.focus != focusPackage {
			t.Errorf("height %d: a click on the field left focus %d", height, v.focus)
		}
	}
}

// The pane resolves Herdr's theme as Herdr does: the built-in theme named in its config, with
// the [theme.custom] colors on top.
func TestHerdrTheme(t *testing.T) {
	name, custom := readHerdrTheme(`
onboarding = false

[theme]
name = "TokyoNight"   # an alias, any case
auto_switch = false

[theme.custom]
red = "#ff0000"  # with a comment
green = 'lightgreen'
yellow = "reset"
blue = not-a-string

[theme.custom.dark]
red = "#000000"

[ui]
name = "not the theme"
`)
	if want := map[string]string{"red": "#ff0000", "green": "lightgreen", "yellow": "reset"}; name != "TokyoNight" || !reflect.DeepEqual(custom, want) {
		t.Fatalf("read name %q, custom %v", name, custom)
	}
	theme := resolveHerdrTheme(name, custom)
	// Herdr's keybinds panel: text on panel_bg, an accent border.
	if got := themePanel(theme); got != "\x1b[38;2;192;202;245;48;2;26;27;38m" {
		t.Errorf("tokyo-night panel style %q", got)
	}
	if got := themeAccent(theme); got != "\x1b[38;2;122;162;247m" {
		t.Errorf("tokyo-night accent %q", got)
	}
	for _, c := range []struct{ token, fallback, want string }{
		{"red", "31", "38;2;255;0;0"},      // overridden
		{"green", "32", "92"},              // overridden with a name
		{"yellow", "33", "33"},             // a reset alias keeps the terminal's color
		{"blue", "34", "38;2;122;162;247"}, // the theme's
	} {
		if got := themeFg(theme, c.token, c.fallback); got != c.want {
			t.Errorf("%s: %q, want %q", c.token, got, c.want)
		}
	}
	// No name is Herdr's default theme; an unknown one, or terminal, leaves the palette's colors.
	if got := resolveHerdrTheme("", nil)["panel_bg"]; got != "#181825" {
		t.Errorf("default theme panel_bg %q, want catppuccin's", got)
	}
	for _, name := range []string{"terminal", "a-theme-from-the-future"} {
		if theme := resolveHerdrTheme(name, nil); themePanel(theme) != "" || themeFg(theme, "red", "31") != "31" {
			t.Errorf("theme %q gave colors: %v", name, theme)
		}
	}
	if got := themePanel(map[string]string{"panel_bg": "reset", "text": "#ffffff"}); got != "\x1b[38;2;255;255;255m" {
		t.Errorf("panel style with panel_bg reset %q, want text alone", got)
	}
	if params, ok := colorParams("#abc", true); !ok || params != "48;2;170;187;204" {
		t.Errorf("short hex background: %q %v", params, ok)
	}
	for _, bad := range []string{"", "#12345", "rgb(1,2)", "rgb(1,2,300)", "transparent", "chartreuse"} {
		if _, ok := colorParams(bad, false); ok {
			t.Errorf("%q parsed as a color", bad)
		}
	}
	for name, palette := range herdrBuiltinThemes {
		for token, color := range palette {
			if _, ok := colorParams(color, false); !ok {
				t.Errorf("%s.%s = %q isn't a color", name, token, color)
			}
		}
	}
}

// Every row of a popup is the same width, so its right border is one straight line.
func TestPopupRowsAreOneWidth(t *testing.T) {
	v := selectionViewer()
	v.openRowMenu(5, 5)
	editor := newFormatEditor(defaultCopyFormat)
	for name, box := range map[string][]string{
		"help":           v.helpBox(40, 120),
		"menu":           v.menu.box(40, 120),
		"format":         editor.box(40, 120),
		"compact format": editor.box(20, 60),
	} {
		if len(box) == 0 {
			t.Errorf("%s: no popup", name)
		}
		for i, row := range box {
			if w, want := cellWidth(sgr.ReplaceAllString(row, "")), cellWidth(sgr.ReplaceAllString(box[0], "")); w != want {
				t.Errorf("%s: row %d is %d cells, the top border %d", name, i, w, want)
			}
		}
	}
}
