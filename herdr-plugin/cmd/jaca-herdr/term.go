package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"
)

// Terminal control with the standard library only: stty for raw mode and size, ANSI escapes
// for drawing.

func sttyRun(args ...string) (string, error) {
	cmd := exec.Command("stty", args...)
	cmd.Stdin = os.Stdin
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// Once the terminal is restored, paint drops frames, so a frame the main loop was building when a
// signal arrived can't land on the primary screen. paint and restore share screenMu, so the
// restore sequence never interleaves with a frame. A paint stuck in a write (Herdr not reading
// the pty) would then block restore too; the signal path bounds that wait instead of skipping
// the lock.
var (
	screenMu sync.Mutex
	restored atomic.Bool
)

// paint writes one frame, unless the terminal was already restored.
func paint(frame string) {
	screenMu.Lock()
	defer screenMu.Unlock()
	if !restored.Load() {
		fmt.Print(frame)
	}
}

// The rows last painted, so paintRows rewrites only the ones that changed. Cleared when the
// terminal size is read, since a resize invalidates what is on screen. Only the pane's main loop
// reads or writes it.
var (
	shownRows  []string
	shownValid bool
)

// paintRows draws a frame given as its rows from the top, each at most the pane's width and ending
// with its styles reset. A row equal to the one already on screen is not rewritten, and nothing is
// cleared first, so a redraw with the same text writes nothing and the pane doesn't blink. A
// changed row is erased and rewritten in the same write, inside a synchronized update where the
// terminal supports one.
func paintRows(rows []string) {
	var b strings.Builder
	if !shownValid {
		b.WriteString("\x1b[H\x1b[2J")
		shownRows = shownRows[:0]
	}
	for i, row := range rows {
		if i < len(shownRows) && shownRows[i] == row {
			continue
		}
		// Erased before the text rather than after: after a full-width row the cursor still sits
		// on the last cell, which an erase to the end of the line would blank.
		fmt.Fprintf(&b, "\x1b[%d;1H\x1b[2K%s", i+1, row)
	}
	for i := len(rows); i < len(shownRows); i++ {
		fmt.Fprintf(&b, "\x1b[%d;1H\x1b[2K", i+1)
	}
	shownRows, shownValid = append(shownRows[:0], rows...), true
	if b.Len() > 0 {
		paint("\x1b[?2026h" + b.String() + "\x1b[?2026l")
	}
}

// panelStyle is what a popup is drawn on and borderStyle what its border is drawn in: Herdr's
// panel and accent colors (herdrtheme.go), or nothing where they aren't known, so the pane's
// default colors show.
var (
	panelStyle  = themePanel(herdrTheme)
	borderStyle = themeAccent(herdrTheme)
	buttonStyle = themeButton(herdrTheme)
)

// The labels of a popup's close button, as in Herdr's own overlays: at its top right, it closes
// the popup, or goes back a step where there is one.
const (
	escClose = " esc close "
	escBack  = " esc back "
)

// closeButton draws a popup's close button.
func closeButton(label string) string { return buttonStyle + label + sgrReset }

// framed colors a popup row's border: the whole row when it is a border (top, bottom or a
// separator), else its first and last cells.
func framed(row string) string {
	if borderStyle == "" || row == "" {
		return row
	}
	rs := []rune(row)
	switch rs[0] {
	case '╭', '╰', '├':
		// A title inside the border resets the styles; the border's color comes back after it.
		return borderStyle + strings.ReplaceAll(row, sgrReset, sgrReset+borderStyle) + sgrReset
	case '│':
		if len(rs) > 1 && rs[len(rs)-1] == '│' {
			return borderStyle + "│" + sgrReset + string(rs[1:len(rs)-1]) + borderStyle + "│" + sgrReset
		}
	}
	return row
}

// onPanel puts a drawn row on the panel: its colors, if it has any, come back after every style
// reset inside the row, and the row is filled out to w cells.
func onPanel(row string, w int) string {
	fill := strings.Repeat(" ", max(0, w-cellWidth(stripSGR(row))))
	return panelStyle + strings.ReplaceAll(row, sgrReset, sgrReset+panelStyle) + fill + sgrReset
}

// rowAt sets row n (1-based) of a frame, adding empty rows above it as needed.
func rowAt(rows []string, n int, s string) []string {
	for len(rows) < n {
		rows = append(rows, "")
	}
	rows[n-1] = s
	return rows
}

// Cleanups to run when a signal ends the process (deferred calls don't run then). Each must bound
// its own time, so the process still exits.
var (
	exitHooksMu sync.Mutex
	exitHooks   []func()
)

// onSignalExit registers f to run, after the terminal is restored, when a signal ends the pane.
func onSignalExit(f func()) {
	exitHooksMu.Lock()
	exitHooks = append(exitHooks, f)
	exitHooksMu.Unlock()
}

// enterRaw switches the terminal to raw mode on the alternate screen and returns a restore func,
// safe to call more than once. SIGTERM, SIGHUP and SIGINT (Herdr closing the pane) restore the
// terminal before the process exits, since deferred calls don't run then.
func enterRaw() (func(), error) {
	saved, err := sttyRun("-g")
	if err != nil {
		return nil, err
	}
	if _, err := sttyRun("raw", "-echo"); err != nil {
		return nil, err
	}
	fmt.Print("\x1b[?1049h\x1b[?25l")
	refreshTermSize()
	// The first caller restores; later ones wait (bounded) until it has finished, so main can't
	// print or exit while the terminal is still half restored.
	restoreDone := make(chan struct{})
	restore := func() {
		if !restored.CompareAndSwap(false, true) {
			select {
			case <-restoreDone:
			case <-time.After(time.Second):
			}
			return
		}
		defer close(restoreDone)
		// The tty mode first: stty doesn't write to the pty, so a paint stuck in a write can't
		// leave the terminal raw.
		sttyRun(saved)
		screenMu.Lock()
		defer screenMu.Unlock()
		fmt.Print(mouseOff + "\x1b[?25h\x1b[?1049l")
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT)
	go func() {
		sig := <-sigs
		// Restore and the hooks can block (a write to a pty nobody reads): give them a bounded
		// time, then exit regardless.
		done := make(chan struct{})
		go func() {
			defer close(done)
			restore()
			exitHooksMu.Lock()
			hooks := append([]func(){}, exitHooks...)
			exitHooksMu.Unlock()
			for _, h := range hooks {
				h()
			}
		}()
		select {
		case <-done:
		case <-time.After(500 * time.Millisecond):
		}
		code := 1
		if s, ok := sig.(syscall.Signal); ok {
			code = 128 + int(s)
		}
		os.Exit(code)
	}()
	return restore, nil
}

// The terminal size, read once on entering raw mode and again on SIGWINCH, so drawing doesn't
// fork stty every frame. Only the pane's main loop reads or writes it.
var termRows, termCols = 24, 80

// termSize returns the cached rows, cols.
func termSize() (int, int) { return termRows, termCols }

// refreshTermSize re-reads the size (24x80 when unknown). Call it on SIGWINCH.
func refreshTermSize() {
	termRows, termCols = queryTermSize()
	shownValid = false
}

func queryTermSize() (int, int) {
	out, err := sttyRun("size")
	if err == nil {
		parts := strings.Fields(out)
		if len(parts) == 2 {
			r, e1 := strconv.Atoi(parts[0])
			c, e2 := strconv.Atoi(parts[1])
			if e1 == nil && e2 == nil && r > 0 && c > 0 {
				return r, c
			}
		}
	}
	return 24, 80
}

const (
	sgrReset = "\x1b[0m"
	sgrBold  = "\x1b[1m"
	sgrDim   = "\x1b[2m"
	sgrRev   = "\x1b[7m"
)

// The colors the panes draw with: the terminal palette's, or the user's Herdr theme override
// for that token (herdrtheme.go). colorX is the bare SGR parameters, to combine with others.
var (
	colorRed     = themeFg(herdrTheme, "red", "31")
	colorGreen   = themeFg(herdrTheme, "green", "32")
	colorYellow  = themeFg(herdrTheme, "yellow", "33")
	colorBlue    = themeFg(herdrTheme, "blue", "34")
	colorMagenta = themeFg(herdrTheme, "mauve", "35")
	colorCyan    = themeFg(herdrTheme, "teal", "36")

	sgrRed     = "\x1b[" + colorRed + "m"
	sgrGreen   = "\x1b[" + colorGreen + "m"
	sgrYellow  = "\x1b[" + colorYellow + "m"
	sgrMagenta = "\x1b[" + colorMagenta + "m"
	sgrCyan    = "\x1b[" + colorCyan + "m"
)

// fit pads or truncates s (plain text, no escapes) to exactly w display cells.
func fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	c := clip(s, w)
	return c + strings.Repeat(" ", w-cellWidth(c))
}

