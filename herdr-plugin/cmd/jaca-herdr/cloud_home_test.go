package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// cloudFake stands in for jacad: it records each call with its parameters and answers with the
// JSON held for the method, or fails with the error held for it.
type cloudFake struct {
	made    []string
	results map[string]string
	errs    map[string]error
}

func (f *cloudFake) call(method string, params, out any, _ time.Duration) error {
	entry := method
	if params != nil {
		raw, _ := json.Marshal(params)
		entry += " " + string(raw)
	}
	f.made = append(f.made, entry)
	if err := f.errs[method]; err != nil {
		return err
	}
	if result, ok := f.results[method]; ok && out != nil {
		return json.Unmarshal([]byte(result), out)
	}
	return nil
}

// calls are the recorded calls to one method.
func (f *cloudFake) calls(method string) []string {
	var out []string
	for _, entry := range f.made {
		if entry == method || strings.HasPrefix(entry, method+" ") {
			out = append(out, entry)
		}
	}
	return out
}

// testCloudCalls is the test clock and calls that run in place.
func testCloudCalls() (cloudCalls, *toolClock, *cloudFake) {
	clock := &toolClock{now: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)}
	fake := &cloudFake{results: map[string]string{}, errs: map[string]error{}}
	return cloudCalls{
		now:   func() time.Time { return clock.now },
		after: clock.after,
		call:  fake.call,
		spawn: func(work func() func()) { work()() },
	}, clock, fake
}

const cloudStateJSON = `{"binaryPath":"/opt/homebrew/bin/gcloud","isDetecting":false,
	"authState":{"state":"authenticated","account":"dev@example.com"},
	"projects":[
		{"projectID":"acme-prod","displayName":"Production","selectedLogName":"projects/acme-prod/logs/run.googleapis.com%2Fstdout",
		 "logNames":["projects/acme-prod/logs/run.googleapis.com%2Fstderr","projects/acme-prod/logs/run.googleapis.com%2Fstdout"]},
		{"projectID":"acme-staging","displayName":"","logNames":[]},
		{"projectID":"a-project-with-a-very-long-identifier-0123456789","displayName":"A display name that is far longer than any pane this is drawn in will ever be wide","logNames":[]}],
	"queryTemplates":[],"sqlTemplates":[]}`

type cloudHomeFixture struct {
	h        *cloudHome
	clock    *toolClock
	fake     *cloudFake
	launched []cloudSessionSpec
	terminal int
}

// testCloudHome is a home on a pane that has quit, signed in, with three projects.
func testCloudHome(t *testing.T) *cloudHomeFixture {
	t.Helper()
	quit := &pane{work: make(chan func()), done: make(chan struct{})}
	close(quit.done)
	calls, clock, fake := testCloudCalls()
	f := &cloudHomeFixture{h: buildCloudHome(quit, calls), clock: clock, fake: fake}
	f.h.launch = func(spec cloudSessionSpec) { f.launched = append(f.launched, spec) }
	f.h.openTerminal = func() error { f.terminal++; return nil }
	f.h.apply(cloudStateFrom(t, cloudStateJSON))
	f.h.moved = false // as after the first draw
	return f
}

