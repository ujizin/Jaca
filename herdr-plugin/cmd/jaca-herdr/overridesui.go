package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// overridesUI is the network viewer's two override popups: the rule list (the app's
// OverridesPopover) and the rule editor (OverrideEditorSheet).
type overridesUI struct {
	selected int
	menu     *popupMenu
	draft    *ruleDraft
	ruleRows map[int]int // list popup: box row -> the rule drawn on it
	hidden   bool        // the pane is too small to draw the popup: only Esc acts on it

	// The open popup as last drawn: where it sits and what a click on each span does.
	top, left int // 0-based
	hits      []boxHit
}

// boxHit is a clickable span of a popup: a row and columns of the box, 0-based.
type boxHit struct {
	row, x0, x1 int
	act         func(col int)
	drag        func(col int) // what dragging onto it with the button held does; nil for nothing
}

// popupBox builds a popup row by row inside a border, recording clickable spans.
type popupBox struct {
	w, inner int
	rows     []string
	hits     []boxHit
}

func newPopupBox(title string, w int) *popupBox {
	return &popupBox{w: w, inner: w - 4, rows: []string{titledBorder(title, w)}}
}

// add appends a content row, filled out to the box's inner width.
func (b *popupBox) add(content string) {
	b.rows = append(b.rows, "│ "+padStyled(content, b.inner)+sgrReset+" │")
}

// hit makes columns x0..x1 of the next row's content clickable.
func (b *popupBox) hit(x0, x1 int, act func(col int)) {
	b.hits = append(b.hits, boxHit{row: len(b.rows), x0: x0 + 2, x1: x1 + 2, act: act})
}

// button appends text to a row being built and makes it clickable; it returns the row so far.
func (b *popupBox) button(row *string, label, style string, act func()) {
	x := cellWidth(stripSGR(*row))
	b.hit(x, x+cellWidth(label)-1, func(int) { act() })
	*row += style + label + sgrReset
}

func (b *popupBox) close() []string {
	return append(b.rows, "╰"+strings.Repeat("─", b.w-2)+"╯")
}

// closeRow is a popup's first row: the close button at the right, under a blank row.
func (b *popupBox) closeRow(act func()) {
	b.add("")
	b.hit(b.inner-len(escClose), b.inner-1, func(int) { act() })
	b.add(strings.Repeat(" ", b.inner-len(escClose)) + closeButton(escClose))
}

func (v *netViewer) openOverridesList() {
	v.focus = netOverrides
	v.ovui.menu = nil
	v.ovui.selected = clampIndex(v.ovui.selected, len(v.overrides.Rules))
}

func (v *netViewer) closeOverrides() {
	v.focus, v.ovui.menu, v.ovui.draft = netList, nil, nil
}

// ruleCall sends an override change. The new state comes back on overrides.state.
func (v *netViewer) ruleCall(method string, params map[string]any) {
	v.err = ""
	go func() {
		if err := v.p.c.Call(method, params, nil); err != nil {
			v.p.post(func() { v.err = err.Error() })
		}
	}()
}

// blockedReason is why a rule can't run on this capture ("not here"), "" when it can or when
// that isn't known yet (OverridesPopover.blockedReason).
func (v *netViewer) blockedReason(r overrideRule) string {
	if !r.Enabled || !v.overrides.MasterEnabled || !v.state.HasRunningSource {
		return ""
	}
	return skipReason(r, v.transport(), v.state.InterceptCapabilities, true)
}

// overridesStatus is the popup's status line and its tint (OverridesPopover's status line).
func (v *netViewer) overridesStatus() (text, style string) {
	arming := v.arming()
	switch {
	case arming.State == "active":
		hosts := "No hosts routed"
		if n := len(arming.Hosts); n > 0 {
			sorted := append([]string(nil), arming.Hosts...)
			sort.Strings(sorted)
			plural := "s"
			if n == 1 {
				plural = ""
			}
			hosts = fmt.Sprintf("Diverting %d host%s: %s", n, plural, strings.Join(sorted, ", "))
		}
		return hosts + " · " + v.transport().portLabel(arming.Port), sgrDim
	case arming.State == "failed":
		return "Overrides inactive  " + arming.Message, sgrRed
	case arming.blockedMessage() != "":
		return arming.blockedMessage(), sgrYellow
	case !v.feature:
		return "Turn on Agent HTTPS debugging in Settings to apply these rules.", sgrDim
	case len(v.overrides.Rules) == 0:
		return "", ""
	}
	return "Start capture to apply these overrides.", sgrDim
}

// newOverride opens the editor on a blank rule for the host being looked at.
func (v *netViewer) newOverride() {
	host := ""
	if t, ok := v.selected(); ok {
		host = t.Host
	} else if len(v.txns) > 0 {
		host = v.txns[len(v.txns)-1].Host
	}
	v.openEditor(blankRule(host), true, "")
}

// ruleMenu is the app's rule row menu.
func (v *netViewer) openRuleMenu(i, x, y int) {
	rules := v.overrides.Rules
	if i < 0 || i >= len(rules) {
		return
	}
	rule := rules[i]
	items := []menuItem{{"Edit override…", func() { v.openEditor(rule, false, "") }}}
	if i > 0 {
		items = append(items, menuItem{"Move up", func() { v.moveRule(rule, -1) }})
	}
	if i < len(rules)-1 {
		items = append(items, menuItem{"Move down", func() { v.moveRule(rule, 1) }})
	}
	items = append(items, menuItem{},
		menuItem{"Duplicate", func() { v.ruleCall("overrides.duplicate", map[string]any{"id": rule.ID}) }},
		menuItem{"Delete", func() { v.ruleCall("overrides.remove", map[string]any{"id": rule.ID}) }})
	v.ovui.menu = &popupMenu{x: x, y: y, items: items}
}

func (v *netViewer) moveRule(rule overrideRule, offset int) {
	v.ruleCall("overrides.move", map[string]any{"id": rule.ID, "offset": offset})
	v.ovui.selected = clampIndex(v.ovui.selected+offset, len(v.overrides.Rules))
}

func (v *netViewer) toggleRule(i int) {
	if i >= 0 && i < len(v.overrides.Rules) && v.overrides.MasterEnabled {
		r := v.overrides.Rules[i]
		v.ruleCall("overrides.setEnabled", map[string]any{"id": r.ID, "enabled": !r.Enabled})
	}
}

