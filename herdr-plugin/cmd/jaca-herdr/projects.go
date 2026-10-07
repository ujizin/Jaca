package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// The Projects pane (the app's ProjectsAreaView and ProjectsModel): the projects Claude Code
// has run in and the folders the user added, each opening to its checkouts, with the app's
// actions on them. Everything it lists comes from jacad's retained projects.state topic.

// autoRefreshTTL is ProjectsModel.autoRefreshTTL: a scan older than this is redone on open.
const autoRefreshTTL = 30 * time.Second

// The view modes, with the values the app stores (ProjectsViewMode).
const (
	modeTree = "tree"
	modeList = "list"
)

// The actions of a row.
const (
	actToggle  = iota // open or close a project
	actFinder         // Open in Finder
	actZed            // Open in Zed
	actCopy           // Copy name
	actActions        // Cache & worktree actions
	actHerdr          // Open in Herdr
	actRemove         // Remove (a folder the user added)
	actMenu           // the row menu
)

// projRow is one row the cursor moves over: a project, or a checkout of an open project.
type projRow struct {
	key        string // what the row is, so a press that waits finds it again
	project    projectRow
	checkout   checkoutRow
	isCheckout bool
	depth      int
	children   int // the projects nested under a project (Tree)
}

func projectKey(path string) string { return "p:" + path }

func checkoutKey(project, checkout string) string { return "c:" + project + "\x00" + checkout }

// projectsViewer is the pane's one screen.
type projectsViewer struct {
	p *pane
	cloudCalls
	// post runs f on the pane's loop from a call still in flight (a launch's progress).
	// Replaced in tests.
	post func(f func())

	state  projectsState
	loaded bool // a projects.state has arrived

	mode     string          // modeTree or modeList
	expanded map[string]bool // the open projects, by path
	rows     []projRow

	selected int
	top      int // the first body line drawn
	follow   bool
	moved    bool // an event changed the rows since they were drawn

	// The size scan's answer, held for the pane's lifetime as the app holds it for a launch.
	sizeApproved, sizeDeclined bool
	seenGeneration             int

	herdr           bool   // Herdr is there to launch in
	zed             string // the Zed app, "" when it is not installed
	herdrCommand    string // the Claude command, "" until it is configured
	herdrConfigured bool
	choosing        bool // the folder chooser is open

	armed      map[string]time.Time // the buttons waiting for their second press
	guard      pressGuard           // so a held key doesn't press over and over
	quietUntil time.Time            // until then keys are ignored: a sheet closed by itself
	toast      string
	toastUntil time.Time
	err        string // why the last call failed
	help       bool
	sheet      cloudSheet
	menu       *popupMenu
	snack      snackbar

	// As last drawn, for the mouse.
	shown    map[int]homeLine // by screen row (1-based)
	bodyRows int
}

// runProjectsPane is the Projects pane.
//
// Keys: j/k or arrows move, Enter or Space opens and closes a project (on a checkout, the row
// menu), Left/Right close and open, o opens the folder in Finder, z in Zed, y copies a
// checkout's name, c opens its cache and worktree actions, h opens it in Herdr, x or
// Backspace removes a folder the user added (press twice to confirm, as in the app), m or a
// right click opens the row menu, a adds a folder, r rescans, t switches Tree and List, s
// opens the Herdr settings, C, N and S answer the disk usage bar, ? shows the keys, q quits.
func runProjectsPane() int {
	c, err := dial()
	if err != nil {
		fmt.Fprintln(os.Stderr, "jaca:", err)
		return 1
	}
	defer c.Close()
	go sendRightClicksToPane()
	go nameOwnTab(projectsTabName)
	return runPane(c, func(p *pane) screen { return newProjectsViewer(p) })
}

// projectsTabName is the pane's tab, named like the Gradle and Xcode ones.
const projectsTabName = "Projects - Jaca"

// ownTabEnv is set by whoever opens a pane in a tab of its own (the key binding in the README)
// and wants the tab named after it. A pane opened beside others leaves their tab's name alone.
const ownTabEnv = "JACA_HERDR_OWN_TAB"

// nameOwnTab renames the tab the pane runs in, when it was opened in one of its own.
func nameOwnTab(name string) {
	tab := os.Getenv("HERDR_TAB_ID")
	if os.Getenv(ownTabEnv) == "" || tab == "" {
		return
	}
	_ = exec.Command(envOr("HERDR_BIN_PATH", "herdr"), "tab", "rename", tab, name).Run()
}

func newProjectsViewer(p *pane) *projectsViewer {
	v := buildProjectsViewer(p, newCloudCalls(p))
	v.herdr = underHerdr()
	v.loadSettings()
	v.open()
	return v
}

// buildProjectsViewer is the viewer before it asks jacad, or this Mac, for anything.
func buildProjectsViewer(p *pane, calls cloudCalls) *projectsViewer {
	return &projectsViewer{
		p: p, cloudCalls: calls, post: func(f func()) { p.post(f) },
		mode: modeTree, expanded: map[string]bool{}, armed: map[string]time.Time{}, follow: true,
	}
}

// loadSettings reads what the pane shares with the app: the view mode and the Claude command.
func (v *projectsViewer) loadSettings() {
	if mode, set, _ := readDefault(viewModeKey); set && (mode == modeTree || mode == modeList) {
		v.mode = mode
	}
	v.takeHerdrCommand(readDefault(herdrCommandKey))
}

// takeHerdrCommand takes a read of the stored Claude command. One that could not be read
// leaves what the pane knows and counts as configured, so the pane neither asks for the
// command again nor writes the default over one it could not see.
func (v *projectsViewer) takeHerdrCommand(command string, set, read bool) {
	switch {
	case !read:
		v.herdrConfigured = true
		if v.herdrCommand == "" {
			v.herdrCommand = defaultHerdrCommand
		}
	case set:
		v.herdrCommand, v.herdrConfigured = command, true
	default:
		v.herdrCommand, v.herdrConfigured = "", false
	}
}

// withHerdrCommand reads the stored command again, off the loop, before a sheet that shows or
// uses it opens: the app may have changed it since the pane started.
func (v *projectsViewer) withHerdrCommand(open func()) {
	v.spawn(func() func() {
		command, set, read := readDefault(herdrCommandKey)
		return func() {
			v.takeHerdrCommand(command, set, read)
			if v.sheet == nil && v.menu == nil && !v.help {
				open()
			}
		}
	})
}

