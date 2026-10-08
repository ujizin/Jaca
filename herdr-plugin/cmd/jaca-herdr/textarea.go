package main

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// textArea is a multi-line plain text editor: the override body. Lines don't wrap; the view
// scrolls to keep the cursor in sight. Shift with a movement key selects, and typing or deleting
// replaces the selection.
type textArea struct {
	lines    [][]rune
	row, col int // the cursor: a line and a position in it, 0..len
	top      int // the first line drawn
	left     int // the first cell drawn

	selecting      bool // text is selected: from the anchor to the cursor
	selRow, selCol int  // the anchor

	undo, redo []textSnapshot
	lastEdit   string // the kind of the last change, so a run of typing undoes as one step
}

// textSnapshot is the text and cursor at one point, to go back to.
type textSnapshot struct {
	text     string
	row, col int
}

// undoLimit bounds the undo history.
const undoLimit = 200

func (t *textArea) snapshot() textSnapshot { return textSnapshot{t.String(), t.row, t.col} }

func (t *textArea) restore(s textSnapshot) {
	t.setText(s.text)
	t.row, t.col, t.selecting = s.row, s.col, false
	t.ensure()
}

// checkpoint records the text as an undo step before a change of the given kind. Consecutive
// changes of the same kind ("type", "delete") share one step; "" always starts a new one.
func (t *textArea) checkpoint(kind string) {
	if kind != "" && kind == t.lastEdit {
		return
	}
	t.undo = append(t.undo, t.snapshot())
	if len(t.undo) > undoLimit {
		t.undo = t.undo[len(t.undo)-undoLimit:]
	}
	t.redo, t.lastEdit = nil, kind
}

// stepBack undoes the last step; stepForward redoes it. Each reports whether there was one.
func (t *textArea) stepBack() bool {
	if len(t.undo) == 0 {
		return false
	}
	t.redo = append(t.redo, t.snapshot())
	t.restore(t.undo[len(t.undo)-1])
	t.undo, t.lastEdit = t.undo[:len(t.undo)-1], ""
	return true
}

func (t *textArea) stepForward() bool {
	if len(t.redo) == 0 {
		return false
	}
	t.undo = append(t.undo, t.snapshot())
	t.restore(t.redo[len(t.redo)-1])
	t.redo, t.lastEdit = t.redo[:len(t.redo)-1], ""
	return true
}

// replace sets the whole text as one undoable step (Format).
func (t *textArea) replace(text string) {
	t.checkpoint("")
	t.setText(text)
	t.row, t.col, t.selecting, t.lastEdit = 0, 0, false, ""
}

// set loads a text to edit, with a fresh undo history.
func (t *textArea) set(text string) {
	t.setText(text)
	t.row, t.col, t.top, t.left, t.selecting = 0, 0, 0, 0, false
	t.undo, t.redo, t.lastEdit = nil, nil, ""
}

func (t *textArea) setText(text string) {
	t.lines = nil
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		t.lines = append(t.lines, []rune(line))
	}
}

func (t *textArea) String() string {
	parts := make([]string, len(t.lines))
	for i, line := range t.lines {
		parts[i] = string(line)
	}
	return strings.Join(parts, "\n")
}

// ensure keeps the area editable: at least one line, the cursor and the anchor inside the text.
func (t *textArea) ensure() {
	if len(t.lines) == 0 {
		t.lines = [][]rune{nil}
	}
	t.row = max(0, min(t.row, len(t.lines)-1))
	t.col = max(0, min(t.col, len(t.lines[t.row])))
	t.selRow = max(0, min(t.selRow, len(t.lines)-1))
	t.selCol = max(0, min(t.selCol, len(t.lines[t.selRow])))
}

// selection is the selected range in reading order, and whether anything is selected.
func (t *textArea) selection() (r0, c0, r1, c1 int, ok bool) {
	if !t.selecting || (t.selRow == t.row && t.selCol == t.col) {
		return 0, 0, 0, 0, false
	}
	if t.selRow < t.row || (t.selRow == t.row && t.selCol < t.col) {
		return t.selRow, t.selCol, t.row, t.col, true
	}
	return t.row, t.col, t.selRow, t.selCol, true
}