// clip truncates s (plain text, no escapes) to at most w display cells, ending in "…" when cut.
func clip(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if cellWidth(s) <= w {
		return s
	}
	var b strings.Builder
	used, prev := 0, 0
	for _, r := range s {
		rw := widthAfter(r, prev)
		if used+rw > w-1 {
			break
		}
		b.WriteRune(r)
		used += rw
		prev = runeWidth(r)
	}
	b.WriteString("…")
	return b.String()
}

// cellWidth is s's width in terminal cells.
func cellWidth(s string) int {
	n, prev := 0, 0
	for _, r := range s {
		n += widthAfter(r, prev)
		prev = runeWidth(r)
	}
	return n
}

// widthAfter is r's width given the previous rune's: U+FE0F after a narrow rune asks for emoji
// presentation, which takes a second cell.
func widthAfter(r rune, prev int) int {
	if r == 0xFE0F && prev == 1 {
		return 1
	}
	return runeWidth(r)
}

// runeWidth is a conservative cell width: 0 for combining marks and zero-width joiners, 2 for
// East Asian wide/fullwidth ranges and emoji, 1 otherwise.
func runeWidth(r rune) int {
	switch {
	case r >= 0x0300 && r <= 0x036F, r >= 0x200B && r <= 0x200F, r >= 0xFE00 && r <= 0xFE0F, r == 0x20E3:
		return 0
	case r >= 0x1100 && r <= 0x115F,
		r >= 0x2E80 && r <= 0x303E, r >= 0x3041 && r <= 0x33FF, r >= 0x3400 && r <= 0x4DBF,
		r >= 0x4E00 && r <= 0x9FFF, r >= 0xA000 && r <= 0xA4CF, r >= 0xAC00 && r <= 0xD7A3,
		r >= 0xF900 && r <= 0xFAFF, r >= 0xFE30 && r <= 0xFE4F, r >= 0xFF00 && r <= 0xFF60,
		r >= 0xFFE0 && r <= 0xFFE6, r >= 0x1F300 && r <= 0x1F64F, r >= 0x1F900 && r <= 0x1F9FF,
		r >= 0x1F680 && r <= 0x1F6FF, r >= 0x1FA70 && r <= 0x1FAFF, r >= 0x1F7E0 && r <= 0x1F7EB,
		r == 0x1F004, r == 0x1F0CF, r == 0x1F18E, r >= 0x1F191 && r <= 0x1F19A,
		r >= 0x1F200 && r <= 0x1F251, r >= 0x20000 && r <= 0x3FFFD:
		return 2
	}
	if emojiPresentation(r) {
		return 2
	}
	return 1
}