// open subscribes to projects.state, which is retained, so the current state arrives at once,
// and looks for Zed.
func (v *projectsViewer) open() {
	v.spawn(func() func() {
		err := v.call("events.subscribe", map[string]any{"topics": []string{projectsTopic}}, nil, callTimeout)
		return func() { v.failed(err) }
	})
	v.spawn(func() func() {
		app := detectZed()
		return func() { v.zed = app }
	})
}

// leave has nothing to release: the connection closing ends the subscription.
func (v *projectsViewer) leave(bool) {}

func (v *projectsViewer) handleEvent(ev event) {
	switch ev.Topic {
	case projectsTopic:
		if st, err := decodeProjectsState(ev.Data); err == nil {
			v.apply(st)
		}
	case "events.dropped":
		// The skipped update may have been the latest state: fetch it.
		var note droppedNote
		if json.Unmarshal(ev.Data, &note) == nil && note.Topic == projectsTopic {
			v.fetch()
		}
	}
}

func (v *projectsViewer) fetch() {
	v.spawn(func() func() {
		var raw json.RawMessage
		err := v.call("projects.state", nil, &raw, syncTimeout)
		return func() {
			if err != nil {
				v.failed(err)
				return
			}
			if st, err := decodeProjectsState(raw); err == nil {
				v.apply(st)
			}
		}
	})
}

// apply takes a new state (ProjectsModel.apply). The cursor stays on the row it was on, an
// open actions sheet closes when its checkout is gone, and with sizes approved a new scan
// generation asks for the sizes of the new set of checkouts.
func (v *projectsViewer) apply(st projectsState) {
	first := !v.loaded
	at := v.keyAt(v.selected)
	before := v.rowKeys()
	v.state, v.loaded = st, true
	v.rebuild()
	// Rows that changed place are drawn at the next tick: until then a click is not for them.
	v.moved = v.moved || !slices.Equal(before, v.rowKeys())
	v.reselect(at)

	newScan := st.ScanGeneration != v.seenGeneration
	v.seenGeneration = st.ScanGeneration
	if newScan && v.sizeApproved {
		v.requestSizes()
	}
	if sheet, ok := v.sheet.(*actionsSheet); ok {
		if _, found := sheet.current(); !found {
			v.sheet = nil
			v.hush()
		}
	}
	if first && v.shouldAutoRefresh() {
		v.refresh()
	}
}

func (v *projectsViewer) rowKeys() []string {
	keys := make([]string, len(v.rows))
	for i, row := range v.rows {
		keys[i] = row.key
	}
	return keys
}

func (v *projectsViewer) keyAt(i int) string {
	if i < 0 || i >= len(v.rows) {
		return ""
	}
	return v.rows[i].key
}

// reselect puts the cursor back on the row named key, or keeps it inside the rows.
func (v *projectsViewer) reselect(key string) {
	v.selected = clampIndex(v.selected, len(v.rows))
	for i, row := range v.rows {
		if row.key == key {
			v.selected = i
			return
		}
	}
}

// rebuild lays the projects out as rows per the view mode: a project, then, while it is
// open, the projects nested under it and its checkouts (ProjectNodeView).
func (v *projectsViewer) rebuild() {
	nodes := projectFlat(v.state.Projects)
	if v.mode == modeTree {
		nodes = projectTree(v.state.Projects)
	}
	v.rows = v.rows[:0]
	var add func(node projectNode, depth int)
	add = func(node projectNode, depth int) {
		p := node.project
		v.rows = append(v.rows, projRow{key: projectKey(p.Path), project: p, depth: depth, children: len(node.children)})
		if !v.expanded[p.Path] {
			return
		}
		for _, child := range node.children {
			add(child, depth+1)
		}
		for _, c := range p.Checkouts {
			v.rows = append(v.rows, projRow{key: checkoutKey(p.Path, c.Path), project: p, checkout: c, isCheckout: true, depth: depth + 1})
		}
	}
	for _, node := range nodes {
		add(node, 0)
	}
}

// relayout rebuilds the rows after the user changed what shows, keeping the cursor's row.
func (v *projectsViewer) relayout() {
	at := v.keyAt(v.selected)
	v.rebuild()
	v.reselect(at)
	v.follow = true
}

func (r projRow) expandable() bool {
	return !r.isCheckout && (r.children > 0 || len(r.project.Checkouts) > 0)
}

// isFirstLoad and hasNoData are ProjectsModel's: the loader shows only for a first scan with
// nothing to draw yet.
func (v *projectsViewer) isFirstLoad() bool {
	return len(v.state.Projects) == 0 && v.state.IsRefreshing && !v.state.HasCompletedScan
}

func (v *projectsViewer) hasNoData() bool { return len(v.state.Projects) == 0 }

func (v *projectsViewer) shouldAutoRefresh() bool {
	return v.state.LastRefresh.IsZero() || v.now().Sub(v.state.LastRefresh) > autoRefreshTTL
}

func (v *projectsViewer) needsSizeApproval() bool {
	return !v.sizeApproved && !v.sizeDeclined && v.state.sizableCheckouts() > 0
}

// refresh is the app's "Rescan projects". Progress arrives on projects.state.
func (v *projectsViewer) refresh() {
	if v.state.IsRefreshing {
		return
	}
	v.state.IsRefreshing = true
	v.spawn(func() func() {
		err := v.call("projects.refresh", nil, nil, callTimeout)
		return func() {
			if err != nil {
				// jacad refused the call: nothing is refreshing.
				v.state.IsRefreshing = false
				v.failed(err)
			}
		}
	})
}

func (v *projectsViewer) requestSizes() {
	v.spawn(func() func() {
		err := v.call("projects.computeSizes", nil, nil, callTimeout)
		return func() { v.failed(err) }
	})
}

// approveSizeScan, declineSizeScan and cancelSizeScan are ProjectsModel's.
func (v *projectsViewer) approveSizeScan() {
	v.sizeDeclined = false
	if v.sizeApproved {
		return
	}
	v.sizeApproved = true
	v.requestSizes()
}

