package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// toolClock is the test clock: after's callbacks run when advance reaches their time.
type toolClock struct {
	now    time.Time
	timers []toolTimer
}

type toolTimer struct {
	at time.Time
	f  func()
}

func (c *toolClock) after(d time.Duration, f func()) {
	c.timers = append(c.timers, toolTimer{c.now.Add(d), f})
}

func (c *toolClock) advance(d time.Duration) {
	c.now = c.now.Add(d)
	var due, later []toolTimer
	for _, timer := range c.timers {
		if timer.at.After(c.now) {
			later = append(later, timer)
		} else {
			due = append(due, timer)
		}
	}
	c.timers = later
	for _, timer := range due {
		timer.f()
	}
}

// toolCalls stands in for jacad: it records each call and answers with the JSON held for the
// method, or for the method and its one parameter ("gradle.kill 48213").
type toolCalls struct {
	made    []string
	results map[string]string
}

func (c *toolCalls) call(method string, params, out any, _ time.Duration) error {
	name := method
	if m, ok := params.(map[string]any); ok {
		for _, key := range []string{"pid", "name", "path"} {
			if value, ok := m[key]; ok {
				name = fmt.Sprintf("%s %v", method, value)
			}
		}
	}
	c.made = append(c.made, name)
	result, ok := c.results[name]
	if !ok {
		result, ok = c.results[method]
	}
	if !ok {
		return errors.New("no reply")
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal([]byte(result), out)
}

// testTools wires a pane's shared part to the test clock and to calls that run in place, on a
// pane that has quit.
func testTools(tp *toolPane) (*toolClock, *toolCalls) {
	quit := &pane{work: make(chan func()), done: make(chan struct{})}
	close(quit.done)
	*tp = newToolPane(quit, nil)
	clock := &toolClock{now: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)}
	calls := &toolCalls{results: map[string]string{}}
	tp.now = func() time.Time { return clock.now }
	tp.after = clock.after
	tp.call = calls.call
	tp.spawn = func(work func() func()) { work()() }
	return clock, calls
}

const gradleDaemonJSON = `{"cpu":3.4,"jdk":"21","maxHeap":"12g","memoryMB":2874,"pid":48213,"uptime":"2h 14m","version":"9.4.1"}`

// testGradleViewer holds two cache folders and three daemons: 48213 idle, 500 busy with no JDK or
// heap, 70000 with a long version.
func testGradleViewer(t *testing.T) (*gradleViewer, *toolClock, *toolCalls) {
	t.Helper()
	v := &gradleViewer{removing: map[int32]bool{}, listed: map[int32]bool{}, deleting: map[string]bool{}}
	clock, calls := testTools(&v.toolPane)
	v.owner = v
	var list []gradleDaemon
	if err := json.Unmarshal([]byte(`[`+gradleDaemonJSON+`,
		{"cpu":87.5,"memoryMB":512,"pid":500,"uptime":"3m","version":"8.7"},
		{"cpu":0,"jdk":"17","maxHeap":"4g","memoryMB":1024,"pid":70000,"uptime":"11d 4h","version":"9.5.0-milestone-3"}]`), &list); err != nil {
		t.Fatal(err)
	}
	v.apply(list)
	v.measured([]gradleCache{{"modules-2", 6143}, {"8.7", 512}}, nil)
	v.selected = 0 // the cursor followed its daemon when the cache folders arrived above it
	return v, clock, calls
}

const derivedEntryJSON = `{"kind":"stale","name":"Jaca","path":"/Users/me/Library/Developer/Xcode/DerivedData/Jaca-bqz","sizeMB":1832,"workspacePath":"/Users/me/wt/x/Jaca.xcodeproj"}`

// testXcodeViewer holds two stale entries, a shared one, a live one and one of a kind this
// client doesn't know.
func testXcodeViewer(t *testing.T) (*xcodeViewer, *toolClock, *toolCalls) {
	t.Helper()
	v := &xcodeViewer{removing: map[string]bool{}}
	clock, calls := testTools(&v.toolPane)
	v.owner = v
	var list []derivedEntry
	if err := json.Unmarshal([]byte(`[`+derivedEntryJSON+`,
		{"kind":"stale","name":"Old","path":"/dd/Old-aaa","sizeMB":300,"workspacePath":"/Users/me/old/Old.xcworkspace"},
		{"kind":"shared","name":"ModuleCache.noindex","path":"/dd/ModuleCache.noindex","sizeMB":900},
		{"kind":"live","name":"App","path":"/dd/App-ccc","sizeMB":40,"workspacePath":"/Users/me/app/App.xcodeproj"},
		{"kind":"archived","name":"Later","path":"/dd/Later-ddd","sizeMB":8}]`), &list); err != nil {
		t.Fatal(err)
	}
	v.scanned(list, nil)
	return v, clock, calls
}

