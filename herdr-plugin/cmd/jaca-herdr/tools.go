package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// The maintenance panes (Gradle, Xcode), their picker, and what the two panes share: a cursor
// over rows that each carry a button confirmed by a second press, a toast, and a status bar.

// The app's timings: how long a button stays armed, how long a toast shows, and the fade of a
// removed row.
const (
	armWindow = 3 * time.Second
	toastLife = 2600 * time.Millisecond
	fadeTime  = 280 * time.Millisecond
)

// slowTimeout bounds the calls that run du or delete a large folder in jacad.
const slowTimeout = 10 * time.Minute

// toolOwner is the pane a toolPane draws and drives.
type toolOwner interface {
	// lines are the pane's rows at this width: head stays put, body scrolls.
	lines(cols int) (head, body []toolLine)
	rowCount() int      // the rows the cursor moves over
	press(i int)        // one press of row i's button
	rowID(i int) string // what row i is, so a press that waits finds it again
	refresh()
	key(k byte) // a key the shared ones don't take
	helpKeys() [][2]string
	leave(wait bool)
}

// toolLine is one drawn row of a pane.
type toolLine struct {
	text   string
	item   int    // the row under the cursor this line draws, -1 for a heading
	x0, x1 int    // the columns (1-based) of its button, 0 for none
	press  func() // what a click on the button does
}

// heading is a line the cursor skips.
func heading(text string) toolLine { return toolLine{text: text, item: -1} }

// toolPane is the state and the loop methods the Gradle and Xcode panes share.
type toolPane struct {
	p     *pane
	owner toolOwner
	back  func() // returns to the picker when the pane runs inside it

	// The clock and the calls to jacad, replaced in tests. after runs f on the pane's loop once d
	// has passed. spawn runs work off the loop and what it returns on the loop.
	now   func() time.Time
	after func(d time.Duration, f func())
	call  func(method string, params, out any, timeout time.Duration) error
	spawn func(work func() func())

	guard      pressGuard           // so a held key doesn't arm and confirm by itself
	armed      map[string]time.Time // the buttons waiting for their second press, and when each lapses
	toast      string
	toastUntil time.Time
	err        string // why the last call failed, as jacad or the client reported it
	help       bool

	selected int
	top      int  // the first body line drawn
	follow   bool // bring the selected row into view at the next draw
	moved    bool // an event changed the rows since they were drawn

	// As last drawn, for the mouse.
	shown          map[int]toolLine // by screen row (1-based)
	bodyRows       int
	helpX0, helpX1 int
}

func newToolPane(p *pane, back func()) toolPane {
	t := toolPane{p: p, back: back, now: time.Now, armed: map[string]time.Time{}, follow: true}
	t.after = func(d time.Duration, f func()) {
		go func() {
			time.Sleep(d)
			p.post(f) // nothing to undo when the pane has quit
		}()
	}
	t.call = func(method string, params, out any, timeout time.Duration) error {
		return p.c.CallTimeout(method, params, out, timeout)
	}
	t.spawn = func(work func() func()) {
		go func() {
			// A result that arrives after the pane quit is dropped: these calls leave nothing
			// open in jacad.
			p.post(work())
		}()
	}
	return t
}

// confirm is one press of the button named key: the first arms it for armWindow and a second
// within that time confirms. Each button is armed on its own, as the app's rows are.
func (t *toolPane) confirm(key string) bool {
	now := t.now()
	// A second press waits before it gets here (pressGuard): what counts is when it was made.
	if until, ok := t.armed[key]; ok && t.guard.pressed(now).Before(until) {
		delete(t.armed, key)
		return true
	}
	t.armed[key] = now.Add(armWindow)
	t.after(armWindow, t.expire)
	t.after(armWindow+keyRepeat.wait(), t.expire)
	return false
}

func (t *toolPane) isArmed(key string) bool {
	until, ok := t.armed[key]
	return ok && t.now().Before(until)
}

// flash shows a toast for toastLife. A new one replaces the one showing.
func (t *toolPane) flash(msg string) {
	t.toast, t.toastUntil = msg, t.now().Add(toastLife)
	t.after(toastLife, t.expire)
}