// emojiPresentation reports the BMP symbols that render as emoji (two cells) by default. The
// rest of U+2600–27BF (✓ ✗ ★ ❯ ➜) are text symbols, one cell.
func emojiPresentation(r rune) bool {
	switch {
	case r == 0x231A, r == 0x231B, r >= 0x23E9 && r <= 0x23EC, r == 0x23F0, r == 0x23F3,
		r == 0x25FD, r == 0x25FE, r == 0x2614, r == 0x2615, r >= 0x2648 && r <= 0x2653,
		r == 0x267F, r == 0x2693, r == 0x26A1, r == 0x26AA, r == 0x26AB, r == 0x26BD, r == 0x26BE,
		r == 0x26C4, r == 0x26C5, r == 0x26CE, r == 0x26D4, r == 0x26EA, r == 0x26F2, r == 0x26F3,
		r == 0x26F5, r == 0x26FA, r == 0x26FD, r == 0x2705, r == 0x270A, r == 0x270B, r == 0x2728,
		r == 0x274C, r == 0x274E, r >= 0x2753 && r <= 0x2755, r == 0x2757,
		r >= 0x2795 && r <= 0x2797, r == 0x27B0, r == 0x27BF, r == 0x2B1B, r == 0x2B1C,
		r == 0x2B50, r == 0x2B55:
		return true
	}
	return false
}