func cloudStateFrom(t *testing.T, raw string) cloudState {
	t.Helper()
	var st cloudState
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func withAuth(st cloudState, state, account string) cloudState {
	st.AuthState = cloudAuthState{State: state, Account: account}
	if state == cloudAuthNotInstalled {
		st.BinaryPath = ""
	}
	return st
}

func plainFrame(rows []string) string { return sgr.ReplaceAllString(strings.Join(rows, "\n"), "") }

var cloudSizes = [][2]int{{40, 160}, {30, 100}, {24, 80}, {60, 220}, {20, 50}, {12, 30}, {5, 9}, {2, 3}}

func checkFrame(t *testing.T, name string, frame []string, rows, cols int) {
	t.Helper()
	if len(frame) != rows {
		t.Errorf("%s at %dx%d: %d rows", name, rows, cols, len(frame))
	}
	for i, row := range frame {
		if w := plainWidth(row); w > cols {
			t.Errorf("%s at %dx%d: row %d is %d cells: %q", name, rows, cols, i, w, sgr.ReplaceAllString(row, ""))
		}
	}
}

func checkSheet(t *testing.T, name string, s cloudSheet, wantFrom [2]int) {
	t.Helper()
	for _, size := range cloudSizes {
		box, top, left := s.box(size[0], size[1])
		if len(box) == 0 {
			if size[0] >= wantFrom[0] && size[1] >= wantFrom[1] {
				t.Errorf("%s: no popup at %dx%d", name, size[0], size[1])
			}
			continue
		}
		if top+len(box) > size[0] {
			t.Errorf("%s at %dx%d: %d rows from row %d", name, size[0], size[1], len(box), top)
		}
		for i, row := range box {
			if w := plainWidth(row); w != plainWidth(box[0]) || left+w > size[1] {
				t.Errorf("%s at %dx%d: row %d is %d cells, the border %d: %q", name, size[0], size[1], i, w, plainWidth(box[0]), sgr.ReplaceAllString(row, ""))
			}
		}
	}
	s.box(40, 120) // a sheet takes keys as it was last drawn: leave it drawn in a pane that holds it
}

// Every row of the home fits the pane, in each auth state, with and without projects.
func TestCloudHomeRowsFit(t *testing.T) {
	f := testCloudHome(t)
	base := f.h.state
	empty := base
	empty.Projects = nil
	states := map[string]cloudState{
		"signed in":     base,
		"not installed": withAuth(base, cloudAuthNotInstalled, ""),
		"signed out":    withAuth(base, cloudAuthNotAuthenticated, ""),
		"empty":         empty,
		"empty, out":    withAuth(empty, cloudAuthNotAuthenticated, ""),
	}
	for name, st := range states {
		f.h.apply(st)
		f.h.flash("Removed " + base.Projects[2].title())
		for _, size := range cloudSizes {
			for selected := range base.Projects {
				f.h.selected, f.h.follow = selected, true
				checkFrame(t, name, f.h.frame(size[0], size[1]), size[0], size[1])
			}
		}
	}
	// Before any state arrives the body is empty, not the empty state.
	quit := &pane{work: make(chan func()), done: make(chan struct{})}
	close(quit.done)
	calls, _, _ := testCloudCalls()
	fresh := buildCloudHome(quit, calls)
	frame := fresh.frame(24, 80)
	checkFrame(t, "fresh", frame, 24, 80)
	if got := plainFrame(frame); strings.Contains(got, "No GCP projects yet") || !strings.Contains(got, "—") {
		t.Errorf("a home with no state shows %q", got)
	}
}

func TestCloudHomeAccountAndBanner(t *testing.T) {
	f := testCloudHome(t)
	base := f.h.state
	detecting := withAuth(base, cloudAuthUnknown, "")
	detecting.IsDetecting = true
	steps := []struct {
		name    string
		state   cloudState
		account string
		banner  []string
		absent  []string
	}{
		{"detecting", detecting, "Detecting gcloud…", nil, []string{"gcloud auth login", "Authentication required", "gcloud not found"}},
		{"unknown", withAuth(base, cloudAuthUnknown, ""), "—", nil, []string{"gcloud auth login"}},
		{"not installed", withAuth(base, cloudAuthNotInstalled, ""), "gcloud CLI not found",
			[]string{"gcloud not found", "The gcloud CLI wasn't found. Install the Google Cloud SDK, then click Re-check."},
			[]string{"gcloud auth login", "Open Terminal"}},
		{"signed out", withAuth(base, cloudAuthNotAuthenticated, ""), "Not signed in",
			[]string{"Authentication required", "Sign in to gcloud in a terminal, then click Re-check.", "│ gcloud auth login │", " Copy ", " Open Terminal ", " Re-check "},
			[]string{"gcloud not found"}},
		{"signed in", base, "Signed in as dev@example.com", nil, []string{"Authentication required", "gcloud not found", "gcloud auth login"}},
	}
	for _, step := range steps {
		f.h.apply(step.state)
		frame := f.h.frame(30, 120)
		got := plainFrame(frame)
		if row := strings.TrimRight(sgr.ReplaceAllString(frame[1], ""), " "); row != step.account {
			t.Errorf("%s: the account line is %q, want %q", step.name, row, step.account)
		}
		if !strings.HasPrefix(got, "CLOUD LOGGING") {
			t.Errorf("%s: the header starts %q", step.name, got[:20])
		}
		for _, want := range step.banner {
			if !strings.Contains(got, want) {
				t.Errorf("%s: %q is missing from\n%s", step.name, want, got)
			}
		}
		for _, not := range step.absent {
			if strings.Contains(got, not) {
				t.Errorf("%s: %q shows in\n%s", step.name, not, got)
			}
		}
		for _, want := range []string{" Re-check ", " From URL… ", " Add project "} {
			if !strings.Contains(sgr.ReplaceAllString(frame[0], ""), want) {
				t.Errorf("%s: the header has no %q", step.name, want)
			}
		}
	}
}

// clickOn presses the left button on the first cell of text on the home as last drawn.
func (f *cloudHomeFixture) clickOn(t *testing.T, rows, cols int, text string, nth int) {
	t.Helper()
	frame := f.h.frame(rows, cols)
	for y, row := range frame {
		plain := sgr.ReplaceAllString(row, "")
		if i := strings.Index(plain, text); i >= 0 {
			if nth > 0 {
				nth--
				continue
			}
			x := cellWidth(plain[:i]) + 1
			f.h.handleKey([]byte("\x1b[<0;" + itoa(x+1) + ";" + itoa(y+1) + "M"))
			return
		}
	}
	t.Fatalf("no %q on the home:\n%s", text, plainFrame(frame))
}

func itoa(n int) string {
	raw, _ := json.Marshal(n)
	return string(raw)
}

// The header's sheets open only when gcloud was found; the banner's buttons copy the command,
// open a terminal and re-check.
func TestCloudHomeHeaderAndBannerButtons(t *testing.T) {
	f := testCloudHome(t)
	var copied string
	saved := copyToClipboard
	copyToClipboard = func(text string) error { copied = text; return nil }
	t.Cleanup(func() { copyToClipboard = saved })

	f.h.apply(withAuth(f.h.state, cloudAuthNotInstalled, ""))
	f.clickOn(t, 30, 120, " Add project ", 0)
	f.h.handleKey([]byte("a"))
	f.h.handleKey([]byte("u"))
	if f.h.sheet != nil {
		t.Fatal("a sheet opened without gcloud")
	}
	f.clickOn(t, 30, 120, " Re-check ", 0)
	f.h.handleKey([]byte("r"))
	if got := f.fake.calls("cloud.detect"); len(got) != 2 {
		t.Errorf("Re-check made %v", f.fake.made)
	}

	f.h.apply(withAuth(cloudStateFrom(t, cloudStateJSON), cloudAuthNotAuthenticated, ""))
	f.clickOn(t, 30, 120, " Copy ", 0)
	if copied != "gcloud auth login" || f.h.toast != "Copied" {
		t.Errorf("Copy put %q on the clipboard and toasted %q", copied, f.h.toast)
	}
	f.clickOn(t, 30, 120, " Open Terminal ", 0)
	if f.terminal != 1 {
		t.Errorf("Open Terminal ran %d times", f.terminal)
	}
	f.h.openTerminal = func() error { return errors.New("no workspace") }
	f.clickOn(t, 30, 120, " Open Terminal ", 0)
	if f.h.err != "no workspace" {
		t.Errorf("a failed terminal shows %q", f.h.err)
	}
	f.clickOn(t, 30, 120, " Re-check ", 1) // the banner's
	if got := f.fake.calls("cloud.detect"); len(got) != 3 {
		t.Errorf("the banner's Re-check made %v", f.fake.made)
	}
	f.clickOn(t, 30, 120, " From URL… ", 0)
	if _, ok := f.h.sheet.(*urlSheet); !ok {
		t.Errorf("From URL… opened %T", f.h.sheet)
	}
	f.h.handleKey([]byte{0x1b})
	f.clickOn(t, 30, 120, " Add project ", 0)
	if _, ok := f.h.sheet.(*addProjectSheet); !ok {
		t.Errorf("Add project opened %T", f.h.sheet)
	}
	f.h.handleKey([]byte{0x1b})
	if f.h.sheet != nil {
		t.Error("Esc left the sheet open")
	}
	f.clickOn(t, 30, 120, "Help", 0)
	if !f.h.help {
		t.Error("Help didn't open the keys")
	}
}

// While the home shows "Not signed in" it re-checks every three seconds, one call at a time,
// and stops once signed in or away.
func TestCloudHomeAuthPolling(t *testing.T) {
	f := testCloudHome(t)
	checks := func() int { return len(f.fake.calls("cloud.refreshAuth")) }
	f.clock.advance(10 * time.Second)
	if checks() != 0 {
		t.Fatalf("a signed-in home re-checked: %v", f.fake.made)
	}
	signedOut := withAuth(f.h.state, cloudAuthNotAuthenticated, "")
	f.h.apply(signedOut)
	f.h.apply(signedOut) // a second state must not start a second timer
	f.clock.advance(2 * time.Second)
	if checks() != 0 {
		t.Fatal("re-checked before three seconds")
	}
	f.clock.advance(time.Second)
	f.clock.advance(3 * time.Second)
	if checks() != 2 {
		t.Fatalf("%d checks after six seconds", checks())
	}
	// A check still in flight holds the next one back.
	var finish func()
	f.h.spawn = func(work func() func()) { finish = work() }
	f.clock.advance(3 * time.Second)
	f.clock.advance(3 * time.Second)
	if checks() != 3 {
		t.Fatalf("%d checks with one in flight", checks())
	}
	finish()
	f.h.spawn = func(work func() func()) { work()() }
	f.clock.advance(3 * time.Second)
	if checks() != 4 {
		t.Fatalf("%d checks after the slow one answered", checks())
	}
	// A session took the pane: no checks until the home is back.
	f.h.away = true
	f.clock.advance(9 * time.Second)
	if checks() != 4 {
		t.Fatalf("%d checks while away", checks())
	}
	signedOutJSON, _ := json.Marshal(signedOut)
	f.fake.results["cloud.state"] = string(signedOutJSON)
	f.h.back()
	f.clock.advance(3 * time.Second)
	if checks() != 5 {
		t.Fatalf("%d checks after coming back", checks())
	}
	if len(f.fake.calls("cloud.state")) != 1 {
		t.Errorf("coming back didn't read the state: %v", f.fake.made)
	}
	f.h.apply(withAuth(f.h.state, cloudAuthAuthenticated, "dev@example.com"))
	f.clock.advance(9 * time.Second)
	if checks() != 5 {
		t.Fatalf("%d checks once signed in", checks())
	}
}

func TestCloudHomeProjectRows(t *testing.T) {
	f := testCloudHome(t)
	got := plainFrame(f.h.frame(40, 160))
	for _, want := range []string{
		"Production", "acme-prod · run.googleapis.com/stdout",
		"acme-staging · no log name selected",
		" Log names ", " Rename ", " Remove ", " New session ",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("%q is missing from\n%s", want, got)
		}
	}
	if strings.Contains(got, "No GCP projects yet") {
		t.Error("the empty state shows with projects")
	}

	// A new state replaces the rows, and the cursor stays on its project.
	f.h.selected = 1
	next := cloudStateFrom(t, `{"binaryPath":"/usr/bin/gcloud","authState":{"state":"authenticated","account":"a@b.c"},
		"projects":[{"projectID":"zeta","displayName":"Zeta"},{"projectID":"other"},{"projectID":"acme-staging","displayName":"Staging"}]}`)
	f.h.apply(next)
	if f.h.selected != 2 {
		t.Errorf("the cursor is on row %d", f.h.selected)
	}
	got = plainFrame(f.h.frame(40, 160))
	if !strings.Contains(got, "Zeta") || !strings.Contains(got, "Staging") || strings.Contains(got, "Production") {
		t.Errorf("the rows didn't follow the state:\n%s", got)
	}
	if !strings.Contains(got, "Signed in as a@b.c") {
		t.Errorf("the account didn't follow the state:\n%s", got)
	}

	next.Projects = nil
	f.h.apply(next)
	got = plainFrame(f.h.frame(40, 160))
	for _, want := range []string{"No GCP projects yet", "Add a project id to start streaming its Cloud Logging.", " Add project "} {
		if !strings.Contains(got, want) {
			t.Errorf("the empty state has no %q:\n%s", want, got)
		}
	}
	f.h.handleKey([]byte("\r"))
	if _, ok := f.h.sheet.(*addProjectSheet); !ok {
		t.Errorf("Enter on the empty state opened %T", f.h.sheet)
	}
}

