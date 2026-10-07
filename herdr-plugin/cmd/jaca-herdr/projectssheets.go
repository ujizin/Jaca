package main

import (
	"fmt"
	"strings"
)

// The Projects pane's sheets: a checkout's actions (CheckoutActionsSheet), the Herdr launch
// (HerdrLaunchSheet) and the Herdr settings (HerdrConfigSheet). Each is a bordered popup with
// focus stops: Tab and Shift-Tab move between them, Enter presses, Esc closes.

// actionsSheet is the app's CheckoutActionsSheet: Clean build cache, and for a linked
// worktree Delete worktree, which takes two presses.
type actionsSheet struct {
	sheetFrame
	v                 *projectsViewer
	project, checkout string // by path: the sheet follows the checkout across states
}

// The actions sheet's stops.
const (
	actionsStopClean = iota
	actionsStopDelete
	actionsStopClose
)

func newActionsSheet(v *projectsViewer, project, checkout string) *actionsSheet {
	delete(v.armed, deleteWorktreeKey(checkout))
	s := &actionsSheet{v: v, project: project, checkout: checkout}
	// Close has the focus at first: Enter on a sheet just opened cleans or deletes nothing.
	s.stop = len(s.stops()) - 1
	return s
}

func (s *actionsSheet) current() (checkoutRow, bool) { return s.v.checkoutOf(s.project, s.checkout) }

// stops are the sheet's buttons in order. The main checkout has no Delete.
func (s *actionsSheet) stops() []int {
	if c, ok := s.current(); ok && !c.IsMain {
		return []int{actionsStopClean, actionsStopDelete, actionsStopClose}
	}
	return []int{actionsStopClean, actionsStopClose}
}

func (s *actionsSheet) focused() int {
	stops := s.stops()
	return stops[clampIndex(s.stop, len(stops))]
}

func (s *actionsSheet) close() { s.v.closeSheet(s) }

func deleteWorktreeKey(checkout string) string { return "delete:" + checkout }

// clean starts the cleaning and closes the sheet, as in the app: there is no confirm.
func (s *actionsSheet) clean() {
	if c, ok := s.current(); !ok || c.Cleaning {
		return
	}
	s.v.clearCache(s.project, s.checkout)
	s.close()
}

// pressDelete is one press of Delete worktree: the second within armWindow deletes and
// closes the sheet.
func (s *actionsSheet) pressDelete() {
	if c, ok := s.current(); !ok || c.IsMain {
		return
	}
	if !s.v.confirm(deleteWorktreeKey(s.checkout)) {
		return
	}
	s.v.deleteWorktree(s.project, s.checkout)
	s.close()
}

// press runs act through the guard, so a held key neither cleans again nor confirms the
// delete its first press armed. It does nothing once the sheet has closed.
func (s *actionsSheet) press(act func()) {
	s.v.guard.press(s.v.after, func() {
		if s.v.sheet == cloudSheet(s) {
			act()
		}
	})
}

func (s *actionsSheet) key(k []byte) {
	switch {
	case isEsc(k):
		s.close()
	case s.hidden:
	case s.tab(k, len(s.stops())):
	case isEnter(k):
		switch s.focused() {
		case actionsStopClean:
			s.press(s.clean)
		case actionsStopDelete:
			s.press(s.pressDelete)
		default:
			s.press(s.close)
		}
	}
}

func (s *actionsSheet) mouse(m mouseEvent) { s.click(m) }

// box is the sheet whole or not at all: with rows cut away, a button could be pressed without
// being on screen.
func (s *actionsSheet) box(rows, cols int) ([]string, int, int) {
	b := s.build(rows, cols)
	if b != nil && len(b.rows)+1 > rows {
		b = nil
	}
	return s.place(b, rows, cols)
}

