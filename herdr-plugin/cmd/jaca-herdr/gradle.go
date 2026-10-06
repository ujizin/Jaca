package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// gradleDaemon mirrors GradleDaemon's wire format (Sources/Core/Gradle/GradleDaemon.swift).
type gradleDaemon struct {
	PID      int32   `json:"pid"`
	Version  string  `json:"version"`
	Uptime   string  `json:"uptime"`
	CPU      float64 `json:"cpu"`
	MemoryMB int     `json:"memoryMB"`
	JDK      *string `json:"jdk"`
	MaxHeap  *string `json:"maxHeap"`
}

func (d gradleDaemon) isBusy() bool { return d.CPU > 20 }

// ramText and cpuText match GradleDaemonRow in Sources/Features/Gradle/GradleDaemonsView.swift.
func (d gradleDaemon) ramText() string {
	if d.MemoryMB >= 1024 {
		return fmt.Sprintf("%.1f GB", float64(d.MemoryMB)/1024)
	}
	return fmt.Sprintf("%d MB", d.MemoryMB)
}

func (d gradleDaemon) cpuText() string { return fmt.Sprintf("%d%%", int(math.Round(d.CPU))) }

const gradleTopic = "gradle.daemons"

type gradlePane struct {
	daemons    []gradleDaemon
	loaded     bool
	selected   int
	confirmPID int32 // the row armed for "Confirm kill?" (0 = none)
	confirmAt  time.Time
	toast      string
	toastAt    time.Time
	err        string
}

// runGradlePane renders the live daemon list from jacad's shared gradle.daemons topic.
// Keys: j/k or arrows move, x kills (press twice to confirm, like the app), r refreshes, q quits.
func runGradlePane() int {
	c, err := dial()
	if err != nil {
		fmt.Fprintln(os.Stderr, "jaca:", err)
		return 1
	}
	defer c.Close()
	if err := c.Subscribe(gradleTopic); err != nil {
		fmt.Fprintln(os.Stderr, "jaca:", err)
		return 1
	}

	restore, err := enterRaw()
	if err != nil {
		fmt.Fprintln(os.Stderr, "jaca: not a terminal:", err)
		return 1
	}
	defer restore()

	keys := make(chan key, 16)
	go readKeys(keys)
	resize := make(chan os.Signal, 1)
	signal.Notify(resize, syscall.SIGWINCH)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	killed := make(chan killResult, 4)
	done := make(chan struct{})
	defer close(done)

	p := &gradlePane{}
	p.draw()
	for {
		select {
		case ev := <-c.Events:
			switch ev.Topic {
			case gradleTopic:
				var list []gradleDaemon
				if json.Unmarshal(ev.Data, &list) == nil {
					p.setDaemons(list)
				}
			case "events.dropped":
				// The skipped update may have been the latest list: fetch it.
				var note droppedNote
				if json.Unmarshal(ev.Data, &note) == nil && note.Topic == gradleTopic {
					p.refresh(c)
				}
			}
		case <-c.Closed:
			restore()
			fmt.Fprintln(os.Stderr, "jaca: jacad closed the connection")
			return 1
		case k, ok := <-keys:
			if !ok || k == keyQuit {
				return 0
			}
			p.handle(k, c, killed, done)
		case r := <-killed:
			// The same toasts as GradleDaemonsModel.kill.
			if r.ok {
				p.flash(fmt.Sprintf("Killed %d", r.pid))
			} else {
				p.flash(fmt.Sprintf("Couldn't kill %d", r.pid))
			}
		case <-resize:
			refreshTermSize()
		case <-tick.C:
			if !p.expire() {
				continue
			}
		}
		p.expire()
		p.draw()
	}
}

func (p *gradlePane) setDaemons(list []gradleDaemon) {
	p.daemons, p.loaded = list, true
	p.selected = clampIndex(p.selected, len(list))
}

// refresh fetches the list on the UI loop, so a live update can't arrive between the reply and
// its use and be overwritten by the older list. readLoop never blocks on the pane, so the
// reply can't wait on this loop.
func (p *gradlePane) refresh(c *client) {
	var list []gradleDaemon
	if c.CallTimeout("gradle.list", nil, &list, syncTimeout) == nil {
		p.setDaemons(list)
	}
}

type killResult struct {
	pid int32
	ok  bool
}