func (v *projectsViewer) declineSizeScan() {
	v.sizeDeclined = true
	v.cancelSizeScan()
}

func (v *projectsViewer) cancelSizeScan() {
	v.state.IsComputingSizes = false
	v.spawn(func() func() {
		err := v.call("projects.cancelSizes", nil, nil, callTimeout)
		return func() { v.failed(err) }
	})
}

// setMode switches Tree and List and stores the choice where the app reads it.
func (v *projectsViewer) setMode(mode string) {
	if mode == v.mode {
		return
	}
	v.mode = mode
	v.relayout()
	v.spawn(func() func() {
		err := writeDefault(viewModeKey, mode)
		return func() { v.failed(err) }
	})
}

func (v *projectsViewer) toggleMode() {
	if v.mode == modeTree {
		v.setMode(modeList)
	} else {
		v.setMode(modeTree)
	}
}

func (v *projectsViewer) toggle(path string) {
	if v.expanded[path] {
		delete(v.expanded, path)
	} else {
		v.expanded[path] = true
	}
	v.relayout()
}

// confirm is one press of the button named key: the first arms it for armWindow and a second
// within that time confirms.
func (v *projectsViewer) confirm(key string) bool {
	now := v.now()
	// A second press waits before it gets here (pressGuard): what counts is when it was made.
	if until, ok := v.armed[key]; ok && v.guard.pressed(now).Before(until) {
		delete(v.armed, key)
		return true
	}
	v.armed[key] = now.Add(armWindow)
	v.after(armWindow, v.expire)
	v.after(armWindow+keyRepeat.wait(), v.expire)
	return false
}

func (v *projectsViewer) isArmed(key string) bool {
	until, ok := v.armed[key]
	return ok && v.now().Before(until)
}

// flash shows a toast for toastLife. A new one replaces the one showing.
func (v *projectsViewer) flash(msg string) {
	msg = strings.Join(strings.Fields(msg), " ") // one line: git's stderr has several
	v.toast, v.toastUntil = msg, v.now().Add(toastLife)
	v.after(toastLife, v.expire)
}

// expire drops the armed buttons and the toast whose time has passed.
func (v *projectsViewer) expire() {
	now := v.now()
	for key, until := range v.armed {
		if !now.Before(until.Add(keyRepeat.wait())) {
			delete(v.armed, key)
		}
	}
	if v.toast != "" && !now.Before(v.toastUntil) {
		v.toast = ""
	}
}

// failed records why a call failed, for the line above the status bar.
func (v *projectsViewer) failed(err error) {
	if err != nil {
		v.err = "jaca: " + err.Error()
	}
}

// hush is for a sheet that closes by itself: the keys the user is still typing into it don't
// act on the rows.
func (v *projectsViewer) hush() { v.quietUntil = v.now().Add(hushTime) }

// closeSheet closes s if it is still the open sheet.
func (v *projectsViewer) closeSheet(s cloudSheet) {
	if v.sheet == s {
		v.sheet = nil
	}
	// A Delete worktree armed in the sheet does not outlive it, as the app's does not.
	if a, ok := s.(*actionsSheet); ok {
		delete(v.armed, deleteWorktreeKey(a.checkout))
	}
}

// addFolder is ProjectsModel.addFolder: the folder chooser, off the loop, then jacad.
func (v *projectsViewer) addFolder() {
	if v.choosing {
		return
	}
	v.choosing = true
	v.spawn(func() func() {
		path, err := chooseProjectFolder()
		var added bool
		if err == nil && path != "" {
			err = v.call("projects.addFolder", map[string]any{"path": path}, &added, callTimeout)
		}
		return func() {
			v.choosing = false
			switch {
			case err != nil:
				v.failed(err)
			case path == "": // cancelled
			case added:
				v.flash("Added " + filepath.Base(path))
			default:
				v.flash("Already added")
			}
		}
	})
}

func removeProjectKey(path string) string { return "remove:" + path }

// remove is one press of a project's Remove: the second within armWindow takes the folder
// off the list (ProjectsModel.removeUserProject). The folder on disk stays.
func (v *projectsViewer) remove(p projectRow) {
	if !p.isUser() || !v.confirm(removeProjectKey(p.Path)) {
		return
	}
	id := p.Path
	v.spawn(func() func() {
		var name *string
		err := v.call("projects.removeFolder", map[string]any{"id": id}, &name, callTimeout)
		return func() {
			v.failed(err)
			if err == nil && name != nil {
				v.flash("Removed " + *name)
			}
		}
	})
}

// patchCheckout changes a checkout in the state held, until jacad's next state replaces it.
func (v *projectsViewer) patchCheckout(project, checkout string, change func(c *checkoutRow)) {
	for i := range v.state.Projects {
		if v.state.Projects[i].Path != project {
			continue
		}
		for j := range v.state.Projects[i].Checkouts {
			if v.state.Projects[i].Checkouts[j].Path == checkout {
				change(&v.state.Projects[i].Checkouts[j])
			}
		}
	}
	at := v.keyAt(v.selected)
	v.rebuild()
	v.reselect(at)
}

func (v *projectsViewer) checkoutOf(project, checkout string) (checkoutRow, bool) {
	p, ok := v.state.project(project)
	if !ok {
		return checkoutRow{}, false
	}
	return p.checkout(checkout)
}

// clearCache cleans a checkout's build caches (ProjectsModel.clearCache).
func (v *projectsViewer) clearCache(project, checkout string) {
	c, ok := v.checkoutOf(project, checkout)
	if !ok || c.Cleaning {
		return
	}
	v.patchCheckout(project, checkout, func(c *checkoutRow) { c.Cleaning = true })
	v.spawn(func() func() {
		var outcome *struct {
			Name    string  `json:"name"`
			FreedMB int     `json:"freedMB"`
			Error   *string `json:"error"`
		}
		err := v.call("projects.clearCache", map[string]any{"project": project, "checkout": checkout}, &outcome, slowTimeout)
		return func() {
			switch {
			case err != nil:
				v.patchCheckout(project, checkout, func(c *checkoutRow) { c.Cleaning = false })
				v.failed(err)
			case outcome == nil: // already cleaning, or the checkout is gone
				v.patchCheckout(project, checkout, func(c *checkoutRow) { c.Cleaning = false })
			case outcome.Error != nil:
				msg := []rune(*outcome.Error)
				v.flash("Clean failed · " + string(msg[:min(len(msg), 50)]))
			default:
				v.flash(fmt.Sprintf("Freed %s · %s", formatSize(outcome.FreedMB), outcome.Name))
			}
		}
	})
}

