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

	// The open log session's id for the signal path, which runs off the loop.
	liveMu sync.Mutex
	liveID string
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

func (p *pane) setLive(id string) {
	p.liveMu.Lock()
	p.liveID = id
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
		id := p.liveID
		p.liveMu.Unlock()
		if id != "" {
			_ = c.CallTimeout("logs.close", map[string]any{"id": id}, nil, teardownTimeout)
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

// textInput is a one-line text field edited at its end.
type textInput struct {
	text []rune
}

func (t *textInput) String() string { return string(t.text) }
func (t *textInput) set(s string)   { t.text = []rune(s) }

// handle applies an editing key: printable text appends, Backspace removes the last rune,
// Ctrl-U clears. It reports whether the text changed. Escape sequences (arrows) are ignored.
func (t *textInput) handle(k []byte) bool {
	if len(k) == 0 || k[0] == 0x1b {
		return false
	}
	if len(k) == 1 {
		switch k[0] {
		case 0x7f, 0x08:
			if len(t.text) == 0 {
				return false
			}
			t.text = t.text[:len(t.text)-1]
			return true
		case 0x15:
			if len(t.text) == 0 {
				return false
			}
			t.text = nil
			return true
		}
	}
	changed := false
	for len(k) > 0 {
		r, size := utf8.DecodeRune(k)
		k = k[size:]
		if r == utf8.RuneError || r < 0x20 || r == 0x7f {
			continue
		}
		t.text = append(t.text, r)
		changed = true
	}
	return changed
}

const sgrUnder = "\x1b[4m"

// renderField draws a text field w cells wide: the placeholder while empty, and when focused a
// cursor cell after the end of the text. style underlines the field (sgrUnder), or is empty for
// a field that sits inside a box.
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
	shown := tailCells(sanitize(t.String()), w-1)
	pad := strings.Repeat(" ", w-1-cellWidth(shown))
	return style + shown + sgrReset + sgrRev + " " + sgrReset + style + pad + sgrReset
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
