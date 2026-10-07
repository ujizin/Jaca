package main

import (
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

// launchWith runs herdrLaunch against a fake herdr and returns what it ran and reported.
func launchWith(t *testing.T, target herdrTarget, reply func(args []string) (herdrResult, error)) (calls [][]string, progress []string, workspace, tab string, err error) {
	t.Helper()
	fake := &herdrFake{reply: reply}
	savedRun, savedWait := herdrRun, herdrPaneWait
	herdrRun, herdrPaneWait = fake.run, 0
	t.Cleanup(func() { herdrRun, herdrPaneWait = savedRun, savedWait })
	workspace, tab, err = herdrLaunch(target, defaultHerdrCommand, func(msg string) { progress = append(progress, msg) })
	return fake.calls, progress, workspace, tab, err
}

// A project root that is a git repo, with no Space yet: the Space is created, and claude
// starts a worktree named after the tab on the branch brought up to date.
func TestHerdrLaunchProjectRootWithGit(t *testing.T) {
	target := herdrTarget{projectRoot: "/Users/dev/workspace/jaca/", projectName: "jaca", folder: "/Users/dev/workspace/jaca",
		hasGit: true, tabName: "Fix: login flicker!"}
	lists := 0
	reply := herdrReplies(`{"workspace_id":"ws_other","label":"site","worktree":{"repo_root":"/Users/dev/workspace/site","checkout_path":"/Users/dev/workspace/site"}},{"workspace_id":"ws_plain","label":null}`)
	calls, progress, workspace, tab, err := launchWith(t, target, func(args []string) (herdrResult, error) {
		if args[0] == "pane" && args[1] == "list" {
			// The tab's pane is not listed at first.
			if lists++; lists < 3 {
				return herdrResult{stdout: `{"result":{"panes":[{"pane_id":"pane_1","tab_id":"tab_1"}]}}`}, nil
			}
		}
		return reply(args)
	})
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"workspace", "list"},
		{"workspace", "create", "--cwd", "/Users/dev/workspace/jaca", "--label", "jaca", "--focus"},
		{"tab", "create", "--workspace", "ws_new", "--cwd", "/Users/dev/workspace/jaca", "--label", "Fix: login flicker!", "--focus"},
		{"pane", "list", "--workspace", "ws_new"},
		{"pane", "list", "--workspace", "ws_new"},
		{"pane", "list", "--workspace", "ws_new"},
		{"pane", "run", "pane_9", "cd '/Users/dev/workspace/jaca' && git fetch --all --prune && git pull --ff-only ; claude --permission-mode bypassPermissions --worktree 'Fix-login-flicker'"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Errorf("herdr ran\n%q, want\n%q", calls, want)
	}
	wantProgress := []string{
		"Finding Herdr Space for jaca…",
		"Creating Herdr Space 'jaca'…",
		"Creating tab 'Fix: login flicker!' in Herdr…",
		"Creating worktree 'Fix-login-flicker' + starting Claude…",
	}
	if !reflect.DeepEqual(progress, wantProgress) {
		t.Errorf("progress\n%q, want\n%q", progress, wantProgress)
	}
	if workspace != "jaca" || tab != "Fix: login flicker!" {
		t.Errorf("launched in %q/%q", workspace, tab)
	}
}

// A project root without git, in a Space found by its label: claude runs in the folder.
func TestHerdrLaunchProjectRootWithoutGit(t *testing.T) {
	target := herdrTarget{projectRoot: "/Users/dev/my notes", projectName: "my notes", folder: "/Users/dev/my notes", tabName: "plan"}
	calls, progress, workspace, tab, err := launchWith(t, target,
		herdrReplies(`{"workspace_id":"ws_a","label":"other"},{"workspace_id":"ws_notes","label":"my notes"},{"workspace_id":"ws_b","label":"my notes"}`))
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"workspace", "list"},
		{"workspace", "focus", "ws_notes"},
		{"tab", "create", "--workspace", "ws_notes", "--cwd", "/Users/dev/my notes", "--label", "plan", "--focus"},
		{"pane", "list", "--workspace", "ws_notes"},
		{"pane", "run", "pane_9", "cd '/Users/dev/my notes' && claude --permission-mode bypassPermissions"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Errorf("herdr ran\n%q, want\n%q", calls, want)
	}
	wantProgress := []string{"Finding Herdr Space for my notes…", "Creating tab 'plan' in Herdr…", "Starting Claude in my notes…"}
	if !reflect.DeepEqual(progress, wantProgress) {
		t.Errorf("progress %q", progress)
	}
	if workspace != "my notes" || tab != "plan" {
		t.Errorf("launched in %q/%q", workspace, tab)
	}
}