// deleteWorktree removes a linked worktree (ProjectsModel.deleteWorktree).
func (v *projectsViewer) deleteWorktree(project, checkout string) {
	c, ok := v.checkoutOf(project, checkout)
	if !ok || c.IsMain {
		return
	}
	v.spawn(func() func() {
		var outcome *struct {
			Name   string `json:"name"`
			OK     bool   `json:"ok"`
			Stderr string `json:"stderr"`
		}
		err := v.call("projects.deleteWorktree", map[string]any{"project": project, "checkout": checkout}, &outcome, slowTimeout)
		return func() {
			switch {
			case err != nil:
				v.failed(err)
			case outcome == nil: // the checkout is gone, or is the main one
			case !outcome.OK:
				if msg := strings.TrimSpace(outcome.Stderr); msg != "" {
					v.flash(msg)
				} else {
					v.flash("Couldn't remove worktree")
				}
			default:
				v.flash("Deleted " + outcome.Name)
			}
		}
	})
}

// openInFinder opens the folder itself. One that is gone does nothing, as in the app.
func (v *projectsViewer) openInFinder(path string) {
	if !pathExists(path) {
		return
	}
	v.spawn(func() func() {
		err := openFolder(path)
		return func() { v.failed(err) }
	})
}

// openInZed opens the folder as a workspace in Zed (ProjectsModel.openInZed).
func (v *projectsViewer) openInZed(path string) {
	if v.zed == "" {
		return
	}
	if !pathExists(path) {
		v.flash("Folder no longer exists")
		return
	}
	app := v.zed
	v.spawn(func() func() {
		err := launchZed(app, path)
		return func() { v.failed(err) }
	})
	v.flash("Opening " + filepath.Base(path) + " in Zed")
}

func (v *projectsViewer) copyName(name string) {
	if err := copyToClipboard(name); err != nil {
		v.failed(err)
		return
	}
	v.snack.show(v.p, copiedNotice)
}

// claudeCommand is ProjectsModel.herdrClaudeCommand: the stored one, else the default.
func (v *projectsViewer) claudeCommand() string {
	if v.herdrConfigured {
		return v.herdrCommand
	}
	return defaultHerdrCommand
}

// persistHerdrCommand stores the command where the app reads it; an empty one is the default.
func (v *projectsViewer) persistHerdrCommand(command string) {
	command = strings.TrimSpace(command)
	if command == "" {
		command = defaultHerdrCommand
	}
	v.herdrCommand, v.herdrConfigured = command, true
	v.spawn(func() func() {
		err := writeDefault(herdrCommandKey, command)
		return func() { v.failed(err) }
	})
}

func (v *projectsViewer) openHerdrSettings() {
	if v.herdr {
		v.withHerdrCommand(func() { v.sheet = newHerdrConfigSheet(v) })
	}
}

// openInHerdr starts a launch for a project root or a worktree: the sheet asks for the
// session's name first (ProjectsModel.openInHerdr).
func (v *projectsViewer) openInHerdr(row projRow) {
	if !v.canAct(row, actHerdr) {
		return
	}
	target := herdrTarget{
		projectRoot: row.project.Path, projectName: row.project.name(),
		folder: row.project.Path, hasGit: row.project.IsGitRepo,
	}
	if row.isCheckout {
		target.folder, target.isWorktree = row.checkout.Path, !row.checkout.IsMain
	}
	v.withHerdrCommand(func() { v.sheet = newHerdrLaunchSheet(v, target) })
}

// launchHerdr runs the launch off the loop, with a toast for each step and for the result
// (ProjectsModel.launchHerdr).
func (v *projectsViewer) launchHerdr(target herdrTarget) {
	command := v.claudeCommand()
	post := v.post
	v.spawn(func() func() {
		workspace, tab, err := herdrLaunch(target, command, func(msg string) {
			post(func() { v.flash(msg) })
		})
		return func() {
			if err != nil {
				v.flash("Herdr: " + err.Error())
				return
			}
			v.flash("Launched in Herdr · " + workspace + "/" + tab)
		}
	})
}

// herdrLabel is the tooltip of a row's Herdr button: a project and its main checkout start a
// new worktree, a linked worktree opens as it is.
func herdrLabel(row projRow) string {
	if row.isCheckout && !row.checkout.IsMain {
		return "Open in Herdr"
	}
	return "Open in Herdr (new worktree)"
}

// canAct reports whether a row has an action, as the app shows or hides its buttons.
func (v *projectsViewer) canAct(row projRow, action int) bool {
	removing := row.isCheckout && row.checkout.Removing
	switch action {
	case actToggle:
		return row.expandable()
	case actFinder:
		return !removing
	case actZed:
		return v.zed != "" && !removing
	case actCopy:
		return row.isCheckout && !removing
	case actActions:
		return row.isCheckout && !row.checkout.Cleaning && !removing
	case actHerdr:
		return v.herdr && row.project.isClaudeProject() && !removing
	case actRemove:
		return !row.isCheckout && row.project.isUser()
	case actMenu:
		return !removing
	}
	return false
}

// act does one of a row's actions, if the row has it.
func (v *projectsViewer) act(row projRow, action int) {
	if !v.canAct(row, action) {
		return
	}
	path := row.project.Path
	if row.isCheckout {
		path = row.checkout.Path
	}
	switch action {
	case actToggle:
		v.toggle(row.project.Path)
	case actFinder:
		v.openInFinder(path)
	case actZed:
		v.openInZed(path)
	case actCopy:
		v.copyName(row.checkout.name())
	case actActions:
		v.sheet = newActionsSheet(v, row.project.Path, row.checkout.Path)
	case actHerdr:
		v.openInHerdr(row)
	case actRemove:
		v.remove(row.project)
	case actMenu:
		v.openMenu(row, 0, 0)
	}
}