func (p *gradlePane) handle(k key, c *client, killed chan<- killResult, done <-chan struct{}) {
	switch k {
	case keyUp:
		if p.selected > 0 {
			p.selected--
		}
		p.confirmPID = 0
	case keyDown:
		if p.selected < len(p.daemons)-1 {
			p.selected++
		}
		p.confirmPID = 0
	case keyRefresh:
		p.refresh(c)
	case keyKill:
		if p.selected < 0 || p.selected >= len(p.daemons) {
			return
		}
		d := p.daemons[p.selected]
		// Two-press confirm with a 3s window, as GradleDaemonRow.handleKillTap does.
		if p.confirmPID == d.PID && time.Since(p.confirmAt) < 3*time.Second {
			p.confirmPID = 0
			go func(pid int32) {
				var ok bool
				if err := c.Call("gradle.kill", map[string]any{"pid": pid}, &ok); err != nil {
					ok = false
				}
				select {
				case killed <- killResult{pid: pid, ok: ok}:
				case <-done:
				}
			}(d.PID)
		} else {
			p.confirmPID = d.PID
			p.confirmAt = time.Now()
		}
	}
}

func (p *gradlePane) flash(msg string) {
	p.toast = msg
	p.toastAt = time.Now()
}

// expire clears a lapsed kill confirmation or toast, reporting whether anything changed.
func (p *gradlePane) expire() bool {
	changed := false
	if p.confirmPID != 0 && time.Since(p.confirmAt) >= 3*time.Second {
		p.confirmPID, changed = 0, true
	}
	if p.toast != "" && time.Since(p.toastAt) >= 2600*time.Millisecond {
		p.toast, changed = "", true
	}
	return changed
}

func (p *gradlePane) draw() {
	rows, cols := termSize()
	var b strings.Builder
	b.WriteString("\x1b[H\x1b[2J")
	line := func(s string) { b.WriteString(s + "\x1b[K\r\n") }

	line(sgrDim + "DAEMONS" + sgrReset)
	line("")
	switch {
	case !p.loaded:
		// Nothing received yet: show nothing rather than a guess.
	case len(p.daemons) == 0:
		line("No Gradle daemons running")
	default:
		// Windowed around the selection like the projects list, so the header stays put.
		table := p.table(cols)
		room := max(1, rows-3)
		start := 0
		if p.selected >= room {
			start = p.selected - room + 1
		}
		for i := start; i < len(table) && i < start+room; i++ {
			line(table[i])
		}
	}
	if p.toast != "" && rows > 4 {
		b.WriteString(fmt.Sprintf("\x1b[%d;1H%s%s%s\x1b[K", rows, sgrBold, fit(p.toast, cols), sgrReset))
	}
	paint(b.String())
}

// table lays out the daemons like GradleDaemonRow, in aligned columns: name, PID, tags,
// uptime, CPU, and the kill button. The name column shrinks first when the pane is narrow.
func (p *gradlePane) table(cols int) []string {
	type cells struct{ name, pid, tags, uptime, cpu, button string }
	rows := make([]cells, len(p.daemons))
	var w cells
	width := cellWidth
	widen := func(cur *string, s string) {
		if width(s) > width(*cur) {
			*cur = strings.Repeat(" ", width(s))
		}
	}
	for i, d := range p.daemons {
		state := "IDLE"
		if d.isBusy() {
			state = "BUSY"
		}
		tags := []string{state}
		if d.JDK != nil {
			tags = append(tags, "JDK "+*d.JDK)
		}
		if d.MaxHeap != nil {
			tags = append(tags, *d.MaxHeap)
		}
		tags = append(tags, d.ramText())
		button := "Kill"
		if p.confirmPID == d.PID {
			button = "Confirm kill?"
		}
		// Fields come from process command lines: strip control bytes before they reach the terminal.
		rows[i] = cells{sanitize(fmt.Sprintf("Gradle %s", d.Version)), fmt.Sprintf("PID %d", d.PID),
			sanitize(strings.Join(tags, " · ")), sanitize(d.Uptime), d.cpuText(), "[" + button + "]"}
		widen(&w.name, rows[i].name)
		widen(&w.pid, rows[i].pid)
		widen(&w.tags, rows[i].tags)
		widen(&w.uptime, rows[i].uptime)
		widen(&w.cpu, rows[i].cpu)
		widen(&w.button, rows[i].button)
	}
	// Two-cell gutters between six columns, plus the two-cell selection marker.
	fixed := 2 + width(w.pid) + width(w.tags) + width(w.uptime) + width(w.cpu) + width(w.button) + 2*5
	nameW := min(width(w.name), max(6, cols-fixed))
	out := make([]string, len(rows))
	for i, r := range rows {
		marker := "  "
		if i == p.selected {
			marker = sgrRev + " " + sgrReset + " "
		}
		button := r.button
		if p.confirmPID == p.daemons[i].PID {
			button = sgrRed + button + sgrReset
		}
		out[i] = marker + sgrBold + fit(r.name, nameW) + sgrReset + "  " +
			sgrDim + fit(r.pid, width(w.pid)) + sgrReset + "  " +
			fit(r.tags, width(w.tags)) + "  " +
			fmt.Sprintf("%*s", width(w.uptime), r.uptime) + "  " +
			fmt.Sprintf("%*s", width(w.cpu), r.cpu) + "  " + button
	}
	return out
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
