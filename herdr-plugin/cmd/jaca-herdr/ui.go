package main

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

// screen is one full-pane view. Its methods run on the pane's loop only.
type screen interface {
	handleEvent(ev event)
	handleKey(k []byte) bool // true quits the pane
	draw()
	// leave releases what the screen holds in jacad. While the pane keeps running the calls go
	// off the loop; on exit (wait) they finish before the connection closes.
	leave(wait bool)
}

// pane runs one screen at a time over a jacad connection: the shared loop of the device picker
// and the log viewer.
type pane struct {
	c      *client
	screen screen
	quit   bool          // set by a screen (from a posted result) to end the pane
	work   chan func()   // results of off-loop calls, run on the loop
	done   chan struct{} // closed when the pane exits

	// The open session's id and the method that closes it, for the signal path, which runs off
	// the loop.
	liveMu    sync.Mutex
	liveID    string
	liveClose string
}

// post runs f on the pane's loop. False when the pane has quit, so f never runs.
func (p *pane) post(f func()) bool {
	select {
	case p.work <- f:
		return true
	case <-p.done:
		return false
	}
}

// setLive records the session jacad holds for this pane ("" for none) and the method that
// closes it (logs.close, network.close).
func (p *pane) setLive(closeMethod, id string) {
	p.liveMu.Lock()
	p.liveClose, p.liveID = closeMethod, id
	p.liveMu.Unlock()
}

// runPane drives the screen first returns until a key quits it or jacad goes away.
func runPane(c *client, first func(p *pane) screen) int {
	p := &pane{c: c, work: make(chan func()), done: make(chan struct{})}
	p.screen = first(p)
	defer close(p.done)
	// Deferred before restore so it runs after it: the terminal is back before teardown waits.
	defer func() { p.screen.leave(true) }()
	restore, err := enterRaw()
	if err != nil {
		fmt.Fprintln(os.Stderr, "jaca: not a terminal:", err)
		return 1
	}
	defer restore()
	// A signal (Herdr closing the pane) skips the deferred leave: close the session there too, or
	// it streams in jacad until the orphan reaper runs. A logs.open still in flight has no id
	// yet; that session is left to the reaper.
	onSignalExit(func() {
		p.liveMu.Lock()
		method, id := p.liveClose, p.liveID
		p.liveMu.Unlock()
		if id != "" {
			_ = c.CallTimeout(method, map[string]any{"id": id}, nil, teardownTimeout)
		}
	})

	paint(mouseOn)
	keys := make(chan []byte, 16)
	go readRawKeys(keys)
	resize := make(chan os.Signal, 1)
	signal.Notify(resize, syscall.SIGWINCH)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()

	// Keys, results and resizes redraw at once; events only mark the pane dirty and the tick
	// draws, so a chatty device costs one frame per tick rather than one per batch.
	p.screen.draw()
	dirty := false
	for {
		select {
		case ev := <-c.Events:
			p.screen.handleEvent(ev)
			dirty = true
			continue
		case <-c.Closed:
			restore()
			fmt.Fprintln(os.Stderr, "jaca: jacad closed the connection")
			return 1
		case k, ok := <-keys:
			if !ok || p.screen.handleKey(k) {
				return 0
			}
		case f := <-p.work:
			f()
			if p.quit {
				return 0
			}
		case <-resize:
			refreshTermSize()
		case <-tick.C:
			if !dirty {
				continue
			}
		}
		p.screen.draw()
		dirty = false
	}
}

// clampIndex keeps a selection inside 0..<n (0 for an empty list).
func clampIndex(i, n int) int {
	return max(0, min(i, n-1))
}

// listWindow is the first row to draw so that selected stays inside room rows.
func listWindow(selected, room int) int {
	if selected >= room {
		return selected - room + 1
	}
	return 0
}

// rowMarker is the two cells in front of a list row: a bar on the selected one.
func rowMarker(selected bool) string {
	if selected {
		return sgrRev + " " + sgrReset + " "
	}
	return "  "
}

// listRow draws a list row w cells wide: the selected one as a reversed bar across the whole
// row (its own colors dropped), the others as they are, indented alike.
func listRow(content string, selected bool, w int) string {
	if !selected {
		return "  " + content
	}
	return sgrRev + sgrBold + fit("  "+stripSGR(content), w) + sgrReset
}

// titledBorder is a popup's top border w cells wide with its title set into it, the way Herdr
// titles its own popups. A title too long for the border is left out.
func titledBorder(title string, w int) string {
	// The corners, "─ " before the title and a space after it leave the rest for the rule.
	rest := w - 5 - cellWidth(title)
	if rest < 1 {
		return "╭" + strings.Repeat("─", max(0, w-2)) + "╮"
	}
	return "╭─ " + sgrBold + title + sgrReset + " " + strings.Repeat("─", rest) + "╮"
}

func isEnter(k []byte) bool { return len(k) == 1 && (k[0] == '\r' || k[0] == '\n') }
func isEsc(k []byte) bool   { return len(k) == 1 && k[0] == 0x1b }

// isArrowUp and isArrowDown are the arrow keys alone, for lists under a text field (where j
// and k are text).
func isArrowUp(k []byte) bool   { return len(k) >= 3 && k[0] == 0x1b && k[1] == '[' && k[2] == 'A' }
func isArrowDown(k []byte) bool { return len(k) >= 3 && k[0] == 0x1b && k[1] == '[' && k[2] == 'B' }