// pressRow is an action on the selected row for the guard to run, now or after a wait: it
// does nothing when the row under the cursor is no longer the one the key was pressed on.
func (v *projectsViewer) pressRow(action int) func() {
	if v.moved || v.selected < 0 || v.selected >= len(v.rows) {
		return func() {} // also for rows an update moved and the screen doesn't show yet
	}
	key := v.rows[v.selected].key
	return func() {
		if v.sheet == nil && v.selected >= 0 && v.selected < len(v.rows) && v.rows[v.selected].key == key {
			v.follow = true // the row a key presses is shown, with its armed button
			v.act(v.rows[v.selected], action)
		}
	}
}

// menuItems are a row's actions with the app's labels, in the order of its buttons.
func (v *projectsViewer) menuItems(row projRow) []menuItem {
	var items []menuItem
	add := func(label string, action int) {
		if v.canAct(row, action) {
			key := row.key
			items = append(items, menuItem{label, func() {
				// The row as it is now: the menu may have been open across an update.
				for _, current := range v.rows {
					if current.key == key {
						v.act(current, action)
						return
					}
				}
			}})
		}
	}
	add(herdrLabel(row), actHerdr)
	add("Open in Zed", actZed)
	add("Copy name", actCopy)
	add("Open in Finder", actFinder)
	add("Cache & worktree actions", actActions)
	if v.isArmed(removeProjectKey(row.project.Path)) {
		add("Confirm?", actRemove)
	} else {
		add("Remove", actRemove)
	}
	return items
}

// openMenu shows the row's menu at a cell, or beside the row when a key opened it.
func (v *projectsViewer) openMenu(row projRow, x, y int) {
	items := v.menuItems(row)
	if len(items) == 0 {
		return
	}
	if x == 0 {
		x, y = 5, 2
		for screenRow := 1; screenRow <= len(v.shown); screenRow++ {
			if line, ok := v.shown[screenRow]; ok && line.item == v.selected {
				y = screenRow + 1
				break
			}
		}
	}
	v.menu = &popupMenu{x: x, y: y, items: items}
}

func (v *projectsViewer) move(by int) {
	v.selected = clampIndex(v.selected+by, len(v.rows))
	v.follow = true
}

// collapse closes the selected project, or goes to the project a row is under.
func (v *projectsViewer) collapse() {
	if v.selected < 0 || v.selected >= len(v.rows) {
		return
	}
	row := v.rows[v.selected]
	if !row.isCheckout && v.expanded[row.project.Path] {
		v.toggle(row.project.Path)
		return
	}
	for i := v.selected - 1; i >= 0; i-- {
		if !v.rows[i].isCheckout && v.rows[i].depth < row.depth {
			v.selected, v.follow = i, true
			return
		}
	}
}

func (v *projectsViewer) expand() {
	if v.selected < 0 || v.selected >= len(v.rows) {
		return
	}
	if row := v.rows[v.selected]; row.expandable() && !v.expanded[row.project.Path] {
		v.toggle(row.project.Path)
	}
}

func (v *projectsViewer) handleKey(k []byte) bool {
	if len(k) == 1 && k[0] == 0x03 {
		return true
	}
	if m, ok := parseMouse(k); ok {
		if m.press && m.button == 0 {
			v.guard.drop()
		}
		v.mouse(m)
		return false
	}
	v.guard.note(k, v.now()) // a sheet's keys too: Enter held past its close is still held
	if v.help {
		if isEsc(k) || isEnter(k) || (len(k) == 1 && (k[0] == 'q' || k[0] == '?')) {
			v.help = false
		}
		return false
	}
	if v.sheet != nil {
		v.sheet.key(k)
		return false
	}
	if v.menu != nil {
		v.menuKey(k)
		return false
	}
	// Keys still arriving from a sheet that closed on its own don't act on the rows.
	if v.now().Before(v.quietUntil) {
		return false
	}
	v.err = "" // any key dismisses the last failure
	press := func(act func()) { v.guard.press(v.after, act) }
	if nav, ok := decodeNav(k); ok && (nav.dir == "left" || nav.dir == "right") {
		if nav.dir == "left" {
			v.collapse()
		} else {
			v.expand()
		}
		return false
	}
	switch {
	case isPageUp(k):
		v.move(-max(1, v.bodyRows/2))
	case isPageDown(k):
		v.move(max(1, v.bodyRows/2))
	case isUp(k):
		v.move(-1)
	case isDown(k):
		v.move(1)
	case isEnter(k), string(k) == " ":
		switch {
		case v.loaded && v.hasNoData() && !v.isFirstLoad(): // the empty state's button
			press(v.addFolder)
		case v.selected < len(v.rows) && v.rows[v.selected].isCheckout:
			press(v.pressRow(actMenu))
		default:
			press(v.pressRow(actToggle))
		}
	case len(k) != 1:
	case k[0] == 'q':
		return true
	case k[0] == '?':
		v.help = true
	case k[0] == 'o':
		press(v.pressRow(actFinder))
	case k[0] == 'z':
		press(v.pressRow(actZed))
	case k[0] == 'y':
		press(v.pressRow(actCopy))
	case k[0] == 'c':
		press(v.pressRow(actActions))
	case k[0] == 'h':
		press(v.pressRow(actHerdr))
	case k[0] == 'm':
		press(v.pressRow(actMenu))
	case k[0] == 'x' || k[0] == 0x7f || k[0] == 0x08:
		press(v.pressRow(actRemove))
	case k[0] == 'a':
		press(v.addFolder)
	case k[0] == 'r':
		press(v.refresh)
	case k[0] == 't':
		press(v.toggleMode)
	case k[0] == 's':
		press(v.openHerdrSettings)
	case k[0] == 'C':
		press(func() {
			if v.needsSizeApproval() {
				v.approveSizeScan()
			}
		})
	case k[0] == 'N':
		press(func() {
			if v.needsSizeApproval() {
				v.declineSizeScan()
			}
		})
	case k[0] == 'S':
		press(func() {
			if !v.needsSizeApproval() && v.state.IsComputingSizes {
				v.cancelSizeScan()
			}
		})
	}
	return false
}