// A linked worktree, in the Space of its repo: claude runs in the worktree as it is. The
// Space keeps the label Herdr has for it, and one without a label reads as the project.
func TestHerdrLaunchWorktree(t *testing.T) {
	folder := "/Users/dev/workspace/jaca/.claude/worktrees/it's-here"
	target := herdrTarget{projectRoot: "/Users/dev/workspace/jaca", projectName: "jaca", folder: folder,
		isWorktree: true, hasGit: true, tabName: "review"}
	for _, c := range []struct{ workspaces, label string }{
		{`{"workspace_id":"ws_1","label":"Jaca app","worktree":{"repo_root":"/Users/dev/workspace/jaca","checkout_path":"/elsewhere"}}`, "Jaca app"},
		{`{"workspace_id":"ws_1","worktree":{"checkout_path":"/Users/dev/workspace/jaca"}}`, "jaca"},
	} {
		calls, progress, workspace, tab, err := launchWith(t, target, herdrReplies(c.workspaces))
		if err != nil {
			t.Fatal(err)
		}
		want := [][]string{
			{"workspace", "list"},
			{"workspace", "focus", "ws_1"},
			{"tab", "create", "--workspace", "ws_1", "--cwd", folder, "--label", "review", "--focus"},
			{"pane", "list", "--workspace", "ws_1"},
			{"pane", "run", "pane_9", `cd '/Users/dev/workspace/jaca/.claude/worktrees/it'\''s-here' && claude --permission-mode bypassPermissions`},
		}
		if !reflect.DeepEqual(calls, want) {
			t.Errorf("herdr ran\n%q, want\n%q", calls, want)
		}
		wantProgress := []string{"Finding Herdr Space for jaca…", "Creating tab 'review' in Herdr…", "Opening worktree 'it's-here' + starting Claude…"}
		if !reflect.DeepEqual(progress, wantProgress) {
			t.Errorf("progress %q", progress)
		}
		if workspace != c.label || tab != "review" {
			t.Errorf("launched in %q/%q", workspace, tab)
		}
	}
}

