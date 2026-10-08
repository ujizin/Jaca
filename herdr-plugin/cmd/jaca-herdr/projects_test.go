package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// projectsStateJSON is a projects.state as jacad sends it, with fields left out the way an
// older daemon would: a container folder with two repos under it, a folder the user added,
// and a repo outside the home directory with no source.
const projectsStateJSON = `{"projects":[
	{"path":"/Users/dev/workspace","exists":true,"isGitRepo":false,"source":"claude","sessionCount":3,
	 "lastActive":"2026-10-07T11:00:00.123Z","checkouts":[]},
	{"path":"/Users/dev/workspace/jaca","exists":true,"isGitRepo":true,"source":"claude","sessionCount":0,
	 "lastActive":"2026-10-07T10:00:00.000Z","checkouts":[
		{"path":"/Users/dev/workspace/jaca","isMain":true,"branch":"main","age":"2 hours ago","exists":true,
		 "sizeMB":2048,"cacheMB":512,"sizeComputed":true,"lastCommit":"2026-10-07T09:00:00.000Z",
		 "orphan":false,"isClaudeManaged":false,"hasClaudeSessions":false,"claudeSessionCount":0,
		 "cleaning":false,"dropped":false,"removing":false},
		{"path":"/Users/dev/workspace/jaca/.claude/worktrees/fix-login","isMain":false,"branch":"fix-login",
		 "base":"main","age":"3 days ago","isClaudeManaged":true,"hasClaudeSessions":true,
		 "sizeMB":300,"cacheMB":100,"sizeComputed":true},
		{"path":"/Users/dev/workspace/jaca/.claude/worktrees/old","orphan":true,"orphanKind":{"detached":{}},
		 "isClaudeManaged":true,"claudeLastActive":"2026-10-07T11:57:00.000Z"},
		{"path":"/Users/dev/workspace/jaca/.claude/worktrees/gone","branch":"gone-branch","orphan":true,
		 "orphanKind":{"deleted":{}}}]},
	{"path":"/Users/dev/workspace/site","isGitRepo":true,"source":"claude","checkouts":[
		{"path":"/Users/dev/workspace/site","isMain":true,"sizeMB":1500,"sizeComputed":true}]},
	{"path":"/Users/dev/notes","source":"user","checkouts":[]},
	{"path":"/opt/tools","isGitRepo":true,"checkouts":[{"path":"/opt/tools","isMain":true,"branch":"trunk"}]}],
	"isRefreshing":false,"isComputingSizes":false,"hasCompletedScan":true,
	"lastRefresh":"2026-10-07T11:59:50.500Z","scanGeneration":4}`

const (
	jacaPath     = "/Users/dev/workspace/jaca"
	fixLoginPath = "/Users/dev/workspace/jaca/.claude/worktrees/fix-login"
)

// herdrFake stands in for the herdr CLI: it records each command and answers with reply.
type herdrFake struct {
	calls [][]string
	reply func(args []string) (herdrResult, error)
}

func (h *herdrFake) run(args ...string) (herdrResult, error) {
	h.calls = append(h.calls, args)
	if h.reply == nil {
		return herdrResult{}, errors.New("no herdr in this test")
	}
	return h.reply(args)
}

type projectsFixture struct {
	v     *projectsViewer
	clock *toolClock
	fake  *cloudFake
	herdr *herdrFake

	defaults map[string]string // what the app's settings hold
	written  []string          // each write, as key=value
	missing  map[string]bool   // the paths that are not on disk
	opened   []string          // the folders opened in Finder
	zed      []string          // the folders opened in Zed
	copied   []string
	chosen   string // what the folder chooser returns
	chooses  int
}

// newProjectsFixture is a viewer that has no state yet, on a pane that has quit, with
// everything that leaves the pane (settings, Herdr, Finder, Zed, the chooser, the clipboard)
// replaced.
func newProjectsFixture(t *testing.T) *projectsFixture {
	t.Helper()
	f := &projectsFixture{herdr: &herdrFake{}, defaults: map[string]string{}, missing: map[string]bool{}}
	savedHome, savedRead, savedWrite, savedHerdr, savedWait := homeDir, readDefault, writeDefault, herdrRun, herdrPaneWait
	savedExists, savedOpen, savedDetect, savedZed, savedChoose, savedCopy := pathExists, openFolder, detectZed, launchZed, chooseProjectFolder, copyToClipboard
	t.Cleanup(func() {
		homeDir, readDefault, writeDefault, herdrRun, herdrPaneWait = savedHome, savedRead, savedWrite, savedHerdr, savedWait
		pathExists, openFolder, detectZed, launchZed, chooseProjectFolder, copyToClipboard = savedExists, savedOpen, savedDetect, savedZed, savedChoose, savedCopy
	})
	homeDir = "/Users/dev"
	readDefault = func(key string) (string, bool, bool) { value, ok := f.defaults[key]; return value, ok, true }
	writeDefault = func(key, value string) error {
		f.defaults[key] = value
		f.written = append(f.written, key+"="+value)
		return nil
	}
	herdrRun, herdrPaneWait = f.herdr.run, 0
	pathExists = func(path string) bool { return !f.missing[path] }
	openFolder = func(path string) error { f.opened = append(f.opened, path); return nil }
	detectZed = func() string { return "/Applications/Zed.app" }
	launchZed = func(app, folder string) error { f.zed = append(f.zed, app+" "+folder); return nil }
	chooseProjectFolder = func() (string, error) { f.chooses++; return f.chosen, nil }
	copyToClipboard = func(text string) error { f.copied = append(f.copied, text); return nil }

	quit := &pane{work: make(chan func()), done: make(chan struct{})}
	close(quit.done)
	calls, clock, fake := testCloudCalls()
	f.clock, f.fake = clock, fake
	f.v = buildProjectsViewer(quit, calls)
	f.v.post = func(run func()) { run() }
	f.v.herdr = true
	f.v.loadSettings()
	f.v.open()
	return f
}

// testProjects is a viewer holding projectsStateJSON, as after its first draw.
func testProjects(t *testing.T) *projectsFixture {
	t.Helper()
	f := newProjectsFixture(t)
	f.state(t, projectsStateJSON)
	f.v.moved = false
	return f
}