func (s *actionsSheet) build(rows, cols int) *popupBox {
	c, ok := s.current()
	w := min(cols, 68)
	if !ok || w < 40 || rows < 12 {
		return nil
	}
	focused := s.focused()
	b := newPopupBox(clip(sanitize(c.name()), w-8), w)
	b.closeRow(s.close)
	b.add(sgrDim + c.typeTag() + sgrReset)
	b.add(sgrDim + clipMiddle(sanitize(c.displayPath()), b.inner) + sgrReset)
	b.add("")

	b.add(sgrBold + "Clean build cache" + sgrReset)
	sheetText(b, "Runs `./gradlew clean` (if present) and deletes the matching Xcode DerivedData. Frees build-cache space — safe to do, but the next build will be slower while it regenerates.", sgrDim, 6)
	b.add(sgrDim + clip(fmt.Sprintf("Cache here: %s of %s", formatSize(c.CacheMB), formatSize(c.SizeMB)), b.inner) + sgrReset)
	clean := homeButton{label: "Clean cache", act: s.clean, focused: focused == actionsStopClean}
	if c.Cleaning {
		clean.label, clean.act = "Cleaning…", nil
	}
	row := ""
	sheetButton(b, &row, clean)
	b.add(row)
	b.add("")

	if c.IsMain {
		sheetText(b, "This is the project's main checkout, so it can't be removed — only its build cache can be cleaned.", sgrDim, 4)
	} else {
		b.add(sgrBold + "Delete worktree" + sgrReset)
		if c.SizeComputed {
			b.add(sgrDim + formatSize(c.SizeMB) + " on disk" + sgrReset)
		}
		del := homeButton{label: "Delete worktree", kind: buttonCritical, act: s.pressDelete, focused: focused == actionsStopDelete}
		if s.v.isArmed(deleteWorktreeKey(s.checkout)) {
			del.label, del.kind = "Confirm delete?", buttonArmed
		}
		row = ""
		sheetButton(b, &row, del)
		b.add(row)
	}
	b.add("")
	sheetButtons(b, []homeButton{{label: "Close", act: s.close, focused: focused == actionsStopClose}})
	return b
}

// herdrLaunchSheet is the app's HerdrLaunchSheet: the session's name, which labels the Herdr
// tab and names the new worktree, and on the first launch the Claude command too.
type herdrLaunchSheet struct {
	sheetFrame
	v             *projectsViewer
	target        herdrTarget
	name, command textInput
	needsConfig   bool // the command has not been confirmed yet: this sheet asks for it
}

// The launch sheet's stops. The command is one only on the first launch.
const (
	launchStopName = iota
	launchStopCommand
	launchStopCancel
	launchStopOpen
)

func newHerdrLaunchSheet(v *projectsViewer, target herdrTarget) *herdrLaunchSheet {
	s := &herdrLaunchSheet{v: v, target: target, needsConfig: !v.herdrConfigured}
	s.command.set(v.claudeCommand())
	return s
}

func (s *herdrLaunchSheet) stops() []int {
	if s.needsConfig {
		return []int{launchStopName, launchStopCommand, launchStopCancel, launchStopOpen}
	}
	return []int{launchStopName, launchStopCancel, launchStopOpen}
}

func (s *herdrLaunchSheet) focused() int {
	stops := s.stops()
	return stops[clampIndex(s.stop, len(stops))]
}

func (s *herdrLaunchSheet) focus(stop int) {
	for i, candidate := range s.stops() {
		if candidate == stop {
			s.stop = i
		}
	}
}

func (s *herdrLaunchSheet) close() { s.v.closeSheet(s) }

func (s *herdrLaunchSheet) canOpen() bool { return strings.TrimSpace(s.name.String()) != "" }

// submit is ProjectsModel.confirmHerdrLaunch: the command is stored on the first launch, the
// tab is named, and the launch starts.
func (s *herdrLaunchSheet) submit() {
	if !s.canOpen() {
		return
	}
	if s.needsConfig {
		s.v.persistHerdrCommand(s.command.String())
	}
	s.close()
	target := s.target
	target.tabName = strings.TrimSpace(s.name.String())
	s.v.launchHerdr(target)
}

func (s *herdrLaunchSheet) press(act func()) {
	s.v.guard.press(s.v.after, func() {
		if s.v.sheet == cloudSheet(s) {
			act()
		}
	})
}

func (s *herdrLaunchSheet) key(k []byte) {
	switch {
	case isEsc(k):
		s.close()
	case s.hidden:
	case s.tab(k, len(s.stops())):
	case isEnter(k):
		if s.focused() == launchStopCancel {
			s.press(s.close)
		} else {
			s.press(s.submit)
		}
	case s.focused() == launchStopName:
		s.name.handle(k)
	case s.focused() == launchStopCommand:
		s.command.handle(k)
	}
}

func (s *herdrLaunchSheet) mouse(m mouseEvent) { s.click(m) }

func (s *herdrLaunchSheet) box(rows, cols int) ([]string, int, int) {
	return s.place(s.build(rows, cols), rows, cols)
}