// menuKey: arrows (or j/k) move, Enter runs the item, Esc or any other key closes the menu.
func (v *projectsViewer) menuKey(k []byte) {
	menu := v.menu
	switch {
	case isUp(k):
		menu.move(-1)
	case isDown(k):
		menu.move(1)
	case isEnter(k):
		v.guard.press(v.after, func() {
			if v.menu != menu {
				return
			}
			v.menu = nil
			if i := menu.selected; i >= 0 && i < len(menu.items) && menu.items[i].act != nil {
				menu.items[i].act()
			}
		})
	default:
		v.menu = nil
	}
}

func (v *projectsViewer) mouse(m mouseEvent) {
	const wheelUp, wheelDown, wheelLines, right = 64, 65, 3, 2
	if !m.press {
		return
	}
	if v.help {
		if m.button == 0 {
			v.help = false
		}
		return
	}
	if v.sheet != nil {
		v.sheet.mouse(m)
		return
	}
	if menu := v.menu; menu != nil {
		// A click on an item runs it; any other press closes the menu.
		if m.button != 0 && m.button != right {
			return
		}
		v.menu = nil
		if i := menu.itemAt(m.x, m.y); m.button == 0 && i >= 0 && menu.items[i].act != nil {
			menu.items[i].act()
		}
		return
	}
	switch {
	case m.button == wheelUp:
		v.top, v.follow = max(0, v.top-wheelLines), false
	case m.button == wheelDown:
		v.top, v.follow = v.top+wheelLines, false
	case m.button == 0 || m.button == right:
		v.err = ""
		line, ok := v.shown[m.y]
		if !ok {
			return
		}
		if line.item >= 0 && v.moved {
			return // the row under the pointer is not the one on screen: the click is dropped
		}
		if line.item >= 0 {
			v.selected = clampIndex(line.item, len(v.rows))
		}
		if m.button == right {
			if line.item >= 0 && line.item < len(v.rows) && v.canAct(v.rows[line.item], actMenu) {
				v.openMenu(v.rows[line.item], m.x, m.y)
			}
			return
		}
		for _, hit := range line.hits {
			if m.x >= hit.x0 && m.x <= hit.x1 {
				hit.act()
				return
			}
		}
	}
}

// helpKeys are the pane's keys for the ? popup. Labels are the app's buttons and tooltips
// where it has one; the rest are placeholders that need specified copy.
func (v *projectsViewer) helpKeys() [][2]string {
	keys := [][2]string{
		{"j  k", "Select row"},
		{"Enter  Space  ←  →", "Expand / collapse"},
		{"o", "Open in Finder"},
	}
	if v.zed != "" {
		keys = append(keys, [2]string{"z", "Open in Zed"})
	}
	keys = append(keys,
		[2]string{"y", "Copy name"},
		[2]string{"c", "Cache & worktree actions"})
	if v.herdr {
		keys = append(keys, [2]string{"h", "Open in Herdr"})
	}
	keys = append(keys,
		[2]string{"x  Backspace", "Remove"},
		[2]string{"m", "Row menu"},
		[2]string{"a", "Add a project folder"},
		[2]string{"r", "Rescan projects"},
		[2]string{"t", "Tree / List"})
	if v.herdr {
		keys = append(keys, [2]string{"s", "Herdr settings"})
	}
	switch {
	case v.needsSizeApproval():
		keys = append(keys, [2]string{"C", "Calculate"}, [2]string{"N", "Not now"})
	case v.state.IsComputingSizes:
		keys = append(keys, [2]string{"S", "Stop"})
	}
	return append(keys,
		[2]string{"PgUp  PgDn", "Scroll"},
		[2]string{"?", "Help"},
		[2]string{"q", "Quit"})
}

func (v *projectsViewer) draw() {
	rows, cols := termSize()
	paintRows(v.compose(rows, cols))
}

// compose is the whole pane: the frame with the open popup and the copy notice over it.
func (v *projectsViewer) compose(rows, cols int) []string {
	frame := v.frame(rows, cols)
	switch {
	case v.help:
		if box := keysBox(v.helpKeys(), rows, cols); len(box) > 0 {
			frame = overlay(frame, box, max(0, (rows-len(box))/2), max(0, (cols-cellWidth(stripSGR(box[0])))/2))
		} else {
			v.help = false // no room to draw it: it is closed, not left holding the keys unseen
		}
	case v.sheet != nil:
		if box, top, left := v.sheet.box(rows, cols); len(box) > 0 {
			frame = overlay(frame, box, top, left)
		} else {
			v.closeSheet(v.sheet) // no room to draw it: it is closed, not left holding the keys unseen
		}
	case v.menu != nil:
		if box := v.menu.box(rows, cols); len(box) > 0 {
			frame = overlay(frame, box, v.menu.top-1, v.menu.left-1)
		} else {
			v.menu = nil // no room to draw it
		}
	}
	return v.snack.over(frame, rows, cols)
}

// frame is the pane without its popup, rows rows of at most cols cells: the header and the
// disk usage bar, the rows scrolled to the selected one, and in a pane tall enough for them
// the toast line and the status bar on the last two rows.
func (v *projectsViewer) frame(rows, cols int) []string {
	v.moved = false
	v.selected = clampIndex(v.selected, len(v.rows))
	head, body := v.headLines(cols), v.bodyLines(cols)
	footer := rows >= len(head)+3
	room := max(1, rows-len(head))
	if footer {
		room = rows - len(head) - 2
	}
	if v.follow {
		first, last := -1, -1
		for i, line := range body {
			if line.item == v.selected {
				if first < 0 {
					first = i
				}
				last = i
			}
		}
		switch {
		case first < 0:
		case v.selected == 0 && last < room: // the first row shows from the top
			v.top = 0
		case first < v.top:
			v.top = first
		case last >= v.top+room:
			v.top = max(0, min(first, last-room+1))
		}
		v.follow = false
	}
	v.top = max(0, min(v.top, len(body)-room))
	v.bodyRows = room

	lines := append([]homeLine(nil), head...)
	lines = append(lines, body[v.top:min(len(body), v.top+room)]...)
	if footer {
		for len(lines) < rows-2 {
			lines = append(lines, plainLine(""))
		}
		notice := ""
		switch {
		case v.toast != "":
			notice = sgrBold + clip(sanitize(v.toast), cols) + sgrReset
		case v.err != "":
			notice = sgrRed + clip(sanitize(v.err), cols) + sgrReset
		}
		lines = append(lines, plainLine(notice), v.statusBar(cols))
	}
	for len(lines) < rows {
		lines = append(lines, plainLine(""))
	}
	if len(lines) > rows {
		lines = lines[:max(0, rows)]
	}
	v.shown = map[int]homeLine{}
	frame := make([]string, len(lines))
	for i, line := range lines {
		// A row the layout couldn't fit is cut as plain text and loses its buttons.
		if cellWidth(stripSGR(line.text)) > cols {
			line = homeLine{text: clip(stripSGR(line.text), cols), item: line.item}
		}
		frame[i] = line.text
		v.shown[i+1] = line
	}
	return frame
}