func (f *projectsFixture) state(t *testing.T, raw string) {
	t.Helper()
	st, err := decodeProjectsState([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	f.v.apply(st)
}

// with returns projectsStateJSON with one piece of text replaced.
func stateWith(t *testing.T, old, new string) string {
	t.Helper()
	if !strings.Contains(projectsStateJSON, old) {
		t.Fatalf("the state has no %q", old)
	}
	return strings.Replace(projectsStateJSON, old, new, 1)
}

// open expands the projects at these paths.
func (f *projectsFixture) open(paths ...string) {
	for _, path := range paths {
		f.v.expanded[path] = true
	}
	f.v.relayout()
}

// selectRow puts the cursor on a project (checkout "") or a checkout.
func (f *projectsFixture) selectRow(t *testing.T, project, checkout string) {
	t.Helper()
	key := projectKey(project)
	if checkout != "" {
		key = checkoutKey(project, checkout)
	}
	for i, row := range f.v.rows {
		if row.key == key {
			f.v.selected = i
			return
		}
	}
	t.Fatalf("no row %q among %q", key, f.v.rowKeys())
}

func (f *projectsFixture) names() []string {
	var names []string
	for _, row := range f.v.rows {
		if row.isCheckout {
			names = append(names, "  "+row.checkout.name())
		} else {
			names = append(names, row.project.name())
		}
	}
	return names
}

func (f *projectsFixture) text(rows, cols int) string { return plainFrame(f.v.frame(rows, cols)) }

// find is the cell (1-based) of the nth row of the frame holding text.
func (f *projectsFixture) find(t *testing.T, rows, cols int, text string, nth int) (x, y int) {
	t.Helper()
	frame := f.v.frame(rows, cols)
	for i, row := range frame {
		plain := sgr.ReplaceAllString(row, "")
		if at := strings.Index(plain, text); at >= 0 {
			if nth > 0 {
				nth--
				continue
			}
			return cellWidth(plain[:at]) + 2, i + 1
		}
	}
	t.Fatalf("no %q in the pane:\n%s", text, plainFrame(frame))
	return 0, 0
}

func (f *projectsFixture) clickOn(t *testing.T, rows, cols int, text string, nth int) {
	t.Helper()
	x, y := f.find(t, rows, cols, text, nth)
	f.v.handleKey([]byte("\x1b[<0;" + itoa(x) + ";" + itoa(y) + "M"))
}

// clickSheet clicks the nth row holding text in the open sheet, drawn in a pane rows by cols.
func (f *projectsFixture) clickSheet(t *testing.T, rows, cols int, text string, nth int) {
	t.Helper()
	box, top, left := f.v.sheet.box(rows, cols)
	for i, row := range box {
		plain := sgr.ReplaceAllString(row, "")
		if at := strings.Index(plain, text); at >= 0 {
			if nth > 0 {
				nth--
				continue
			}
			f.v.handleKey([]byte("\x1b[<0;" + itoa(left+cellWidth(plain[:at])+2) + ";" + itoa(top+i+1) + "M"))
			return
		}
	}
	t.Fatalf("no %q in the sheet:\n%s", text, plainFrame(box))
}

func (f *projectsFixture) sheetText(t *testing.T) string {
	t.Helper()
	if f.v.sheet == nil {
		t.Fatal("no sheet is open")
	}
	box, _, _ := f.v.sheet.box(40, 120)
	return plainFrame(box)
}

func (f *projectsFixture) keys(keys ...string) {
	for _, k := range keys {
		f.v.handleKey([]byte(k))
	}
}

// typeText sends text a character at a time, as typed.
func (f *projectsFixture) typeText(text string) {
	for _, r := range text {
		f.v.handleKey([]byte(string(r)))
	}
}

func wantContains(t *testing.T, text string, parts ...string) {
	t.Helper()
	for _, part := range parts {
		if !strings.Contains(text, part) {
			t.Errorf("no %q in:\n%s", part, text)
		}
	}
}

func wantLacks(t *testing.T, text string, parts ...string) {
	t.Helper()
	for _, part := range parts {
		if strings.Contains(text, part) {
			t.Errorf("%q is in:\n%s", part, text)
		}
	}
}

func TestProjectsDecode(t *testing.T) {
	st, err := decodeProjectsState([]byte(projectsStateJSON))
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Projects) != 5 || !st.HasCompletedScan || st.IsRefreshing || st.ScanGeneration != 4 {
		t.Fatalf("decoded %+v", st)
	}
	if want := time.Date(2026, 10, 7, 11, 59, 50, 500e6, time.UTC); !st.LastRefresh.Equal(want) {
		t.Errorf("lastRefresh is %v", st.LastRefresh)
	}
	jaca := st.Projects[1]
	if jaca.Path != jacaPath || !jaca.IsGitRepo || jaca.Source != "claude" || len(jaca.Checkouts) != 4 {
		t.Fatalf("jaca is %+v", jaca)
	}
	main, fix, old, gone := jaca.Checkouts[0], jaca.Checkouts[1], jaca.Checkouts[2], jaca.Checkouts[3]
	if !main.IsMain || main.Branch != "main" || main.SizeMB != 2048 || main.CacheMB != 512 || !main.SizeComputed || main.LastCommit.IsZero() {
		t.Errorf("the main checkout is %+v", main)
	}
	if fix.IsMain || fix.Base != "main" || fix.Age != "3 days ago" || !fix.HasClaudeSessions || !fix.IsClaudeManaged {
		t.Errorf("the worktree is %+v", fix)
	}
	// Fields jacad left out read as their defaults.
	if old.Branch != "" || old.SizeComputed || old.Cleaning || old.ClaudeLastActive.IsZero() || !old.LastCommit.IsZero() {
		t.Errorf("the orphan is %+v", old)
	}
	if !old.Orphan || !old.detached() || !gone.Orphan || gone.detached() {
		t.Errorf("orphan kinds: old detached %v, gone detached %v", old.detached(), gone.detached())
	}
	if tools := st.Projects[4]; tools.Source != "" || !tools.isUser() || tools.LastActive.IsZero() == false {
		t.Errorf("tools is %+v", tools)
	}

	// The enum's other possible encoding, and no kind at all.
	st, err = decodeProjectsState([]byte(`{"projects":[{"path":"/r","checkouts":[
		{"path":"/r/a","orphan":true,"orphanKind":"detached"},
		{"path":"/r/b","orphan":true,"orphanKind":"deleted"},
		{"path":"/r/c","orphan":true},
		{"path":"/r/d","orphan":true,"orphanKind":null}]}]}`))
	if err != nil || len(st.Projects) != 1 || len(st.Projects[0].Checkouts) != 4 {
		t.Fatalf("decoded %+v, %v", st, err)
	}
	var detached []bool
	for _, c := range st.Projects[0].Checkouts {
		detached = append(detached, c.detached())
	}
	if !reflect.DeepEqual(detached, []bool{true, false, false, false}) {
		t.Errorf("detached is %v", detached)
	}
	if !st.LastRefresh.IsZero() || st.HasCompletedScan {
		t.Errorf("a state with only projects decoded as %+v", st)
	}

	// A record without a path is left out; a field of another type is skipped, not fatal.
	st, err = decodeProjectsState([]byte(`{"projects":[{"isGitRepo":true},"text",
		{"path":"/ok","sessionCount":"many","lastActive":12,"checkouts":[{"isMain":true},{"path":"/ok","sizeMB":"big","isMain":true}]}],
		"scanGeneration":"x","lastRefresh":"yesterday"}`))
	if err != nil || len(st.Projects) != 1 || st.Projects[0].Path != "/ok" {
		t.Fatalf("decoded %+v, %v", st, err)
	}
	if cs := st.Projects[0].Checkouts; len(cs) != 1 || !cs[0].IsMain || cs[0].SizeMB != 0 {
		t.Errorf("checkouts are %+v", cs)
	}
	if _, err := decodeProjectsState([]byte(`not json`)); err == nil {
		t.Error("text that is not JSON decoded")
	}
	if st, err := decodeProjectsState([]byte(`{}`)); err != nil || len(st.Projects) != 0 {
		t.Errorf("an empty state decoded as %+v, %v", st, err)
	}
}

func TestProjectTree(t *testing.T) {
	at := func(hour int) wireTime { return wireTime{time.Date(2026, 10, 7, hour, 0, 0, 0, time.UTC)} }
	projects := []projectRow{
		{Path: "/w/zeta"},
		{Path: "/w", LastActive: at(1)},
		{Path: "/w/repo/sub", LastActive: at(9)},
		{Path: "/w/repo", LastActive: at(2)},
		{Path: "/w/alpha"},
		{Path: "/w2", LastActive: at(5)}, // shares a prefix with /w, but is not under it
		{Path: "/solo", Checkouts: []checkoutRow{{Path: "/solo", LastCommit: at(8)}}},
		{Path: "/w/same", LastActive: at(2)},
	}
	var walk func(nodes []projectNode, depth int) []string
	walk = func(nodes []projectNode, depth int) []string {
		var out []string
		for _, n := range nodes {
			out = append(out, strings.Repeat(">", depth)+n.project.Path)
			out = append(out, walk(n.children, depth+1)...)
		}
		return out
	}
	// Each level: the latest first, equal dates by name, then the undated by name. /w/repo/sub
	// goes under its longest ancestor, /w/repo.
	want := []string{"/solo", "/w2", "/w", ">/w/repo", ">>/w/repo/sub", ">/w/same", ">/w/alpha", ">/w/zeta"}
	if got := walk(projectTree(projects), 0); !reflect.DeepEqual(got, want) {
		t.Errorf("tree is\n%q, want\n%q", got, want)
	}
	// The List view keeps jacad's order and nests nothing.
	flat := projectFlat(projects)
	for i, n := range flat {
		if n.project.Path != projects[i].Path || len(n.children) != 0 {
			t.Errorf("flat node %d is %+v", i, n)
		}
	}
	if got := projectTree(nil); len(got) != 0 {
		t.Errorf("no projects gave %v", got)
	}
}

func TestProjectsDerivedValues(t *testing.T) {
	f := testProjects(t)
	st := f.v.state
	if st.totalWorktrees() != 3 || st.sizableCheckouts() != 6 {
		t.Errorf("%d worktrees, %d sizable checkouts", st.totalWorktrees(), st.sizableCheckouts())
	}
	byName := map[string]projectRow{}
	for _, p := range st.Projects {
		byName[p.name()] = p
	}
	claude := map[string]bool{}
	sized := map[string]bool{}
	for name, p := range byName {
		claude[name], sized[name] = p.isClaudeProject(), p.sizesComputed()
	}
	if want := map[string]bool{"workspace": true, "jaca": true, "site": true, "notes": false, "tools": false}; !reflect.DeepEqual(claude, want) {
		t.Errorf("isClaudeProject is %v", claude)
	}
	// Every checkout has a size, and there is at least one.
	if want := map[string]bool{"workspace": false, "jaca": false, "site": true, "notes": false, "tools": false}; !reflect.DeepEqual(sized, want) {
		t.Errorf("sizesComputed is %v", sized)
	}
	// A user folder is a Claude project through its sessions or a checkout's.
	for _, p := range []projectRow{
		{Path: "/a", Source: "user", SessionCount: 1},
		{Path: "/a", Source: "user", Checkouts: []checkoutRow{{Path: "/a/w", IsClaudeManaged: true}}},
		{Path: "/a", Checkouts: []checkoutRow{{Path: "/a/w", HasClaudeSessions: true}}},
	} {
		if !p.isClaudeProject() {
			t.Errorf("%+v is not a Claude project", p)
		}
	}
	jaca := byName["jaca"]
	if jaca.worktreeCount() != 3 || jaca.totalSizeMB() != 2348 || jaca.displayPath() != "~/workspace/jaca" {
		t.Errorf("jaca: %d worktrees, %d MB, %q", jaca.worktreeCount(), jaca.totalSizeMB(), jaca.displayPath())
	}
	if got := byName["tools"].displayPath(); got != "/opt/tools" {
		t.Errorf("a path outside the home directory reads %q", got)
	}
	if tildePath("/Users/dev") != "~" || tildePath("/Users/developer/x") != "/Users/developer/x" {
		t.Errorf("tildePath: %q, %q", tildePath("/Users/dev"), tildePath("/Users/developer/x"))
	}
	names := []string{jaca.Checkouts[0].name(), jaca.Checkouts[2].name(), checkoutRow{Path: "/r", IsMain: true}.name(), projectRow{Path: "/"}.name()}
	if want := []string{"main", "old", "main checkout", "/"}; !reflect.DeepEqual(names, want) {
		t.Errorf("names are %q", names)
	}
	if worktreesTag(1) != "1 worktree" || worktreesTag(4) != "4 worktrees" || projectsTag(1) != "1 project" || projectsTag(2) != "2 projects" {
		t.Error("the count tags")
	}
}

func TestRelativeAge(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		ago  time.Duration
		want string
	}{
		{5 * time.Second, "5 sec ago"},
		{3 * time.Minute, "3 min ago"},
		{2 * time.Hour, "2 hr ago"},
		{26 * time.Hour, "1 day ago"},
		{4 * 24 * time.Hour, "4 days ago"},
		{15 * 24 * time.Hour, "2 wk ago"},
		{95 * 24 * time.Hour, "3 mo ago"},
		{400 * 24 * time.Hour, "1 yr ago"},
		{-3 * time.Minute, "in 3 min"},
	} {
		if got := relativeAge(now.Add(-c.ago), now); got != c.want {
			t.Errorf("%v ago reads %q, want %q", c.ago, got, c.want)
		}
	}
}

// The three states of ProjectsAreaView.content, with the app's text.
func TestProjectsStates(t *testing.T) {
	f := newProjectsFixture(t)
	if got := strings.TrimSpace(strings.ReplaceAll(f.text(24, 80), "Help", "")); got != "" {
		t.Errorf("before a state arrives the pane shows %q", got)
	}

	// The first state has no scan behind it: the pane asks for one and shows the loader.
	f.state(t, `{"projects":[],"isRefreshing":false,"hasCompletedScan":false,"scanGeneration":0}`)
	if got := f.fake.calls("projects.refresh"); len(got) != 1 {
		t.Fatalf("the first state made %v", f.fake.made)
	}
	text := f.text(24, 80)
	wantContains(t, text, "Scanning ~/.claude/projects…")
	wantLacks(t, text, "PROJECTS", "No projects found")

	f.state(t, `{"projects":[],"isRefreshing":false,"hasCompletedScan":true,"scanGeneration":1,"lastRefresh":"2026-10-07T12:00:00.000Z"}`)
	text = f.text(24, 100)
	wantContains(t, text, "No projects found",
		"Projects Claude Code has run in appear here automatically. You can also add any folder.", " Add folder… ")
	wantLacks(t, text, "PROJECTS", "Scanning")
	// Enter presses the empty state's button, and so does a click.
	f.chosen = ""
	f.keys("\r")
	f.clickOn(t, 24, 100, " Add folder… ", 0)
	if f.chooses != 2 {
		t.Errorf("the chooser opened %d times", f.chooses)
	}

	f.state(t, projectsStateJSON)
	lines := strings.Split(f.text(40, 160), "\n")
	wantContains(t, lines[0], "PROJECTS", " Add a project folder ", " Rescan projects ", " Herdr settings ", " Tree ", " List ")
	if got := strings.TrimRight(lines[1], " "); got != "5 projects · 3 worktrees" {
		t.Errorf("the counts line is %q", got)
	}
	wantContains(t, strings.Join(lines, "\n"), "Calculate disk usage?", " Not now ", " Calculate ",
		"Reads every file in 6 checkouts. Cached sizes stay on screen either way.")
	wantLacks(t, strings.Join(lines, "\n"), "Refreshing…")

	// Without Herdr there is no settings button; while refreshing the header says so.
	f.v.herdr = false
	f.state(t, stateWith(t, `"isRefreshing":false`, `"isRefreshing":true`))
	lines = strings.Split(f.text(40, 160), "\n")
	wantLacks(t, lines[0], "Herdr settings")
	if got := strings.TrimRight(lines[1], " "); !strings.HasPrefix(got, "5 projects · 3 worktrees") || !strings.HasSuffix(got, "Refreshing…") {
		t.Errorf("the counts line is %q", got)
	}
}