func TestCloudHomeEvents(t *testing.T) {
	f := testCloudHome(t)
	f.h.handleEvent(event{Topic: cloudStateTopic, Data: []byte(`{"authState":{"state":"notInstalled"},"projects":[]}`)})
	if f.h.accountLine() != "gcloud CLI not found" || len(f.h.state.Projects) != 0 || f.h.available() {
		t.Errorf("the event left %+v", f.h.state)
	}
	f.fake.results["cloud.state"] = cloudStateJSON
	f.h.handleEvent(event{Topic: "events.dropped", Data: []byte(`{"topic":"devices.list","count":1}`)})
	if len(f.fake.calls("cloud.state")) != 0 {
		t.Error("another topic's drop fetched the state")
	}
	f.h.handleEvent(event{Topic: "events.dropped", Data: []byte(`{"topic":"cloud.state","count":2}`)})
	if len(f.h.state.Projects) != 3 || f.h.accountLine() != "Signed in as dev@example.com" {
		t.Errorf("a dropped state wasn't fetched: %+v", f.h.state)
	}
	f.h.handleEvent(event{Topic: cloudStateTopic, Data: []byte(`not json`)})
	if len(f.h.state.Projects) != 3 {
		t.Error("an unreadable event cleared the state")
	}
}

func TestCloudHomeRemoveTakesTwoPresses(t *testing.T) {
	f := testCloudHome(t)
	removed := func() []string { return f.fake.calls("cloud.removeProject") }
	f.h.handleKey([]byte("x"))
	if len(removed()) != 0 {
		t.Fatal("one press removed the project")
	}
	got := plainFrame(f.h.frame(40, 160))
	if !strings.Contains(got, " Confirm? ") || strings.Count(got, " Remove ") != 2 {
		t.Errorf("the armed row reads\n%s", got)
	}
	// The window lapses: the next press arms again.
	f.clock.advance(3 * time.Second)
	if got := plainFrame(f.h.frame(40, 160)); strings.Contains(got, "Confirm?") {
		t.Errorf("the button is still armed after three seconds:\n%s", got)
	}
	f.h.handleKey([]byte{0x7f})
	if len(removed()) != 0 {
		t.Fatal("a press after the window removed the project")
	}
	// Another row's button is armed on its own.
	f.h.handleKey([]byte("j"))
	f.h.handleKey([]byte("x"))
	if len(removed()) != 0 {
		t.Fatal("a press on another row confirmed the first")
	}
	f.h.handleKey([]byte("k"))
	f.clock.advance(time.Second)
	f.clickOn(t, 40, 160, " Confirm? ", 0)
	if got := removed(); len(got) != 1 || got[0] != `cloud.removeProject {"id":"acme-prod"}` {
		t.Fatalf("the second press made %v", got)
	}
	if f.h.toast != "Removed Production" {
		t.Errorf("the toast is %q", f.h.toast)
	}
	f.clock.advance(2599 * time.Millisecond)
	if f.h.toast == "" {
		t.Error("the toast went early")
	}
	f.clock.advance(time.Millisecond)
	if f.h.toast != "" {
		t.Errorf("the toast %q outlived its time", f.h.toast)
	}
}