func frameText(frame []string) string { return sgr.ReplaceAllString(strings.Join(frame, "\n"), "") }

// checkWidths fails when a row of the frame is wider than the pane or the frame is taller.
func checkWidths(t *testing.T, what string, frame []string, rows, cols int) {
	t.Helper()
	if len(frame) > rows {
		t.Fatalf("%s: %d rows in a pane of %d", what, len(frame), rows)
	}
	for i, row := range frame {
		if got := plainWidth(row); got > cols {
			t.Fatalf("%s: row %d is %d cells in a %d-cell pane: %q", what, i, got, cols, sgr.ReplaceAllString(row, ""))
		}
	}
}

func TestGradlePayloadsDecode(t *testing.T) {
	var d gradleDaemon
	if err := json.Unmarshal([]byte(gradleDaemonJSON), &d); err != nil {
		t.Fatal(err)
	}
	if d.PID != 48213 || d.Version != "9.4.1" || d.Uptime != "2h 14m" || d.CPU != 3.4 || d.MemoryMB != 2874 ||
		d.JDK == nil || *d.JDK != "21" || d.MaxHeap == nil || *d.MaxHeap != "12g" {
		t.Errorf("daemon decoded as %+v", d)
	}
	if d.isBusy() || d.ramText() != "2.8 GB" || d.cpuText() != "3%" {
		t.Errorf("busy %v, ram %q, cpu %q", d.isBusy(), d.ramText(), d.cpuText())
	}
	var bare gradleDaemon
	if err := json.Unmarshal([]byte(`{"cpu":20.5,"memoryMB":1023,"pid":7,"uptime":"1m","version":"8.0"}`), &bare); err != nil {
		t.Fatal(err)
	}
	if bare.JDK != nil || bare.MaxHeap != nil || !bare.isBusy() || bare.ramText() != "1023 MB" || bare.cpuText() != "21%" {
		t.Errorf("daemon without jdk and heap decoded as %+v", bare)
	}
	var caches []gradleCache
	if err := json.Unmarshal([]byte(`[{"name":"modules-2","sizeMB":6143}]`), &caches); err != nil {
		t.Fatal(err)
	}
	if len(caches) != 1 || caches[0].Name != "modules-2" || caches[0].SizeMB != 6143 {
		t.Errorf("caches decoded as %+v", caches)
	}
}

func TestXcodePayloadDecodes(t *testing.T) {
	var e derivedEntry
	if err := json.Unmarshal([]byte(derivedEntryJSON), &e); err != nil {
		t.Fatal(err)
	}
	if e.Name != "Jaca" || e.Path != "/Users/me/Library/Developer/Xcode/DerivedData/Jaca-bqz" || e.SizeMB != 1832 ||
		e.kind() != "stale" || e.subtitle() != "/Users/me/wt/x/Jaca.xcodeproj" || e.tag().text != "STALE" {
		t.Errorf("entry decoded as %+v", e)
	}
	v, _, _ := testXcodeViewer(t)
	if shared := v.entries[2]; shared.WorkspacePath != nil || shared.subtitle() != "Shared cache" || shared.tag().text != "SHARED" {
		t.Errorf("shared entry reads %+v", shared)
	}
	if live := v.entries[3]; live.tag().text != "LIVE" {
		t.Errorf("live entry is tagged %q", live.tag().text)
	}
	// A kind this client doesn't know reads as shared, so "clean stale" leaves it.
	if unknown := v.entries[4]; unknown.kind() != "shared" || unknown.tag().text != "SHARED" {
		t.Errorf("unknown kind reads as %q", unknown.kind())
	}
}

func TestToolSummariesAndSizes(t *testing.T) {
	for mb, want := range map[int]string{0: "0 MB", 1023: "1023 MB", 1024: "1.00 GB", 1832: "1.79 GB", 6143: "6.00 GB"} {
		if got := formatSize(mb); got != want {
			t.Errorf("formatSize(%d) = %q, want %q", mb, got, want)
		}
	}
	x, _, _ := testXcodeViewer(t)
	if got := x.summary(); got != "3.01 GB total · 5 entries" {
		t.Errorf("summary %q", got)
	}
	if count, mb := x.stale(); count != 2 || mb != 2132 {
		t.Errorf("stale: %d entries, %d MB", count, mb)
	}
	text := frameText(x.frame(24, 100))
	for _, want := range []string{"DERIVED DATA", "3.01 GB total · 5 entries", "Clean 2 stale", "Shared cache", "1.79 GB", "Delete"} {
		if !strings.Contains(text, want) {
			t.Errorf("the Xcode pane lacks %q:\n%s", want, text)
		}
	}
	// One entry is still "entries", as in the app.
	x.entries = x.entries[3:4]
	if got := x.summary(); got != "40 MB total · 1 entries" {
		t.Errorf("summary of one entry %q", got)
	}
	if strings.Contains(frameText(x.frame(24, 100)), "Clean") {
		t.Error("the clean button shows without stale entries")
	}

	g, _, _ := testGradleViewer(t)
	text = frameText(g.frame(24, 120))
	for _, want := range []string{"GRADLE CACHE", "6.50 GB total", "modules-2", "DAEMONS", "Gradle 9.4.1", "PID 48213", "IDLE",
		"BUSY", "JDK 21", "12g", "2.8 GB", "2h 14m", "3%", "88%", "512 MB", "Kill", "Help"} {
		if !strings.Contains(text, want) {
			t.Errorf("the Gradle pane lacks %q:\n%s", want, text)
		}
	}
}