// Projects start closed. Tree nests the repos under their folder; List keeps jacad's order.
func TestProjectsExpandCollapse(t *testing.T) {
	f := testProjects(t)
	if got, want := f.names(), []string{"workspace", "notes", "tools"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("rows are %q", got)
	}
	text := f.text(40, 160)
	wantContains(t, text, "▸ workspace  Claude  2 projects", "▸ tools", "~/workspace", "/opt/tools")
	wantLacks(t, text, "▸ notes", "▾") // nothing under notes, so no chevron

	f.keys("\r") // workspace is selected
	if got, want := f.names(), []string{"workspace", "jaca", "site", "notes", "tools"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("rows are %q", got)
	}
	wantContains(t, f.text(40, 160), "▾ workspace", "▸ jaca  Claude  3 worktrees", "▸ site  Claude", "1.46 GB")

	// Right opens the row under the cursor, Left closes it, and Left again goes to its parent.
	f.keys("j", "\x1b[C")
	want := []string{"workspace", "jaca", "  main", "  fix-login", "  old", "  gone-branch", "site", "notes", "tools"}
	if got := f.names(); !reflect.DeepEqual(got, want) {
		t.Fatalf("rows are %q", got)
	}
	f.keys("j", "j", "\x1b[D")
	if f.v.keyAt(f.v.selected) != projectKey(jacaPath) {
		t.Errorf("Left on a checkout went to %q", f.v.keyAt(f.v.selected))
	}
	f.keys("\x1b[D")
	if f.v.expanded[jacaPath] || f.v.keyAt(f.v.selected) != projectKey(jacaPath) {
		t.Errorf("Left on an open project: expanded %v, on %q", f.v.expanded, f.v.keyAt(f.v.selected))
	}
	f.keys("\x1b[D")
	if f.v.keyAt(f.v.selected) != projectKey("/Users/dev/workspace") {
		t.Errorf("Left on a closed project went to %q", f.v.keyAt(f.v.selected))
	}

	// A click on a project's name line opens and closes it; one on its path line only selects.
	f.clickOn(t, 40, 160, "jaca", 0)
	if !f.v.expanded[jacaPath] {
		t.Error("a click on the name line didn't open the project")
	}
	x, y := f.find(t, 40, 160, "jaca", 0)
	f.v.handleKey([]byte("\x1b[<0;" + itoa(x) + ";" + itoa(y+1) + "M"))
	if !f.v.expanded[jacaPath] || f.v.keyAt(f.v.selected) != projectKey(jacaPath) {
		t.Error("a click on the path line closed the project or selected another row")
	}
	// Space closes it again, after a moment: the same key twice could be a held key.
	f.keys(" ")
	f.clock.advance(time.Second)
	f.keys(" ")
	f.clock.advance(time.Second)
	if !f.v.expanded[jacaPath] {
		t.Error("two presses of Space didn't close and reopen the project")
	}

	// List: every project at the top level, in the order jacad sent them.
	f.keys("t")
	if f.v.mode != modeList || !reflect.DeepEqual(f.written, []string{"jaca.projectsViewMode=list"}) {
		t.Errorf("mode %q, written %q", f.v.mode, f.written)
	}
	want = []string{"workspace", "jaca", "  main", "  fix-login", "  old", "  gone-branch", "site", "notes", "tools"}
	if got := f.names(); !reflect.DeepEqual(got, want) {
		t.Errorf("list rows are %q", got)
	}
	wantLacks(t, f.text(40, 160), "2 projects")
	f.clickOn(t, 40, 160, " Tree ", 0)
	if f.v.mode != modeTree || f.written[len(f.written)-1] != "jaca.projectsViewMode=tree" {
		t.Errorf("mode %q, written %q", f.v.mode, f.written)
	}
}

// The view mode and the Claude command come from the app's settings.
func TestProjectsSettings(t *testing.T) {
	f := newProjectsFixture(t)
	if f.v.mode != modeTree || f.v.herdrConfigured || f.v.claudeCommand() != "claude --permission-mode bypassPermissions" {
		t.Errorf("with nothing stored: mode %q, command %q", f.v.mode, f.v.claudeCommand())
	}
	f.defaults[viewModeKey], f.defaults[herdrCommandKey] = "list", "claude --model opus"
	f.v.loadSettings()
	if f.v.mode != modeList || !f.v.herdrConfigured || f.v.claudeCommand() != "claude --model opus" {
		t.Errorf("stored: mode %q, command %q", f.v.mode, f.v.claudeCommand())
	}
	f.defaults[viewModeKey] = "grid"
	f.v.mode = modeTree
	f.v.loadSettings()
	if f.v.mode != modeTree {
		t.Errorf("an unknown mode gave %q", f.v.mode)
	}
}

func TestProjectsTagsAndSubtitles(t *testing.T) {
	f := testProjects(t)
	f.open("/Users/dev/workspace", jacaPath)
	text := f.text(40, 170)
	wantContains(t, text,
		"main  folder", "~/workspace/jaca · 2 hours ago", "2.00 GB",
		"fix-login  worktree  Claude", "~/workspace/jaca/.claude/worktrees/fix-login · from main · 3 days ago", "300 MB",
		// No age from jacad: the last Claude session, three minutes before the test clock.
		"old  worktree  No project  Orphan", "~/workspace/jaca/.claude/worktrees/old · detached HEAD · 3 min ago", "—",
		"gone-branch  worktree  Orphan", "~/workspace/jaca/.claude/worktrees/gone · branch gone")

	// Tags in the app's colors: Claude green, Orphan yellow, the rest neutral.
	frame := strings.Join(f.v.frame(40, 170), "\n")
	wantContains(t, frame, sgrGreen+"Claude"+sgrReset, sgrYellow+"Orphan"+sgrReset, sgrDim+"worktree"+sgrReset, sgrDim+"No project"+sgrReset)

	// Cleaning, a size just freed (green) and a worktree being removed (dimmed).
	raw := stateWith(t, `"sizeMB":300,"cacheMB":100,"sizeComputed":true`, `"sizeMB":300,"cacheMB":100,"sizeComputed":true,"cleaning":true`)
	raw = strings.Replace(raw, `"dropped":false`, `"dropped":true`, 1)
	raw = strings.Replace(raw, `"branch":"gone-branch"`, `"branch":"gone-branch","removing":true`, 1)
	f.state(t, raw)
	frame = strings.Join(f.v.frame(40, 170), "\n")
	wantContains(t, frame, "cleaning…", sgrGreen+"2.00 GB"+sgrReset)
	wantContains(t, frame, sgrDim+"        gone-branch  worktree  Orphan")
}

// The disk usage bar (ProjectsModel's approve, decline and cancel).
func TestProjectsSizeScan(t *testing.T) {
	f := testProjects(t)
	sizes := func() int { return len(f.fake.calls("projects.computeSizes")) }
	cancels := func() int { return len(f.fake.calls("projects.cancelSizes")) }

	// Nothing is asked for before the user answers, on any generation.
	f.state(t, stateWith(t, `"scanGeneration":4`, `"scanGeneration":5`))
	if sizes() != 0 {
		t.Fatalf("a new generation asked for sizes unapproved: %v", f.fake.made)
	}
	f.keys("S") // Stop has nothing to stop
	if cancels() != 0 {
		t.Fatalf("S before a scan made %v", f.fake.made)
	}

	f.keys("C")
	if sizes() != 1 || !f.v.sizeApproved || f.v.needsSizeApproval() {
		t.Fatalf("Calculate made %v", f.fake.made)
	}
	wantLacks(t, f.text(40, 160), "Calculate disk usage?")

	computing := strings.Replace(stateWith(t, `"scanGeneration":4`, `"scanGeneration":5`), `"isComputingSizes":false`, `"isComputingSizes":true`, 1)
	f.state(t, computing)
	text := f.text(40, 160)
	wantContains(t, text, "Calculating disk usage…", " Stop ", "Reads every file in 6 checkouts. Cached sizes stay on screen either way.")
	wantLacks(t, text, "Not now", " Calculate ")
	if sizes() != 1 {
		t.Fatalf("the same generation asked again: %v", f.fake.made)
	}

	// Each completed scan after the approval is sized again.
	f.state(t, strings.Replace(computing, `"scanGeneration":5`, `"scanGeneration":6`, 1))
	if sizes() != 2 {
		t.Fatalf("a new generation made %v", f.fake.made)
	}

	f.clickOn(t, 40, 160, " Stop ", 0)
	if cancels() != 1 || f.v.state.IsComputingSizes {
		t.Fatalf("Stop made %v", f.fake.made)
	}
	wantLacks(t, f.text(40, 160), "Calculating disk usage…", "Calculate disk usage?")
	f.state(t, stateWith(t, `"scanGeneration":4`, `"scanGeneration":7`))
	if sizes() != 3 {
		t.Errorf("after Stop a new generation made %v", f.fake.made)
	}

	// Not now: the scan is cancelled, the bar goes, and no generation asks again.
	f = testProjects(t)
	f.clickOn(t, 40, 160, " Not now ", 0)
	if len(f.fake.calls("projects.cancelSizes")) != 1 || !f.v.sizeDeclined || f.v.needsSizeApproval() {
		t.Fatalf("Not now made %v", f.fake.made)
	}
	wantLacks(t, f.text(40, 160), "Calculate disk usage?")
	f.state(t, stateWith(t, `"scanGeneration":4`, `"scanGeneration":9`))
	f.keys("C")
	if len(f.fake.calls("projects.computeSizes")) != 0 {
		t.Errorf("after Not now: %v", f.fake.made)
	}

	// No git checkout to size: no question.
	f = newProjectsFixture(t)
	f.state(t, `{"projects":[{"path":"/Users/dev/notes","source":"user"}],"hasCompletedScan":true,"lastRefresh":"2026-10-07T12:00:00Z"}`)
	wantLacks(t, f.text(40, 160), "Calculate disk usage?")
}