func isPageUp(k []byte) bool {
	return len(k) >= 4 && k[0] == 0x1b && k[1] == '[' && k[2] == '5' && k[3] == '~'
}

// isShiftPageDown is Shift+PgDn, which terminals send as PgDn with modifier 2.
func isShiftPageDown(k []byte) bool { return string(k) == "\x1b[6;2~" }

func isPageDown(k []byte) bool {
	return len(k) >= 4 && k[0] == 0x1b && k[1] == '[' && k[2] == '6' && k[3] == '~'
}

// textInput is a one-line text field with a cursor.
type textInput struct {
	text []rune
	pos  int // the cursor, 0..len(text)
}

func (t *textInput) String() string { return string(t.text) }

// set replaces the text and puts the cursor at its end.
func (t *textInput) set(s string) { t.text = []rune(s); t.pos = len(t.text) }

// handle applies an editing key and reports whether the text changed. Text inserts at the
// cursor. Left, Right, Home and End move it (by word with Alt or Ctrl); Ctrl-A and Ctrl-E are
// Home and End. Backspace and Delete remove a character, Alt+Backspace or Ctrl-W the word
// before the cursor, Ctrl-U everything. Other escape sequences are ignored.
func (t *textInput) handle(k []byte) bool {
	if len(k) == 0 {
		return false
	}
	t.pos = max(0, min(t.pos, len(t.text)))
	wordStart := func() int {
		c := t.pos
		for c > 0 && !isWordRune(t.text[c-1]) {
			c--
		}
		for c > 0 && isWordRune(t.text[c-1]) {
			c--
		}
		return c
	}
	cut := func(from, to int) bool {
		if from < 0 || from >= to {
			return false
		}
		t.text = append(t.text[:from:from], t.text[to:]...)
		t.pos = from
		return true
	}
	if key, ok := decodeNav(k); ok {
		switch key.dir {
		case "left":
			if key.word {
				t.pos = wordStart()
			} else {
				t.pos = max(0, t.pos-1)
			}
		case "right":
			if key.word {
				for t.pos < len(t.text) && !isWordRune(t.text[t.pos]) {
					t.pos++
				}
				for t.pos < len(t.text) && isWordRune(t.text[t.pos]) {
					t.pos++
				}
			} else {
				t.pos = min(len(t.text), t.pos+1)
			}
		case "home":
			t.pos = 0
		case "end":
			t.pos = len(t.text)
		case "delete":
			return cut(t.pos, min(len(t.text), t.pos+1))
		}
		return false
	}
	if k[0] == 0x1b {
		return string(k) == "\x1b\x7f" && cut(wordStart(), t.pos) // Alt+Backspace
	}
	if len(k) == 1 {
		switch k[0] {
		case 0x7f, 0x08:
			return cut(t.pos-1, t.pos)
		case 0x17:
			return cut(wordStart(), t.pos)
		case 0x15:
			t.pos = len(t.text)
			return cut(0, t.pos)
		case 0x01:
			t.pos = 0
			return false
		case 0x05:
			t.pos = len(t.text)
			return false
		}
	}
	changed := false
	for len(k) > 0 {
		r, size := utf8.DecodeRune(k)
		k = k[size:]
		if r == utf8.RuneError || r < 0x20 || r == 0x7f {
			continue
		}
		t.text = append(t.text[:t.pos:t.pos], append([]rune{r}, t.text[t.pos:]...)...)
		t.pos++
		changed = true
	}
	return changed
}

const sgrUnder = "\x1b[4m"

// renderField draws a text field w cells wide: the placeholder while empty, and when focused the
// cursor as a reversed cell, with the text scrolled to keep it in view. style underlines the
// field (sgrUnder), or is empty for a field that sits inside a box.
func renderField(t *textInput, placeholder string, focused bool, w int, style string) string {
	if w <= 0 {
		return ""
	}
	switch {
	case !focused && len(t.text) == 0:
		return style + sgrDim + fit(placeholder, w) + sgrReset
	case !focused:
		return style + fit(sanitize(t.String()), w) + sgrReset
	case len(t.text) == 0:
		return sgrRev + " " + sgrReset + style + sgrDim + fit(placeholder, w-1) + sgrReset
	}
	pos := max(0, min(t.pos, len(t.text)))
	start := 0
	for cellWidth(string(t.text[start:pos])) > w-1 { // scroll until the cursor's cell fits
		start++
	}
	var b strings.Builder
	b.WriteString(style)
	used := 0
	for i := start; i < len(t.text); i++ {
		r := t.text[i]
		if r < 0x20 || r == 0x7f {
			r = ' '
		}
		rw := runeWidth(r)
		if used+rw > w {
			break
		}
		if i == pos {
			b.WriteString(sgrReset + sgrRev + string(r) + sgrReset + style)
		} else {
			b.WriteRune(r)
		}
		used += rw
	}
	if pos == len(t.text) && used < w {
		b.WriteString(sgrReset + sgrRev + " " + sgrReset + style)
		used++
	}
	return b.String() + strings.Repeat(" ", max(0, w-used)) + sgrReset
}

// tailCells is the longest suffix of s (plain text) that fits in w display cells.
func tailCells(s string, w int) string {
	rs := []rune(s)
	start := len(rs)
	for start > 0 && cellWidth(string(rs[start-1:])) <= w {
		start--
	}
	return string(rs[start:])
}