// expire drops the armed buttons and the toast whose time has passed.
func (t *toolPane) expire() {
	now := t.now()
	for key, until := range t.armed {
		if !now.Before(until.Add(keyRepeat.wait())) {
			delete(t.armed, key)
		}
	}
	if t.toast != "" && !now.Before(t.toastUntil) {
		t.toast = ""
	}
}

// failed records why a call failed, for the line above the status bar.
func (t *toolPane) failed(err error) {
	if err != nil {
		t.err = err.Error()
	}
}

func (t *toolPane) move(by int) {
	t.selected = clampIndex(t.selected+by, t.owner.rowCount())
	t.follow = true
}

func (t *toolPane) handleKey(k []byte) bool {
	if len(k) == 1 && k[0] == 0x03 {
		return true
	}
	if m, ok := parseMouse(k); ok {
		if m.press && m.button == 0 {
			t.guard.drop()
		}
		t.mouse(m)
		return false
	}
	t.guard.note(k, t.now())
	if t.help {
		if isEsc(k) || isEnter(k) || (len(k) == 1 && (k[0] == 'q' || k[0] == '?')) {
			t.help = false
		}
		return false
	}
	switch {
	case isPageUp(k):
		t.move(-max(1, t.bodyRows))
	case isPageDown(k):
		t.move(max(1, t.bodyRows))
	case isUp(k):
		t.move(-1)
	case isDown(k):
		t.move(1)
	case isEnter(k):
		t.guard.press(t.after, t.pressRow())
	case isEsc(k):
		if t.back != nil {
			t.owner.leave(false)
			t.back()
		}
	case len(k) != 1:
	case k[0] == 'q':
		return true
	case k[0] == 'x' || k[0] == 0x7f || k[0] == 0x08:
		t.guard.press(t.after, t.pressRow())
	case k[0] == 'r':
		t.owner.refresh()
	case k[0] == '?':
		t.help = true
	default:
		// A pane's own keys may confirm too (the Xcode pane's clean-stale).
		key := k[0]
		t.guard.press(t.after, func() { t.owner.key(key) })
	}
	return false
}

// pressRow is a press of the selected row's button for the guard to run, now or after a wait:
// it does nothing when the row under the cursor is no longer the one the key was pressed on.
func (t *toolPane) pressRow() func() {
	if t.moved || t.selected < 0 || t.selected >= t.owner.rowCount() {
		return func() {} // also for rows an update moved and the screen doesn't show yet
	}
	id := t.owner.rowID(t.selected)
	return func() {
		if t.selected >= 0 && t.selected < t.owner.rowCount() && t.owner.rowID(t.selected) == id {
			t.pressSelected()
		}
	}
}

func (t *toolPane) pressSelected() {
	if t.selected >= 0 && t.selected < t.owner.rowCount() {
		t.follow = true // the row a key presses is shown, with its armed button
		t.owner.press(t.selected)
	}
}

func (t *toolPane) mouse(m mouseEvent) {
	const wheelUp, wheelDown, wheelLines = 64, 65, 3
	if !m.press {
		return
	}
	if t.help {
		if m.button == 0 {
			t.help = false
		}
		return
	}
	rows, _ := termSize()
	switch {
	case m.button == wheelUp:
		t.top, t.follow = max(0, t.top-wheelLines), false
	case m.button == wheelDown:
		t.top, t.follow = t.top+wheelLines, false
	case m.button != 0:
	case m.y == rows && t.helpX0 > 0 && m.x >= t.helpX0 && m.x <= t.helpX1:
		t.help = true
	default:
		line, ok := t.shown[m.y]
		if !ok {
			return
		}
		if line.item >= 0 && t.moved {
			return // the row under the pointer is not the one on screen: the click is dropped
		}
		if line.item >= 0 {
			t.selected = clampIndex(line.item, t.owner.rowCount())
		}
		if line.press != nil && line.x0 > 0 && m.x >= line.x0 && m.x <= line.x1 {
			line.press()
		}
	}
}

func (t *toolPane) draw() {
	rows, cols := termSize()
	frame := t.frame(rows, cols)
	if t.help {
		if box := keysBox(t.owner.helpKeys(), rows, cols); len(box) > 0 {
			frame = overlay(frame, box, max(0, (rows-len(box))/2), max(0, (cols-cellWidth(stripSGR(box[0])))/2))
		} else {
			t.help = false // no room to draw it: it is closed, not left holding the keys unseen
		}
	}
	paintRows(frame)
}