// The app's refresh rules: on open when the last scan is missing or older than 30 seconds,
// and on r unless one is already running.
func TestProjectsRefreshRules(t *testing.T) {
	refreshes := func(f *projectsFixture) int { return len(f.fake.calls("projects.refresh")) }

	f := testProjects(t) // scanned 9.5 seconds before the test clock
	if refreshes(f) != 0 {
		t.Fatalf("a fresh state refreshed: %v", f.fake.made)
	}
	f = newProjectsFixture(t)
	f.state(t, stateWith(t, `"lastRefresh":"2026-10-07T11:59:50.500Z"`, `"lastRefresh":"2026-10-07T11:59:29.000Z"`))
	if refreshes(f) != 1 || !f.v.state.IsRefreshing {
		t.Fatalf("a scan 31 seconds old made %v", f.fake.made)
	}
	wantContains(t, f.text(40, 160), "Refreshing…")
	// Only the first state decides: a later one, however old, does not refresh by itself.
	f.state(t, stateWith(t, `"lastRefresh":"2026-10-07T11:59:50.500Z"`, `"lastRefresh":"2026-10-07T11:00:00.000Z"`))
	if refreshes(f) != 1 {
		t.Fatalf("a later state refreshed: %v", f.fake.made)
	}
	f = newProjectsFixture(t)
	f.state(t, stateWith(t, `"lastRefresh":"2026-10-07T11:59:50.500Z",`, ``))
	if refreshes(f) != 1 {
		t.Fatalf("a state with no lastRefresh made %v", f.fake.made)
	}

	f = testProjects(t)
	f.keys("r")
	if refreshes(f) != 1 || !f.v.state.IsRefreshing {
		t.Fatalf("r made %v", f.fake.made)
	}
	f.keys("r")
	f.clock.advance(time.Second)
	f.clickOn(t, 40, 160, " Rescan projects ", 0)
	if refreshes(f) != 1 {
		t.Errorf("r while refreshing made %v", f.fake.made)
	}

	// jacad refused: nothing is refreshing, and the pane says why.
	f = testProjects(t)
	f.fake.errs["projects.refresh"] = errors.New("boom")
	f.keys("r")
	if f.v.state.IsRefreshing {
		t.Error("still refreshing after the call failed")
	}
	wantContains(t, f.text(40, 160), "jaca: boom")
	f.keys("j") // any key dismisses it
	wantLacks(t, f.text(40, 160), "jaca: boom")
}

func TestProjectsAddFolder(t *testing.T) {
	f := testProjects(t)
	f.chosen = "/Users/dev/code/app"
	f.fake.results["projects.addFolder"] = "true"
	f.keys("a")
	if got := f.fake.calls("projects.addFolder"); !reflect.DeepEqual(got, []string{`projects.addFolder {"path":"/Users/dev/code/app"}`}) {
		t.Fatalf("made %v", f.fake.made)
	}
	if f.v.toast != "Added app" {
		t.Errorf("toast %q", f.v.toast)
	}
	wantContains(t, f.text(40, 160), "Added app")
	f.clock.advance(toastLife)
	if f.v.toast != "" {
		t.Errorf("the toast is still %q after %v", f.v.toast, toastLife)
	}

	f.fake.results["projects.addFolder"] = "false"
	f.clickOn(t, 40, 160, " Add a project folder ", 0)
	if f.v.toast != "Already added" {
		t.Errorf("toast %q", f.v.toast)
	}

	// Cancelled: nothing is sent and nothing is said.
	f.v.toast, f.chosen = "", ""
	f.v.addFolder()
	if len(f.fake.calls("projects.addFolder")) != 2 || f.v.toast != "" || f.v.err != "" {
		t.Errorf("a cancelled chooser: %v, toast %q, err %q", f.fake.made, f.v.toast, f.v.err)
	}

	f.chosen = "/Users/dev/file.txt"
	f.fake.errs["projects.addFolder"] = errors.New("Not a folder: /Users/dev/file.txt")
	f.v.addFolder()
	if f.v.err != "jaca: Not a folder: /Users/dev/file.txt" || f.v.toast != "" || f.v.choosing {
		t.Errorf("err %q, toast %q", f.v.err, f.v.toast)
	}
}

// Remove takes two presses, and only a folder the user added has it.
func TestProjectsRemoveFolder(t *testing.T) {
	f := testProjects(t)
	removes := func() []string { return f.fake.calls("projects.removeFolder") }
	f.fake.results["projects.removeFolder"] = `"tools"`

	f.keys("x") // workspace came from Claude: no Remove
	if len(f.v.armed) != 0 || len(removes()) != 0 {
		t.Fatalf("x on a Claude project armed %v", f.v.armed)
	}
	wantLacks(t, f.text(40, 160), " Remove ")

	f.selectRow(t, "/opt/tools", "")
	wantContains(t, f.text(40, 160), " Remove ")
	f.clock.advance(5 * time.Second)
	f.keys("x")
	if len(removes()) != 0 {
		t.Fatalf("one press removed: %v", f.fake.made)
	}
	text := f.text(40, 160)
	wantContains(t, text, " Confirm? ")
	wantLacks(t, text, " Remove ")
	f.clock.advance(500 * time.Millisecond)
	f.keys("x")
	f.clock.advance(time.Second)
	if got := removes(); !reflect.DeepEqual(got, []string{`projects.removeFolder {"id":"/opt/tools"}`}) {
		t.Fatalf("two presses made %v", f.fake.made)
	}
	if f.v.toast != "Removed tools" {
		t.Errorf("toast %q", f.v.toast)
	}

	// The second press came too late: it arms again.
	f.clock.advance(10 * time.Second)
	f.keys("\x7f")
	f.clock.advance(armWindow + time.Second)
	wantLacks(t, f.text(40, 160), " Confirm? ")
	f.keys("\x7f")
	f.clock.advance(time.Second)
	if len(removes()) != 1 {
		t.Errorf("a press after the arm window removed: %v", f.fake.made)
	}

	// In a pane too narrow for every button, Remove is the one that stays.
	f.selectRow(t, "/opt/tools", "")
	f.clock.advance(10 * time.Second)
	f.keys("x")
	narrow := f.text(30, 40)
	wantContains(t, narrow, " Confirm? ")
	wantLacks(t, narrow, "Open in Finder")

	// Two clicks on the button confirm too. A null result (not a user folder any more) says
	// nothing; a failed call says why.
	f = testProjects(t)
	f.selectRow(t, "/Users/dev/notes", "")
	f.fake.results["projects.removeFolder"] = "null"
	f.clickOn(t, 40, 160, " Remove ", 0)
	f.clickOn(t, 40, 160, " Confirm? ", 0)
	if len(f.fake.calls("projects.removeFolder")) != 1 || f.v.toast != "" || f.v.err != "" {
		t.Errorf("made %v, toast %q, err %q", f.fake.made, f.v.toast, f.v.err)
	}
	f.fake.errs["projects.removeFolder"] = errors.New("no reply")
	f.clickOn(t, 40, 160, " Remove ", 0)
	f.clickOn(t, 40, 160, " Confirm? ", 0)
	if f.v.err != "jaca: no reply" || f.v.toast != "" {
		t.Errorf("toast %q, err %q", f.v.toast, f.v.err)
	}
}

func TestProjectsOpenAndCopy(t *testing.T) {
	f := testProjects(t)
	f.open("/Users/dev/workspace", jacaPath)

	// Finder: the project's folder or the checkout's; a missing one does nothing.
	f.selectRow(t, jacaPath, "")
	f.keys("o")
	f.selectRow(t, jacaPath, fixLoginPath)
	f.clickOn(t, 40, 170, " Open in Finder ", 0)
	f.missing[fixLoginPath] = true
	f.v.act(f.v.rows[f.v.selected], actFinder)
	if !reflect.DeepEqual(f.opened, []string{jacaPath, fixLoginPath}) || f.v.toast != "" {
		t.Errorf("opened %q, toast %q", f.opened, f.v.toast)
	}

	// Zed: the app's two toasts.
	f.keys("z")
	if len(f.zed) != 0 || f.v.toast != "Folder no longer exists" {
		t.Errorf("a missing folder: zed %q, toast %q", f.zed, f.v.toast)
	}
	delete(f.missing, fixLoginPath)
	f.clickOn(t, 40, 170, " Open in Zed ", 0)
	if !reflect.DeepEqual(f.zed, []string{"/Applications/Zed.app " + fixLoginPath}) || f.v.toast != "Opening fix-login in Zed" {
		t.Errorf("zed %q, toast %q", f.zed, f.v.toast)
	}
	// Without Zed there is no action, key or button.
	f.v.zed, f.v.toast = "", ""
	f.clock.advance(5 * time.Second)
	f.keys("z")
	if len(f.zed) != 1 || f.v.toast != "" {
		t.Errorf("without Zed: %q, toast %q", f.zed, f.v.toast)
	}
	wantLacks(t, f.text(40, 170), "Open in Zed")

	// Copy name: a checkout's name, with the copy notice. A project row has none.
	f.keys("y")
	if !reflect.DeepEqual(f.copied, []string{"fix-login"}) || f.v.snack.text != "Copied" {
		t.Errorf("copied %q, notice %q", f.copied, f.v.snack.text)
	}
	f.selectRow(t, jacaPath, "")
	f.clock.advance(5 * time.Second)
	f.keys("y")
	if len(f.copied) != 1 {
		t.Errorf("y on a project copied %q", f.copied)
	}
	wantLacks(t, f.text(40, 170), "Copy name")
	saved := copyToClipboard
	copyToClipboard = func(string) error { return errors.New("pbcopy: not found") }
	f.selectRow(t, jacaPath, fixLoginPath)
	f.v.act(f.v.rows[f.v.selected], actCopy)
	copyToClipboard = saved
	if f.v.err != "jaca: pbcopy: not found" {
		t.Errorf("err %q", f.v.err)
	}
}