func (v *netViewer) toggleMaster() {
	v.ruleCall("overrides.setMaster", map[string]any{"enabled": !v.overrides.MasterEnabled})
}

// showLog reveals Jaca's log in Finder, as the popup's link does.
func showLog() {
	go func() { _ = exec.Command("sh", "-c", `open -R "$HOME/.jaca/logs/jaca.log"`).Run() }()
}

// overridesKey handles a key in whichever override popup is open.
func (v *netViewer) overridesKey(k []byte) {
	if v.ovui.hidden {
		// Nothing is drawn, so nothing is edited or saved unseen. Esc closes; a bigger pane shows it again.
		if isEsc(k) {
			v.closeOverrides()
		}
		return
	}
	if v.focus == netEditor {
		v.editorKey(k)
		return
	}
	ui := &v.ovui
	if ui.menu != nil {
		switch {
		case isEsc(k):
			ui.menu = nil
		case isUp(k):
			ui.menu.move(-1)
		case isDown(k):
			ui.menu.move(1)
		case isEnter(k):
			menu := ui.menu
			ui.menu = nil
			menu.items[menu.selected].act()
		}
		return
	}
	rules := v.overrides.Rules
	i := clampIndex(ui.selected, len(rules))
	has := len(rules) > 0
	switch {
	case isEsc(k) || (len(k) == 1 && (k[0] == 'q' || k[0] == 'o')):
		v.closeOverrides()
	case isUp(k):
		ui.selected = clampIndex(i-1, len(rules))
	case isDown(k):
		ui.selected = clampIndex(i+1, len(rules))
	case isEnter(k):
		if has {
			v.openEditor(rules[i], false, "")
		} else {
			v.newOverride()
		}
	case len(k) != 1:
	case k[0] == 'n':
		v.newOverride()
	case k[0] == 'm':
		v.toggleMaster()
	case k[0] == 'L':
		showLog()
	case !has:
	case k[0] == ' ':
		v.toggleRule(i)
	case k[0] == 'd':
		v.ruleCall("overrides.duplicate", map[string]any{"id": rules[i].ID})
	case k[0] == 'x' || k[0] == 0x7f || k[0] == 0x08: // x or Backspace
		v.ruleCall("overrides.remove", map[string]any{"id": rules[i].ID})
		ui.selected = clampIndex(i, len(rules)-1)
	case k[0] == 'K' && i > 0:
		v.moveRule(rules[i], -1)
	case k[0] == 'J' && i < len(rules)-1:
		v.moveRule(rules[i], 1)
	}
}

// overridesMouse handles a press on whichever override popup is open.
func (v *netViewer) overridesMouse(m mouseEvent) {
	ui := &v.ovui
	if ui.hidden {
		return
	}
	if v.focus == netOverrides && ui.menu != nil {
		menu := ui.menu
		ui.menu = nil
		if i := menu.itemAt(m.x, m.y); m.button == 0 && i >= 0 {
			menu.items[i].act()
		}
		return
	}
	if m.button == 64 || m.button == 65 {
		by := 1
		if m.button == 64 {
			by = -1
		}
		if v.focus == netEditor {
			v.ovui.draft.wheel(by * 3)
		} else {
			ui.selected = clampIndex(ui.selected+by, len(v.overrides.Rules))
		}
		return
	}
	row, col := m.y-1-ui.top, m.x-1-ui.left
	for i := len(ui.hits) - 1; i >= 0; i-- {
		if h := ui.hits[i]; h.row == row && col >= h.x0 && col <= h.x1 {
			if m.button&mouseDrag != 0 { // the pointer moved with the button held
				if h.drag != nil {
					h.drag(col - h.x0)
				}
				return
			}
			if m.button == 2 && v.focus == netOverrides {
				if n, ok := v.ruleAt(row); ok {
					ui.selected = n
					v.openRuleMenu(n, m.x, m.y)
				}
				return
			}
			if m.button == 0 {
				h.act(col - h.x0)
			}
			return
		}
	}
}

// ruleAt is the rule drawn on a row of the list popup.
func (v *netViewer) ruleAt(row int) (int, bool) {
	n, ok := v.ovui.ruleRows[row]
	return n, ok
}

// overridesBox draws the open override popup and where to put it (0-based top and left).
func (v *netViewer) overridesBox(rows, cols int) (box []string, top, left int) {
	ui := &v.ovui
	var b *popupBox
	if v.focus == netEditor && ui.draft != nil {
		b = v.editorBox(rows, cols)
	} else {
		b = v.listBox(rows, cols)
	}
	ui.hidden = b == nil
	if b == nil {
		ui.hits = nil
		return nil, 0, 0
	}
	box = b.close()
	if over := len(box) - rows; over > 0 {
		// Too tall: rows are cut above the footer, which holds Save and why it can't be saved.
		keep := len(box) - 2
		box = append(box[:keep-over], box[keep:]...)
	}
	ui.hits = b.hits
	ui.top, ui.left = max(0, (rows-len(box))/2), max(0, (cols-b.w)/2)
	if v.focus == netOverrides && ui.menu != nil {
		// The rule menu draws over the list, in the same overlay.
		menu := ui.menu.box(rows, cols)
		for i, row := range menu {
			at := ui.menu.top - 1 + i - ui.top
			if at >= 0 && at < len(box) {
				box[at] += fmt.Sprintf("%s\x1b[%dG%s", sgrReset, ui.menu.left, framed(row))
			}
		}
	}
	return box, ui.top, ui.left
}

