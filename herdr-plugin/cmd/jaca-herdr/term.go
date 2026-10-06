package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
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

// screenMu orders frames against restore: once the terminal is restored, paint drops frames, so
// a frame the main loop was building when a signal arrived can't land on the primary screen.
var (
	screenMu sync.Mutex
	restored bool
)

// paint writes one frame, unless the terminal was already restored.
func paint(frame string) {
	screenMu.Lock()
	defer screenMu.Unlock()
	if !restored {
		fmt.Print(frame)
	}
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
			screenMu.Lock()
			defer screenMu.Unlock()
			restored = true
			fmt.Print("\x1b[?25h\x1b[?1049l")
			sttyRun(saved)
		})
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT)
	go func() {
		sig := <-sigs
		restore()
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
	used := 0
	for _, r := range s {
		rw := runeWidth(r)
		if used+rw > w-1 {
			break
		}
		b.WriteRune(r)
		used += rw
	}
	b.WriteString("…")
	return b.String()
}

// cellWidth is s's width in terminal cells.
func cellWidth(s string) int {
	n := 0
	for _, r := range s {
		n += runeWidth(r)
	}
	return n
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
		r >= 0x1F680 && r <= 0x1F6FF, r >= 0x1FA70 && r <= 0x1FAFF, r >= 0x2600 && r <= 0x27BF,
		r >= 0x20000 && r <= 0x3FFFD:
		return 2
	}
	return 1
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
// CSI (ESC [ … final byte), OSC (ESC ] … BEL or ESC \), or ESC plus one rune.
func skipEscape(rs []rune, i int) int {
	if i+1 >= len(rs) {
		return i
	}
	switch rs[i+1] {
	case '[':
		j := i + 2
		for j < len(rs) && !(rs[j] >= 0x40 && rs[j] <= 0x7e) {
			j++
		}
		return min(j, len(rs)-1)
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