func TestProjectsClearCache(t *testing.T) {
	f := testProjects(t)
	f.open("/Users/dev/workspace", jacaPath)
	cleaning := func() bool {
		c, _ := f.v.checkoutOf(jacaPath, fixLoginPath)
		return c.Cleaning
	}

	f.fake.results["projects.clearCache"] = `{"name":"fix-login","freedMB":1024}`
	f.v.clearCache(jacaPath, fixLoginPath)
	want := `projects.clearCache {"checkout":"` + fixLoginPath + `","project":"` + jacaPath + `"}`
	if got := f.fake.calls("projects.clearCache"); !reflect.DeepEqual(got, []string{want}) {
		t.Fatalf("made %v", f.fake.made)
	}
	if f.v.toast != "Freed 1.00 GB · fix-login" || !cleaning() {
		t.Errorf("toast %q, cleaning %v", f.v.toast, cleaning())
	}
	// Already cleaning: nothing more is sent until jacad's state says it is done.
	f.v.clearCache(jacaPath, fixLoginPath)
	if len(f.fake.calls("projects.clearCache")) != 1 {
		t.Fatalf("a second clean went out: %v", f.fake.made)
	}
	wantContains(t, f.text(40, 170), "cleaning…")
	f.state(t, projectsStateJSON)

	f.fake.results["projects.clearCache"] = `{"name":"fix-login","freedMB":0,"error":"0123456789012345678901234567890123456789012345678901234567890"}`
	f.v.clearCache(jacaPath, fixLoginPath)
	if f.v.toast != "Clean failed · 01234567890123456789012345678901234567890123456789" {
		t.Errorf("toast %q", f.v.toast)
	}
	f.state(t, projectsStateJSON)

	// Null (already cleaning in jacad, or gone): the row is back, and nothing is said.
	f.v.toast = ""
	f.fake.results["projects.clearCache"] = "null"
	f.v.clearCache(jacaPath, fixLoginPath)
	if f.v.toast != "" || f.v.err != "" || cleaning() {
		t.Errorf("null: toast %q, err %q, cleaning %v", f.v.toast, f.v.err, cleaning())
	}
	f.fake.errs["projects.clearCache"] = errors.New("no reply from jacad after 10m0s")
	f.v.clearCache(jacaPath, fixLoginPath)
	if f.v.err != "jaca: no reply from jacad after 10m0s" || cleaning() {
		t.Errorf("err %q, cleaning %v", f.v.err, cleaning())
	}
	// A checkout that is not there.
	f.v.clearCache(jacaPath, "/nowhere")
	if len(f.fake.calls("projects.clearCache")) != 4 {
		t.Errorf("made %v", f.fake.made)
	}
}

func TestProjectsDeleteWorktree(t *testing.T) {
	f := testProjects(t)
	f.open("/Users/dev/workspace", jacaPath)
	deletes := func() []string { return f.fake.calls("projects.deleteWorktree") }

	f.fake.results["projects.deleteWorktree"] = `{"name":"fix-login","ok":true,"stderr":""}`
	f.v.deleteWorktree(jacaPath, fixLoginPath)
	want := `projects.deleteWorktree {"checkout":"` + fixLoginPath + `","project":"` + jacaPath + `"}`
	if got := deletes(); !reflect.DeepEqual(got, []string{want}) || f.v.toast != "Deleted fix-login" {
		t.Fatalf("made %v, toast %q", f.fake.made, f.v.toast)
	}
	f.fake.results["projects.deleteWorktree"] = `{"name":"fix-login","ok":false,"stderr":"\n fatal: 'fix-login' contains modified files \n"}`
	f.v.deleteWorktree(jacaPath, fixLoginPath)
	if f.v.toast != "fatal: 'fix-login' contains modified files" {
		t.Errorf("toast %q", f.v.toast)
	}
	f.fake.results["projects.deleteWorktree"] = `{"name":"fix-login","ok":false,"stderr":"  "}`
	f.v.deleteWorktree(jacaPath, fixLoginPath)
	if f.v.toast != "Couldn't remove worktree" {
		t.Errorf("toast %q", f.v.toast)
	}
	f.v.toast = ""
	f.fake.results["projects.deleteWorktree"] = "null"
	f.v.deleteWorktree(jacaPath, fixLoginPath)
	if f.v.toast != "" || f.v.err != "" {
		t.Errorf("null: toast %q, err %q", f.v.toast, f.v.err)
	}
	f.fake.errs["projects.deleteWorktree"] = errors.New("git is busy")
	f.v.deleteWorktree(jacaPath, fixLoginPath)
	if f.v.err != "jaca: git is busy" {
		t.Errorf("err %q", f.v.err)
	}
	// The main checkout can't be removed: nothing is sent.
	f.v.deleteWorktree(jacaPath, jacaPath)
	if len(deletes()) != 5 {
		t.Errorf("made %v", f.fake.made)
	}
}

// The actions sheet (CheckoutActionsSheet): its text, its keys and its buttons.
func TestProjectsActionsSheet(t *testing.T) {
	f := testProjects(t)
	f.open("/Users/dev/workspace", jacaPath)
	f.fake.results["projects.clearCache"] = `{"name":"fix-login","freedMB":100}`
	f.fake.results["projects.deleteWorktree"] = `{"name":"fix-login","ok":true,"stderr":""}`

	f.selectRow(t, jacaPath, fixLoginPath)
	f.keys("c")
	text := f.sheetText(t)
	wantContains(t, text, "fix-login", "worktree", "~/workspace/jaca/.claude/worktrees/fix-login",
		"Clean build cache", "Runs `./gradlew clean` (if present) and deletes the", "Frees build-cache space — safe to do, but",
		"Cache here: 100 MB of 300 MB", " Clean cache ",
		"Delete worktree", "300 MB on disk", " Delete worktree ", " Close ", escClose)
	wantLacks(t, text, "main checkout, so it can't be removed", "Confirm delete?")
	checkSheet(t, "actions", f.v.sheet, [2]int{12, 40})

	// The description, whole, as the app has it.
	flat := strings.Join(strings.Fields(strings.NewReplacer("│", " ").Replace(text)), " ")
	wantContains(t, flat, "Runs `./gradlew clean` (if present) and deletes the matching Xcode DerivedData. Frees build-cache space — safe to do, but the next build will be slower while it regenerates.")

	// Close has the focus: Enter closes and does nothing else.
	f.keys("\r")
	if f.v.sheet != nil || len(f.fake.made) != 1 { // the subscription
		t.Fatalf("Enter on a new sheet: sheet %v, made %v", f.v.sheet, f.fake.made)
	}

	// Delete: the first press arms, the second deletes and closes.
	f.clock.advance(5 * time.Second)
	// c and x do nothing in the sheet: only the focused button is pressed.
	f.keys("c")
	f.clock.advance(600 * time.Millisecond)
	f.keys("c", "x")
	f.clock.advance(time.Second)
	if f.v.sheet == nil || len(f.fake.calls("projects.clearCache"))+len(f.fake.calls("projects.deleteWorktree")) != 0 {
		t.Fatalf("a letter pressed a sheet button: made %v, sheet %v", f.fake.made, f.v.sheet)
	}
	f.keys("\x1b[Z", "\r") // Shift-Tab from Close to Delete worktree
	wantContains(t, f.sheetText(t), " Confirm delete? ")
	wantLacks(t, f.sheetText(t), "│  Delete worktree")
	// Closing the sheet lets go of the armed button.
	f.keys("\x1b")
	f.keys("c")
	wantLacks(t, f.sheetText(t), " Confirm delete? ")
	f.keys("\x1b[Z", "\r")
	f.clock.advance(500 * time.Millisecond)
	f.keys("\r")
	f.clock.advance(time.Second)
	if len(f.fake.calls("projects.deleteWorktree")) != 1 || f.v.sheet != nil || f.v.toast != "Deleted fix-login" {
		t.Fatalf("made %v, sheet %v, toast %q", f.fake.made, f.v.sheet, f.v.toast)
	}
	// The arm lapses after three seconds.
	f.clock.advance(5 * time.Second)
	f.keys("c", "x")
	f.clock.advance(armWindow + time.Second)
	wantLacks(t, f.sheetText(t), "Confirm delete?")
	// With the mouse: two clicks.
	f.clickSheet(t, 40, 120, " Delete worktree ", 1)
	f.clickSheet(t, 40, 120, " Confirm delete? ", 0)
	if len(f.fake.calls("projects.deleteWorktree")) != 2 || f.v.sheet != nil {
		t.Fatalf("two clicks made %v", f.fake.made)
	}

	// Clean has no confirm and closes the sheet: by Tab and Enter, by its key, by a click.
	f.clock.advance(5 * time.Second)
	f.keys("c", "\t", "\r")
	if len(f.fake.calls("projects.clearCache")) != 1 || f.v.sheet != nil || f.v.toast != "Freed 100 MB · fix-login" {
		t.Fatalf("made %v, sheet %v, toast %q", f.fake.made, f.v.sheet, f.v.toast)
	}
	// The checkout is cleaning: the gear is unavailable.
	f.clock.advance(5 * time.Second)
	f.keys("c")
	f.clickOn(t, 40, 170, " Cache & worktree actions ", 0) // drawn, but disabled
	if f.v.sheet != nil {
		t.Fatal("the sheet opened on a checkout being cleaned")
	}
	f.state(t, projectsStateJSON)
	f.clickOn(t, 40, 170, " Cache & worktree actions ", 0)
	f.clickSheet(t, 40, 120, " Clean cache ", 0)
	if len(f.fake.calls("projects.clearCache")) != 2 || f.v.sheet != nil {
		t.Fatalf("a click on Clean cache made %v", f.fake.made)
	}

	// The main checkout: the app's note stands where Delete would, and x does nothing.
	f.state(t, projectsStateJSON)
	f.selectRow(t, jacaPath, jacaPath)
	f.clock.advance(5 * time.Second)
	f.keys("c")
	text = f.sheetText(t)
	flat = strings.Join(strings.Fields(strings.NewReplacer("│", " ").Replace(text)), " ")
	wantContains(t, flat, "─ main ─", "folder ~/workspace/jaca", "Cache here: 512 MB of 2.00 GB",
		"This is the project's main checkout, so it can't be removed — only its build cache can be cleaned.")
	wantLacks(t, text, "Delete worktree", "on disk")
	f.keys("x")
	f.clock.advance(time.Second)
	f.keys("x")
	f.clock.advance(time.Second)
	if len(f.fake.calls("projects.deleteWorktree")) != 2 || len(f.v.armed) != 0 {
		t.Errorf("x on the main checkout: %v, armed %v", f.fake.made, f.v.armed)
	}

	// While it shows, the sheet follows the checkout: Cleaning…, and it closes when the
	// checkout is gone, after which the keys still being typed do nothing for a moment.
	f.state(t, stateWith(t, `"cleaning":false`, `"cleaning":true`))
	wantContains(t, f.sheetText(t), " Cleaning… ")
	wantLacks(t, f.sheetText(t), " Clean cache ")
	f.keys("\x1b")
	f.selectRow(t, jacaPath, fixLoginPath)
	f.clock.advance(5 * time.Second)
	f.keys("c")
	f.state(t, stateWith(t, `"branch":"fix-login"`, `"branch":"fix-login","path":"/elsewhere"`))
	if f.v.sheet != nil {
		t.Fatal("the sheet stayed open on a checkout that is gone")
	}
	f.keys("q")
	f.clock.advance(hushTime)
	if !f.v.handleKey([]byte("q")) {
		t.Error("q after the hush didn't quit")
	}
}