// listBox is the app's overrides popover: the master switch, the rules in precedence order, how
// they stand on this capture, and New override.
func (v *netViewer) listBox(rows, cols int) *popupBox {
	w := min(cols, 84)
	if w < 40 || rows < 10 {
		return nil
	}
	ui := &v.ovui
	b := newPopupBox("Overrides", w)
	b.closeRow(v.closeOverrides)
	ui.ruleRows = map[int]int{}

	// The master switch, labelled with what toggling it does (the app's switch has only this tooltip).
	master, masterStyle := "● Pause all overrides — rules stay saved", sgrGreen
	if !v.overrides.MasterEnabled {
		master, masterStyle = "○ Resume overrides", sgrYellow
	}
	b.hit(0, cellWidth(master)-1, func(int) { v.toggleMaster() })
	b.add(masterStyle + clip(master, b.inner) + sgrReset)
	b.add("")

	rules := v.overrides.Rules
	if len(rules) == 0 {
		b.add(sgrBold + clip("No response overrides", b.inner) + sgrReset)
		for _, text := range []string{
			"Right-click any captured request to override its response, or create one from scratch.",
			"Overrides replace the response your app receives."} {
			for _, part := range wrapWords(text, b.inner) {
				b.add(sgrDim + part + sgrReset)
			}
		}
	}
	// Two rows per rule; the window follows the selection.
	ui.selected = clampIndex(ui.selected, len(rules))
	room := max(1, (rows-14)/2)
	start := listWindow(ui.selected, room)
	for i := start; i < len(rules) && i < start+room; i++ {
		r := rules[i]
		dim := !r.Enabled || !v.overrides.MasterEnabled
		name := sanitize(r.displayName())
		if v.rules.diagnostic(r.ID) != "" {
			name += " !"
		}
		trailing := ""
		switch reason := v.blockedReason(r); {
		case reason != "":
			trailing = "not here"
		case v.overrides.HitCounts[r.ID] > 0:
			trailing = fmt.Sprintf("%d×", v.overrides.HitCounts[r.ID])
		}
		toggle := "○"
		if r.Enabled {
			toggle = "●"
		}
		nameW := b.inner - cellWidth(trailing) - 7 // the indent, the gaps and the switch
		n := i
		ui.ruleRows[len(b.rows)], ui.ruleRows[len(b.rows)+1] = n, n
		b.hit(0, b.inner-4, func(int) { ui.selected = n; v.openEditor(r, false, "") })
		b.hit(b.inner-3, b.inner-1, func(int) { ui.selected = n; v.toggleRule(n) })
		line := fit(name, nameW) + " " + trailing + "  " + toggle + " "
		switch {
		case i == ui.selected:
			b.add(sgrRev + sgrBold + "  " + line + sgrReset)
		case dim:
			b.add(sgrDim + "  " + line + sgrReset)
		default:
			toggleStyle := sgrGreen
			b.add("  " + sgrBold + fit(name, nameW) + sgrReset + " " + sgrYellow + trailing + sgrReset + "  " + toggleStyle + toggle + sgrReset + " ")
		}
		pattern := sanitize(r.Matcher.Pattern)
		if pattern == "" {
			pattern = "No pattern"
		}
		b.hit(0, b.inner-1, func(int) { ui.selected = n; v.openEditor(r, false, "") })
		b.add("  " + sgrDim + clip(pattern, b.inner-2) + sgrReset)
	}
	b.add("")
	if text, style := v.overridesStatus(); text != "" {
		for _, part := range wrapWords(sanitize(text), b.inner) {
			b.add(style + part + sgrReset)
		}
	}
	footer := ""
	b.button(&footer, " New override ", sgrBold+sgrRev, v.newOverride)
	footer += "   "
	b.button(&footer, "Show log", sgrUnder, showLog)
	b.add(footer)
	if v.overrides.LastActivity != "" {
		b.add(sgrDim + clip(sanitize(v.overrides.LastActivity), b.inner) + sgrReset)
	}
	return b
}

// wrapWords breaks text into lines of at most w cells at spaces.
func wrapWords(text string, w int) []string {
	var out []string
	line := ""
	for _, word := range strings.Fields(text) {
		switch {
		case line == "":
			line = word
		case cellWidth(line)+1+cellWidth(word) <= w:
			line += " " + word
		default:
			out = append(out, line)
			line = word
		}
	}
	if line != "" {
		out = append(out, line)
	}
	for i, l := range out {
		out[i] = clip(l, w)
	}
	return out
}

// The editor's method chips and status shortcuts, as in the app.
var (
	editorMethods = []string{"ANY", "GET", "POST", "PUT", "PATCH", "DELETE"}
	quickStatuses = []string{"200", "401", "403", "404", "500", "503"}
)

type headerRow struct{ name, value textInput }

// ruleDraft is a rule being edited: the app's OverrideEditorSheet state.
type ruleDraft struct {
	rule    overrideRule // as opened; fields the editor has no control for are kept from it
	isNew   bool
	warning string // what seeding from a request couldn't carry over

	name, match, hosts, status, delay textInput
	kind                              string          // "glob" or "regex"
	methods                           map[string]bool // empty = any
	respond                           bool            // "Don't send"; else "Send and override"
	headers                           []headerRow
	merge                             bool // header mode, for "Send and override"
	body                              textArea
	enabled                           bool

	openedRespond bool   // the action it opened with,
	openedStatus  string // its status
	openedHeaders string // and its headers, to tell untouched defaults when the action changes

	sync routedHostsSync // the routed hosts, kept in step with the pattern

	stop       editorStop
	bodyNav    bool // the body has the focus but Esc put its keys back on moving between fields
	headersTab bool
	chip       int // the cursor inside a chip group
	bodyRows   int // the body area's height as last drawn
	bodyRow    int // and its first box row, for clicks
	listTop    int // the first header row drawn

	preview    string // the match preview, and what it was computed for
	previewKey string
}

// editorStop is where the editor's focus is: a field or group, and for header rows which one.
type editorStop struct {
	kind string
	i    int
}

func headersKey(rows []headerRow) string {
	var b strings.Builder
	for _, h := range rows {
		b.WriteString(h.name.String() + "\x00" + h.value.String() + "\x01")
	}
	return b.String()
}