func TestCloudHomeActions(t *testing.T) {
	f := testCloudHome(t)
	// Enter presses the action under the cursor, New session at first.
	f.h.handleKey([]byte("\r"))
	if len(f.launched) != 1 {
		t.Fatalf("Enter launched %d sessions", len(f.launched))
	}
	spec := f.launched[0]
	if spec.Config.ProjectID != "acme-prod" || spec.Config.LogName != "projects/acme-prod/logs/run.googleapis.com%2Fstdout" ||
		spec.Config.RawFilter != "" || spec.Name != "Production" || spec.AutoStart {
		t.Errorf("the session is %+v", spec)
	}
	f.h.handleKey([]byte("j"))
	f.h.handleKey([]byte("n"))
	if spec := f.launched[1]; spec.Config.ProjectID != "acme-staging" || spec.Config.LogName != "" || spec.Name != "acme-staging" {
		t.Errorf("the session is %+v", spec)
	}

	// Left and Right, Tab and Shift-Tab move over the row's actions.
	f.h.handleKey([]byte("\x1b[D"))
	if f.h.action != actionRemove {
		t.Errorf("Left chose action %d", f.h.action)
	}
	f.h.handleKey([]byte("\t"))
	f.h.handleKey([]byte("\t"))
	if f.h.action != actionLogNames {
		t.Errorf("Tab wrapped to action %d", f.h.action)
	}
	f.h.handleKey([]byte("\x1b[Z"))
	f.h.handleKey([]byte("\x1b[C"))
	f.h.handleKey([]byte("\x1b[C"))
	if f.h.action != actionRename {
		t.Errorf("the action is %d", f.h.action)
	}
	f.h.handleKey([]byte("\r"))
	rename, ok := f.h.sheet.(*renameSheet)
	if !ok || rename.projectID != "acme-staging" {
		t.Fatalf("Enter on Rename opened %T", f.h.sheet)
	}
	f.h.handleKey([]byte{0x1b})

	// A click on a button presses it on its own row.
	f.clickOn(t, 40, 160, " Log names ", 0)
	sheet, ok := f.h.sheet.(*logNameSheet)
	if !ok || sheet.project.ProjectID != "acme-prod" || f.h.selected != 0 {
		t.Fatalf("the click opened %T on row %d", f.h.sheet, f.h.selected)
	}
	f.h.handleKey([]byte{0x1b})
	if f.h.sheet != nil {
		t.Error("Esc left the log names open")
	}
	// A click on a row selects it.
	f.clickOn(t, 40, 160, "acme-staging · no log name selected", 0)
	if f.h.selected != 1 {
		t.Errorf("the click selected row %d", f.h.selected)
	}
	if !f.h.handleKey([]byte("q")) || !f.h.handleKey([]byte{0x03}) {
		t.Error("q and Ctrl-C don't quit")
	}
}

func typeText(s cloudSheet, text string) {
	for _, r := range text {
		s.key([]byte(string(r)))
	}
}