// Each way a launch fails, with the app's reason.
func TestHerdrLaunchFailures(t *testing.T) {
	target := herdrTarget{projectRoot: "/r", projectName: "r", folder: "/r", hasGit: true}
	ok := herdrReplies("")
	failing := func(command string, res herdrResult, err error) func([]string) (herdrResult, error) {
		return func(args []string) (herdrResult, error) {
			if args[0]+" "+args[1] == command {
				return res, err
			}
			return ok(args)
		}
	}
	for _, c := range []struct {
		name  string
		reply func([]string) (herdrResult, error)
		want  string
	}{
		{"no herdr", failing("workspace list", herdrResult{}, &exec.Error{Name: "herdr", Err: exec.ErrNotFound}), "herdr CLI not found"},
		{"herdr can't start", failing("workspace list", herdrResult{}, errors.New("permission denied")), "launch failed"},
		{"stderr", failing("workspace list", herdrResult{stderr: " server is not running\n", stdout: "ignored", exitCode: 1}, nil), "server is not running"},
		{"stdout", failing("workspace create", herdrResult{stdout: "label taken\n", exitCode: 1}, nil), "label taken"},
		{"exit code", failing("pane run", herdrResult{exitCode: 2}, nil), "herdr exited with code 2"},
		{"not JSON", failing("tab create", herdrResult{stdout: "<html>"}, nil), "Unexpected herdr response: <html>"},
		{"no id", failing("workspace create", herdrResult{stdout: `{"result":{}}`}, nil), `Unexpected herdr response: {"result":{}}`},
		{"a long reply", failing("workspace list", herdrResult{stdout: strings.Repeat("é", 300)}, nil), "Unexpected herdr response: " + strings.Repeat("é", 200)},
		{"no pane", failing("pane list", herdrResult{stdout: `{"result":{"panes":[]}}`}, nil), "couldn't resolve the new tab's pane"},
		{"pane list fails", failing("pane list", herdrResult{exitCode: 1}, nil), "couldn't resolve the new tab's pane"},
	} {
		calls, _, _, _, err := launchWith(t, target, c.reply)
		if err == nil || err.Error() != c.want {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
		if strings.HasPrefix(c.name, "no pane") || c.name == "pane list fails" {
			lists := 0
			for _, call := range calls {
				if call[0] == "pane" && call[1] == "list" {
					lists++
				}
			}
			if lists != herdrPaneTries {
				t.Errorf("%s: looked for the pane %d times", c.name, lists)
			}
		}
		if last := calls[len(calls)-1]; err != nil && c.name != "exit code" && last[0] == "pane" && last[1] == "run" {
			t.Errorf("%s: the line ran after a failure: %q", c.name, calls)
		}
	}
}

func TestHerdrLaunchLine(t *testing.T) {
	// No tab name: the tab is named after the folder, and claude names the worktree.
	target := herdrTarget{projectRoot: "/r/app", projectName: "app", folder: "/r/app", hasGit: true}
	if got := herdrLaunchLine(target, "claude"); got != "cd '/r/app' && git fetch --all --prune && git pull --ff-only ; claude --worktree" {
		t.Errorf("line %q", got)
	}
	if got := herdrRunProgress(target); got != "Creating a new worktree + starting Claude…" {
		t.Errorf("progress %q", got)
	}
	_, progress, _, tab, err := launchWith(t, target, herdrReplies(""))
	if err != nil || tab != "app" || progress[2] != "Creating tab 'app' in Herdr…" {
		t.Errorf("tab %q, progress %q, %v", tab, progress, err)
	}
	for name, want := range map[string]string{
		"fix login flicker":      "fix-login-flicker",
		"  Fix: the / bug!!  ":   "Fix-the-bug",
		"..hidden_file.v2-":      "hidden_file.v2",
		"日本語":                    "",
		"a--b":                   "a-b",
		strings.Repeat("ab", 40): strings.Repeat("ab", 20),
		"":                       "",
	} {
		if got := worktreeSlug(name); got != want {
			t.Errorf("worktreeSlug(%q) = %q, want %q", name, got, want)
		}
	}
	if got := shellQuote("it's"); got != `'it'\''s'` {
		t.Errorf("shellQuote gave %q", got)
	}
}

// Zed is the first installed build, the released one first, found by its bundle identifier
// or in an Applications folder.
func TestFindZed(t *testing.T) {
	saved := homeDir
	homeDir = "/Users/dev"
	t.Cleanup(func() { homeDir = saved })
	none := func(string) string { return "" }
	nowhere := func(string) bool { return false }
	if got := findZed(none, nowhere); got != "" {
		t.Errorf("with no Zed: %q", got)
	}
	byID := func(id string) string {
		return map[string]string{"dev.zed.Zed-Preview": "/Apps/Zed Preview.app", "dev.zed.Zed-Dev": "/Apps/Zed Dev.app"}[id]
	}
	if got := findZed(byID, nowhere); got != "/Apps/Zed Preview.app" {
		t.Errorf("by bundle identifier: %q", got)
	}
	inHome := func(path string) bool { return path == "/Users/dev/Applications/Zed.app" }
	if got := findZed(byID, inHome); got != "/Users/dev/Applications/Zed.app" {
		t.Errorf("the released build in ~/Applications: %q", got)
	}
}