// A held key never confirms a two-press action, however the system repeats; two presses do.
func TestProjectsHeldKeyNeverConfirms(t *testing.T) {
	ms := time.Millisecond
	saved := keyRepeat
	t.Cleanup(func() { keyRepeat = saved })
	cases := []struct {
		name   string
		method string
		key    string
		setup  func(f *projectsFixture)
	}{
		{"Remove", "projects.removeFolder", "x", func(f *projectsFixture) {
			f.fake.results["projects.removeFolder"] = `"tools"`
			f.selectRow(t, "/opt/tools", "")
		}},
		{"Delete worktree with Enter", "projects.deleteWorktree", "\r", func(f *projectsFixture) {
			f.fake.results["projects.deleteWorktree"] = `{"name":"fix-login","ok":true,"stderr":""}`
			f.open("/Users/dev/workspace", jacaPath)
			f.selectRow(t, jacaPath, fixLoginPath)
			f.keys("c", "\x1b[Z") // Shift-Tab from Close to Delete worktree
		}},
	}
	for _, c := range cases {
		for _, delay := range []time.Duration{225 * ms, 375 * ms, 700 * ms, 1800 * ms} {
			for _, interval := range []time.Duration{30 * ms, 90 * ms, 180 * ms, 450 * ms, 900 * ms, 1800 * ms, 2700 * ms} {
				for _, off := range []time.Duration{0, -60 * ms, 80 * ms} { // the first repeat on time, early, late
					keyRepeat = repeatTiming{delay, interval}
					for repeats := 1; repeats <= 12; repeats++ {
						if repeats < 2 && off != 0 {
							continue // released before a second repeat: off time, it can't be told from a press
						}
						f := testProjects(t)
						c.setup(f)
						f.clock.advance(5 * time.Second)
						f.keys(c.key)
						f.clock.advance(delay + off)
						for i := 0; i < repeats; i++ {
							f.keys(c.key)
							if i < repeats-1 {
								f.clock.advance(interval)
							}
						}
						f.clock.advance(5 * time.Second)
						if got := f.fake.calls(c.method); len(got) != 0 {
							t.Fatalf("%s, delay %v%+v, interval %v: a hold of %d repeats made %v", c.name, delay, off, interval, repeats, got)
						}
					}
				}
				keyRepeat = repeatTiming{delay, interval}
				if keyRepeat.due(delay + 200*ms) {
					continue
				}
				f := testProjects(t)
				c.setup(f)
				f.clock.advance(5 * time.Second)
				f.keys(c.key)
				f.clock.advance(delay + 200*ms) // not when a repeat is due
				f.keys(c.key)
				f.clock.advance(keyRepeat.wait())
				if got := f.fake.calls(c.method); len(got) != 1 {
					t.Fatalf("%s, delay %v, interval %v: two presses made %v", c.name, delay, interval, f.fake.made)
				}
			}
		}
	}
}

// A held key opens one Finder window, one sheet, one chooser: the repeats do nothing.
func TestProjectsHeldKeyActsOnce(t *testing.T) {
	f := testProjects(t)
	f.open("/Users/dev/workspace", jacaPath)
	f.selectRow(t, jacaPath, fixLoginPath)
	f.fake.results["projects.clearCache"] = `{"name":"fix-login","freedMB":100}`
	hold := func(key string) {
		f.keys(key)
		f.clock.advance(keyRepeat.delay)
		for i := 0; i < 20; i++ {
			f.keys(key)
			f.clock.advance(keyRepeat.interval)
		}
		f.clock.advance(5 * time.Second)
	}
	hold("o")
	if len(f.opened) != 1 {
		t.Errorf("holding o opened %q", f.opened)
	}
	hold("a")
	if f.chooses != 1 {
		t.Errorf("holding a opened the chooser %d times", f.chooses)
	}
	// c opens the sheet, where c is Clean cache: the hold doesn't reach it.
	hold("c")
	if f.v.sheet == nil || len(f.fake.calls("projects.clearCache")) != 0 {
		t.Errorf("holding c: sheet %v, made %v", f.v.sheet, f.fake.made)
	}
}

// A press or a click made on rows that an update moved before they were drawn is dropped.
func TestProjectsMovedRowsDropPresses(t *testing.T) {
	f := testProjects(t)
	f.fake.results["projects.removeFolder"] = `"tools"`
	x, y := f.find(t, 40, 160, "tools", 0)
	f.selectRow(t, "/opt/tools", "")
	f.keys("x") // armed, on rows as drawn
	f.v.frame(40, 160)

	// notes is gone: tools moves up a row, and the screen still shows the old rows.
	f.state(t, stateWith(t, `{"path":"/Users/dev/notes","source":"user","checkouts":[]},`, ``))
	if !f.v.moved {
		t.Fatal("an update that moved rows didn't mark them moved")
	}
	f.clock.advance(500 * time.Millisecond)
	f.keys("x")
	f.clock.advance(time.Second)
	f.v.handleKey([]byte("\x1b[<0;" + itoa(x) + ";" + itoa(y) + "M"))
	f.v.handleKey([]byte("\x1b[<2;" + itoa(x) + ";" + itoa(y) + "M"))
	if len(f.fake.calls("projects.removeFolder")) != 0 || f.v.menu != nil || len(f.opened) != 0 {
		t.Fatalf("on moved rows: %v, menu %v", f.fake.made, f.v.menu)
	}
	// Once drawn, the rows take presses again.
	f.v.frame(40, 160)
	f.clock.advance(5 * time.Second)
	f.keys("x")
	f.clock.advance(500 * time.Millisecond)
	f.keys("x")
	f.clock.advance(time.Second)
	if len(f.fake.calls("projects.removeFolder")) != 1 {
		t.Errorf("after the draw: %v", f.fake.made)
	}

	// A state that changes no row's place (a size) moves nothing, and the cursor stays on its row.
	f = testProjects(t)
	f.selectRow(t, "/opt/tools", "")
	f.state(t, stateWith(t, `"sizeMB":1500`, `"sizeMB":1600`))
	if f.v.moved || f.v.keyAt(f.v.selected) != projectKey("/opt/tools") {
		t.Errorf("moved %v, on %q", f.v.moved, f.v.keyAt(f.v.selected))
	}
	// A press that waits acts only on the row it was made on.
	f.fake.results["projects.removeFolder"] = `"tools"`
	f.keys("x")
	f.clock.advance(500 * time.Millisecond)
	f.keys("x", "k")
	f.clock.advance(time.Second)
	if len(f.fake.calls("projects.removeFolder")) != 1 {
		t.Errorf("a second press followed by another key: %v", f.fake.made)
	}
}

// The row menu lists a row's actions with the app's labels.
func TestProjectsRowMenu(t *testing.T) {
	f := testProjects(t)
	f.open("/Users/dev/workspace", jacaPath)
	labels := func() []string {
		var out []string
		if f.v.menu != nil {
			for _, item := range f.v.menu.items {
				out = append(out, item.label)
			}
		}
		return out
	}
	menuFor := func(project, checkout string) []string {
		f.v.menu = nil
		f.selectRow(t, project, checkout)
		f.v.frame(40, 170)
		f.clock.advance(5 * time.Second)
		f.keys("m")
		return labels()
	}

	if got, want := menuFor(jacaPath, fixLoginPath), []string{"Open in Herdr", "Open in Zed", "Copy name", "Open in Finder", "Cache & worktree actions"}; !reflect.DeepEqual(got, want) {
		t.Errorf("a worktree's menu is %q", got)
	}
	if got, want := menuFor(jacaPath, jacaPath), []string{"Open in Herdr (new worktree)", "Open in Zed", "Copy name", "Open in Finder", "Cache & worktree actions"}; !reflect.DeepEqual(got, want) {
		t.Errorf("the main checkout's menu is %q", got)
	}
	if got, want := menuFor(jacaPath, ""), []string{"Open in Herdr (new worktree)", "Open in Zed", "Open in Finder"}; !reflect.DeepEqual(got, want) {
		t.Errorf("a Claude project's menu is %q", got)
	}
	if got, want := menuFor("/opt/tools", ""), []string{"Open in Zed", "Open in Finder", "Remove"}; !reflect.DeepEqual(got, want) {
		t.Errorf("a user folder's menu is %q", got)
	}
	// The menu sits inside the pane, under the row it is for.
	box := f.v.menu.box(40, 170)
	if len(box) != 5 || f.v.menu.top < 1 || f.v.menu.top+len(box)-1 > 40 {
		t.Errorf("the menu is %d rows at row %d", len(box), f.v.menu.top)
	}

	// Remove from the menu is one press of the button: it arms, and the menu then reads Confirm?.
	f.fake.results["projects.removeFolder"] = `"tools"`
	f.keys("j", "j", "\r")
	if f.v.menu != nil || !f.v.isArmed(removeProjectKey("/opt/tools")) || len(f.fake.calls("projects.removeFolder")) != 0 {
		t.Fatalf("Remove from the menu: armed %v, made %v", f.v.armed, f.fake.made)
	}
	f.keys("m")
	if got := labels(); got[len(got)-1] != "Confirm?" {
		t.Errorf("the menu of an armed row is %q", got)
	}
	f.v.compose(40, 170)
	x, y := f.v.menu.left+2, f.v.menu.top+3
	f.v.handleKey([]byte("\x1b[<0;" + itoa(x) + ";" + itoa(y) + "M"))
	if len(f.fake.calls("projects.removeFolder")) != 1 || f.v.menu != nil {
		t.Errorf("a click on Confirm? made %v", f.fake.made)
	}

	// Without Herdr and Zed, and on a checkout being cleaned, those items are gone.
	f.v.herdr, f.v.zed = false, ""
	f.state(t, stateWith(t, `"sizeMB":300,"cacheMB":100,"sizeComputed":true`, `"sizeMB":300,"cacheMB":100,"sizeComputed":true,"cleaning":true`))
	if got, want := menuFor(jacaPath, fixLoginPath), []string{"Copy name", "Open in Finder"}; !reflect.DeepEqual(got, want) {
		t.Errorf("the menu is %q", got)
	}

	// Esc closes it; Enter on a checkout opens it; a right click opens it at the pointer.
	f.keys("\x1b")
	if f.v.menu != nil {
		t.Fatal("Esc left the menu open")
	}
	f.clock.advance(5 * time.Second)
	f.keys("\r")
	if f.v.menu == nil {
		t.Fatal("Enter on a checkout didn't open its menu")
	}
	f.keys("j", "\r") // Open in Finder
	if !reflect.DeepEqual(f.opened, []string{fixLoginPath}) || f.v.menu != nil {
		t.Errorf("Enter in the menu opened %q", f.opened)
	}
	x, y = f.find(t, 40, 170, "gone-branch", 0)
	f.v.handleKey([]byte("\x1b[<2;" + itoa(x) + ";" + itoa(y) + "M"))
	if f.v.menu == nil || f.v.menu.x != x || f.v.menu.y != y || f.v.rows[f.v.selected].checkout.name() != "gone-branch" {
		t.Fatalf("a right click: menu %+v, selected %d", f.v.menu, f.v.selected)
	}
	// A click outside it closes it and does nothing else.
	f.v.handleKey([]byte("\x1b[<0;1;1M"))
	if f.v.menu != nil || len(f.opened) != 1 {
		t.Errorf("a click outside the menu: menu %v, opened %q", f.v.menu, f.opened)
	}
}