func TestCloudAddProjectSheet(t *testing.T) {
	f := testCloudHome(t)
	open := func() *addProjectSheet {
		f.clock.advance(hushTime) // past the quiet that follows a sheet closing on its result
		f.h.handleKey([]byte("a"))
		s, ok := f.h.sheet.(*addProjectSheet)
		if !ok {
			t.Fatalf("a opened %T", f.h.sheet)
		}
		return s
	}
	s := open()
	checkSheet(t, "add", s, [2]int{12, 40})
	box, _, _ := s.box(30, 100)
	got := plainFrame(box)
	for _, want := range []string{"Add a GCP project", "PROJECT ID", "my-gcp-project-123", "DISPLAY NAME (OPTIONAL)", "Production", " Add project ", escClose} {
		if !strings.Contains(got, want) {
			t.Errorf("%q is missing from\n%s", want, got)
		}
	}
	// A blank id submits nothing.
	typeText(s, "  ")
	f.h.handleKey([]byte("\r"))
	if len(f.fake.calls("cloud.addProject")) != 0 || f.h.sheet == nil {
		t.Fatal("a blank id was submitted")
	}

	// A failure stays in the sheet with its message.
	f.fake.results["cloud.addProject"] = `{"result":"failure","message":"Project not found or no access."}`
	typeText(s, "new-one")
	f.h.handleKey([]byte("\t"))
	typeText(s, "Fresh")
	f.h.handleKey([]byte("\r"))
	if got := f.fake.calls("cloud.addProject"); len(got) != 1 || got[0] != `cloud.addProject {"displayName":"Fresh","id":"new-one"}` {
		t.Fatalf("the sheet made %v", got)
	}
	if f.h.sheet == nil || s.err != "Project not found or no access." || s.validating || f.h.toast != "" {
		t.Fatalf("after a failure: err %q, toast %q", s.err, f.h.toast)
	}
	checkSheet(t, "add failed", s, [2]int{12, 40})
	if box, _, _ := s.box(30, 100); !strings.Contains(plainFrame(box), "Project not found or no access.") {
		t.Errorf("the failure isn't drawn:\n%s", plainFrame(box))
	}

	// A call that fails reads as the app's fallback.
	f.fake.errs["cloud.addProject"] = errors.New("no reply")
	f.h.handleKey([]byte("\r"))
	if s.err != "Couldn't validate the project." {
		t.Errorf("a failed call reads %q", s.err)
	}
	delete(f.fake.errs, "cloud.addProject")

	// Added closes the sheet and toasts the title.
	f.fake.results["cloud.addProject"] = `{"result":"added"}`
	f.h.handleKey([]byte("\r"))
	if f.h.sheet != nil || f.h.toast != "Added Fresh" {
		t.Fatalf("after added: sheet %T, toast %q", f.h.sheet, f.h.toast)
	}

	// Already there closes it without a toast.
	f.clock.advance(3 * time.Second)
	s = open()
	f.fake.results["cloud.addProject"] = `{"result":"alreadyExists"}`
	typeText(s, "acme-prod")
	f.h.handleKey([]byte("\r"))
	if f.h.sheet != nil || f.h.toast != "" {
		t.Fatalf("after alreadyExists: sheet %T, toast %q", f.h.sheet, f.h.toast)
	}

	// While the call is in flight the button reads Validating… and a second Enter does nothing.
	s = open()
	var finish func()
	f.h.spawn = func(work func() func()) { finish = work() }
	f.fake.results["cloud.addProject"] = `{"result":"added"}`
	typeText(s, "slow")
	f.h.handleKey([]byte("\r"))
	f.h.handleKey([]byte("\r"))
	if box, _, _ := s.box(30, 100); !strings.Contains(plainFrame(box), " Validating… ") || strings.Contains(plainFrame(box), " Add project ") {
		t.Errorf("in flight the sheet reads\n%s", plainFrame(box))
	}
	finish()
	if f.h.sheet != nil || f.h.toast != "Added slow" || len(f.fake.calls("cloud.addProject")) != 5 {
		t.Errorf("after the slow add: sheet %T, toast %q, calls %v", f.h.sheet, f.h.toast, f.fake.made)
	}
}

func TestCloudURLSheet(t *testing.T) {
	f := testCloudHome(t)
	f.h.handleKey([]byte("u"))
	s, ok := f.h.sheet.(*urlSheet)
	if !ok {
		t.Fatalf("u opened %T", f.h.sheet)
	}
	checkSheet(t, "url", s, [2]int{12, 40})
	box, _, _ := s.box(30, 100)
	got := plainFrame(box)
	for _, want := range []string{"New session from URL", "Paste a Logs Explorer URL (console.cloud.google.com/logs/query…).",
		"https://console.cloud.google.com/logs/query;query=…?project=…", " Start session "} {
		if !strings.Contains(got, want) {
			t.Errorf("%q is missing from\n%s", want, got)
		}
	}
	if strings.Contains(got, "PROJECT") || strings.Contains(got, "FILTER") {
		t.Errorf("an empty field has a preview:\n%s", got)
	}
	// No project: the button is disabled and Enter says why.
	clickSubmit := func() {
		box, top, left := s.box(30, 100)
		for y, row := range box {
			plain := sgr.ReplaceAllString(row, "")
			if i := strings.Index(plain, " Start session "); i >= 0 {
				s.mouse(mouseEvent{button: 0, x: left + cellWidth(plain[:i]) + 2, y: top + y + 1, press: true})
			}
		}
	}
	typeText(s, "https://example.com/nothing")
	clickSubmit()
	if s.err != "" || len(f.launched) != 0 {
		t.Fatalf("a disabled button acted: err %q", s.err)
	}
	f.h.handleKey([]byte("\r"))
	if s.err != "" || len(f.launched) != 0 { // Enter is as disabled as the button
		t.Fatalf("Enter without a project: err %q", s.err)
	}
	checkSheet(t, "url error", s, [2]int{12, 40})

	// A pasted URL of a known project: the preview shows, and the session opens on its filter.
	s.url.set("")
	s.key([]byte("https://console.cloud.google.com/logs/query;query=severity%3E%3DERROR%0Aresource.type%3D%22cloud_run_revision%22;duration=PT1H?project=acme-prod"))
	if s.err != "" {
		t.Errorf("typing left the error %q", s.err)
	}
	box, _, _ = s.box(30, 100)
	got = plainFrame(box)
	for _, want := range []string{"PROJECT", "acme-prod", "FILTER", "severity>=ERROR"} {
		if !strings.Contains(got, want) {
			t.Errorf("the preview has no %q:\n%s", want, got)
		}
	}
	checkSheet(t, "url preview", s, [2]int{12, 40})
	clickSubmit()
	if f.h.sheet != nil || len(f.launched) != 1 || len(f.fake.calls("cloud.addProject")) != 0 {
		t.Fatalf("a known project: sheet %T, %d sessions, calls %v", f.h.sheet, len(f.launched), f.fake.made)
	}
	spec := f.launched[0]
	if spec.Config.ProjectID != "acme-prod" || spec.Config.RawFilter != "severity>=ERROR\nresource.type=\"cloud_run_revision\"" ||
		spec.Name != "Production" || spec.AutoStart {
		t.Errorf("the session is %+v", spec)
	}

	// An unknown project is added first; a failure stays in the sheet.
	f.h.handleKey([]byte("u"))
	s = f.h.sheet.(*urlSheet)
	s.key([]byte("https://console.cloud.google.com/logs/query;query=textPayload%3A%22boom%22?project=brand-new"))
	f.fake.results["cloud.addProject"] = `{"result":"failure","message":"Project not found or no access."}`
	f.h.handleKey([]byte("\r"))
	if f.h.sheet == nil || s.err != "Project not found or no access." || s.working || len(f.launched) != 1 {
		t.Fatalf("a failed add: err %q, %d sessions", s.err, len(f.launched))
	}
	f.fake.results["cloud.addProject"] = `{"result":"added"}`
	f.h.handleKey([]byte("\r"))
	if got := f.fake.calls("cloud.addProject"); len(got) != 2 || got[1] != `cloud.addProject {"displayName":"","id":"brand-new"}` {
		t.Fatalf("the sheet made %v", got)
	}
	if f.h.sheet != nil || len(f.launched) != 2 || f.h.toast != "Added brand-new" {
		t.Fatalf("after the add: sheet %T, %d sessions, toast %q", f.h.sheet, len(f.launched), f.h.toast)
	}
	if spec := f.launched[1]; spec.Config.ProjectID != "brand-new" || spec.Config.RawFilter != `textPayload:"boom"` || spec.Name != "brand-new" {
		t.Errorf("the session is %+v", spec)
	}

	// In flight the button reads Starting….
	f.clock.advance(hushTime)
	f.h.handleKey([]byte("u"))
	s = f.h.sheet.(*urlSheet)
	s.key([]byte("https://console.cloud.google.com/logs/query?project=slow-one"))
	var finish func()
	f.h.spawn = func(work func() func()) { finish = work() }
	f.h.handleKey([]byte("\r"))
	f.h.handleKey([]byte("\r"))
	if box, _, _ := s.box(30, 100); !strings.Contains(plainFrame(box), " Starting… ") {
		t.Errorf("in flight the sheet reads\n%s", plainFrame(box))
	}
	finish()
	if len(f.launched) != 3 || len(f.fake.calls("cloud.addProject")) != 3 {
		t.Errorf("the slow start: %d sessions, calls %v", len(f.launched), f.fake.made)
	}
}