func TestGradleEmptyAndLoadingStates(t *testing.T) {
	v := &gradleViewer{removing: map[int32]bool{}, listed: map[int32]bool{}, deleting: map[string]bool{}}
	testTools(&v.toolPane)
	v.owner = v
	if text := frameText(v.frame(24, 80)); strings.Contains(text, "GRADLE CACHE") || strings.Contains(text, "No Gradle daemons running") {
		t.Errorf("before anything arrived:\n%s", text)
	}
	v.cacheLoading = true
	v.apply(nil)
	text := frameText(v.frame(24, 80))
	for _, want := range []string{"GRADLE CACHE", "Calculating…", "Measuring cache size…", "No Gradle daemons running"} {
		if !strings.Contains(text, want) {
			t.Errorf("while measuring, the pane lacks %q:\n%s", want, text)
		}
	}
	// No cache folders: the section goes.
	v.measured(nil, nil)
	if text := frameText(v.frame(24, 80)); strings.Contains(text, "GRADLE CACHE") {
		t.Errorf("an empty cache still shows its section:\n%s", text)
	}
	// A failed measure ends the loading state and says why.
	v.cacheLoading = true
	v.measured(nil, errors.New("gradle.caches: no reply from jacad after 10m0s"))
	if text := frameText(v.frame(24, 80)); v.cacheLoading || !strings.Contains(text, "no reply from jacad") {
		t.Errorf("after a failed measure:\n%s", text)
	}
}

func TestXcodeEmptyAndLoadingStates(t *testing.T) {
	v := &xcodeViewer{removing: map[string]bool{}}
	_, calls := testTools(&v.toolPane)
	v.owner = v
	v.loading = true
	if text := frameText(v.frame(24, 80)); !strings.Contains(text, "Scanning DerivedData…") || !strings.Contains(text, "0 MB total · 0 entries") {
		t.Errorf("while scanning:\n%s", text)
	}
	v.scanned(nil, nil)
	if text := frameText(v.frame(24, 80)); !strings.Contains(text, "No DerivedData found") {
		t.Errorf("with nothing found:\n%s", text)
	}
	// r rescans, and a scan that fails says why.
	v.handleKey([]byte("r"))
	if len(calls.made) != 1 || calls.made[0] != "xcode.list" || v.loading {
		t.Errorf("r made %v, loading %v", calls.made, v.loading)
	}
	if text := frameText(v.frame(24, 80)); !strings.Contains(text, "no reply") {
		t.Errorf("after a failed scan:\n%s", text)
	}
}

// Every row is at most the pane's width and the frame at most its height, at any size, with
// buttons armed, rows dimmed and a toast showing.
func TestToolPanesFitThePane(t *testing.T) {
	g, _, _ := testGradleViewer(t)
	x, _, _ := testXcodeViewer(t)
	for _, state := range []string{"idle", "armed"} {
		if state == "armed" {
			g.press(0)
			g.press(2)
			g.removing[500] = true
			g.deleting["8.7"] = true
			g.flash("Couldn't kill 48213")
			x.press(0)
			x.pressClean()
			x.removing["/dd/App-ccc"] = true
			x.flash("Couldn't delete ModuleCache.noindex")
		}
		for _, rows := range []int{1, 2, 3, 5, 8, 24, 50} {
			for cols := 1; cols <= 170; cols++ {
				for selected := 0; selected < 5; selected++ {
					g.selected, g.follow = selected, true
					checkWidths(t, "gradle "+state, g.frame(rows, cols), rows, cols)
					x.selected, x.follow = selected, true
					checkWidths(t, "xcode "+state, x.frame(rows, cols), rows, cols)
				}
			}
		}
	}
	// The keys popups fit too.
	for cols := 1; cols <= 100; cols += 3 {
		for _, rows := range []int{2, 4, 10, 24} {
			checkWidths(t, "gradle help", keysBox(g.helpKeys(), rows, cols), rows, cols)
			checkWidths(t, "xcode help", keysBox(x.helpKeys(), rows, cols), rows, cols)
		}
	}
}

