package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
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

// enterRaw switches the terminal to raw mode on the alternate screen and returns a restore func.
func enterRaw() (func(), error) {
	saved, err := sttyRun("-g")
	if err != nil {
		return nil, err
	}
	if _, err := sttyRun("raw", "-echo"); err != nil {
		return nil, err
	}
	fmt.Print("\x1b[?1049h\x1b[?25l")
	return func() {
		fmt.Print("\x1b[?25h\x1b[?1049l")
		sttyRun(saved)
	}, nil
}

// termSize returns rows, cols (24x80 when unknown).
func termSize() (int, int) {
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
	n := utf8.RuneCountInString(s)
	if n > w {
		r := []rune(s)
		if w == 1 {
			return string(r[:1])
		}
		return string(r[:w-1]) + "…"
	}
	return s + strings.Repeat(" ", w-n)
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