// openEditor opens the rule editor on a rule: a saved one, a blank one, or one seeded from a
// request (with what seeding couldn't carry over).
func (v *netViewer) openEditor(rule overrideRule, isNew bool, warning string) {
	d := &ruleDraft{rule: rule, isNew: isNew, warning: warning, kind: rule.Matcher.Kind, methods: map[string]bool{},
		enabled: rule.Enabled, stop: editorStop{kind: "name"}}
	if d.kind != "regex" {
		d.kind = "glob"
	}
	d.name.set(rule.Name)
	d.match.set(rule.Matcher.Pattern)
	for _, m := range rule.Matcher.Methods {
		d.methods[strings.ToUpper(m)] = true
	}
	d.sync = initialRoutedHosts(rule.RoutedHosts, derivedRoutedHosts(rule.Matcher))
	d.showHosts()
	if rule.DelayMillis != 0 {
		d.delay.set(strconv.Itoa(rule.DelayMillis))
	}
	var list []headerPair
	var body *bodyRef
	if rule.Action.Kind == "editResponse" {
		edit := rule.Action.Edit
		list, body, d.merge = edit.Headers, edit.Body, edit.HeaderMode != "replace"
		if edit.StatusCode != nil {
			d.status.set(strconv.Itoa(*edit.StatusCode))
		}
	} else {
		d.respond = true
		list, body = rule.Action.Respond.Headers, &rule.Action.Respond.Body
		status := rule.Action.Respond.StatusCode
		if rule.Action.Kind != "respond" { // an action the editor has no control for opens as a fresh one
			status = 200
		}
		d.status.set(strconv.Itoa(status))
	}
	for _, h := range list {
		var row headerRow
		row.name.set(h.Name)
		row.value.set(h.Value)
		d.headers = append(d.headers, row)
	}
	if body != nil {
		// A body that can't be read (a missing blob, not UTF-8) opens empty, as in the app.
		text, _ := loadBodyText(*body)
		d.body.set(text)
	} else {
		d.body.set("")
	}
	d.openedRespond, d.openedStatus, d.openedHeaders = d.respond, d.status.String(), headersKey(d.headers)
	v.ovui.draft, v.focus = d, netEditor
}

func (d *ruleDraft) matcher() ruleMatcher {
	m := ruleMatcher{Pattern: d.match.String(), Kind: d.kind}
	// Methods the chips don't show (a seeded HEAD, say) stay in the set.
	for method := range d.methods {
		m.Methods = append(m.Methods, method)
	}
	sort.Strings(m.Methods)
	return m
}

// showHosts puts the routed hosts in the Hosts to route field: sorted, comma-separated.
func (d *ruleDraft) showHosts() {
	hosts := append([]string(nil), d.sync.Hosts...)
	sort.Strings(hosts)
	d.hosts.set(strings.Join(hosts, ", "))
}

// matcherChanged brings the routed hosts in step after the pattern or its kind changed: a host
// the pattern names replaces them, and hosts that came from the old pattern don't outlive it.
func (d *ruleDraft) matcherChanged() {
	d.sync = d.sync.afterMatcherChange(derivedRoutedHosts(d.matcher()))
	d.showHosts()
}

// needsHosts is when the pattern names no host to route, so the user must (OverrideEditorSheet.needsExplicitHosts).
func (d *ruleDraft) needsHosts() bool {
	return strings.TrimSpace(d.match.String()) != "" && len(derivedRoutedHosts(d.matcher())) == 0
}

// parsedHosts is the Hosts to route field: comma-separated, trimmed, lowercased.
func (d *ruleDraft) parsedHosts() []string {
	var out []string
	for _, part := range strings.Split(d.hosts.String(), ",") {
		if host := strings.ToLower(strings.TrimSpace(part)); host != "" {
			out = append(out, host)
		}
	}
	return out
}

// routedHosts is the hosts the rule routes.
func (d *ruleDraft) routedHosts() []string { return d.sync.Hosts }

// patternProblem is why the pattern can't be used, "" when it can. A regex Go can't compile but
// the app's engine may (lookaround, backreferences) is let through: jacad applies it, and the
// pane just can't preview it.
func (d *ruleDraft) patternProblem() string {
	m := d.matcher()
	msg := patternError(m)
	if msg != "" && m.Kind == "regex" && regexMayNeedICU(m.Pattern) {
		return ""
	}
	return msg
}

// blocked is why the rule can't be saved yet, "" when it can (OverrideEditorSheet's save-blocked reason).
func (d *ruleDraft) blocked() string {
	switch {
	case strings.TrimSpace(d.match.String()) == "":
		return "Enter a URL or pattern to match."
	case d.patternProblem() != "":
		return "Fix the pattern to save."
	case d.needsHosts() && len(d.sync.Hosts) == 0:
		return "Add at least one host to route."
	}
	return ""
}

func (d *ruleDraft) headerPairs() []headerPair {
	out := []headerPair{}
	for _, h := range d.headers {
		if name := h.name.String(); name != "" {
			out = append(out, headerPair{Name: name, Value: h.value.String()})
		}
	}
	return out
}

// build is the rule as the form stands (OverrideEditorSheet.save).
func (d *ruleDraft) build() (overrideRule, error) {
	r := d.rule
	r.Name = d.name.String()
	if strings.TrimSpace(r.Name) == "" {
		r.Name = d.match.String()
	}
	r.Matcher, r.Enabled, r.RoutedHosts = d.matcher(), d.enabled, d.routedHosts()
	r.DelayMillis, _ = strconv.Atoi(strings.TrimSpace(d.delay.String()))
	status, statusErr := strconv.Atoi(strings.TrimSpace(d.status.String()))
	text := d.body.String()
	if d.respond {
		if statusErr != nil {
			status = 200
		}
		body, err := makeBodyRef(text)
		if err != nil {
			return r, err
		}
		r.Action = ruleAction{Kind: "respond", Respond: respondSpec{StatusCode: status, Headers: d.headerPairs(), Body: body}}
		return r, nil
	}
	// Send and override: a blank status or body keeps the origin's.
	edit := responseEdit{HeaderMode: "replace", Headers: d.headerPairs(), RemoveHeaders: d.rule.Action.Edit.RemoveHeaders}
	if d.merge {
		edit.HeaderMode = "merge"
	}
	if statusErr == nil {
		edit.StatusCode = &status
	}
	if text != "" {
		body, err := makeBodyRef(text)
		if err != nil {
			return r, err
		}
		edit.Body = &body
	}
	r.Action = ruleAction{Kind: "editResponse", Edit: edit}
	return r, nil
}

// setRespond switches the action. Going to "Send and override", a status and headers that are
// still the ones the rule opened with as "Don't send" are dropped, so the real response's show
// through, and the headers merge.
func (d *ruleDraft) setRespond(respond bool) {
	if d.respond == respond {
		return
	}
	d.respond = respond
	if respond || !d.openedRespond {
		return
	}
	if d.status.String() == d.openedStatus {
		d.status.set("")
	}
	if headersKey(d.headers) == d.openedHeaders {
		d.headers = nil
	}
	d.merge = true
}