// sanitize makes device text safe to draw: tabs become spaces, and escape sequences, other C0
// controls, DEL and C1 controls are removed, so a log line can't move the cursor or restyle
// the pane.
func sanitize(s string) string {
	if !needsSanitize(s) {
		return s
	}
	var b strings.Builder
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case r == '\t':
			b.WriteString("    ")
		case r == 0x1b:
			i = skipEscape(rs, i)
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func needsSanitize(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return true
		}
	}
	return false
}

// skipEscape returns the index of the last rune of the escape sequence starting at rs[i] (ESC):
// CSI (ESC [ parameter/intermediate bytes, then a final byte), OSC (ESC ] … BEL or ESC \),
// ESC with intermediates plus a final byte (ESC ( B), or ESC plus one rune. A malformed CSI
// ends at the first byte that can't belong to it, so the text after it survives.
func skipEscape(rs []rune, i int) int {
	if i+1 >= len(rs) {
		return i
	}
	switch rs[i+1] {
	case '[':
		j := i + 2
		for j < len(rs) && rs[j] >= 0x20 && rs[j] <= 0x3f {
			j++
		}
		if j < len(rs) && rs[j] >= 0x40 && rs[j] <= 0x7e {
			return j
		}
		return j - 1
	case ']':
		for j := i + 2; j < len(rs); j++ {
			if rs[j] == 0x07 {
				return j
			}
			if rs[j] == 0x1b && j+1 < len(rs) && rs[j+1] == '\\' {
				return j + 1
			}
		}
		return len(rs) - 1
	}
	if rs[i+1] >= 0x20 && rs[i+1] <= 0x2f {
		j := i + 1
		for j < len(rs) && rs[j] >= 0x20 && rs[j] <= 0x2f {
			j++
		}
		if j < len(rs) && rs[j] >= 0x30 && rs[j] <= 0x7e {
			return j
		}
		return j - 1
	}
	return i + 1
}

// key is one decoded keypress.
type key int

const (
	keyNone key = iota
	keyUp
	keyDown
	keyQuit
	keyKill
	keyRefresh
)

// readKeys decodes stdin bytes into keys until stdin closes.
func readKeys(out chan<- key) {
	buf := make([]byte, 16)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			close(out)
			return
		}
		b := buf[:n]
		switch {
		case n >= 3 && b[0] == 0x1b && b[1] == '[' && b[2] == 'A':
			out <- keyUp
		case n >= 3 && b[0] == 0x1b && b[1] == '[' && b[2] == 'B':
			out <- keyDown
		case b[0] == 'k':
			out <- keyUp
		case b[0] == 'j':
			out <- keyDown
		case b[0] == 'q' || b[0] == 0x03:
			out <- keyQuit
		case b[0] == 'x':
			out <- keyKill
		case b[0] == 'r':
			out <- keyRefresh
		}
	}
}

// readRawKeys forwards keypresses, one per send: a key, an escape sequence (arrows, a mouse
// report), or a pasted run of text.
func readRawKeys(out chan<- []byte) {
	buf := make([]byte, 256)
	var pending []byte
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			close(out)
			return
		}
		var keys [][]byte
		keys, pending = splitInput(append(pending, buf[:n]...))
		for _, k := range keys {
			out <- k
		}
	}
}