func TestCloudRenameSheet(t *testing.T) {
	f := testCloudHome(t)
	f.h.handleKey([]byte("e"))
	s, ok := f.h.sheet.(*renameSheet)
	if !ok {
		t.Fatalf("e opened %T", f.h.sheet)
	}
	checkSheet(t, "rename", s, [2]int{8, 40})
	if s.name.String() != "Production" {
		t.Errorf("the field starts as %q", s.name.String())
	}
	s.name.set("")
	box, _, _ := s.box(30, 100)
	for _, want := range []string{"Rename project", "Display name", " Save ", " Cancel "} {
		if !strings.Contains(plainFrame(box), want) {
			t.Errorf("%q is missing from\n%s", want, plainFrame(box))
		}
	}
	typeText(s, "Prod EU")
	f.h.handleKey([]byte("\r"))
	if got := f.fake.calls("cloud.setDisplayName"); len(got) != 1 || got[0] != `cloud.setDisplayName {"id":"acme-prod","name":"Prod EU"}` {
		t.Fatalf("Save made %v", got)
	}
	if f.h.sheet != nil || f.h.toast != "Renamed" {
		t.Errorf("after Save: sheet %T, toast %q", f.h.sheet, f.h.toast)
	}
	// Cancel changes nothing.
	f.clock.advance(3 * time.Second)
	f.h.handleKey([]byte("e"))
	f.h.handleKey([]byte("\t"))
	f.h.handleKey([]byte("\t"))
	f.h.handleKey([]byte("\r"))
	if f.h.sheet != nil || f.h.toast != "" || len(f.fake.calls("cloud.setDisplayName")) != 1 {
		t.Errorf("after Cancel: sheet %T, toast %q, calls %v", f.h.sheet, f.h.toast, f.fake.made)
	}
}

type logNameFixture struct {
	s       *logNameSheet
	fake    *cloudFake
	closed  int
	flashed []string
}

// testLogNameSheet is a sheet over n cached names, the third of them selected.
func testLogNameSheet(n int) *logNameFixture {
	calls, _, fake := testCloudCalls()
	p := cloudProject{ProjectID: "acme-prod", DisplayName: "Production"}
	for i := 0; i < n; i++ {
		p.LogNames = append(p.LogNames, cloudLogNameFull("acme-prod", "svc/log-"+string(rune('a'+i))))
	}
	if n > 2 {
		p.SelectedLogName = p.LogNames[2]
	}
	f := &logNameFixture{fake: fake}
	f.s = newLogNameSheetWith(calls, p, func() { f.closed++ })
	f.s.flash = func(msg string) { f.flashed = append(f.flashed, msg) }
	f.s.opened()
	return f
}

func TestCloudLogNameSheetDraws(t *testing.T) {
	for _, n := range []int{0, 3, 8, 9, 40} {
		f := testLogNameSheet(n)
		checkSheet(t, "log names", f.s, [2]int{14, 40})
		box, _, _ := f.s.box(40, 100)
		got := plainFrame(box)
		for _, want := range []string{"Log names", "Selecting a log name applies it to every session for Production.",
			"Add a log name manually (e.g. stdout)…", " Add ", " Refresh ", " Done ", escClose} {
			if !strings.Contains(got, want) {
				t.Errorf("%d names: %q is missing from\n%s", n, want, got)
			}
		}
		if has := strings.Contains(got, "Filter…"); has != (n > 8) {
			t.Errorf("%d names: the filter field shows: %v", n, has)
		}
		if n > 2 && (!strings.Contains(got, "● svc/log-c") || !strings.Contains(got, "○ svc/log-a")) {
			t.Errorf("%d names: the list reads\n%s", n, got)
		}
		if n > 2 && f.s.cursor != 2 {
			t.Errorf("%d names: the cursor starts on row %d", n, f.s.cursor)
		}
	}
	// A long project title keeps the caption to two rows.
	f := testLogNameSheet(3)
	f.s.project.DisplayName = strings.Repeat("a very long project title ", 12)
	checkSheet(t, "log names, long title", f.s, [2]int{14, 40})
}