// frame is the pane without its popup: the head, the body scrolled to the selected row, and in
// a pane tall enough for them the toast line and the status bar on the last two rows.
func (t *toolPane) frame(rows, cols int) []string {
	t.moved = false
	t.selected = clampIndex(t.selected, t.owner.rowCount())
	head, body := t.owner.lines(cols)
	footer := rows >= len(head)+3
	room := max(1, rows-len(head))
	if footer {
		room = rows - len(head) - 2
	}
	if t.follow {
		for i, line := range body {
			if line.item != t.selected {
				continue
			}
			switch {
			case t.selected == 0 && i < room: // the first row shows with the headings above it
				t.top = 0
			case i < t.top:
				t.top = i
			case i >= t.top+room:
				t.top = i - room + 1
			}
			break
		}
		t.follow = false
	}
	t.top = max(0, min(t.top, len(body)-room))
	t.bodyRows = room

	lines := append([]toolLine(nil), head...)
	lines = append(lines, body[t.top:min(len(body), t.top+room)]...)
	t.helpX0, t.helpX1 = 0, 0
	if footer {
		for len(lines) < rows-2 {
			lines = append(lines, heading(""))
		}
		notice := ""
		switch {
		case t.toast != "":
			notice = sgrBold + clip(sanitize(t.toast), cols) + sgrReset
		case t.err != "":
			notice = sgrRed + clip(sanitize(t.err), cols) + sgrReset
		}
		lines = append(lines, heading(notice), heading(t.statusBar(cols)))
	}
	if len(lines) > rows {
		lines = lines[:max(0, rows)]
	}
	t.shown = map[int]toolLine{}
	frame := make([]string, len(lines))
	for i, line := range lines {
		// A row the layout couldn't fit is cut as plain text and loses its button.
		if cellWidth(stripSGR(line.text)) > cols {
			line = toolLine{text: clip(stripSGR(line.text), cols), item: line.item}
		}
		frame[i] = line.text
		t.shown[i+1] = line
	}
	return frame
}

// statusBar holds Help at its right, as the other viewers' do.
func (t *toolPane) statusBar(cols int) string {
	const help = "Help"
	if cols < len(help) {
		return ""
	}
	t.helpX0, t.helpX1 = cols-len(help)+1, cols
	return strings.Repeat(" ", cols-len(help)) + sgrUnder + help + sgrReset
}

// headingRow is an overline with a value flush right. The value is left out where it doesn't fit.
func headingRow(title, value string, cols int) string {
	room := cols - cellWidth(title) - 2 - cellWidth(value)
	if value == "" || room < 0 {
		return sgrDim + clip(title, cols) + sgrReset
	}
	return sgrDim + title + sgrReset + strings.Repeat(" ", room+2) + value
}

// rowButton is a row's button: its label in red, or the label it has while it waits for its
// second press on a red block.
func rowButton(label, armedLabel string, armed bool) cell {
	if armed {
		return cell{" " + armedLabel + " ", badgeStyle(colorRed)}
	}
	return cell{" " + label + " ", sgrRed}
}

// cell is one column of a list row: its text and the style it is drawn in.
type cell struct{ text, style string }

// rowTable lays list rows out in aligned columns: the first left columns, a gap that takes the
// spare width, then the rest flush right.
type rowTable struct {
	left   int   // how many columns sit before the gap
	flex   int   // the column cut to fit when the pane is narrow
	middle bool  // flex is cut in its middle (a path), not at its end
	right  []int // the right-aligned columns
	min    int   // the width flex keeps before other columns are left out for it
	drop   []int // the columns left out, in this order, while flex would get under min cells
	button int   // the column that holds each row's button
}

// tableRow is one laid-out row and the cells its button takes (0-based; x0 is -1 when the button
// was left out).
type tableRow struct {
	text   string
	x0, x1 int
}