func TestToolsPickerFitsThePane(t *testing.T) {
	quit := &pane{work: make(chan func()), done: make(chan struct{})}
	close(quit.done)
	p := &toolsPicker{p: quit}
	for _, withErr := range []bool{false, true} {
		if withErr {
			p.err = "plugin pane open failed: no such entrypoint"
		}
		for _, rows := range []int{1, 2, 3, 4, 10, 24} {
			for cols := 1; cols <= 120; cols++ {
				checkWidths(t, "picker", p.frame(rows, cols), rows, cols)
				for i, row := range panelFrame(p.frame(rows, cols), rows, cols) {
					if got := plainWidth(row); got != cols {
						t.Fatalf("picker panel row %d is %d cells in a %d-cell pane", i, got, cols)
					}
				}
			}
		}
	}
	p.err = ""
	text := frameText(p.frame(10, 40))
	if !strings.Contains(text, "  Gradle") || !strings.Contains(text, "  Xcode") || !strings.Contains(text, escClose) {
		t.Errorf("picker reads:\n%s", text)
	}
	if toolOptions[0].tab != "Gradle - Jaca" || toolOptions[1].tab != "Xcode - Jaca" ||
		toolOptions[0].entrypoint != "gradle" || toolOptions[1].entrypoint != "xcode" {
		t.Errorf("options %+v", toolOptions)
	}
}

func TestToolsPickerKeys(t *testing.T) {
	quit := &pane{work: make(chan func()), done: make(chan struct{})}
	close(quit.done)
	p := &toolsPicker{p: quit}
	p.frame(10, 40)
	p.handleKey([]byte("j"))
	p.handleKey([]byte("j"))
	if p.selected != 1 {
		t.Errorf("two steps down selected %d", p.selected)
	}
	p.handleKey([]byte("k"))
	if p.selected != 0 {
		t.Errorf("a step up selected %d", p.selected)
	}
	if !p.handleKey([]byte{0x1b}) || !p.handleKey([]byte("q")) {
		t.Error("Esc and q should close the picker")
	}
	if !p.handleKey([]byte(fmt.Sprintf("\x1b[<0;%d;2M", p.closeX0))) {
		t.Error("a click on the close button should close the picker")
	}
	if p.handleKey([]byte("\x1b[<0;3;9M")) || p.handleKey([]byte(fmt.Sprintf("\x1b[<0;%d;2m", p.closeX0))) {
		t.Error("a click under the options or a release closed the picker")
	}
}

func TestConfirmTakesTwoPressesWithinThreeSeconds(t *testing.T) {
	var tp toolPane
	clock, _ := testTools(&tp)
	if tp.confirm("a") {
		t.Fatal("the first press confirmed")
	}
	if !tp.isArmed("a") || tp.isArmed("b") {
		t.Fatal("the first press should arm its own button only")
	}
	// Another button arms on its own and leaves the first armed.
	if tp.confirm("b") || !tp.isArmed("a") || !tp.isArmed("b") {
		t.Fatal("a second button should arm beside the first")
	}
	clock.advance(2900 * time.Millisecond)
	if !tp.isArmed("a") {
		t.Fatal("the button lapsed before three seconds")
	}
	if !tp.confirm("a") {
		t.Fatal("the second press within three seconds should confirm")
	}
	if tp.isArmed("a") {
		t.Fatal("a confirmed button stays armed")
	}
	clock.advance(100 * time.Millisecond)
	if tp.isArmed("b") || len(tp.armed) != 0 {
		t.Fatalf("after three seconds %v is still armed", tp.armed)
	}
	// Past the window a press arms again.
	if tp.confirm("b") {
		t.Fatal("a press after the window confirmed")
	}
	// A press that re-arms outlives the timer of the lapsed one.
	clock.advance(armWindow)
	tp.confirm("b")
	clock.advance(armWindow - time.Millisecond)
	if !tp.isArmed("b") {
		t.Fatal("the re-armed button lapsed early")
	}
}

func TestToastShowsForItsLifeAndIsReplaced(t *testing.T) {
	var tp toolPane
	clock, _ := testTools(&tp)
	tp.flash("Killed 1")
	clock.advance(2000 * time.Millisecond)
	tp.flash("Killed 2")
	clock.advance(600 * time.Millisecond) // the first toast's time: the second stays
	if tp.toast != "Killed 2" {
		t.Fatalf("toast %q, want the second", tp.toast)
	}
	clock.advance(1999 * time.Millisecond)
	if tp.toast != "Killed 2" {
		t.Fatalf("the toast went %v early", time.Millisecond)
	}
	clock.advance(time.Millisecond)
	if tp.toast != "" {
		t.Fatalf("toast %q still shows after its life", tp.toast)
	}
}

