package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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
// signal arrived can't land on the primary screen. restore only tries screenMu: a paint stuck in
// a write (Herdr not reading the pty) must not keep a signal from reaching os.Exit.
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
	var once sync.Once
	restore := func() {
		once.Do(func() {
			restored.Store(true)
			if screenMu.TryLock() {
				defer screenMu.Unlock()
			}
			fmt.Print("\x1b[?25h\x1b[?1049l")
			sttyRun(saved)
		})
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT)
	go func() {
		sig := <-sigs
		restore()
		exitHooksMu.Lock()
		hooks := append([]func(){}, exitHooks...)
		exitHooksMu.Unlock()
		for _, h := range hooks {
			h()
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
	sgrRed   = "\x1b[31m"
	sgrGreen = "\x1b[32m"
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

// readRawKeys forwards raw keypress bytes (one read = one key or escape sequence).
func readRawKeys(out chan<- []byte) {
	buf := make([]byte, 16)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			close(out)
			return
		}
		k := make([]byte, n)
		copy(k, buf[:n])
		out <- k
	}
}

func isUp(k []byte) bool {
	return (len(k) >= 3 && k[0] == 0x1b && k[1] == '[' && k[2] == 'A') || (len(k) == 1 && k[0] == 'k')
}

func isDown(k []byte) bool {
	return (len(k) >= 3 && k[0] == 0x1b && k[1] == '[' && k[2] == 'B') || (len(k) == 1 && k[0] == 'j')
}
