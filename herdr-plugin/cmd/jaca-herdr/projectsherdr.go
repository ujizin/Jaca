package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// What the Projects pane does outside jacad: the launch of a Claude Code session in Herdr
// (the app's HerdrService), the app's settings it shares, Zed, Finder and the folder chooser.

// The app's defaults domain and the two keys the pane shares with it (ProjectsModel).
const (
	jacaDefaultsDomain  = "dev.srsouza.Jaca"
	viewModeKey         = "jaca.projectsViewMode"
	herdrCommandKey     = "jaca.herdr.claudeCommand"
	defaultHerdrCommand = "claude --permission-mode bypassPermissions"
)

// readDefault reads one of the app's settings. set is false when the key has no value; read is
// false when `defaults` could not be asked in time, which says nothing about the value.
// Replaced in tests.
var readDefault = func(key string) (value string, set, read bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "defaults", "read", jacaDefaultsDomain, key)
	cmd.WaitDelay = 100 * time.Millisecond
	out, err := cmd.Output()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return strings.TrimRight(string(out), "\n"), true, true
	case ctx.Err() == nil && errors.As(err, &exit) && exit.ExitCode() == 1:
		return "", false, true // `defaults read` exits 1 for a key or a domain that is not there
	}
	return "", false, false
}

// writeDefault stores one of the app's settings, as a string. Replaced in tests.
var writeDefault = func(key, value string) error {
	out, err := exec.Command("defaults", "write", jacaDefaultsDomain, key, "-string", value).CombinedOutput()
	if err != nil {
		if text := firstLine(out); text != "" {
			return errors.New(text)
		}
	}
	return err
}

// herdrResult is what a herdr command printed and its exit code.
type herdrResult struct {
	stdout, stderr string
	exitCode       int
}

// herdrRun runs the herdr CLI. The error is for a command that could not be started; one
// that ran and failed comes back with its exit code. Replaced in tests.
var herdrRun = func(args ...string) (herdrResult, error) {
	cmd := exec.Command(envOr("HERDR_BIN_PATH", "herdr"), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	res := herdrResult{stdout: stdout.String(), stderr: stderr.String()}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		res.exitCode = exit.ExitCode()
		return res, nil
	}
	return res, err
}

// herdrPaneWait is the pause between two looks for a new tab's pane, and herdrPaneTries how
// many looks there are (HerdrService.resolvePane).
var herdrPaneWait = 200 * time.Millisecond

const herdrPaneTries = 30

// herdrTarget is what to launch and where (HerdrService.LaunchTarget).
type herdrTarget struct {
	projectRoot string // keys the Space
	projectName string
	folder      string // where claude runs: the project root or a worktree
	isWorktree  bool   // a linked worktree: claude runs in it as it is
	hasGit      bool   // the root is a git repo: claude starts a new worktree
	tabName     string // what the user typed: the tab's label and the new worktree's name
}

// herdrFailure is why a herdr command failed (HerdrService.message): what it wrote to stderr,
// else to stdout, else its exit code.
func herdrFailure(res herdrResult) error {
	if text := strings.TrimSpace(res.stderr); text != "" {
		return errors.New(text)
	}
	if text := strings.TrimSpace(res.stdout); text != "" {
		return errors.New(text)
	}
	return fmt.Errorf("herdr exited with code %d", res.exitCode)
}

// herdrStartError is the reason for a herdr command that could not be started: the app's
// text for a missing CLI, else the one its toast falls back to.
func herdrStartError(err error) error {
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return errors.New("herdr CLI not found")
	}
	return errors.New("launch failed")
}

// herdrBadResponse is the app's error for a reply that can't be read, with its first 200
// characters.
func herdrBadResponse(stdout string) error {
	if rs := []rune(stdout); len(rs) > 200 {
		stdout = string(rs[:200])
	}
	return errors.New("Unexpected herdr response: " + stdout)
}

// herdrCall runs a command that has to succeed and decodes its reply's result into out.
func herdrCall(out any, args ...string) (herdrResult, error) {
	res, err := herdrRun(args...)
	if err != nil {
		return res, herdrStartError(err)
	}
	if res.exitCode != 0 {
		return res, herdrFailure(res)
	}
	envelope := struct {
		Result any `json:"result"`
	}{Result: out}
	if json.Unmarshal([]byte(res.stdout), &envelope) != nil {
		return res, herdrBadResponse(res.stdout)
	}
	return res, nil
}