func TestGradleKeysArmThenKill(t *testing.T) {
	v, clock, calls := testGradleViewer(t)
	calls.results["gradle.kill"] = "true"
	// Rows are sorted by pid under the two cache folders.
	if v.keyAt(2) != "pid:500" || v.keyAt(3) != "pid:48213" || v.keyAt(4) != "pid:70000" {
		t.Fatalf("rows %q %q %q", v.keyAt(2), v.keyAt(3), v.keyAt(4))
	}
	for i := 0; i < 3; i++ {
		v.handleKey([]byte("j"))
	}
	v.handleKey([]byte("x"))
	if len(calls.made) != 0 || !v.isArmed("pid:48213") {
		t.Fatalf("one press: calls %v, armed %v", calls.made, v.armed)
	}
	if text := frameText(v.frame(24, 120)); !strings.Contains(text, "Confirm kill?") {
		t.Errorf("the armed row lacks its label:\n%s", text)
	}
	// Left alone for three seconds it disarms, and the next press arms again.
	clock.advance(armWindow)
	if text := frameText(v.frame(24, 120)); strings.Contains(text, "Confirm kill?") {
		t.Errorf("the button is still armed after three seconds:\n%s", text)
	}
	v.handleKey([]byte{'\r'})
	if len(calls.made) != 0 {
		t.Fatalf("a press after the window killed: %v", calls.made)
	}
	v.handleKey([]byte{0x7f})
	if len(calls.made) != 1 || calls.made[0] != "gradle.kill 48213" {
		t.Fatalf("two presses made %v", calls.made)
	}
	// Dimmed until the fade ends, then gone with the toast.
	if !v.removing[48213] || len(v.daemons) != 3 || v.toast != "" {
		t.Fatalf("right after the kill: removing %v, %d daemons, toast %q", v.removing, len(v.daemons), v.toast)
	}
	v.handleKey([]byte("x")) // a press on the row being removed does nothing
	if len(calls.made) != 1 || v.isArmed("pid:48213") {
		t.Fatalf("a press on a row being removed: calls %v, armed %v", calls.made, v.armed)
	}
	clock.advance(fadeTime)
	if len(v.daemons) != 2 || v.removing[48213] || v.toast != "Killed 48213" {
		t.Fatalf("after the fade: %d daemons, toast %q", len(v.daemons), v.toast)
	}
	clock.advance(toastLife)
	if v.toast != "" {
		t.Fatalf("toast %q outlived its time", v.toast)
	}
}

func TestGradleKillOutcomes(t *testing.T) {
	// Killed: the row stays through later lists and the fade, then goes.
	v, clock, _ := testGradleViewer(t)
	v.removing[500] = true
	v.killed(500, true)
	v.apply(v.daemons[1:]) // jacad's next list no longer has it
	if len(v.daemons) != 3 || v.daemons[0].PID != 500 {
		t.Fatalf("the row left before its fade: %+v", v.daemons)
	}
	clock.advance(fadeTime)
	if len(v.daemons) != 2 || v.toast != "Killed 500" || v.removing[500] {
		t.Fatalf("killed: %d daemons, toast %q", len(v.daemons), v.toast)
	}

	// Not killed and no longer listed: it exited on its own, so the row goes without a toast.
	v, _, _ = testGradleViewer(t)
	v.removing[500] = true
	v.apply(v.daemons[1:])
	v.killed(500, false)
	if len(v.daemons) != 2 || v.toast != "" || v.removing[500] {
		t.Fatalf("already gone: %d daemons, toast %q", len(v.daemons), v.toast)
	}

	// Not killed and still listed: the row comes back.
	v, _, calls := testGradleViewer(t)
	calls.results["gradle.kill"] = "false"
	v.selected = 2
	v.press(2)
	v.press(2)
	if len(v.daemons) != 3 || v.removing[500] || v.toast != "Couldn't kill 500" {
		t.Fatalf("failed kill: %d daemons, removing %v, toast %q", len(v.daemons), v.removing, v.toast)
	}
	// A call that errors counts as not killed.
	delete(calls.results, "gradle.kill")
	v.press(2)
	v.press(2)
	if v.removing[500] || v.toast != "Couldn't kill 500" {
		t.Fatalf("errored kill: removing %v, toast %q", v.removing, v.toast)
	}
}