func (s *herdrLaunchSheet) build(rows, cols int) *popupBox {
	w := min(cols, 68)
	if w < 40 || rows < 12 {
		return nil
	}
	focused := s.focused()
	b := newPopupBox("Open in Herdr", w)
	b.closeRow(s.close)
	b.add(sgrDim + "Name this session" + sgrReset)
	b.add("")
	b.add(sgrBold + "What are you working on?" + sgrReset)
	sheetField(b, &s.name, "e.g. fix login flicker", focused == launchStopName, func() { s.focus(launchStopName) })
	sheetText(b, "Names the Herdr tab, and the new git worktree when starting one.", sgrDim, 2)
	b.add("")
	if s.needsConfig {
		b.add(sgrBold + "Claude command" + sgrReset)
		sheetText(b, "Herdr runs this to start Claude Code. Saved for all projects — change it later from the gear in the Projects header.", sgrDim, 4)
		sheetField(b, &s.command, "", focused == launchStopCommand, func() { s.focus(launchStopCommand) })
		b.add("")
	}
	open := homeButton{label: "Open in Herdr", kind: buttonPrimary, focused: focused == launchStopOpen}
	if s.canOpen() {
		open.act = s.submit
	}
	sheetButtons(b, []homeButton{
		{label: "Cancel", act: s.close, focused: focused == launchStopCancel},
		open,
	})
	return b
}

// herdrConfigSheet is the app's HerdrConfigSheet: the Claude command Herdr runs, for every
// project.
type herdrConfigSheet struct {
	sheetFrame
	v       *projectsViewer
	command textInput
}

// The settings sheet's stops. Reset to default is one only while the command differs from it.
const (
	configStopCommand = iota
	configStopReset
	configStopCancel
	configStopSave
)

func newHerdrConfigSheet(v *projectsViewer) *herdrConfigSheet {
	s := &herdrConfigSheet{v: v}
	s.command.set(v.claudeCommand())
	return s
}

func (s *herdrConfigSheet) isDefault() bool {
	return strings.TrimSpace(s.command.String()) == defaultHerdrCommand
}

func (s *herdrConfigSheet) stops() []int {
	if s.isDefault() {
		return []int{configStopCommand, configStopCancel, configStopSave}
	}
	return []int{configStopCommand, configStopReset, configStopCancel, configStopSave}
}

func (s *herdrConfigSheet) focused() int {
	stops := s.stops()
	return stops[clampIndex(s.stop, len(stops))]
}

func (s *herdrConfigSheet) focus(stop int) {
	for i, candidate := range s.stops() {
		if candidate == stop {
			s.stop = i
		}
	}
}

func (s *herdrConfigSheet) close() { s.v.closeSheet(s) }

// reset puts the default command back in the field, where it can still be edited.
func (s *herdrConfigSheet) reset() {
	s.command.set(defaultHerdrCommand)
	s.focus(configStopCommand)
}

// save is ProjectsModel.saveHerdrConfig.
func (s *herdrConfigSheet) save() {
	s.v.persistHerdrCommand(s.command.String())
	s.close()
}

func (s *herdrConfigSheet) press(act func()) {
	s.v.guard.press(s.v.after, func() {
		if s.v.sheet == cloudSheet(s) {
			act()
		}
	})
}

func (s *herdrConfigSheet) key(k []byte) {
	switch {
	case isEsc(k):
		s.close()
	case s.hidden:
	case s.tab(k, len(s.stops())):
	case isEnter(k):
		switch s.focused() {
		case configStopReset:
			s.press(s.reset)
		case configStopCancel:
			s.press(s.close)
		default:
			s.press(s.save)
		}
	case s.focused() == configStopCommand:
		s.command.handle(k)
	}
}

func (s *herdrConfigSheet) mouse(m mouseEvent) { s.click(m) }

func (s *herdrConfigSheet) box(rows, cols int) ([]string, int, int) {
	return s.place(s.build(rows, cols), rows, cols)
}

func (s *herdrConfigSheet) build(rows, cols int) *popupBox {
	w := min(cols, 68)
	if w < 40 || rows < 12 {
		return nil
	}
	focused := s.focused()
	b := newPopupBox("Open in Herdr", w)
	b.closeRow(s.close)
	b.add(sgrDim + "Applies to all projects" + sgrReset)
	b.add("")
	b.add(sgrBold + "Claude command" + sgrReset)
	sheetText(b, "Herdr opens a new tab in the project's Space and runs this to start Claude Code. On a project root that's a git repo, Jaca refreshes to latest and appends `--worktree`; in a worktree it just runs the command there.", sgrDim, 6)
	sheetField(b, &s.command, "", focused == configStopCommand, func() { s.focus(configStopCommand) })
	if !s.isDefault() {
		row := ""
		sheetButton(b, &row, homeButton{label: "Reset to default", act: s.reset, focused: focused == configStopReset})
		b.add(row)
	}
	b.add("")
	sheetButtons(b, []homeButton{
		{label: "Cancel", act: s.close, focused: focused == configStopCancel},
		{label: "Save", kind: buttonPrimary, act: s.save, focused: focused == configStopSave},
	})
	return b
}