// statusBar holds Help at its right, as the other viewers' do.
func (v *projectsViewer) statusBar(cols int) homeLine {
	const help = "Help"
	line := plainLine("")
	if cols < len(help) {
		return line
	}
	line.text = strings.Repeat(" ", cols-len(help))
	line.button(sgrUnder+help+sgrReset, func() { v.help = true })
	return line
}

// headLines are the rows that stay put while there are projects: the header
// (ProjectsAreaView.header, with the sidebar header's two buttons) and the disk usage bar.
func (v *projectsViewer) headLines(cols int) []homeLine {
	if !v.loaded || v.hasNoData() {
		return nil
	}
	kindOf := func(mode string) int {
		if v.mode == mode {
			return buttonPrimary
		}
		return buttonPlain
	}
	buttons := []homeButton{
		{label: "Add a project folder", act: v.addFolder},
		{label: "Rescan projects", act: v.refresh},
	}
	if v.herdr {
		buttons = append(buttons, homeButton{label: "Herdr settings", act: v.openHerdrSettings})
	}
	buttons = append(buttons,
		homeButton{label: "Tree", kind: kindOf(modeTree), act: func() { v.setMode(modeTree) }},
		homeButton{label: "List", kind: kindOf(modeList), act: func() { v.setMode(modeList) }})
	// The switch shows whole or not at all.
	buttons = fitButtons(buttons, max(0, cols-len("PROJECTS")-2))
	if len(buttons) == 1 {
		buttons = nil
	}
	counts := fmt.Sprintf("%d projects · %d worktrees", len(v.state.Projects), v.state.totalWorktrees())
	refreshing := ""
	if v.state.IsRefreshing {
		refreshing = "Refreshing…"
	}
	lines := []homeLine{
		splitLine("", "PROJECTS", sgrDim, buttons, cols, -1),
		plainLine(leftRight(counts, "", refreshing, sgrDim, cols)),
		plainLine(""),
	}
	return append(lines, v.sizeScanLines(cols)...)
}

// leftRight is a row with one text at its left and another flush right. The right one is
// left out where both don't fit.
func leftRight(left, leftStyle, right, rightStyle string, cols int) string {
	room := cols - cellWidth(left) - 2 - cellWidth(right)
	if right == "" || room < 0 {
		return leftStyle + clip(left, cols) + sgrReset
	}
	return leftStyle + left + sgrReset + strings.Repeat(" ", room+2) + rightStyle + right + sgrReset
}

// sizeScanLines is the disk usage bar (ProjectsAreaView.sizeScanBar): the question before a
// scan with its two answers, or the scan running with Stop.
func (v *projectsViewer) sizeScanLines(cols int) []homeLine {
	var title string
	var buttons []homeButton
	switch {
	case v.needsSizeApproval():
		title = "Calculate disk usage?"
		if v.state.IsComputingSizes { // another client's scan, as the app's bar reads then
			title = "Calculating disk usage…"
		}
		buttons = []homeButton{
			{label: "Not now", act: v.declineSizeScan},
			{label: "Calculate", kind: buttonPrimary, act: v.approveSizeScan},
		}
	case v.state.IsComputingSizes:
		title = "Calculating disk usage…"
		buttons = []homeButton{{label: "Stop", act: v.cancelSizeScan}}
	default:
		return nil
	}
	lines := []homeLine{splitLine("", title, sgrBold, buttons, cols, -1)}
	text := fmt.Sprintf("Reads every file in %d checkouts. Cached sizes stay on screen either way.", v.state.sizableCheckouts())
	for _, part := range wrapCapped(text, cols, 2) {
		lines = append(lines, plainLine(sgrDim+part+sgrReset))
	}
	return append(lines, plainLine(""))
}

// bodyLines are the rows that scroll: the loader of a first scan, the empty state, or two
// lines for each project and checkout. Before the first state arrives nothing is drawn.
func (v *projectsViewer) bodyLines(cols int) []homeLine {
	switch {
	case !v.loaded:
		return nil
	case v.isFirstLoad():
		return []homeLine{plainLine(""), centered("Scanning ~/.claude/projects…", sgrDim, cols)}
	case v.hasNoData():
		return v.emptyLines(cols)
	}
	lines := make([]homeLine, 0, 2*len(v.rows))
	for i, row := range v.rows {
		lines = append(lines, v.rowLines(i, row, cols)...)
	}
	return lines
}

// emptyLines is the pane with no projects (ProjectsAreaView.emptyState).
func (v *projectsViewer) emptyLines(cols int) []homeLine {
	lines := []homeLine{plainLine(""), centered("No projects found", sgrBold, cols)}
	for _, part := range wrapWords("Projects Claude Code has run in appear here automatically. You can also add any folder.", cols) {
		lines = append(lines, centered(part, sgrDim, cols))
	}
	button := homeButton{label: "Add folder…", kind: buttonPrimary, act: v.addFolder, focused: true}
	row := plainLine("")
	if button.width() <= cols {
		row.text = strings.Repeat(" ", (cols-button.width())/2)
		row.button(button.draw(), button.act)
	}
	return append(lines, plainLine(""), row)
}