func TestCloudLogNameSheetRefresh(t *testing.T) {
	// An empty cache refreshes on open; a cached list doesn't.
	f := testLogNameSheet(0)
	if got := f.fake.calls("cloud.refreshLogNames"); len(got) != 1 || got[0] != `cloud.refreshLogNames {"id":"acme-prod"}` {
		t.Fatalf("opening made %v", f.fake.made)
	}
	box, _, _ := f.s.box(40, 100)
	if !strings.Contains(plainFrame(box), "No log names found yet — Refresh, or add one manually above.") {
		t.Errorf("the empty list reads\n%s", plainFrame(box))
	}
	if cached := testLogNameSheet(3); len(cached.fake.made) != 0 {
		t.Errorf("a cached list made %v", cached.fake.made)
	}

	// While it refreshes the empty list says so, and a second refresh waits.
	var finish func()
	f.s.calls.spawn = func(work func() func()) { finish = work() }
	f.fake.results["cloud.refreshLogNames"] = `"Couldn't list logs."`
	f.s.refresh()
	f.s.refresh()
	if len(f.fake.calls("cloud.refreshLogNames")) != 2 {
		t.Fatalf("two refreshes at once: %v", f.fake.made)
	}
	box, _, _ = f.s.box(40, 100)
	if !strings.Contains(plainFrame(box), "Loading log names…") {
		t.Errorf("while refreshing the list reads\n%s", plainFrame(box))
	}
	finish()
	if f.s.refreshing || len(f.flashed) != 1 || f.flashed[0] != "Couldn't list logs." {
		t.Errorf("a failed refresh flashed %v", f.flashed)
	}
	// A refresh that worked says nothing; the names arrive with the state.
	f.s.calls.spawn = func(work func() func()) { work()() }
	f.fake.results["cloud.refreshLogNames"] = `null`
	f.s.focus = lnRefresh
	f.s.key([]byte("\r"))
	if len(f.flashed) != 1 || len(f.fake.calls("cloud.refreshLogNames")) != 3 {
		t.Errorf("a refresh that worked flashed %v, calls %v", f.flashed, f.fake.made)
	}
	p := f.s.project
	p.LogNames = []string{"projects/acme-prod/logs/stderr", "projects/acme-prod/logs/stdout"}
	f.s.setProject(p)
	box, _, _ = f.s.box(40, 100)
	if got := plainFrame(box); !strings.Contains(got, "○ stderr") || !strings.Contains(got, "○ stdout") {
		t.Errorf("the new names aren't listed:\n%s", got)
	}
}

func TestCloudLogNameSheetFilter(t *testing.T) {
	f := testLogNameSheet(12)
	f.s.focus = lnFilter
	typeText(f.s, "LOG-K")
	if got := f.s.filtered(); len(got) != 1 || cloudLogID(got[0]) != "svc/log-k" {
		t.Fatalf("the filter leaves %v", got)
	}
	box, _, _ := f.s.box(40, 100)
	if got := plainFrame(box); strings.Contains(got, "svc/log-a") || !strings.Contains(got, "svc/log-k") {
		t.Errorf("the filtered list reads\n%s", got)
	}
	// Enter in the filter chooses the row under the cursor.
	f.s.key([]byte("\r"))
	if got := f.fake.calls("cloud.setSelectedLogName"); len(got) != 1 || got[0] != `cloud.setSelectedLogName {"id":"acme-prod","logName":"projects/acme-prod/logs/svc%2Flog-k"}` {
		t.Fatalf("choosing made %v", f.fake.made)
	}
	if f.closed != 1 {
		t.Errorf("choosing closed the sheet %d times", f.closed)
	}
	// A filter that matches nothing keeps the field, so it can be cleared.
	f = testLogNameSheet(12)
	f.s.focus = lnFilter
	typeText(f.s, "zzz")
	box, _, _ = f.s.box(40, 100)
	if got := plainFrame(box); !strings.Contains(got, "zzz") || !strings.Contains(got, "No log names found yet") {
		t.Errorf("a filter with no match reads\n%s", got)
	}
	f.s.key([]byte("\r"))
	if len(f.fake.made) != 0 || f.closed != 0 {
		t.Errorf("Enter on an empty list made %v", f.fake.made)
	}
	// With eight names or fewer the filter is not a stop.
	for _, stop := range testLogNameSheet(8).s.stops() {
		if stop == lnFilter {
			t.Error("eight names have a filter stop")
		}
	}
}

func TestCloudLogNameSheetSelection(t *testing.T) {
	f := testLogNameSheet(5)
	if f.s.focus != lnList {
		t.Fatalf("a cached list opens on %q", f.s.focus)
	}
	f.s.key([]byte("\x1b[B"))
	f.s.key([]byte("j"))
	f.s.key([]byte("\x1b[A"))
	f.s.key([]byte("\r"))
	if got := f.fake.calls("cloud.setSelectedLogName"); len(got) != 1 || got[0] != `cloud.setSelectedLogName {"id":"acme-prod","logName":"projects/acme-prod/logs/svc%2Flog-d"}` {
		t.Fatalf("Enter made %v", f.fake.made)
	}
	if f.closed != 1 {
		t.Errorf("Enter closed the sheet %d times", f.closed)
	}

	// A click on a name chooses it; one on Done or the close button only closes.
	f = testLogNameSheet(5)
	click := func(text string) {
		t.Helper()
		box, top, left := f.s.box(40, 100)
		for y, row := range box {
			plain := sgr.ReplaceAllString(row, "")
			if i := strings.Index(plain, text); i >= 0 {
				f.s.mouse(mouseEvent{button: 0, x: left + cellWidth(plain[:i]) + 2, y: top + y + 1, press: true})
				return
			}
		}
		t.Fatalf("no %q in\n%s", text, plainFrame(box))
	}
	click("svc/log-a")
	if got := f.fake.calls("cloud.setSelectedLogName"); len(got) != 1 || !strings.HasSuffix(got[0], `svc%2Flog-a"}`) || f.closed != 1 {
		t.Fatalf("the click made %v and closed %d times", f.fake.made, f.closed)
	}
	click(" Done ")
	click(escClose)
	f.s.key([]byte{0x1b})
	if f.closed != 4 || len(f.fake.made) != 1 {
		t.Errorf("closing: %d times, calls %v", f.closed, f.fake.made)
	}
	click(" Refresh ")
	if len(f.fake.calls("cloud.refreshLogNames")) != 1 {
		t.Errorf("Refresh made %v", f.fake.made)
	}
	// A failed selection is reported.
	f.fake.errs["cloud.setSelectedLogName"] = errors.New("jacad closed the connection")
	click("svc/log-b")
	if len(f.flashed) != 1 || f.flashed[0] != "jacad closed the connection" {
		t.Errorf("a failed selection flashed %v", f.flashed)
	}
	// The wheel moves the cursor.
	f.s.cursor = 0
	f.s.mouse(mouseEvent{button: 65, x: 1, y: 1, press: true})
	if f.s.cursor != 1 {
		t.Errorf("the wheel left the cursor on row %d", f.s.cursor)
	}
}