// layout draws rows at most w cells wide. Column 0 is never left out; when the cut column is,
// column 0 is cut instead.
func (t rowTable) layout(rows [][]cell, w int) []tableRow {
	n := 0
	for _, row := range rows {
		n = max(n, len(row))
	}
	out := make([]tableRow, len(rows))
	for i := range out {
		out[i].x0 = -1
	}
	if n == 0 {
		return out
	}
	width := make([]int, n)
	for _, row := range rows {
		for i, c := range row {
			width[i] = max(width[i], cellWidth(c.text))
		}
	}
	present := make([]bool, n)
	for i := range present {
		present[i] = width[i] > 0
	}
	flex, middle := t.flex, t.middle
	if flex < 0 || flex >= n || !present[flex] {
		flex, middle = 0, false
	}
	drop := t.drop
	flexW, fixed := 0, 0
	for {
		fixed = 0
		count := 0
		for i := range present {
			if !present[i] {
				continue
			}
			count++
			if i != flex {
				fixed += width[i]
			}
		}
		fixed += 2 * max(0, count-1)
		flexW = min(width[flex], w-fixed)
		if flexW >= min(width[flex], t.min) || len(drop) == 0 {
			break
		}
		d := drop[0]
		drop = drop[1:]
		if d <= 0 || d >= n || !present[d] {
			continue
		}
		present[d] = false
		if d == flex {
			flex, middle = 0, false
		}
	}
	flexW = max(0, flexW)
	slack := max(0, w-fixed-flexW)
	rightAligned := map[int]bool{}
	for _, i := range t.right {
		rightAligned[i] = true
	}

	for r, row := range rows {
		var b strings.Builder
		used, gapDone := 0, false
		for i := 0; i < n; i++ {
			if !present[i] {
				continue
			}
			if used > 0 {
				b.WriteString("  ")
				used += 2
			}
			if i >= t.left && !gapDone {
				b.WriteString(strings.Repeat(" ", slack))
				used += slack
				gapDone = true
			}
			var c cell
			if i < len(row) {
				c = row[i]
			}
			cw := width[i]
			if i == flex {
				cw = flexW
				if middle {
					c.text = clipMiddle(c.text, cw)
				} else {
					c.text = clip(c.text, cw)
				}
			}
			tw := cellWidth(c.text)
			pad := strings.Repeat(" ", max(0, cw-tw))
			if rightAligned[i] {
				b.WriteString(pad)
				used += len(pad)
				pad = ""
			}
			if i == t.button && tw > 0 {
				out[r].x0, out[r].x1 = used, used+tw-1
			}
			if c.style != "" && tw > 0 {
				b.WriteString(c.style + c.text + sgrReset)
			} else {
				b.WriteString(c.text)
			}
			b.WriteString(pad)
			used += tw + len(pad)
		}
		out[r].text = b.String()
	}
	return out
}

// clipMiddle truncates s (plain text) to at most w cells by replacing its middle with "…", as
// the app truncates a path.
func clipMiddle(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if cellWidth(s) <= w {
		return s
	}
	if w < 3 {
		return clip(s, w)
	}
	headW := (w - 1) / 2
	var head strings.Builder
	used, prev := 0, 0
	for _, r := range s {
		rw := widthAfter(r, prev)
		if used+rw > headW {
			break
		}
		head.WriteRune(r)
		used += rw
		prev = runeWidth(r)
	}
	return head.String() + "…" + tailCells(s, w-1-used)
}

// tableLines turns laid-out rows into the pane's lines, the first of them row first of the
// cursor: the selection marker in front, a row being removed dimmed, and each button's columns
// recorded for the mouse.
func tableLines(laid []tableRow, first, selected int, removing func(i int) bool, press func(i int)) []toolLine {
	lines := make([]toolLine, len(laid))
	for i, row := range laid {
		item := first + i
		text := row.text
		if removing(i) {
			text = sgrDim + stripSGR(text) + sgrReset
		}
		line := toolLine{text: rowMarker(item == selected) + text, item: item}
		if row.x0 >= 0 {
			// Two cells of marker before the row, and columns count from 1.
			line.x0, line.x1 = row.x0+3, row.x1+3
			line.press = func() { press(item) }
		}
		lines[i] = line
	}
	return lines
}

// toolOption is one pane the tools picker opens: its title (the app's sidebar title), the
// plugin entrypoint and the name of the tab it opens in.
type toolOption struct {
	title      string
	entrypoint string
	tab        string
}