// rowLines are a row's two lines. The first has the name, the tags and the size, and opens
// or closes a project on a click. The second has the path, and on the selected row the
// buttons of its actions: the app draws them on every row, which a terminal has no room for.
func (v *projectsViewer) rowLines(i int, row projRow, cols int) []homeLine {
	selected := i == v.selected
	indent := strings.Repeat(" ", min(2*row.depth, max(0, cols/4)))
	chevron := "  "
	switch {
	case row.expandable() && v.expanded[row.project.Path]:
		chevron = "▾ "
	case row.expandable():
		chevron = "▸ "
	}
	prefix := rowMarker(selected) + indent

	var name, path string
	var tags []cell
	var value cell
	if row.isCheckout {
		c := row.checkout
		name, path = c.name(), c.subtitle(v.now())
		tags = append(tags, cell{c.typeTag(), sgrDim})
		switch {
		case c.HasClaudeSessions:
			tags = append(tags, cell{"Claude", sgrGreen})
		case c.IsClaudeManaged:
			tags = append(tags, cell{"No project", sgrDim})
		}
		if c.Orphan {
			tags = append(tags, cell{"Orphan", sgrYellow})
		}
		value = cell{c.sizeText(), ""}
		if c.Dropped {
			value.style = sgrGreen
		}
	} else {
		p := row.project
		name, path = p.name(), p.displayPath()
		if p.isClaudeProject() {
			tags = append(tags, cell{"Claude", sgrGreen})
		}
		if row.children > 0 {
			tags = append(tags, cell{projectsTag(row.children), sgrDim})
		}
		if n := p.worktreeCount(); n > 0 {
			tags = append(tags, cell{worktreesTag(n), sgrDim})
		}
		if p.IsGitRepo && p.sizesComputed() {
			value = cell{formatSize(p.totalSizeMB()), ""}
		}
	}

	first := homeLine{text: nameLine(prefix+chevron, sanitize(name), tags, value, cols), item: i}
	if row.expandable() {
		first.hits = []homeHit{{x0: 1, x1: cols, act: func() { v.toggle(row.project.Path) }}}
	}
	var buttons []homeButton
	if selected {
		buttons = v.rowButtons(row)
	}
	second := pathLine(prefix+"  ", sanitize(path), buttons, cols, i)
	if row.isCheckout && row.checkout.Removing {
		// A worktree being deleted fades out in the app; here it is dimmed and takes no clicks.
		first = homeLine{text: sgrDim + stripSGR(first.text) + sgrReset, item: i}
		second = homeLine{text: sgrDim + stripSGR(second.text) + sgrReset, item: i}
	}
	return []homeLine{first, second}
}

// rowButtons are a row's actions as buttons, in the app's order. The app's buttons are
// icons; their tooltips are the labels here.
func (v *projectsViewer) rowButtons(row projRow) []homeButton {
	var buttons []homeButton
	add := func(label string, action, kind int) {
		if v.canAct(row, action) {
			buttons = append(buttons, homeButton{label: label, kind: kind, act: func() { v.act(row, action) }})
		}
	}
	if v.canAct(row, actRemove) {
		if v.isArmed(removeProjectKey(row.project.Path)) {
			buttons = append(buttons, homeButton{label: "Confirm?", kind: buttonArmed, act: func() { v.act(row, actRemove) }})
		} else {
			buttons = append(buttons, homeButton{label: "Remove", kind: buttonCritical, reserve: 2, act: func() { v.act(row, actRemove) }})
		}
	}
	add(herdrLabel(row), actHerdr, buttonPlain)
	add("Open in Zed", actZed, buttonPlain)
	add("Copy name", actCopy, buttonPlain)
	add("Open in Finder", actFinder, buttonPlain)
	if row.isCheckout && row.checkout.Cleaning && !row.checkout.Removing {
		// The app disables the gear while the checkout is cleaning.
		buttons = append(buttons, homeButton{label: "Cache & worktree actions"})
	} else {
		add("Cache & worktree actions", actActions, buttonPlain)
	}
	return buttons
}

// fitRowButtons leaves out buttons from the first until the rest fit in w cells. Remove goes
// last, so the button that is waiting for its second press stays in view.
func fitRowButtons(buttons []homeButton, w int) []homeButton {
	for len(buttons) > 0 && buttonsWidth(buttons) > w {
		drop := 0
		for i, b := range buttons {
			if b.kind != buttonCritical && b.kind != buttonArmed {
				drop = i
				break
			}
		}
		buttons = append(buttons[:drop:drop], buttons[drop+1:]...)
	}
	return buttons
}

func tagsWidth(tags []cell) int {
	w := 0
	for _, tag := range tags {
		w += 2 + cellWidth(tag.text)
	}
	return w
}

// nameLine is a row's first line: the name in bold and its tags, with the value flush right.
// In a narrow pane the value goes first, then the tags from the last, then the name is cut.
func nameLine(prefix, name string, tags []cell, value cell, cols int) string {
	room := cols - cellWidth(stripSGR(prefix))
	valueW := cellWidth(value.text)
	if valueW > 0 && room-valueW-2 < 8 {
		valueW = 0
	}
	left := room
	if valueW > 0 {
		left -= valueW + 2
	}
	for len(tags) > 0 && left-tagsWidth(tags) < min(cellWidth(name), 12) {
		tags = tags[:len(tags)-1]
	}
	nameW := left - tagsWidth(tags)
	if nameW <= 0 {
		return prefix
	}
	name = clip(name, nameW)
	var b strings.Builder
	b.WriteString(prefix + sgrBold + name + sgrReset)
	used := cellWidth(name)
	for _, tag := range tags {
		b.WriteString("  " + tag.style + tag.text + sgrReset)
		used += 2 + cellWidth(tag.text)
	}
	if valueW > 0 {
		b.WriteString(strings.Repeat(" ", max(0, room-used-valueW)) + value.style + value.text + sgrReset)
	}
	return b.String()
}

// pathLine is a row's second line: the path, cut in its middle as the app cuts it, with the
// buttons flush right. Buttons are left out while the path would get under a short path's
// worth of cells.
func pathLine(prefix, path string, buttons []homeButton, cols, item int) homeLine {
	line := homeLine{text: prefix, item: item}
	room := cols - line.width()
	buttons = fitRowButtons(buttons, max(0, room-min(cellWidth(path), 32)-2))
	bw := buttonsWidth(buttons)
	textW := room
	if bw > 0 {
		textW -= bw + 2
	}
	if textW > 0 {
		line.text += sgrDim + clipMiddle(path, textW) + sgrReset
	}
	if bw > 0 {
		line.text += strings.Repeat(" ", max(0, cols-line.width()-bw))
		line.buttons(buttons)
	}
	return line
}