// splitInput cuts terminal input into keys and returns the unfinished tail to prepend to the next
// read: an escape sequence or a UTF-8 character can arrive split across reads, and several keys
// can arrive in one. A short run of text is one key per character, so repeated keys each count;
// a long one (a paste) stays whole.
func splitInput(in []byte) (keys [][]byte, rest []byte) {
	emit := func(k []byte) { keys = append(keys, append([]byte(nil), k...)) }
	for i := 0; i < len(in); {
		if in[i] != 0x1b {
			j := i
			for j < len(in) && in[j] != 0x1b {
				j++
			}
			run := in[i:j]
			if j == len(in) { // hold back a character still missing bytes
				for cut := 1; cut <= 3 && cut <= len(run); cut++ {
					if tail := run[len(run)-cut:]; utf8.RuneStart(tail[0]) {
						if !utf8.FullRune(tail) {
							run, rest = run[:len(run)-cut], append([]byte(nil), tail...)
						}
						break
					}
				}
			}
			if utf8.RuneCount(run) > 8 {
				emit(run)
			} else {
				for len(run) > 0 {
					_, size := utf8.DecodeRune(run)
					emit(run[:size])
					run = run[size:]
				}
			}
			i = j
			continue
		}
		switch {
		case i+1 == len(in): // Esc pressed on its own
			emit(in[i:])
			i = len(in)
		case in[i+1] == '[':
			j := i + 2
			for j < len(in) && in[j] >= 0x20 && in[j] <= 0x3f {
				j++
			}
			if j == len(in) {
				if len(in)-i > 32 { // not a sequence this pane knows: drop it
					return keys, nil
				}
				return keys, append([]byte(nil), in[i:]...)
			}
			emit(in[i : j+1])
			i = j + 1
		case in[i+1] == 'O' && i+2 < len(in): // SS3: arrows in application mode
			emit(in[i : i+3])
			i += 3
		default:
			emit(in[i : i+2])
			i += 2
		}
	}
	return keys, rest
}

// Mouse reporting (button presses, drags and the wheel, SGR coordinates). It is on for the whole
// pane or not at all, and while it is on the terminal sends the mouse to the pane instead of
// selecting text, so a pane that wants clicks draws and copies its own selection.
const (
	mouseOn  = "\x1b[?1002h\x1b[?1006h"
	mouseOff = "\x1b[?1006l\x1b[?1002l"
)

// mouseDrag is set in a report's button while the pointer moves with a button held.
const mouseDrag = 32

var sgrPattern = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// stripSGR removes the styles from a drawn row, leaving the text on screen.
func stripSGR(s string) string { return sgrPattern.ReplaceAllString(s, "") }

// mouseEvent is one SGR mouse report: ESC [ < button ; column ; row, then M (press) or m (release).
type mouseEvent struct {
	button int // 0 left, 1 middle, 2 right; 64 wheel up, 65 wheel down
	x, y   int // 1-based cell
	press  bool
}

func parseMouse(k []byte) (mouseEvent, bool) {
	if len(k) < 6 || k[0] != 0x1b || k[1] != '[' || k[2] != '<' {
		return mouseEvent{}, false
	}
	final := k[len(k)-1]
	parts := strings.Split(string(k[3:len(k)-1]), ";")
	if len(parts) != 3 || (final != 'M' && final != 'm') {
		return mouseEvent{}, false
	}
	var n [3]int
	for i, part := range parts {
		v, err := strconv.Atoi(part)
		if err != nil {
			return mouseEvent{}, false
		}
		n[i] = v
	}
	return mouseEvent{button: n[0], x: n[1], y: n[2], press: final == 'M'}, true
}

func isUp(k []byte) bool {
	return (len(k) >= 3 && k[0] == 0x1b && k[1] == '[' && k[2] == 'A') || (len(k) == 1 && k[0] == 'k')
}

func isDown(k []byte) bool {
	return (len(k) >= 3 && k[0] == 0x1b && k[1] == '[' && k[2] == 'B') || (len(k) == 1 && k[0] == 'j')
}