var toolOptions = []toolOption{
	{"Gradle", "gradle", "Gradle - Jaca"},
	{"Xcode", "xcode", "Xcode - Jaca"},
}

// toolsPicker lists the maintenance panes. Choosing one opens it in a new Herdr tab (launch), or
// in this pane when there is no Herdr to open one.
type toolsPicker struct {
	p      *pane
	launch bool

	selected  int
	launching bool   // a tab is being opened
	err       string // why the last tab didn't open, as Herdr reported it

	firstRow         int // the screen row (1-based) of the first option, as last drawn
	closeX0, closeX1 int // the close button's columns on the second row, as last drawn
}

// runToolsPane is the picker: j/k move, Enter or a click chooses, Esc or q closes.
func runToolsPane() int {
	c, err := dial()
	if err != nil {
		fmt.Fprintln(os.Stderr, "jaca:", err)
		return 1
	}
	defer c.Close()
	launch := os.Getenv("HERDR_BIN_PATH") != "" || os.Getenv("HERDR_ENV") != ""
	return runPane(c, func(p *pane) screen { return &toolsPicker{p: p, launch: launch} })
}

func (t *toolsPicker) handleEvent(event) {}

func (t *toolsPicker) leave(bool) {}

func (t *toolsPicker) handleKey(k []byte) bool {
	if m, ok := parseMouse(k); ok {
		if !m.press || m.button != 0 {
			return false
		}
		// A click on the close button is Esc; one on an option chooses it.
		if m.y == 2 && m.x >= t.closeX0 && m.x <= t.closeX1 {
			return true
		}
		if i := m.y - t.firstRow; t.firstRow > 0 && i >= 0 && i < len(toolOptions) {
			t.selected = i
			t.confirm()
		}
		return false
	}
	switch {
	case len(k) == 1 && (k[0] == 'q' || k[0] == 0x03), isEsc(k):
		return true
	case isUp(k):
		t.selected = clampIndex(t.selected-1, len(toolOptions))
	case isDown(k):
		t.selected = clampIndex(t.selected+1, len(toolOptions))
	case isEnter(k):
		t.confirm()
	}
	return false
}

func (t *toolsPicker) confirm() {
	if t.launching || t.selected < 0 || t.selected >= len(toolOptions) {
		return
	}
	option := toolOptions[t.selected]
	if !t.launch {
		t.err = ""
		if option.entrypoint == "xcode" {
			t.p.screen = newXcodeViewer(t.p, t.back)
		} else {
			t.p.screen = newGradleViewer(t.p, t.back)
		}
		return
	}
	t.launching, t.err = true, ""
	go func() {
		err := openTab(option.entrypoint, option.tab)
		t.p.post(func() {
			t.launching = false
			if err != nil {
				t.err = err.Error()
				return
			}
			t.p.quit = true
		})
	}()
}

// back returns from a pane that ran in this one.
func (t *toolsPicker) back() { t.p.screen = t }

func (t *toolsPicker) draw() {
	rows, cols := termSize()
	paintRows(panelFrame(t.frame(rows, cols), rows, cols))
}

// frame is the popup's rows: the close button at the top right, a row down and a cell in from
// the border as in the device picker, then the options.
func (t *toolsPicker) frame(rows, cols int) []string {
	frame := []string{""}
	t.closeX0, t.closeX1, t.firstRow = 0, 0, 0
	if w := cols - len(escClose) - 1; w >= 0 {
		t.closeX0, t.closeX1 = w+1, w+len(escClose)
		frame = append(frame, strings.Repeat(" ", w)+closeButton(escClose))
	} else {
		frame = append(frame, "")
	}
	frame = append(frame, "") // a row between the button and the selected row's highlight
	if t.err != "" {
		frame = append(frame, sgrRed+clip(sanitize(t.err), cols)+sgrReset)
	}
	t.firstRow = len(frame) + 1
	for i, option := range toolOptions {
		row := listRow(clip(option.title, max(0, cols-2)), i == t.selected, cols)
		if cellWidth(stripSGR(row)) > cols {
			row = clip(option.title, cols)
		}
		frame = append(frame, row)
	}
	if len(frame) > rows {
		frame = frame[:max(0, rows)]
	}
	return frame
}