func TestGradleListKeepsOrderAndCursor(t *testing.T) {
	v, _, _ := testGradleViewer(t)
	v.selected = 3 // pid 48213
	var list []gradleDaemon
	if err := json.Unmarshal([]byte(`[{"pid":90000,"version":"9.0"},{"pid":48213,"version":"9.4.1"},{"pid":12,"version":"8.0"}]`), &list); err != nil {
		t.Fatal(err)
	}
	v.apply(list)
	if len(v.daemons) != 3 || v.daemons[0].PID != 12 || v.daemons[1].PID != 48213 || v.daemons[2].PID != 90000 {
		t.Fatalf("daemons %+v", v.daemons)
	}
	if v.keyAt(v.selected) != "pid:48213" {
		t.Errorf("the cursor moved to %q", v.keyAt(v.selected))
	}
	// The row under the cursor goes: the cursor stays inside the list.
	v.apply(nil)
	if v.selected != 1 || v.rowCount() != 2 {
		t.Errorf("cursor at %d of %d rows", v.selected, v.rowCount())
	}
	v.handleEvent(event{Topic: gradleTopic, Data: []byte(`[` + gradleDaemonJSON + `]`)})
	if len(v.daemons) != 1 || v.daemons[0].PID != 48213 {
		t.Errorf("after an event: %+v", v.daemons)
	}
	v.handleEvent(event{Topic: gradleTopic, Data: []byte(`{"not":"a list"}`)})
	if len(v.daemons) != 1 {
		t.Errorf("a bad event changed the list: %+v", v.daemons)
	}
}

func TestGradleCacheDelete(t *testing.T) {
	v, _, calls := testGradleViewer(t)
	calls.results["gradle.deleteCache modules-2"] = "true"
	calls.results["gradle.deleteCache 8.7"] = "false"
	v.press(1)
	if len(calls.made) != 0 {
		t.Fatalf("one press deleted: %v", calls.made)
	}
	if text := frameText(v.frame(24, 100)); !strings.Contains(text, "Confirm?") {
		t.Errorf("the armed cache row lacks its label:\n%s", text)
	}
	v.press(1)
	if len(v.caches) != 2 || v.toast != "Couldn't delete 8.7" || v.deleting["8.7"] {
		t.Fatalf("failed delete: %d folders, toast %q", len(v.caches), v.toast)
	}
	v.selected = 1
	v.press(0)
	v.press(0)
	if len(v.caches) != 1 || v.caches[0].Name != "8.7" || v.toast != "Deleted cache modules-2" {
		t.Fatalf("delete: %+v, toast %q", v.caches, v.toast)
	}
	if v.keyAt(v.selected) != "cache:8.7" {
		t.Errorf("the cursor moved to %q", v.keyAt(v.selected))
	}
	if got := frameText(v.frame(24, 100)); !strings.Contains(got, "512 MB total") {
		t.Errorf("the total didn't follow the rows:\n%s", got)
	}
}

func TestGradleRefreshListsAndMeasures(t *testing.T) {
	v, _, calls := testGradleViewer(t)
	calls.results["gradle.list"] = `[` + gradleDaemonJSON + `]`
	calls.results["gradle.caches"] = `[{"name":"modules-2","sizeMB":10}]`
	v.handleKey([]byte("r"))
	if strings.Join(calls.made, ",") != "gradle.list,gradle.caches" {
		t.Fatalf("r made %v", calls.made)
	}
	if len(v.daemons) != 1 || len(v.caches) != 1 || v.cacheTotalMB() != 10 || v.cacheLoading {
		t.Errorf("after r: %d daemons, caches %+v", len(v.daemons), v.caches)
	}
	// A dropped update of the topic fetches the list.
	v.handleEvent(event{Topic: "events.dropped", Data: mustJSON(droppedNote{Topic: gradleTopic, Count: 1})})
	if len(calls.made) != 3 || calls.made[2] != "gradle.list" {
		t.Errorf("a dropped update made %v", calls.made)
	}
}

func TestXcodeDeleteOutcomes(t *testing.T) {
	v, clock, calls := testXcodeViewer(t)
	calls.results["xcode.delete /dd/App-ccc"] = "true"
	calls.results["xcode.delete /dd/Old-aaa"] = "false"
	v.selected = 3
	v.handleKey([]byte("x"))
	if len(calls.made) != 0 || !v.isArmed("/dd/App-ccc") {
		t.Fatalf("one press: calls %v", calls.made)
	}
	v.handleKey([]byte("x"))
	if len(calls.made) != 1 || !v.removing["/dd/App-ccc"] || len(v.entries) != 5 {
		t.Fatalf("two presses: calls %v, removing %v", calls.made, v.removing)
	}
	clock.advance(fadeTime)
	if len(v.entries) != 4 || v.toast != "Deleted App" || v.removing["/dd/App-ccc"] {
		t.Fatalf("after the fade: %d entries, toast %q", len(v.entries), v.toast)
	}
	if got := v.summary(); got != "2.97 GB total · 4 entries" {
		t.Errorf("summary after a delete %q", got)
	}
	// A failed delete brings the row back.
	v.press(1)
	v.press(1)
	if len(v.entries) != 4 || v.removing["/dd/Old-aaa"] || v.toast != "Couldn't delete Old" {
		t.Fatalf("failed delete: %d entries, removing %v, toast %q", len(v.entries), v.removing, v.toast)
	}
}