// herdrLaunch is HerdrService.launch: it finds or creates the project's Space, opens a tab
// in the target folder, finds the tab's pane and runs the launch line there. progress gets
// each step's text, in order. It returns the labels of the Space and the tab.
func herdrLaunch(t herdrTarget, claudeCommand string, progress func(string)) (workspaceLabel, tabLabel string, err error) {
	tabLabel = t.tabName
	if tabLabel == "" {
		tabLabel = filepath.Base(t.folder)
	}

	progress("Finding Herdr Space for " + t.projectName + "…")
	var list struct {
		Workspaces []struct {
			ID       string  `json:"workspace_id"`
			Label    *string `json:"label"`
			Worktree *struct {
				CheckoutPath string `json:"checkout_path"`
				RepoRoot     string `json:"repo_root"`
			} `json:"worktree"`
		} `json:"workspaces"`
	}
	res, err := herdrCall(&list, "workspace", "list")
	if err != nil {
		return "", "", err
	}
	if list.Workspaces == nil {
		// A reply without the list is not "no Space yet": creating one on it would duplicate
		// the project's Space. The app fails the same way.
		return "", "", herdrBadResponse(res.stdout)
	}
	rootPath := filepath.Clean(t.projectRoot)
	workspaceID := ""
	for _, ws := range list.Workspaces {
		inRepo := ws.Worktree != nil && (ws.Worktree.RepoRoot == rootPath || ws.Worktree.CheckoutPath == rootPath)
		if ws.ID == "" || !(inRepo || (ws.Label != nil && *ws.Label == t.projectName)) {
			continue
		}
		workspaceID, workspaceLabel = ws.ID, t.projectName
		if ws.Label != nil {
			workspaceLabel = *ws.Label
		}
		_, _ = herdrRun("workspace", "focus", workspaceID) // the launch goes on without the focus
		break
	}
	if workspaceID == "" {
		progress("Creating Herdr Space '" + t.projectName + "'…")
		var created struct {
			Workspace struct {
				ID string `json:"workspace_id"`
			} `json:"workspace"`
		}
		res, err := herdrCall(&created, "workspace", "create", "--cwd", rootPath, "--label", t.projectName, "--focus")
		if err != nil {
			return "", "", err
		}
		if created.Workspace.ID == "" {
			return "", "", herdrBadResponse(res.stdout)
		}
		workspaceID, workspaceLabel = created.Workspace.ID, t.projectName
	}

	progress("Creating tab '" + tabLabel + "' in Herdr…")
	var tab struct {
		Tab struct {
			ID string `json:"tab_id"`
		} `json:"tab"`
	}
	res, err = herdrCall(&tab, "tab", "create", "--workspace", workspaceID, "--cwd", t.folder, "--label", tabLabel, "--focus")
	if err != nil {
		return "", "", err
	}
	if tab.Tab.ID == "" {
		return "", "", herdrBadResponse(res.stdout)
	}

	// The reply to tab create doesn't carry a pane id that can be used yet: it is read back.
	paneID := ""
	for attempt := 0; attempt < herdrPaneTries && paneID == ""; attempt++ {
		res, err := herdrRun("pane", "list", "--workspace", workspaceID)
		if err != nil {
			return "", "", herdrStartError(err)
		}
		var panes struct {
			Result struct {
				Panes []struct {
					PaneID string `json:"pane_id"`
					TabID  string `json:"tab_id"`
				} `json:"panes"`
			} `json:"result"`
		}
		if res.exitCode == 0 && json.Unmarshal([]byte(res.stdout), &panes) == nil {
			for _, p := range panes.Result.Panes {
				if p.TabID == tab.Tab.ID && p.PaneID != "" {
					paneID = p.PaneID
					break
				}
			}
		}
		if paneID == "" && attempt < herdrPaneTries-1 {
			time.Sleep(herdrPaneWait)
		}
	}
	if paneID == "" {
		return "", "", errors.New("couldn't resolve the new tab's pane")
	}

	progress(herdrRunProgress(t))
	res, err = herdrRun("pane", "run", paneID, herdrLaunchLine(t, claudeCommand))
	if err != nil {
		return "", "", herdrStartError(err)
	}
	if res.exitCode != 0 {
		return "", "", herdrFailure(res)
	}
	return workspaceLabel, tabLabel, nil
}

// herdrRunProgress is the last step's text (HerdrService.runProgress).
func herdrRunProgress(t herdrTarget) string {
	switch {
	case t.isWorktree:
		return "Opening worktree '" + filepath.Base(t.folder) + "' + starting Claude…"
	case t.hasGit:
		if slug := worktreeSlug(t.tabName); slug != "" {
			return "Creating worktree '" + slug + "' + starting Claude…"
		}
		return "Creating a new worktree + starting Claude…"
	}
	return "Starting Claude in " + t.projectName + "…"
}