func TestCloudLogNameSheetManualAdd(t *testing.T) {
	f := testLogNameSheet(3)
	// Add is disabled, and Enter does nothing, while the field is blank.
	f.s.focus = lnManual
	typeText(f.s, "  ")
	f.s.key([]byte("\r"))
	if len(f.fake.made) != 0 || f.closed != 0 {
		t.Fatalf("a blank name made %v", f.fake.made)
	}
	f.s.manual.set("")
	typeText(f.s, " run.googleapis.com/requests ")
	f.s.key([]byte("\r"))
	want := []string{
		`cloud.setLogNames {"id":"acme-prod","names":["projects/acme-prod/logs/run.googleapis.com%2Frequests","projects/acme-prod/logs/svc%2Flog-a","projects/acme-prod/logs/svc%2Flog-b","projects/acme-prod/logs/svc%2Flog-c"]}`,
		`cloud.setSelectedLogName {"id":"acme-prod","logName":"projects/acme-prod/logs/run.googleapis.com%2Frequests"}`,
	}
	if len(f.fake.made) != 2 || f.fake.made[0] != want[0] || f.fake.made[1] != want[1] {
		t.Fatalf("adding made\n%s", strings.Join(f.fake.made, "\n"))
	}
	if f.closed != 1 || f.s.manual.String() != "" {
		t.Errorf("after adding: closed %d times, field %q", f.closed, f.s.manual.String())
	}

	// A name already cached is only selected; a full name is taken as it is.
	f = testLogNameSheet(3)
	f.s.focus = lnManual
	typeText(f.s, "svc/log-b")
	f.s.focus = lnAdd
	f.s.key([]byte("\r"))
	if len(f.fake.made) != 1 || f.fake.made[0] != `cloud.setSelectedLogName {"id":"acme-prod","logName":"projects/acme-prod/logs/svc%2Flog-b"}` {
		t.Fatalf("a cached name made %v", f.fake.made)
	}
	f = testLogNameSheet(0)
	f.fake.made = nil
	typeText(f.s, "projects/other/logs/syslog")
	f.s.key([]byte("\r"))
	if len(f.fake.made) != 2 || f.fake.made[1] != `cloud.setSelectedLogName {"id":"acme-prod","logName":"projects/other/logs/syslog"}` {
		t.Fatalf("a full name made %v", f.fake.made)
	}
}

// The home keeps an open log-name sheet on its project's state, toasts its failures, and closes
// it when the project goes.
func TestCloudHomeLogNameSheet(t *testing.T) {
	f := testCloudHome(t)
	f.h.handleKey([]byte("j"))
	f.fake.results["cloud.refreshLogNames"] = `"Couldn't list logs."`
	f.h.handleKey([]byte("l"))
	s, ok := f.h.sheet.(*logNameSheet)
	if !ok || s.project.ProjectID != "acme-staging" {
		t.Fatalf("l opened %T", f.h.sheet)
	}
	if len(f.fake.calls("cloud.refreshLogNames")) != 1 || f.h.toast != "Couldn't list logs." {
		t.Errorf("opening an empty project: calls %v, toast %q", f.fake.made, f.h.toast)
	}
	for _, size := range cloudSizes {
		checkFrame(t, "home under a sheet", f.h.frame(size[0], size[1]), size[0], size[1])
	}
	next := f.h.state
	next.Projects = append([]cloudProject(nil), next.Projects...)
	next.Projects[1].LogNames = []string{"projects/acme-staging/logs/stdout"}
	f.h.apply(next)
	if len(s.project.LogNames) != 1 {
		t.Error("the sheet didn't follow the state")
	}
	typeText(s, "x") // goes to the sheet's field, not the home's Remove
	if len(f.h.armed) != 0 || s.manual.String() != "x" {
		t.Errorf("a key under a sheet reached the home: armed %v, field %q", f.h.armed, s.manual.String())
	}
	next.Projects = next.Projects[:1]
	f.h.apply(next)
	if f.h.sheet != nil {
		t.Error("the sheet outlived its project")
	}
}

func TestNewTabPane(t *testing.T) {
	id, err := newTabPane([]byte(`{"id":"1","result":{"tab":{"tab_id":"w1:t4"},"root_pane":{"pane_id":"w1:p9"}}}`), nil)
	if err != nil || id != "w1:p9" {
		t.Errorf("got %q, %v", id, err)
	}
	if _, err := newTabPane([]byte(`{"error":{"message":"unknown workspace"}}`), errors.New("exit status 1")); err == nil || err.Error() != "unknown workspace" {
		t.Errorf("an error reply reads %v", err)
	}
	if _, err := newTabPane([]byte("herdr: not running\nmore"), errors.New("exit status 1")); err == nil || err.Error() != "herdr: not running" {
		t.Errorf("a failed run reads %v", err)
	}
	if _, err := newTabPane(nil, errors.New("exec: herdr not found")); err == nil || err.Error() != "exec: herdr not found" {
		t.Errorf("a missing binary reads %v", err)
	}
	if _, err := newTabPane([]byte(`{"result":{}}`), nil); err == nil {
		t.Error("a reply without a pane is not an error")
	}
}

// The sign-in checks stop a while after the last key, and a key starts them again.
func TestCloudHomeAuthPollingStopsWhenIdle(t *testing.T) {
	f := testCloudHome(t)
	checks := func() int { return len(f.fake.calls("cloud.refreshAuth")) }
	f.h.apply(withAuth(f.h.state, cloudAuthNotAuthenticated, ""))
	f.clock.advance(authPollFor + time.Minute)
	idle := checks()
	if idle == 0 {
		t.Fatal("no checks while signed out")
	}
	f.clock.advance(time.Hour)
	if checks() != idle {
		t.Fatalf("%d checks went on after the home was left idle", checks()-idle)
	}
	f.h.handleKey([]byte("j"))
	f.clock.advance(authPollEvery)
	if checks() != idle+1 {
		t.Fatalf("a key did not start the checks again: %d", checks()-idle)
	}
}