func TestXcodeCleanStaleRemovesEveryStaleRow(t *testing.T) {
	v, clock, calls := testXcodeViewer(t)
	calls.results["xcode.delete /Users/me/Library/Developer/Xcode/DerivedData/Jaca-bqz"] = "true"
	calls.results["xcode.delete /dd/Old-aaa"] = "false"
	v.selected = 3 // the live entry
	v.handleKey([]byte("S"))
	if len(calls.made) != 0 || !v.isArmed(staleKey) {
		t.Fatalf("one press: calls %v", calls.made)
	}
	if text := frameText(v.frame(24, 100)); !strings.Contains(text, "Confirm? (2.08 GB)") {
		t.Errorf("the armed clean button lacks its label:\n%s", text)
	}
	v.handleKey([]byte("S"))
	if len(calls.made) != 2 || !v.removing["/dd/Old-aaa"] || len(v.entries) != 5 {
		t.Fatalf("two presses: calls %v, removing %v", calls.made, v.removing)
	}
	// Nothing more starts while it runs.
	v.handleKey([]byte("S"))
	v.handleKey([]byte("S"))
	if len(calls.made) != 2 {
		t.Fatalf("pressed while cleaning: calls %v", calls.made)
	}
	clock.advance(fadeTime)
	if len(v.entries) != 3 || v.toast != "Deleted 1 stale" || len(v.removing) != 0 || v.cleaning {
		t.Fatalf("after the fade: %+v, toast %q", v.entries, v.toast)
	}
	for _, e := range v.entries {
		if e.kind() == "stale" {
			t.Errorf("stale entry %q is still listed", e.Name)
		}
	}
	if v.entries[v.selected].Name != "App" {
		t.Errorf("the cursor moved to %q", v.entries[v.selected].Name)
	}
	if got := v.summary(); got != "948 MB total · 3 entries" {
		t.Errorf("summary after the clean %q", got)
	}
	// With no stale entries left the key does nothing.
	v.handleKey([]byte("S"))
	if v.isArmed(staleKey) {
		t.Error("the clean button armed without stale entries")
	}
}

func TestToolPaneMouse(t *testing.T) {
	oldRows, oldCols := termRows, termCols
	defer func() { termRows, termCols = oldRows, oldCols }()
	termRows, termCols = 24, 100
	click := func(x, y int) []byte { return []byte(fmt.Sprintf("\x1b[<0;%d;%dM", x, y)) }

	v, _, calls := testGradleViewer(t)
	calls.results["gradle.kill"] = "true"
	frame := v.frame(termRows, termCols)
	// Each button's recorded columns hold its label.
	y := 0
	for row, line := range v.shown {
		if line.x0 == 0 {
			continue
		}
		plain := []rune(sgr.ReplaceAllString(frame[row-1], ""))
		if line.x1 > len(plain) {
			t.Fatalf("row %d: button columns %d-%d past the row's %d cells", row, line.x0, line.x1, len(plain))
		}
		if got := string(plain[line.x0-1 : line.x1]); got != " Kill " && got != " Delete " {
			t.Errorf("row %d: button columns hold %q", row, got)
		}
		if line.item == 4 {
			y = row
		}
	}
	if y == 0 {
		t.Fatal("the last daemon's row has no button")
	}
	line := v.shown[y]
	// A click on the row selects it; one on its button also presses it.
	v.handleKey(click(3, y))
	if v.selected != 4 || len(v.armed) != 0 {
		t.Fatalf("a click on the row: selected %d, armed %v", v.selected, v.armed)
	}
	v.handleKey(click(line.x0, y))
	if !v.isArmed("pid:70000") || len(calls.made) != 0 {
		t.Fatalf("a click on the button: armed %v, calls %v", v.armed, calls.made)
	}
	v.frame(termRows, termCols)
	v.handleKey(click(v.shown[y].x1, y))
	if len(calls.made) != 1 || calls.made[0] != "gradle.kill 70000" {
		t.Fatalf("a second click made %v", calls.made)
	}
	// A click on a heading does nothing, Help opens the keys, and a click closes them.
	v.handleKey(click(3, 1))
	if v.selected != 4 {
		t.Errorf("a click on a heading selected %d", v.selected)
	}
	v.handleKey(click(v.helpX0, termRows))
	if !v.help {
		t.Fatal("a click on Help didn't open the keys")
	}
	v.handleKey(click(1, 1))
	if v.help {
		t.Fatal("a click didn't close the keys")
	}
	v.handleKey([]byte("?"))
	if v.handleKey([]byte("q")) || v.help {
		t.Error("q with the keys open should close them, not the pane")
	}
	if !v.handleKey([]byte("q")) {
		t.Error("q should quit the pane")
	}

	x, _, _ := testXcodeViewer(t)
	frame = x.frame(termRows, termCols)
	clean := x.shown[1]
	if got := string([]rune(sgr.ReplaceAllString(frame[0], ""))[clean.x0-1 : clean.x1]); got != " Clean 2 stale " {
		t.Fatalf("the clean button's columns hold %q", got)
	}
	x.handleKey(click(clean.x0, 1))
	if !x.isArmed(staleKey) {
		t.Error("a click on the clean button didn't arm it")
	}
}