// stops are the editor's focus stops in order, for what the form currently shows.
func (d *ruleDraft) stops() []editorStop {
	s := []editorStop{{kind: "name"}, {kind: "match"}, {kind: "kind"}}
	if d.kind == "glob" && generalizePattern(d.match.String()) != d.match.String() {
		s = append(s, editorStop{kind: "generalize"})
	}
	s = append(s, editorStop{kind: "methods"})
	if d.needsHosts() {
		s = append(s, editorStop{kind: "hosts"})
	}
	s = append(s, editorStop{kind: "action"}, editorStop{kind: "status"}, editorStop{kind: "delay"}, editorStop{kind: "tab"})
	if !d.headersTab {
		s = append(s, editorStop{kind: "format"}, editorStop{kind: "body"})
	} else {
		if !d.respond {
			s = append(s, editorStop{kind: "hmode"})
		}
		s = append(s, editorStop{kind: "hadd"})
		for i := range d.headers {
			s = append(s, editorStop{"hname", i}, editorStop{"hvalue", i}, editorStop{"hremove", i})
		}
	}
	return append(s, editorStop{kind: "enabled"}, editorStop{kind: "save"})
}

func (d *ruleDraft) moveStop(by int) {
	stops := d.stops()
	at := 0
	for i, s := range stops {
		if s == d.stop {
			at = i
		}
	}
	d.stop, d.chip, d.bodyNav = stops[(at+by+len(stops))%len(stops)], 0, false
}

// editingBody is when the keys go to the body's text: it has the focus and Esc hasn't put them
// back on moving between fields.
func (d *ruleDraft) editingBody() bool { return d.stop.kind == "body" && !d.bodyNav }

// wheel scrolls the body or the headers list.
func (d *ruleDraft) wheel(by int) {
	if d.headersTab {
		d.listTop = max(0, d.listTop+by)
		return
	}
	d.body.ensure()
	d.body.row = max(0, min(d.body.row+by, len(d.body.lines)-1))
	d.body.col = min(d.body.col, len(d.body.lines[d.body.row]))
}

// format pretty-prints a JSON body; anything else is left alone.
func (d *ruleDraft) format() {
	text := strings.TrimSpace(d.body.String())
	if validJSONDocument(text) {
		d.body.replace(netBodyText([]byte(text), "application/json")) // one undo step
	}
}

// validJSONDocument is whether text is a JSON object or array, which is what the app's parser
// accepts as a document (a bare number or string is not one).
func validJSONDocument(text string) bool {
	return (strings.HasPrefix(text, "{") || strings.HasPrefix(text, "[")) && json.Valid([]byte(text))
}

// bodyStatus is the line under the body editor: JSON is advisory and never blocks saving.
func (d *ruleDraft) bodyStatus() (text string, caution bool) {
	raw := d.body.String()
	switch {
	case strings.TrimSpace(raw) == "":
		return "Empty body", false
	case validJSONDocument(strings.TrimSpace(raw)):
		return "Valid JSON · " + formatNetSize(len(raw)), false
	}
	return "Not valid JSON · " + formatNetSize(len(raw)) + " — saved anyway", true
}

// matchPreview is the editor's line on what the pattern matches among the captured requests.
func (v *netViewer) matchPreview(d *ruleDraft) (text string, caution bool) {
	m := d.matcher()
	if strings.TrimSpace(m.Pattern) == "" {
		return "Enter a pattern to see what it matches.", false
	}
	if patternError(m) != "" {
		return "", false // nothing to say about a pattern the pane can't run
	}
	// Recomputed per 50 captured requests, as the app does, so a busy capture doesn't rescan each frame.
	key := fmt.Sprintf("%s\x00%s\x00%s\x00%d", m.Pattern, m.Kind, strings.Join(m.Methods, ","), len(v.txns)/50)
	if key != d.previewKey {
		probe := d.rule
		probe.Matcher, probe.Enabled, probe.Scope = m, true, ruleScope{}
		set := compileRules([]overrideRule{probe}, true)
		count := 0
		for _, t := range v.txns {
			if _, ok := set.matchingRule(t.URL, t.Method); ok {
				count++
			}
		}
		d.previewKey = key
		d.preview = fmt.Sprintf("Matches %d of %d captured", count, len(v.txns))
		if count == 0 {
			d.preview = ""
		}
	}
	if d.preview == "" {
		return "Nothing captured so far matches this pattern. It will still apply to future requests.", true
	}
	return d.preview, false
}

// saveDraft saves the rule and closes the editor, when nothing blocks it.
func (v *netViewer) saveDraft() {
	d := v.ovui.draft
	if d == nil || d.blocked() != "" {
		return
	}
	rule, err := d.build()
	if err != nil {
		v.err = err.Error()
		return
	}
	v.ruleCall("overrides.save", map[string]any{"rule": rule})
	v.closeEditor()
}

// closeEditor goes back to where the editor was opened from: the request list.
func (v *netViewer) closeEditor() {
	v.ovui.draft, v.focus = nil, netList
}