func herdrReplies(workspaces string) func(args []string) (herdrResult, error) {
	return func(args []string) (herdrResult, error) {
		switch args[0] + " " + args[1] {
		case "workspace list":
			return herdrResult{stdout: `{"result":{"workspaces":[` + workspaces + `]}}`}, nil
		case "workspace create":
			return herdrResult{stdout: `{"result":{"workspace":{"workspace_id":"ws_new"}}}`}, nil
		case "tab create":
			return herdrResult{stdout: `{"result":{"tab":{"tab_id":"tab_7"}}}`}, nil
		case "pane list":
			return herdrResult{stdout: `{"result":{"panes":[{"pane_id":"pane_1","tab_id":"tab_1"},{"pane_id":"pane_9","tab_id":"tab_7"}]}}`}, nil
		}
		return herdrResult{}, nil
	}
}

// Open in Herdr: the launch sheet, the command stored on the first launch, and the toasts.
func TestProjectsOpenInHerdr(t *testing.T) {
	f := testProjects(t)
	f.open("/Users/dev/workspace", jacaPath)
	f.herdr.reply = herdrReplies("")

	// notes and tools are not Claude projects: no Herdr action.
	f.selectRow(t, "/opt/tools", "")
	f.keys("h")
	if f.v.sheet != nil {
		t.Fatal("h on a folder that is not a Claude project opened the sheet")
	}
	wantLacks(t, f.text(40, 170), "Open in Herdr")

	f.selectRow(t, jacaPath, "")
	wantContains(t, f.text(40, 170), " Open in Herdr (new worktree) ")
	f.clock.advance(5 * time.Second)
	f.keys("h")
	text := f.sheetText(t)
	flat := strings.Join(strings.Fields(strings.NewReplacer("│", " ").Replace(text)), " ")
	wantContains(t, flat, "Open in Herdr", "Name this session", "What are you working on?", "e.g. fix login flicker",
		"Names the Herdr tab, and the new git worktree when starting one.",
		// The first launch asks for the command too.
		"Claude command", "Herdr runs this to start Claude Code. Saved for all projects — change it later from the gear in the Projects header.",
		"claude --permission-mode bypassPermissions", " Cancel ", " Open in Herdr ")
	checkSheet(t, "launch", f.v.sheet, [2]int{12, 40})

	// Nothing to open without a name.
	f.keys("\r")
	f.clock.advance(time.Second)
	if f.v.sheet == nil || len(f.herdr.calls) != 0 {
		t.Fatalf("Enter with no name: sheet %v, herdr %q", f.v.sheet, f.herdr.calls)
	}
	f.typeText("  fix login flicker ")
	f.keys("\r")
	if f.v.sheet != nil {
		t.Fatal("the sheet stayed open")
	}
	if !reflect.DeepEqual(f.written, []string{"jaca.herdr.claudeCommand=claude --permission-mode bypassPermissions"}) {
		t.Errorf("written %q", f.written)
	}
	wantCalls := [][]string{
		{"workspace", "list"},
		{"workspace", "create", "--cwd", jacaPath, "--label", "jaca", "--focus"},
		{"tab", "create", "--workspace", "ws_new", "--cwd", jacaPath, "--label", "fix login flicker", "--focus"},
		{"pane", "list", "--workspace", "ws_new"},
		{"pane", "run", "pane_9", "cd '" + jacaPath + "' && git fetch --all --prune && git pull --ff-only ; claude --permission-mode bypassPermissions --worktree 'fix-login-flicker'"},
	}
	if !reflect.DeepEqual(f.herdr.calls, wantCalls) {
		t.Errorf("herdr ran\n%q, want\n%q", f.herdr.calls, wantCalls)
	}
	if f.v.toast != "Launched in Herdr · jaca/fix login flicker" {
		t.Errorf("toast %q", f.v.toast)
	}

	// Configured now: the sheet asks for the name only, and a worktree opens as it is.
	f.herdr.calls = nil
	f.herdr.reply = herdrReplies(`{"workspace_id":"ws_1","label":"Jaca app","worktree":{"repo_root":"` + jacaPath + `"}}`)
	f.selectRow(t, jacaPath, fixLoginPath)
	wantContains(t, f.text(40, 170), " Open in Herdr ")
	f.clickOn(t, 40, 170, " Open in Herdr ", 0)
	wantLacks(t, f.sheetText(t), "Claude command")
	f.typeText("review")
	f.clickSheet(t, 40, 120, " Open in Herdr ", 1)
	if got := f.herdr.calls[len(f.herdr.calls)-1]; !reflect.DeepEqual(got, []string{"pane", "run", "pane_9", "cd '" + fixLoginPath + "' && claude --permission-mode bypassPermissions"}) {
		t.Errorf("herdr ran %q", f.herdr.calls)
	}
	if f.v.toast != "Launched in Herdr · Jaca app/review" || len(f.written) != 1 {
		t.Errorf("toast %q, written %q", f.v.toast, f.written)
	}

	// Cancel and Esc launch nothing; a failure shows the app's toast.
	f.herdr.calls = nil
	f.v.act(f.v.rows[f.v.selected], actHerdr)
	f.typeText("x")
	f.clickSheet(t, 40, 120, " Cancel ", 0)
	f.v.act(f.v.rows[f.v.selected], actHerdr)
	f.typeText("x")
	f.keys("\x1b")
	if f.v.sheet != nil || len(f.herdr.calls) != 0 {
		t.Fatalf("Cancel and Esc: sheet %v, herdr %q", f.v.sheet, f.herdr.calls)
	}
	f.herdr.reply = func([]string) (herdrResult, error) {
		return herdrResult{stderr: "error: server is not running\n", exitCode: 1}, nil
	}
	f.v.act(f.v.rows[f.v.selected], actHerdr)
	f.typeText("x")
	f.keys("\r")
	if f.v.toast != "Herdr: error: server is not running" {
		t.Errorf("toast %q", f.v.toast)
	}

	// Without Herdr the action is not there.
	f.v.herdr = false
	f.clock.advance(5 * time.Second)
	f.keys("h", "s")
	if f.v.sheet != nil {
		t.Error("a Herdr sheet opened without Herdr")
	}
	wantLacks(t, f.text(40, 170), "Open in Herdr", "Herdr settings")
}

// The Herdr settings sheet (HerdrConfigSheet).
func TestProjectsHerdrSettings(t *testing.T) {
	f := testProjects(t)
	f.keys("s")
	text := f.sheetText(t)
	flat := strings.Join(strings.Fields(strings.NewReplacer("│", " ").Replace(text)), " ")
	wantContains(t, flat, "Open in Herdr", "Applies to all projects", "Claude command",
		"Herdr opens a new tab in the project's Space and runs this to start Claude Code. On a project root that's a git repo, Jaca refreshes to latest and appends `--worktree`; in a worktree it just runs the command there.",
		"claude --permission-mode bypassPermissions", " Cancel ", " Save ")
	wantLacks(t, text, "Reset to default") // the command is the default
	checkSheet(t, "settings", f.v.sheet, [2]int{12, 40})

	// Reset to default shows while the command differs, and puts the default back.
	f.typeText(" --verbose")
	wantContains(t, f.sheetText(t), " Reset to default ", "claude --permission-mode bypassPermissions --verbose")
	f.keys("\t", "\r")
	sheet := f.v.sheet.(*herdrConfigSheet)
	if sheet.command.String() != defaultHerdrCommand || sheet.focused() != configStopCommand {
		t.Fatalf("after Reset: %q, focus %d", sheet.command.String(), sheet.focused())
	}
	wantLacks(t, f.sheetText(t), "Reset to default")

	// Save stores it, trimmed, where the app reads it.
	f.keys("\x15")
	f.typeText("  claude --model opus  ")
	f.keys("\t", "\t", "\t", "\r") // Reset, Cancel, Save
	if f.v.sheet != nil || !reflect.DeepEqual(f.written, []string{"jaca.herdr.claudeCommand=claude --model opus"}) {
		t.Fatalf("Save: sheet %v, written %q", f.v.sheet, f.written)
	}
	if f.v.claudeCommand() != "claude --model opus" || !f.v.herdrConfigured {
		t.Errorf("the command is %q", f.v.claudeCommand())
	}

	// Enter in the field saves; an empty command is the default; Cancel stores nothing.
	f.clock.advance(5 * time.Second)
	f.clickOn(t, 40, 160, " Herdr settings ", 0)
	wantContains(t, f.sheetText(t), "claude --model opus", " Reset to default ")
	f.keys("\x15", "\r")
	if f.v.sheet != nil || f.written[len(f.written)-1] != "jaca.herdr.claudeCommand=claude --permission-mode bypassPermissions" {
		t.Fatalf("an empty command: written %q", f.written)
	}
	f.clock.advance(5 * time.Second)
	f.keys("s")
	f.typeText("x")
	f.clickSheet(t, 40, 120, " Cancel ", 0)
	if f.v.sheet != nil || len(f.written) != 2 {
		t.Errorf("Cancel: sheet %v, written %q", f.v.sheet, f.written)
	}
}