// selectedText is the selection's text, "" when there is none.
func (t *textArea) selectedText() string {
	r0, c0, r1, c1, ok := t.selection()
	if !ok {
		return ""
	}
	if r0 == r1 {
		return string(t.lines[r0][c0:c1])
	}
	parts := []string{string(t.lines[r0][c0:])}
	for r := r0 + 1; r < r1; r++ {
		parts = append(parts, string(t.lines[r]))
	}
	return strings.Join(append(parts, string(t.lines[r1][:c1])), "\n")
}

// deleteSelection removes the selected text and leaves the cursor where it was. It reports
// whether there was a selection.
func (t *textArea) deleteSelection() bool {
	r0, c0, r1, c1, ok := t.selection()
	t.selecting = false
	if !ok {
		return false
	}
	joined := append(append([]rune(nil), t.lines[r0][:c0]...), t.lines[r1][c1:]...)
	t.lines = append(append(t.lines[:r0:r0], joined), t.lines[r1+1:]...)
	t.row, t.col = r0, c0
	return true
}

func (t *textArea) selectAll() {
	t.ensure()
	t.selecting, t.selRow, t.selCol = true, 0, 0
	t.row = len(t.lines) - 1
	t.col = len(t.lines[t.row])
}

func (t *textArea) insert(r rune) {
	line := t.lines[t.row]
	line = append(line[:t.col], append([]rune{r}, line[t.col:]...)...)
	t.lines[t.row] = line
	t.col++
}

func (t *textArea) newline() {
	line := t.lines[t.row]
	rest := append([]rune(nil), line[t.col:]...)
	t.lines[t.row] = line[:t.col]
	t.lines = append(t.lines[:t.row+1], append([][]rune{rest}, t.lines[t.row+1:]...)...)
	t.row, t.col = t.row+1, 0
}

// navKey is a movement or delete key with its modifiers.
type navKey struct {
	dir   string // up, down, left, right, home, end, pgup, pgdn, delete
	shift bool   // extends the selection
	word  bool   // by word (Alt or Ctrl)
}