// editorKey: Tab and Shift-Tab move between fields, Ctrl-S saves, Esc closes without saving. In
// a chip group the arrows move and Space toggles. In the body the keys edit it (textArea.handle):
// Tab indents and Shift-Tab takes a level off, Ctrl-C and Ctrl-X copy and cut its selection, and
// Esc hands the keys back to moving between fields (Tab and Shift-Tab then move again). Ctrl-S,
// or ⌘S where the terminal passes it on, saves from anywhere.
func (v *netViewer) editorKey(k []byte) {
	d := v.ovui.draft
	if d == nil {
		v.closeEditor()
		return
	}
	switch {
	case isEsc(k) && d.editingBody():
		// Esc leaves the body's text, so Tab moves on again; a second Esc closes the editor.
		d.bodyNav = true
		return
	case isEsc(k):
		v.closeEditor()
		return
	case len(k) == 1 && k[0] == 0x13, string(k) == "\x1b[115;9u": // Ctrl-S or ⌘S
		v.saveDraft()
		return
	case len(k) == 1 && k[0] == 0x03, string(k) == "\x1b[99;9u": // Ctrl-C or ⌘C copies the body's selection
		if text := d.body.selectedText(); d.stop.kind == "body" && text != "" {
			v.copy(text)
		}
		return
	case len(k) == 1 && k[0] == 0x18, string(k) == "\x1b[120;9u": // Ctrl-X or ⌘X cuts it
		if d.stop.kind == "body" {
			if text := d.body.cut(); text != "" {
				v.copy(text)
			}
		}
		return
	case string(k) == "\x1b\t": // Esc and Tab together: out of the body to the next field
		d.moveStop(1)
		return
	case len(k) == 3 && k[0] == 0x1b && k[1] == '[' && k[2] == 'Z' && !d.editingBody():
		// In the body Shift-Tab takes a level of indent off, as Tab adds one.
		d.moveStop(-1)
		return
	case len(k) == 1 && k[0] == '\t' && !d.editingBody():
		// In the body Tab indents (textArea.handle); Esc first puts it back on moving on.
		d.moveStop(1)
		return
	}
	left := string(k) == "\x1b[D" || string(k) == "\x1bOD"
	right := string(k) == "\x1b[C" || string(k) == "\x1bOC"
	press := isEnter(k) || (len(k) == 1 && k[0] == ' ')
	text := func(t *textInput) {
		switch {
		case isEnter(k) || isArrowDown(k):
			d.moveStop(1)
		case isArrowUp(k):
			d.moveStop(-1)
		default:
			t.handle(k)
		}
	}
	group := func(n int, pick func(i int)) {
		switch {
		case left:
			d.chip = (d.chip - 1 + n) % n
		case right:
			d.chip = (d.chip + 1) % n
		case press:
			pick(d.chip)
		case isArrowDown(k):
			d.moveStop(1)
		case isArrowUp(k):
			d.moveStop(-1)
		}
	}
	button := func(act func()) {
		switch {
		case press:
			act()
		case isArrowDown(k) || right:
			d.moveStop(1)
		case isArrowUp(k) || left:
			d.moveStop(-1)
		}
	}
	before, hostsBefore := d.matcher(), d.hosts.String()
	defer func() {
		if after := d.matcher(); after.Pattern != before.Pattern || after.Kind != before.Kind {
			d.matcherChanged()
		} else if d.hosts.String() != hostsBefore {
			d.sync = routedHostsAfterUserEdit(d.parsedHosts())
		}
	}()
	switch d.stop.kind {
	case "name":
		text(&d.name)
	case "match":
		text(&d.match)
	case "hosts":
		text(&d.hosts)
	case "status":
		text(&d.status)
	case "delay":
		text(&d.delay)
	case "kind":
		group(2, func(i int) { d.kind = []string{"glob", "regex"}[i] })
	case "generalize":
		button(func() {
			d.match.set(generalizePattern(d.match.String()))
			d.stop = editorStop{kind: "methods"} // the Generalize stop is gone with the change
		})
	case "methods":
		group(len(editorMethods), d.toggleMethod)
	case "action":
		group(2, func(i int) { d.setRespond(i == 0) })
	case "tab":
		group(2, func(i int) { d.headersTab = i == 1 })
	case "format":
		button(d.format)
	case "body":
		if d.bodyNav {
			// Moving between fields: the arrows step, Enter goes back to editing the text.
			switch {
			case isEnter(k):
				d.bodyNav = false
			case isArrowDown(k) || right:
				d.moveStop(1)
			case isArrowUp(k) || left:
				d.moveStop(-1)
			}
			break
		}
		d.body.handle(k, max(1, d.bodyRows-1))
	case "hmode":
		group(2, func(i int) { d.merge = i == 1 })
	case "hadd":
		button(func() {
			d.headers = append(d.headers, headerRow{})
			d.stop = editorStop{"hname", len(d.headers) - 1}
		})
	case "hname":
		if d.stop.i < len(d.headers) {
			text(&d.headers[d.stop.i].name)
		}
	case "hvalue":
		if d.stop.i < len(d.headers) {
			text(&d.headers[d.stop.i].value)
		}
	case "hremove":
		button(func() { d.removeHeader(d.stop.i) })
	case "enabled":
		button(func() { d.enabled = !d.enabled })
	case "save":
		button(v.saveDraft)
	default:
		d.stop = editorStop{kind: "name"}
	}
}

func (d *ruleDraft) toggleMethod(i int) {
	if i == 0 { // ANY clears the set
		d.methods = map[string]bool{}
		return
	}
	method := editorMethods[i]
	if d.methods[method] {
		delete(d.methods, method)
	} else {
		d.methods[method] = true
	}
}

func (d *ruleDraft) removeHeader(i int) {
	if i < 0 || i >= len(d.headers) {
		return
	}
	d.headers = append(d.headers[:i], d.headers[i+1:]...)
	d.stop = editorStop{kind: "hadd"}
}

// formColumn builds one column of a popup line by line, each line exactly its width, recording
// clickable spans by line and column.
type formColumn struct {
	w     int
	lines []string
	hits  []boxHit
}

// add appends a line, filled out to the column's width (a line too wide is cut, plain).
func (c *formColumn) add(content string) {
	if cellWidth(stripSGR(content)) > c.w {
		content = clip(stripSGR(content), c.w)
	}
	c.lines = append(c.lines, padStyled(content, c.w)+sgrReset)
}

// hit makes columns x0..x1 of the next n lines clickable.
func (c *formColumn) hit(n, x0, x1 int, act func(col int)) {
	for i := 0; i < n; i++ {
		c.hits = append(c.hits, boxHit{row: len(c.lines) + i, x0: x0, x1: x1, act: act})
	}
}

// button appends a clickable label to a line being built.
func (c *formColumn) button(row *string, label, style string, act func()) {
	x := cellWidth(stripSGR(*row))
	c.hit(1, x, x+cellWidth(label)-1, func(int) { act() })
	*row += style + label + sgrReset
}