// herdrLaunchLine is the shell line `herdr pane run` executes in the tab
// (HerdrService.launchLine): in a linked worktree, or a root without git, claude runs in the
// folder; in a root with git the branch is brought up to date and claude starts a worktree.
func herdrLaunchLine(t herdrTarget, claudeCommand string) string {
	cd := "cd " + shellQuote(t.folder)
	if t.isWorktree || !t.hasGit {
		return cd + " && " + claudeCommand
	}
	worktreeArg := "--worktree"
	if slug := worktreeSlug(t.tabName); slug != "" {
		worktreeArg += " " + shellQuote(slug)
	}
	return cd + " && git fetch --all --prune && git pull --ff-only ; " + claudeCommand + " " + worktreeArg
}

// shellQuote single-quotes s for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// worktreeSlug turns a tab name into a name git takes for a worktree and its branch
// (HerdrService.worktreeSlug): letters, digits, '.', '_' and '-' stay, runs of anything else
// become one '-', the ends are trimmed, and 40 characters are kept.
func worktreeSlug(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	slug := b.String()
	for strings.Contains(slug, "--") {
		slug = strings.ReplaceAll(slug, "--", "-")
	}
	slug = strings.Trim(slug, "-._")
	if len(slug) > 40 {
		slug = slug[:40]
	}
	return slug
}

// Zed's bundle identifiers, the released build first (ExternalEditor.Kind.bundleIdentifiers),
// and the name each build's app has in an Applications folder.
var zedBuilds = [][2]string{
	{"dev.zed.Zed", "Zed.app"},
	{"dev.zed.Zed-Preview", "Zed Preview.app"},
	{"dev.zed.Zed-Nightly", "Zed Nightly.app"},
	{"dev.zed.Zed-Dev", "Zed Dev.app"},
}

// findZed is ExternalEditor.detect: the app of the first build that is installed, or "".
// lookup resolves a bundle identifier to an app, and exists checks a path.
func findZed(lookup func(bundleID string) string, exists func(path string) bool) string {
	for _, build := range zedBuilds {
		if app := lookup(build[0]); app != "" {
			return app
		}
		for _, dir := range []string{"/Applications", filepath.Join(homeDir, "Applications")} {
			if app := filepath.Join(dir, build[1]); exists(app) {
				return app
			}
		}
	}
	return ""
}

// detectZed finds the Zed app on this Mac. Replaced in tests.
var detectZed = func() string {
	return findZed(spotlightApp, pathExists)
}

// spotlightApp asks Spotlight for the app with a bundle identifier, which is how the app's
// NSWorkspace lookup is reached from a command line. "" when it has none or doesn't answer.
func spotlightApp(bundleID string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "mdfind", "kMDItemCFBundleIdentifier == '"+bundleID+"'")
	cmd.WaitDelay = 100 * time.Millisecond
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); strings.HasSuffix(line, ".app") && pathExists(line) {
			return line
		}
	}
	return ""
}

// launchZed opens folder in a new Zed window with the CLI inside the app
// (ExternalEditor.open), or hands it to the app when the CLI is missing or doesn't start.
// Replaced in tests.
var launchZed = func(app, folder string) error {
	cli := filepath.Join(app, "Contents", "MacOS", "cli")
	if info, err := os.Stat(cli); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
		cmd := exec.Command(cli, "--new", folder)
		if cmd.Start() == nil {
			go func() { _ = cmd.Wait() }() // it signals the editor and exits
			return nil
		}
	}
	return exec.Command("open", "-a", app, folder).Run()
}

// pathExists reports whether something is at path. Replaced in tests.
var pathExists = func(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// openFolder opens a folder in Finder, showing its contents. Replaced in tests.
var openFolder = func(path string) error {
	return exec.Command("open", path).Run()
}

// chooseProjectFolder shows the macOS folder chooser with the app's panel message and
// returns the chosen folder: empty when the user cancels. Replaced in tests.
var chooseProjectFolder = func() (string, error) {
	out, err := exec.Command("osascript",
		"-e", "on run argv",
		"-e", "tell current application to activate",
		"-e", "set f to choose folder with prompt (item 1 of argv)",
		"-e", "return POSIX path of f",
		"-e", "end run", "Choose a project folder (a git repo with worktrees, or any folder)").CombinedOutput()
	text := strings.TrimSpace(string(out))
	switch {
	case err == nil:
		// AppleScript ends a folder's path with a slash; jacad keys the project by the path
		// without it, as the app's panel gives it.
		if len(text) > 1 {
			text = strings.TrimRight(text, "/")
		}
		return text, nil
	case strings.Contains(text, "-128"): // userCanceledErr
		return "", nil
	case text != "":
		return "", errors.New(text)
	}
	return "", err
}