// decodeNav reads a cursor key as terminals send it: plain (ESC [ A), or with a modifier
// (ESC [ 1 ; m A, ESC [ 5 ; m ~), where m is 2 for Shift, 3 Alt, 5 Ctrl and one more with Shift.
// Alt+Left and Alt+Right also come as ESC b and ESC f.
func decodeNav(k []byte) (navKey, bool) {
	s := string(k)
	switch s {
	case "\x1bb":
		return navKey{dir: "left", word: true}, true
	case "\x1bf":
		return navKey{dir: "right", word: true}, true
	}
	if len(s) < 3 || s[0] != 0x1b || (s[1] != '[' && s[1] != 'O') {
		return navKey{}, false
	}
	body, final := s[2:len(s)-1], s[len(s)-1]
	number, modifier, _ := strings.Cut(body, ";")
	var key navKey
	switch modifier {
	case "", "1":
	case "2":
		key.shift = true
	case "3", "5":
		key.word = true
	case "4", "6":
		key.shift, key.word = true, true
	default:
		return navKey{}, false
	}
	switch {
	case final == 'A':
		key.dir = "up"
	case final == 'B':
		key.dir = "down"
	case final == 'C':
		key.dir = "right"
	case final == 'D':
		key.dir = "left"
	case final == 'H', final == '~' && (number == "1" || number == "7"):
		key.dir = "home"
	case final == 'F', final == '~' && (number == "4" || number == "8"):
		key.dir = "end"
	case final == '~' && number == "5":
		key.dir = "pgup"
	case final == '~' && number == "6":
		key.dir = "pgdn"
	case final == '~' && number == "3":
		key.dir = "delete"
	default:
		return navKey{}, false
	}
	return key, true
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' }

// wordLeft and wordRight are the cursor column a word away on the current line.
func (t *textArea) wordLeft() int {
	line, c := t.lines[t.row], t.col
	for c > 0 && !isWordRune(line[c-1]) {
		c--
	}
	for c > 0 && isWordRune(line[c-1]) {
		c--
	}
	return c
}

func (t *textArea) wordRight() int {
	line, c := t.lines[t.row], t.col
	for c < len(line) && !isWordRune(line[c]) {
		c++
	}
	for c < len(line) && isWordRune(line[c]) {
		c++
	}
	return c
}

// move moves the cursor. With Shift the selection extends from where it started; without, any
// selection is dropped.
func (t *textArea) move(key navKey, page int) {
	if key.shift && !t.selecting {
		t.selecting, t.selRow, t.selCol = true, t.row, t.col
	} else if !key.shift {
		t.selecting = false
	}
	line := t.lines[t.row]
	switch key.dir {
	case "up":
		t.row--
	case "down":
		t.row++
	case "pgup":
		t.row -= max(1, page)
	case "pgdn":
		t.row += max(1, page)
	case "home":
		t.col = 0
	case "end":
		t.col = len(line)
	case "left":
		switch {
		case key.word && t.col > 0:
			t.col = t.wordLeft()
		case t.col > 0:
			t.col--
		case t.row > 0:
			t.row--
			t.col = len(t.lines[t.row])
		}
	case "right":
		switch {
		case key.word && t.col < len(line):
			t.col = t.wordRight()
		case t.col < len(line):
			t.col++
		case t.row < len(t.lines)-1:
			t.row, t.col = t.row+1, 0
		}
	}
	t.row = max(0, min(t.row, len(t.lines)-1))
	t.col = max(0, min(t.col, len(t.lines[t.row])))
	t.lastEdit = "" // moving the cursor ends a run of typing
}

// The keys that undo and redo. A terminal keeps ⌘Z for itself by default, so Ctrl+Z is the undo
// key. When the terminal is set to pass ⌘ keys on, Herdr sends them to a pane in the Kitty
// keyboard form (ESC [ code ; 9 u, 10 with Shift), which is taken too.
func isUndoKey(k []byte) bool {
	return (len(k) == 1 && k[0] == 0x1a) || string(k) == "\x1b[122;9u"
}

func isRedoKey(k []byte) bool {
	return (len(k) == 1 && k[0] == 0x19) || string(k) == "\x1b[122;10u" || string(k) == "\x1b[90;10u"
}

// handle applies a key and reports whether the text changed.
//
// Text and pasted text (with its line breaks) insert, replacing a selection. Enter breaks the
// line. Backspace and Delete remove the selection, else a character; Alt+Backspace or Ctrl+W a
// word. The arrows, Home, End, PgUp and PgDn move, by word with Alt or Ctrl, and select with
// Shift. Tab indents (every selected line, when several are) and Shift+Tab takes a level off.
// Ctrl+A selects everything, Ctrl+E goes to the line's end. Ctrl+Z undoes, Ctrl+Y redoes. page is
// how many lines a page key moves.
func (t *textArea) handle(k []byte, page int) bool {
	t.ensure()
	if len(k) == 0 {
		return false
	}
	switch {
	case isUndoKey(k):
		return t.stepBack()
	case isRedoKey(k):
		return t.stepForward()
	}
	if key, ok := decodeNav(k); ok {
		if key.dir != "delete" {
			t.move(key, page)
			return false
		}
		return t.remove(false, key.word)
	}
	if k[0] == 0x1b {
		switch string(k) {
		case "\x1b[Z": // Shift+Tab
			return t.shift(true)
		case "\x1b\x7f": // Alt+Backspace
			return t.remove(true, true)
		case "\x1b[97;9u": // ⌘A, where the terminal passes it on
			t.selectAll()
			t.lastEdit = ""
		}
		return false // else a sequence this editor has no use for
	}
	if len(k) == 1 {
		switch k[0] {
		case 0x7f, 0x08:
			return t.remove(true, false)
		case 0x17: // Ctrl-W
			return t.remove(true, true)
		case 0x01: // Ctrl-A
			t.selectAll()
			t.lastEdit = ""
			return false
		case 0x05: // Ctrl-E
			t.move(navKey{dir: "end"}, page)
			return false
		case '\t':
			// Over several selected lines Tab indents them all; else it inserts a level at the cursor.
			if first, last := t.blockRows(); last > first {
				return t.shift(false)
			}
		}
	}
	// One typed character joins the run of typing before it; a line break, a paste or typing
	// over a selection is its own step.
	kind := "type"
	if _, _, _, _, selected := t.selection(); selected || utf8.RuneCount(k) != 1 || k[0] < 0x20 {
		kind = ""
	}
	before, lastEdit, steps := t.snapshot(), t.lastEdit, len(t.undo)
	t.checkpoint(kind)
	t.deleteSelection()
	for len(k) > 0 {
		r, size := utf8.DecodeRune(k)
		k = k[size:]
		switch {
		case r == '\r' || r == '\n':
			t.newline()
		case r == '\t':
			for n := 0; n < indentWidth; n++ {
				t.insert(' ')
			}
		case r == utf8.RuneError || r < 0x20 || r == 0x7f:
		default:
			t.insert(r)
		}
	}
	if t.String() == before.text { // nothing was inserted after all
		t.undo, t.lastEdit = t.undo[:steps], lastEdit
		return false
	}
	return true
}

// remove deletes the selection if there is one; else the character before the cursor (back) or
// under it, or with word the word on that side. It reports whether the text changed.
func (t *textArea) remove(back, word bool) bool {
	if _, _, _, _, selected := t.selection(); selected {
		t.checkpoint("")
		t.deleteSelection()
		t.lastEdit = ""
		return true
	}
	t.selecting = false
	line := t.lines[t.row]
	switch {
	case back && t.col > 0:
		t.checkpoint("delete")
		from := t.col - 1
		if word {
			from = t.wordLeft()
		}
		t.lines[t.row] = append(line[:from], line[t.col:]...)
		t.col = from
	case back && t.row > 0:
		t.checkpoint("delete")
		prev := t.lines[t.row-1]
		t.col = len(prev)
		t.lines[t.row-1] = append(prev, line...)
		t.lines = append(t.lines[:t.row], t.lines[t.row+1:]...)
		t.row--
	case !back && t.col < len(line):
		t.checkpoint("delete")
		to := t.col + 1
		if word {
			to = t.wordRight()
		}
		t.lines[t.row] = append(line[:t.col], line[to:]...)
	case !back && t.row < len(t.lines)-1:
		t.checkpoint("delete")
		t.lines[t.row] = append(line, t.lines[t.row+1]...)
		t.lines = append(t.lines[:t.row+1], t.lines[t.row+2:]...)
	default:
		return false
	}
	return true
}

// indentWidth is how many spaces Tab adds and Shift+Tab takes away.
const indentWidth = 2

// blockRows is the lines a block indent acts on: the selected ones (not a last line the
// selection only reaches the start of), else the cursor's.
func (t *textArea) blockRows() (first, last int) {
	r0, _, r1, c1, ok := t.selection()
	if !ok {
		return t.row, t.row
	}
	if c1 == 0 && r1 > r0 {
		r1--
	}
	return r0, r1
}

// shift moves the block's lines right by a level, or left by up to one (never past the text),
// keeping the cursor and the selection on the same text. It reports whether anything changed.
func (t *textArea) shift(out bool) bool {
	first, last := t.blockRows()
	delta := make(map[int]int, last-first+1)
	for r := first; r <= last; r++ {
		if !out {
			delta[r] = indentWidth
			continue
		}
		for n := 0; n < indentWidth && n < len(t.lines[r]) && t.lines[r][n] == ' '; n++ {
			delta[r]--
		}
	}
	changed := false
	for _, d := range delta {
		changed = changed || d != 0
	}
	if !changed {
		return false
	}
	t.checkpoint("")
	for r, d := range delta {
		if d > 0 {
			t.lines[r] = append([]rune(strings.Repeat(" ", d)), t.lines[r]...)
		} else {
			t.lines[r] = append([]rune(nil), t.lines[r][-d:]...)
		}
	}
	t.col = max(0, t.col+delta[t.row])
	t.selCol = max(0, t.selCol+delta[t.selRow])
	t.lastEdit = ""
	return true
}

// cut removes the selection and returns its text, "" when there is none.
func (t *textArea) cut() string {
	text := t.selectedText()
	if text != "" {
		t.remove(true, false)
	}
	return text
}

// at is the text position under a cell of the area as last drawn (0-based row and column).
func (t *textArea) at(row, col int) (r, c int) {
	t.ensure()
	r = max(0, min(t.top+row, len(t.lines)-1))
	line, cell := t.lines[r], 0
	c = len(line)
	for i, ch := range line {
		if cell >= t.left+col {
			c = i
			break
		}
		cell += runeWidth(ch)
	}
	return r, c
}

// click puts the cursor at a cell of the area and drops the selection; dragTo extends a
// selection from there to another cell.
func (t *textArea) click(row, col int) {
	t.row, t.col = t.at(row, col)
	t.selecting, t.lastEdit = false, ""
}

func (t *textArea) dragTo(row, col int) {
	if !t.selecting {
		t.selecting, t.selRow, t.selCol = true, t.row, t.col
	}
	t.row, t.col = t.at(row, col)
}

// view draws the area w cells wide and h rows tall: the selection reversed, and when focused
// with nothing selected the cursor as a reversed cell.
func (t *textArea) view(w, h int, focused bool) []string {
	t.ensure()
	if w <= 0 || h <= 0 {
		return nil
	}
	// Keep the cursor inside the window.
	if t.row < t.top {
		t.top = t.row
	}
	if t.row >= t.top+h {
		t.top = t.row - h + 1
	}
	t.top = max(0, min(t.top, max(0, len(t.lines)-h)))
	cursorCell := cellWidth(string(t.lines[t.row][:t.col]))
	if cursorCell < t.left {
		t.left = cursorCell
	}
	if cursorCell >= t.left+w {
		t.left = cursorCell - w + 1
	}
	r0, c0, r1, c1, selected := t.selection()
	inSelection := func(row, col int) bool {
		if !selected || row < r0 || row > r1 {
			return false
		}
		return (row > r0 || col >= c0) && (row < r1 || col < c1)
	}
	out := make([]string, 0, h)
	for i := t.top; i < t.top+h; i++ {
		if i >= len(t.lines) {
			out = append(out, strings.Repeat(" ", w))
			continue
		}
		var b strings.Builder
		cell, used := 0, 0
		drewCursor := false
		for j, r := range t.lines[i] {
			rw := runeWidth(r)
			if r < 0x20 || r == 0x7f {
				r, rw = ' ', 1
			}
			if cell >= t.left && used+rw <= w {
				cursor := focused && !selected && i == t.row && j == t.col
				if cursor || inSelection(i, j) {
					b.WriteString(sgrRev + string(r) + sgrReset)
					drewCursor = drewCursor || cursor
				} else {
					b.WriteRune(r)
				}
				used += rw
			}
			cell += rw
		}
		// The end of a line: the cursor's cell there, or a selected line break.
		if used < w {
			atEnd := focused && !selected && i == t.row && !drewCursor
			if atEnd || inSelection(i, len(t.lines[i])) {
				b.WriteString(sgrRev + " " + sgrReset)
				used++
			}
		}
		out = append(out, b.String()+strings.Repeat(" ", max(0, w-used)))
	}
	return out
}