// editorBox is the app's rule editor. In a pane with room it is the sheet's layout: the settings
// form on the left with boxed fields, the response (Body or Headers) on the right, and the
// footer under both. In a smaller pane the two stack in one column with one-line fields.
func (v *netViewer) editorBox(rows, cols int) *popupBox {
	d := v.ovui.draft
	wide := cols >= 120 && rows >= 30
	w := min(cols, 124)
	if wide {
		w = min(cols-2, 156)
	}
	if w < 60 || rows < 16 {
		return nil
	}
	title := "Edit response override"
	if d.isNew {
		title = "New response override"
	}
	b := newPopupBox(title, w)
	b.closeRow(v.closeEditor)
	inner := b.inner
	leftW, rightW := inner, inner
	if wide {
		leftW = 74
		rightW = inner - leftW - 3
	}
	labelW := 16
	at := func(kind string) bool { return d.stop.kind == kind }
	focus := func(kind string) func(int) {
		return func(int) { d.stop, d.chip = editorStop{kind: kind}, 0 }
	}
	label := func(text string, focused bool) string {
		if focused {
			return sgrBold + fit(text, labelW) + sgrReset
		}
		return sgrDim + fit(text, labelW) + sgrReset
	}
	c := &formColumn{w: leftW}
	// field adds a labelled text field w cells wide: a box when there is room, else one line.
	field := func(name, kind string, t *textInput, placeholder string, w int) {
		if !wide {
			c.hit(1, 0, labelW+w-1, focus(kind))
			c.add(label(name, at(kind)) + renderField(t, placeholder, at(kind), w, sgrUnder))
			return
		}
		box := boxedField(t, placeholder, at(kind), w, "")
		c.hit(3, 0, labelW+w-1, focus(kind))
		c.add(strings.Repeat(" ", labelW) + box[0])
		c.add(label(name, at(kind)) + box[1])
		c.add(strings.Repeat(" ", labelW) + box[2])
	}
	// chips draws a group of chips: the chosen ones as filled buttons, the cursor's bracketed
	// while the group has the focus. A click on one picks it.
	chips := func(row *string, kind string, labels []string, on func(i int) bool, pick func(i int)) {
		for i, text := range labels {
			style, open, shut := "", " ", " "
			if on(i) {
				style = buttonStyle
			}
			if at(kind) && d.chip == i {
				open, shut = sgrBold+"["+sgrReset, sgrBold+"]"+sgrReset
			}
			x := cellWidth(stripSGR(*row))
			c.hit(1, x, x+cellWidth(text)+3, func(int) { d.stop, d.chip = editorStop{kind: kind}, i; pick(i) })
			*row += open + style + " " + text + " " + sgrReset + shut + " "
		}
	}
	note := func(text, style string) {
		for _, part := range wrapWords(text, c.w-labelW) {
			c.add(strings.Repeat(" ", labelW) + style + part + sgrReset)
		}
	}
	gap := func() {
		if wide {
			c.add("")
		}
	}
	indent := strings.Repeat(" ", labelW)

	// What to match.
	field("Name", "name", &d.name, "Product state stub", leftW-labelW)
	field("Match", "match", &d.match, "https://api.example.com/v1/users/*", leftW-labelW)
	if msg := d.patternProblem(); msg != "" {
		note(sanitize(msg), sgrRed)
	}
	row := indent
	chips(&row, "kind", []string{"Glob", "Regex"}, func(i int) bool { return (d.kind == "regex") == (i == 1) },
		func(i int) { d.kind = []string{"glob", "regex"}[i]; d.matcherChanged() })
	if d.kind == "glob" && generalizePattern(d.match.String()) != d.match.String() {
		style := sgrUnder
		if at("generalize") {
			style = sgrBold + sgrRev
		}
		row += " "
		c.button(&row, "Generalize", style, func() { d.match.set(generalizePattern(d.match.String())); d.matcherChanged() })
	}
	c.add(row)
	row = indent
	chips(&row, "methods", editorMethods, func(i int) bool {
		if i == 0 {
			return len(d.methods) == 0
		}
		return d.methods[editorMethods[i]]
	}, d.toggleMethod)
	c.add(row)
	if d.kind == "glob" {
		note("`*` one segment · `**` any depth · query ignored unless you write `?`", sgrDim)
	} else {
		note("Regex matching runs on your Mac. It applies wherever Jaca terminates the request — in-process agent capture and HTTPS decryption — but a rule still needs a host to route.", sgrDim)
	}
	// What the pattern matches so far, in a box of its own as in the app.
	if preview, caution := v.matchPreview(d); preview != "" {
		style := ""
		if caution {
			style = sgrYellow
		}
		if !wide {
			note(preview, style)
		} else {
			boxW := leftW - labelW
			rule := strings.Repeat("─", boxW-2)
			c.add(indent + sgrDim + "╭" + rule + "╮")
			for _, part := range wrapWords(preview, boxW-4) {
				c.add(indent + sgrDim + "│" + sgrReset + " " + style + fit(part, boxW-4) + sgrReset + " " + sgrDim + "│")
			}
			c.add(indent + sgrDim + "╰" + rule + "╯")
		}
	}
	if d.needsHosts() {
		note(v.transport().hostsNotice(), sgrYellow)
		field("Hosts to route", "hosts", &d.hosts, "api.example.com, auth.example.com", leftW-labelW)
	}
	if wide {
		c.add(sgrDim + strings.Repeat("─", leftW))
	}

	// What to do with a match.
	row = label("Action", at("action"))
	chips(&row, "action", []string{"Don't send", "Send and override"}, func(i int) bool { return d.respond == (i == 0) },
		func(i int) { d.setRespond(i == 0) })
	c.add(row)
	if d.respond {
		note("The request never leaves the device. Jaca answers it.", sgrDim)
	} else if text := v.transport().originExplainer(); text != "" {
		note(text, sgrYellow)
	}
	if d.warning != "" {
		note(d.warning, sgrYellow)
	}
	gap()
	statusHint := "200"
	if !d.respond {
		statusHint = "Original"
	}
	field("Status", "status", &d.status, statusHint, 14)
	row = indent
	for _, code := range quickStatuses {
		style := sgrDim
		if d.status.String() == code {
			style = buttonStyle
		}
		c.button(&row, " "+code+" ", style, func() { d.status.set(code) })
		row += " "
	}
	c.add(row)
	field("Delay (ms)", "delay", &d.delay, "0", 14)
	left := c

	// The response: Body or Headers, with that tab's tools at the right.
	c = &formColumn{w: rightW}
	named := 0
	for _, h := range d.headers {
		if h.name.String() != "" {
			named++
		}
	}
	headersTitle := "Headers"
	if named > 0 {
		headersTitle = fmt.Sprintf("Headers (%d)", named)
	}
	toolStyle := func(kind string) string {
		if at(kind) {
			return sgrBold + sgrRev
		}
		return sgrUnder
	}
	addHeader := func() {
		d.headers = append(d.headers, headerRow{})
		d.stop = editorStop{"hname", len(d.headers) - 1}
	}
	row = ""
	chips(&row, "tab", []string{"Body", headersTitle}, func(i int) bool { return d.headersTab == (i == 1) },
		func(i int) { d.headersTab = i == 1 })
	row += "  "
	if !d.headersTab {
		c.button(&row, "Format", toolStyle("format"), d.format)
	} else {
		if d.respond {
			// Merge needs the real response, so "Don't send" is fixed on Replace.
			row += sgrDim + " Replace " + sgrReset + "  "
		} else {
			chips(&row, "hmode", []string{"Replace", "Merge"}, func(i int) bool { return d.merge == (i == 1) },
				func(i int) { d.merge = i == 1 })
			row += " "
		}
		c.button(&row, "Add header", toolStyle("hadd"), addHeader)
	}
	c.add(row)

	// The editing area takes the height that is left: beside the form when wide, under it when not.
	total := min(rows, 60) - 6 // less the border, the close rows, the footer rows and the border
	area := total - len(left.lines) - 3
	frame := 0 // cells the area's own border takes on each side
	if wide {
		total = min(rows-2, 50) - 6
		area, frame = max(len(left.lines), total)-4, 2
	}
	area = max(3, area)
	d.bodyRows = area
	editing := d.editingBody() || strings.HasPrefix(d.stop.kind, "hn") || strings.HasPrefix(d.stop.kind, "hv") || at("hremove")
	edge := sgrDim
	if editing {
		edge = borderStyle + sgrBold
	}
	areaW := rightW - 2*frame
	open := func() {
		if wide {
			c.add(edge + "╭" + strings.Repeat("─", rightW-2) + "╮")
		}
	}
	shut := func() {
		if wide {
			c.add(edge + "╰" + strings.Repeat("─", rightW-2) + "╯")
		}
	}
	framed := func(content string) {
		if !wide {
			c.add(content)
			return
		}
		c.add(edge + "│" + sgrReset + " " + padStyled(content, areaW) + sgrReset + " " + edge + "│")
	}
	open()
	if !d.headersTab {
		for i, line := range d.body.view(areaW, area, d.editingBody()) {
			// A click puts the cursor there; dragging on selects from it.
			n := i
			c.hits = append(c.hits, boxHit{row: len(c.lines), x0: frame, x1: frame + areaW - 1,
				act: func(col int) {
					d.stop, d.bodyNav = editorStop{kind: "body"}, false
					d.body.click(n, col)
				},
				drag: func(col int) {
					if d.stop.kind == "body" {
						d.body.dragTo(n, col)
					}
				}})
			framed(line)
		}
		shut()
		status, caution := d.bodyStatus()
		style := sgrDim
		if caution {
			style = sgrYellow
		}
		c.add(style + clip(status, rightW) + sgrReset)
	} else {
		nameW := min(32, areaW/3)
		// Keep the focused header row in the window.
		if strings.HasPrefix(d.stop.kind, "h") && d.stop.kind != "hmode" && d.stop.kind != "hadd" {
			if d.stop.i < d.listTop {
				d.listTop = d.stop.i
			}
			if d.stop.i >= d.listTop+area {
				d.listTop = d.stop.i - area + 1
			}
		}
		d.listTop = max(0, min(d.listTop, max(0, len(d.headers)-area)))
		for i := d.listTop; i < d.listTop+area; i++ {
			if i >= len(d.headers) {
				framed("")
				continue
			}
			h, n := &d.headers[i], i
			on := func(kind string) bool { return d.stop == editorStop{kind, n} }
			remove := sgrDim + "×" + sgrReset
			if on("hremove") {
				remove = sgrBold + sgrRev + "×" + sgrReset
			}
			c.hit(1, frame, frame+nameW-1, func(int) { d.stop = editorStop{"hname", n} })
			c.hit(1, frame+nameW+1, frame+areaW-4, func(int) { d.stop = editorStop{"hvalue", n} })
			c.hit(1, frame+areaW-2, frame+areaW-1, func(int) { d.removeHeader(n) })
			framed(renderField(&h.name, "Name", on("hname"), nameW, sgrUnder) + " " +
				renderField(&h.value, "Value", on("hvalue"), areaW-nameW-4, sgrUnder) + " " + remove)
		}
		shut()
		c.add(sgrDim + clip("Content-Length and Content-Encoding are managed by Jaca.", rightW) + sgrReset)
	}
	right := c

	// Put the columns in the box: side by side, or the form over the response.
	place := func(col *formColumn, row, x int) {
		for _, h := range col.hits {
			b.hits = append(b.hits, boxHit{row: row + h.row, x0: x + h.x0 + 2, x1: x + h.x1 + 2, act: h.act, drag: h.drag})
		}
	}
	base := len(b.rows)
	if wide {
		place(left, base, 0)
		place(right, base, leftW+3)
		blankL, blankR := strings.Repeat(" ", leftW), strings.Repeat(" ", rightW)
		for i := 0; i < max(len(left.lines), len(right.lines)); i++ {
			l, r := blankL, blankR
			if i < len(left.lines) {
				l = left.lines[i]
			}
			if i < len(right.lines) {
				r = right.lines[i]
			}
			b.add(l + " " + sgrDim + "│" + sgrReset + " " + r)
		}
	} else {
		place(left, base, 0)
		for _, l := range left.lines {
			b.add(l)
		}
		b.add("")
		place(right, len(b.rows), 0)
		for _, r := range right.lines {
			b.add(r)
		}
	}
	b.add("")

	// The footer: Enabled, why it can't be saved yet, and Save.
	enabled, enabledStyle := "○ Enabled", sgrDim
	if d.enabled {
		enabled, enabledStyle = "● Enabled", sgrGreen
	}
	if at("enabled") {
		enabledStyle = sgrBold + sgrRev
	}
	const save = "  Save  "
	reason := d.blocked()
	saveStyle := badgeStyle(colorGreen)
	switch {
	case reason != "":
		saveStyle = sgrDim + sgrUnder
	case at("save"):
		saveStyle = badgeStyle(colorGreen) + sgrUnder
	}
	row = ""
	b.button(&row, enabled, enabledStyle, func() { d.enabled = !d.enabled })
	room := inner - cellWidth(enabled) - len(save)
	row += sgrYellow + fit("   "+reason, room-2) + sgrReset + "  "
	b.button(&row, save, saveStyle, v.saveDraft)
	b.add(row)
	return b
}