func TestToolPaneScrollsToTheCursor(t *testing.T) {
	oldRows, oldCols := termRows, termCols
	defer func() { termRows, termCols = oldRows, oldCols }()
	termRows, termCols = 8, 80

	v, _, _ := testXcodeViewer(t)
	// Three rows of header and two of footer leave three for the list.
	if text := frameText(v.frame(8, 80)); !strings.Contains(text, "Jaca") || strings.Contains(text, "Later") {
		t.Fatalf("at the top:\n%s", text)
	}
	for i := 0; i < 10; i++ {
		v.handleKey([]byte("j"))
	}
	text := frameText(v.frame(8, 80))
	if v.selected != 4 || !strings.Contains(text, "Later") || strings.Contains(text, "Old") || !strings.Contains(text, "DERIVED DATA") {
		t.Fatalf("at the bottom (selected %d):\n%s", v.selected, text)
	}
	// The wheel scrolls without moving the cursor.
	v.handleKey([]byte("\x1b[<64;5;5M"))
	if text := frameText(v.frame(8, 80)); v.selected != 4 || !strings.Contains(text, "Jaca") {
		t.Fatalf("after the wheel (selected %d):\n%s", v.selected, text)
	}
	v.handleKey([]byte("\x1b[5~"))
	if v.selected != 1 {
		t.Errorf("PgUp selected %d, want 1", v.selected)
	}
}

func TestRowTableDropsColumnsAndCuts(t *testing.T) {
	rows := [][]cell{
		{{"Gradle 9.4.1", sgrBold}, {"PID 48213", sgrDim}, {"IDLE", ""}, {}, {}, {"2.8 GB", ""}, {"2h 14m", ""}, {"3%", ""}, {" Kill ", sgrRed}},
		{{"Gradle 8.7", sgrBold}, {"PID 500", sgrDim}, {"BUSY", sgrGreen}, {"JDK 21", ""}, {"4g", ""}, {"512 MB", ""}, {"3m", ""}, {"88%", ""}, {" Confirm kill? ", sgrRed}},
	}
	for w := 0; w <= 140; w++ {
		for i, row := range daemonTable.layout(rows, w) {
			plain := sgr.ReplaceAllString(row.text, "")
			// The cut column keeps a cell where there is none to give, which the pane's frame clips.
			if got := cellWidth(plain); got > max(w, 0) && w >= 20 {
				t.Fatalf("row %d is %d cells in %d", i, got, w)
			}
			if row.x0 >= 0 {
				if got := string([]rune(plain)[row.x0 : row.x1+1]); got != rows[i][8].text {
					t.Fatalf("at %d cells row %d's button columns hold %q", w, i, got)
				}
			}
		}
	}
	wide := daemonTable.layout(rows, 100)
	if got := sgr.ReplaceAllString(wide[1].text, ""); cellWidth(got) != 100 || !strings.HasSuffix(got, " Confirm kill? ") ||
		!strings.HasPrefix(got, "Gradle 8.7    PID 500 ") {
		t.Errorf("wide row %q", got)
	}
	// Narrow: the tags go first and the button last.
	narrow := sgr.ReplaceAllString(daemonTable.layout(rows, 40)[1].text, "")
	if strings.Contains(narrow, "JDK 21") || !strings.Contains(narrow, "Confirm kill?") || !strings.Contains(narrow, "Gradle") {
		t.Errorf("narrow row %q", narrow)
	}
	if got := daemonTable.layout(nil, 40); len(got) != 0 {
		t.Errorf("no rows laid out as %v", got)
	}
	if got := daemonTable.layout([][]cell{{}, nil}, 40); len(got) != 2 || got[0].x0 != -1 {
		t.Errorf("empty rows laid out as %v", got)
	}
}

func TestClipMiddle(t *testing.T) {
	const path = "/Users/me/wt/x/Jaca.xcodeproj"
	if got := clipMiddle(path, 40); got != path {
		t.Errorf("a path that fits was cut: %q", got)
	}
	if got := clipMiddle(path, 15); got != "/Users/…odeproj" {
		t.Errorf("clipMiddle(15) = %q", got)
	}
	for w := 0; w <= 40; w++ {
		for _, s := range []string{path, "日本語のパス/プロジェクト.xcodeproj", ""} {
			if got := cellWidth(clipMiddle(s, w)); got > w {
				t.Fatalf("clipMiddle(%q, %d) is %d cells", s, w, got)
			}
		}
	}
}