// The ? popup lists the keys, and closes on any of its keys or a click.
func TestProjectsHelp(t *testing.T) {
	f := testProjects(t)
	f.keys("?")
	if !f.v.help {
		t.Fatal("? didn't open the help")
	}
	box := plainFrame(keysBox(f.v.helpKeys(), 40, 120))
	wantContains(t, box, "Open in Finder", "Open in Zed", "Copy name", "Cache & worktree actions", "Open in Herdr",
		"Remove", "Add a project folder", "Rescan projects", "Herdr settings", "Calculate", "Not now", "Help", "Quit")
	f.keys("o") // the popup holds the keys
	if len(f.opened) != 0 || !f.v.help {
		t.Errorf("a key under the help acted: %q", f.opened)
	}
	f.keys("\x1b")
	if f.v.help {
		t.Error("Esc left the help open")
	}
	f.clickOn(t, 40, 120, "Help", 0)
	if !f.v.help {
		t.Fatal("a click on Help didn't open it")
	}
	f.v.handleKey([]byte("\x1b[<0;5;5M"))
	if f.v.help {
		t.Error("a click left the help open")
	}
	if !f.v.handleKey([]byte("q")) || !f.v.handleKey([]byte{0x03}) {
		t.Error("q and Ctrl-C don't quit")
	}
	// A pane too small for the popup closes it rather than holding the keys unseen.
	f.keys("?")
	f.v.compose(2, 3)
	if f.v.help {
		t.Error("the help stayed open in a pane with no room for it")
	}
}

// Every drawn row fits the pane, at every size, whatever the text jacad sends.
func TestProjectsFramesFitThePane(t *testing.T) {
	long := strings.Repeat("a-very-long-directory-name/", 12)
	hostile := `{"projects":[
		{"path":"/Users/dev/` + long + `repo","isGitRepo":true,"source":"claude","checkouts":[
			{"path":"/Users/dev/` + long + `repo","isMain":true,"branch":"feature/` + strings.Repeat("x", 300) + `","sizeMB":123456,"cacheMB":99999,"sizeComputed":true,"age":"2 hours ago"},
			{"path":"/Users/dev/` + long + `repo/.claude/worktrees/日本語のブランチ名前","branch":"日本語のブランチ名前\u001b[31m\u0007\tred","base":"main\u001b]0;title\u0007","age":"1 day ago","orphan":true,"isClaudeManaged":true,"hasClaudeSessions":true,"sizeMB":5,"sizeComputed":true,"dropped":true},
			{"path":"/Users/dev/` + long + `repo/.claude/worktrees/w3","branch":"w3","removing":true,"cleaning":true}]},
		{"path":"/Users/dev/` + long + `repo/nested/プロジェクト","source":"user","isGitRepo":true,"checkouts":[{"path":"/Users/dev/x","isMain":true}]},
		{"path":"/\u001b[2J‮evil","source":"user"}],
		"isRefreshing":true,"isComputingSizes":true,"hasCompletedScan":true,"scanGeneration":2,"lastRefresh":"2026-10-07T12:00:00Z"}`

	var sizes [][2]int
	for _, rows := range []int{1, 2, 3, 4, 6, 11, 12, 13, 24, 40, 60} {
		for cols := 1; cols <= 200; cols++ {
			if cols > 24 && cols%11 != 0 && cols != 200 {
				continue
			}
			sizes = append(sizes, [2]int{rows, cols})
		}
	}
	// A few sizes for the states that are checked once for each row.
	some := [][2]int{{1, 1}, {2, 3}, {3, 7}, {5, 9}, {6, 20}, {12, 30}, {12, 40}, {13, 41}, {20, 50}, {24, 80}, {30, 100}, {40, 160}, {60, 200}}
	checkAt := func(name string, f *projectsFixture, sizes [][2]int) {
		t.Helper()
		for _, size := range sizes {
			rows, cols := size[0], size[1]
			frame := f.v.frame(rows, cols)
			checkFrame(t, name, frame, rows, cols)
			for _, row := range frame {
				if strings.ContainsAny(sgr.ReplaceAllString(row, ""), "\x1b\x07\t‮") {
					t.Fatalf("%s at %dx%d: a control character reached the pane: %q", name, rows, cols, row)
				}
			}
			if f.v.sheet != nil {
				box, top, left := f.v.sheet.box(rows, cols)
				for i, row := range box {
					if w := plainWidth(row); left+w > cols || top+i >= rows {
						t.Fatalf("%s at %dx%d: sheet row %d is %d cells from column %d: %q", name, rows, cols, i, w, left, row)
					}
				}
			}
			if f.v.menu != nil {
				menu := *f.v.menu
				for _, row := range menu.box(rows, cols) {
					if w := plainWidth(row); menu.left-1+w > cols {
						t.Fatalf("%s at %dx%d: a menu row is %d cells from column %d", name, rows, cols, w, menu.left)
					}
				}
			}
			// The whole pane, with what is open over it, draws without a panic. The pane's
			// state is put back, since a pane too small closes the help and the menu.
			help, menu := f.v.help, f.v.menu
			f.v.compose(rows, cols)
			f.v.help, f.v.menu = help, menu
		}
	}
	// Every size in the Tree view; the List view draws the same rows, and gets the few.
	all := sizes
	check := func(name string, f *projectsFixture) {
		t.Helper()
		checkAt(name, f, all)
	}

	f := newProjectsFixture(t)
	check("no state", f)
	f.state(t, `{"projects":[],"isRefreshing":true}`)
	check("first load", f)
	f.state(t, `{"projects":[],"hasCompletedScan":true,"lastRefresh":"2026-10-07T12:00:00Z"}`)
	check("empty", f)

	for _, mode := range []string{modeTree, modeList} {
		f = newProjectsFixture(t)
		f.v.mode = mode
		if mode == modeList {
			all = some
		}
		f.state(t, hostile)
		check(mode+" closed", f)
		for _, p := range f.v.state.Projects {
			f.v.expanded[p.Path] = true
		}
		f.v.relayout()
		for i := range f.v.rows {
			f.v.selected, f.v.follow = i, true
			checkAt(mode+" open", f, some)
		}
		f.v.flash("fatal: '日本語'\x1b[31m " + strings.Repeat("contains modified or untracked files ", 20))
		f.v.armed[removeProjectKey(f.v.state.Projects[1].Path)] = f.clock.now.Add(time.Hour)
		f.v.selected = 0
		check(mode+" open, with a toast", f)
		f.v.help = true
		check(mode+" help", f)
		f.v.help = false

		root := f.v.state.Projects[0]
		for _, c := range root.Checkouts {
			f.v.sheet = newActionsSheet(f.v, root.Path, c.Path)
			f.v.armed[deleteWorktreeKey(c.Path)] = f.clock.now.Add(time.Hour)
			check(mode+" actions sheet", f)
		}
		launch := newHerdrLaunchSheet(f.v, herdrTarget{projectRoot: root.Path, projectName: root.name(), folder: root.Path, hasGit: true})
		launch.name.set(strings.Repeat("名前 ", 60))
		f.v.sheet = launch
		check(mode+" launch sheet", f)
		config := newHerdrConfigSheet(f.v)
		config.command.set(strings.Repeat("claude --flag ", 40))
		f.v.sheet = config
		check(mode+" settings sheet", f)
		f.v.sheet = nil

		for i, row := range f.v.rows {
			f.v.selected = i
			f.v.frame(40, 120)
			f.v.openMenu(row, 0, 0)
			if f.v.menu != nil {
				checkAt(mode+" menu", f, some)
			}
			f.v.openMenu(row, 500, 500) // a pointer outside the pane
			if f.v.menu != nil {
				checkAt(mode+" menu at the edge", f, some)
			}
			f.v.menu = nil
		}
	}
}

// Keys and clicks in a pane of any size, on any state, do not panic.
func TestProjectsInputAtAnySize(t *testing.T) {
	keys := []string{"j", "k", "\r", " ", "\x1b[C", "\x1b[D", "\x1b[5~", "\x1b[6~", "o", "z", "y", "c", "h", "m", "x", "\x7f",
		"a", "r", "t", "s", "C", "N", "S", "?", "\x1b", "\t", "\x1b[Z", "é", "日"}
	for _, raw := range []string{`{}`, `{"projects":[],"isRefreshing":true}`, projectsStateJSON} {
		for _, size := range [][2]int{{1, 1}, {2, 5}, {6, 20}, {24, 80}, {60, 200}} {
			f := newProjectsFixture(t)
			f.herdr.reply = herdrReplies("")
			f.fake.results["projects.addFolder"] = "true"
			f.state(t, raw)
			for round := 0; round < 3; round++ {
				for _, k := range keys {
					f.v.handleKey([]byte(k))
					f.v.compose(size[0], size[1])
					f.clock.advance(700 * time.Millisecond)
					for _, button := range []int{0, 2, 64, 65, 32} {
						for _, at := range [][2]int{{1, 1}, {size[1], size[0]}, {size[1] / 2, size[0] / 2}, {300, 300}, {0, 0}} {
							f.v.handleKey([]byte("\x1b[<" + itoa(button) + ";" + itoa(at[0]) + ";" + itoa(at[1]) + "M"))
							f.v.handleKey([]byte("\x1b[<" + itoa(button) + ";" + itoa(at[0]) + ";" + itoa(at[1]) + "m"))
						}
					}
					f.v.compose(size[0], size[1])
				}
			}
		}
	}
}

// The events the pane takes from jacad: a state, and a note that one was skipped.
func TestProjectsEvents(t *testing.T) {
	f := newProjectsFixture(t)
	if got := f.fake.calls("events.subscribe"); !reflect.DeepEqual(got, []string{`events.subscribe {"topics":["projects.state"]}`}) {
		t.Fatalf("on open: %v", f.fake.made)
	}
	if f.v.zed != "/Applications/Zed.app" {
		t.Errorf("Zed is %q", f.v.zed)
	}
	f.v.handleEvent(event{Topic: projectsTopic, Data: []byte(projectsStateJSON)})
	if !f.v.loaded || len(f.v.state.Projects) != 5 {
		t.Fatalf("a state event gave %+v", f.v.state)
	}
	f.v.handleEvent(event{Topic: projectsTopic, Data: []byte(`broken`)})
	if len(f.v.state.Projects) != 5 {
		t.Error("an unreadable event replaced the state")
	}
	f.fake.results["projects.state"] = `{"projects":[{"path":"/only"}],"hasCompletedScan":true}`
	f.v.handleEvent(event{Topic: "events.dropped", Data: []byte(`{"topic":"gradle.daemons","count":2}`)})
	if len(f.fake.calls("projects.state")) != 0 {
		t.Errorf("another topic's dropped events fetched: %v", f.fake.made)
	}
	f.v.handleEvent(event{Topic: "events.dropped", Data: []byte(`{"topic":"projects.state","count":2}`)})
	if len(f.v.state.Projects) != 1 || f.v.state.Projects[0].Path != "/only" {
		t.Errorf("after dropped events the state is %+v", f.v.state)
	}
	f.fake.errs["events.subscribe"] = errors.New("jacad closed the connection")
	f.v.open()
	if f.v.err != "jaca: jacad closed the connection" {
		t.Errorf("err %q", f.v.err)
	}
}
