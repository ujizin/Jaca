package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// device mirrors Device's wire format (Sources/Core/Devices/Device.swift).
type device struct {
	ID       string `json:"id"`
	Platform string `json:"platform"`
	Model    string `json:"model"`
	State    string `json:"state"`
}

func (d device) displayModel() string {
	// The emulator AVD name can carry adb's "\r\nOK" suffix; show the first line.
	m := strings.TrimSpace(strings.SplitN(d.Model, "\n", 2)[0])
	if m == "" {
		return d.ID
	}
	return m
}

// platformName and stateLabel match DevicePlatform.displayName and DeviceState.label.
func (d device) platformName() string {
	switch d.Platform {
	case "android":
		return "Android"
	case "iosSimulator":
		return "iOS Simulator"
	case "iosDevice":
		return "iOS Device"
	}
	return d.Platform
}

func (d device) stateLabel() string {
	switch d.State {
	case "connected":
		return "Connected"
	case "unauthorized":
		return "Unauthorized"
	case "offline":
		return "Offline"
	case "booted":
		return "Booted"
	case "shutdown":
		return "Shutdown"
	}
	return "Unknown"
}

func (d device) isReady() bool { return d.State == "connected" || d.State == "booted" }

// logLine mirrors LogLine's wire format (Sources/Core/Logs/LogLineWire.swift).
type logLine struct {
	Seq      uint64  `json:"s"`
	Time     float64 `json:"t"`
	Level    int     `json:"l"`
	Tag      string  `json:"g"`
	PID      int32   `json:"p"`
	Message  string  `json:"m"`
	Marker   bool    `json:"mk"`
	Critical bool    `json:"mc"`
}

// levelShort matches LogLevel.short.
var levelShort = []string{"V", "D", "I", "W", "E", "F"}

type logState struct {
	IsRunning     bool    `json:"isRunning"`
	IsConnecting  bool    `json:"isConnecting"`
	StatusMessage *string `json:"statusMessage"`
}

const devicesTopic = "devices.list"
const maxLines = 20000

// syncTimeout bounds the calls the UI loop waits on, so a stalled jacad can't freeze the keys
// for callTimeout; teardownTimeout bounds the ones made while leaving a stream or the pane.
const (
	syncTimeout     = 5 * time.Second
	teardownTimeout = 2 * time.Second
)

// openResult is a finished logs.open, handed back to the UI loop.
type openResult struct {
	device device
	id     string
	err    error
}

// devicesPane lists devices; Enter streams the selected device's logs in the same pane.
type devicesPane struct {
	c        *client
	devices  []device
	loaded   bool
	selected int

	opening bool   // a logs.open is in flight
	err     string // why the last logs.open failed, as jacad reported it

	// Streaming (session non-empty).
	session  string
	streamOf device
	lines    []logLine
	lastSeq  uint64 // seq of the newest line in lines; valid when hasSeq
	hasSeq   bool
	state    logState
	minLevel int
	follow   bool // pinned to the tail; false once scrolled up
	offset   int  // visible lines scrolled up from the tail when not following

	opened chan openResult
	done   chan struct{} // closed when the pane exits, so result goroutines don't block
}

// runDevicesPane: j/k move, Enter streams logs; while streaming space stops/starts, 1-6 set the
// minimum level (V..F), j/k scroll (leaving follow mode), G follows again, Esc goes back, q quits.
func runDevicesPane() int {
	c, err := dial()
	if err != nil {
		fmt.Fprintln(os.Stderr, "jaca:", err)
		return 1
	}
	defer c.Close()
	if err := c.Subscribe(devicesTopic); err != nil {
		fmt.Fprintln(os.Stderr, "jaca:", err)
		return 1
	}
	p := &devicesPane{c: c, follow: true, opened: make(chan openResult, 1), done: make(chan struct{})}
	defer close(p.done)
	// Deferred before restore so it runs after it: the terminal is back before teardown waits.
	defer p.closeSession(true)
	restore, err := enterRaw()
	if err != nil {
		fmt.Fprintln(os.Stderr, "jaca: not a terminal:", err)
		return 1
	}
	defer restore()

	keys := make(chan []byte, 16)
	go readRawKeys(keys)
	resize := make(chan os.Signal, 1)
	signal.Notify(resize, syscall.SIGWINCH)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()

	// Keys and resizes redraw at once; events only mark the pane dirty and the tick draws, so a
	// chatty device costs one frame per tick rather than one per batch.
	p.draw()
	dirty := false
	for {
		select {
		case ev := <-c.Events:
			p.handleEvent(ev)
			dirty = true
			continue
		case <-c.Closed:
			restore()
			fmt.Fprintln(os.Stderr, "jaca: jacad closed the connection")
			return 1
		case k, ok := <-keys:
			if !ok {
				return 0
			}
			if quit := p.handleKey(k); quit {
				return 0
			}
		case r := <-p.opened:
			p.finishOpen(r)
		case <-resize:
			refreshTermSize()
		case <-tick.C:
			if !dirty {
				continue
			}
		}
		p.draw()
		dirty = false
	}
}

func (p *devicesPane) handleEvent(ev event) {
	switch {
	case ev.Topic == devicesTopic:
		var list []device
		if json.Unmarshal(ev.Data, &list) == nil {
			p.setDevices(list)
		}
	case ev.Topic == "events.dropped":
		var note droppedNote
		if json.Unmarshal(ev.Data, &note) != nil {
			return
		}
		switch {
		case note.Topic == devicesTopic:
			// The skipped update may have been the latest list: fetch it.
			var list []device
			if p.c.CallTimeout("devices.list", nil, &list, syncTimeout) == nil {
				p.setDevices(list)
			}
		case p.session != "" && note.Topic == "logs.lines."+p.session:
			p.backfill()
		case p.session != "" && note.Topic == "logs.state."+p.session:
			var sessions []struct {
				ID    string   `json:"id"`
				State logState `json:"state"`
			}
			if p.c.CallTimeout("logs.list", nil, &sessions, syncTimeout) == nil {
				for _, s := range sessions {
					if s.ID == p.session {
						p.state = s.State
					}
				}
			}
		}
	case p.session != "" && ev.Topic == "logs.lines."+p.session:
		var batch []logLine
		if json.Unmarshal(ev.Data, &batch) == nil {
			p.appendLines(batch)
		}
	case p.session != "" && ev.Topic == "logs.state."+p.session:
		var st logState
		if json.Unmarshal(ev.Data, &st) == nil {
			p.state = st
		}
	}
}

func (p *devicesPane) setDevices(list []device) {
	p.devices, p.loaded = list, true
	p.selected = clampIndex(p.selected, len(list))
}

// clampIndex keeps a selection inside 0..<n (0 for an empty list).
func clampIndex(i, n int) int {
	return max(0, min(i, n-1))
}

// appendLines adds lines newer than the last one held. Seqs grow but skip values (one line can
// split into sub-seqs), so they order lines without revealing gaps; gaps are reported by
// events.dropped and filled by backfill. A scrolled-up view moves its offset by the visible
// lines added, so it holds still while the device keeps logging.
func (p *devicesPane) appendLines(batch []logLine) {
	added := 0
	for _, l := range batch {
		if p.hasSeq && l.Seq <= p.lastSeq {
			continue
		}
		p.lines = append(p.lines, l)
		p.lastSeq, p.hasSeq = l.Seq, true
		if p.isVisible(l) {
			added++
		}
	}
	if !p.follow {
		p.offset += added
	}
	// Trimmed in chunks so a full buffer isn't copied on every batch.
	if len(p.lines) > maxLines+maxLines/10 {
		p.lines = append([]logLine(nil), p.lines[len(p.lines)-maxLines:]...)
	}
}

func (p *devicesPane) isVisible(l logLine) bool { return l.Marker || l.Level >= p.minLevel }

// backfill fetches the session's lines after the newest one held (all of the daemon's replay
// when none is held yet). Live batches that overlap it are skipped by appendLines.
func (p *devicesPane) backfill() {
	params := map[string]any{"id": p.session, "limit": maxLines}
	if p.hasSeq {
		params["afterSeq"] = p.lastSeq
	}
	var lines []logLine
	if p.c.CallTimeout("logs.range", params, &lines, syncTimeout) == nil {
		p.appendLines(lines)
	}
}

func (p *devicesPane) handleKey(k []byte) bool {
	switch {
	case len(k) == 1 && (k[0] == 'q' || k[0] == 0x03):
		return true
	case len(k) == 1 && k[0] == 0x1b: // Esc
		if p.session != "" {
			p.closeSession(false)
		}
	case isUp(k):
		if p.session == "" {
			p.selected = clampIndex(p.selected-1, len(p.devices))
		} else {
			p.follow = false
			p.offset++
		}
	case isDown(k):
		if p.session == "" {
			p.selected = clampIndex(p.selected+1, len(p.devices))
		} else if p.offset > 0 {
			p.offset--
			p.follow = p.offset == 0
		}
	case len(k) == 1 && (k[0] == '\r' || k[0] == '\n'):
		if p.session == "" && !p.opening && p.selected >= 0 && p.selected < len(p.devices) {
			p.openSession(p.devices[p.selected])
		}
	case len(k) == 1 && k[0] == ' ' && p.session != "":
		method := "logs.start"
		if p.state.IsRunning {
			method = "logs.stop"
		}
		go p.c.Call(method, map[string]any{"id": p.session}, nil)
	case len(k) == 1 && k[0] == 'G':
		p.follow, p.offset = true, 0
	case len(k) == 1 && k[0] >= '1' && k[0] <= '6':
		p.minLevel = int(k[0] - '1')
	}
	return false
}

// openSession asks jacad for a stream off the UI loop; finishOpen takes the result.
func (p *devicesPane) openSession(d device) {
	p.opening, p.err = true, ""
	go func() {
		var info struct {
			ID string `json:"id"`
		}
		err := p.c.Call("logs.open", map[string]any{"device": d, "autoStart": true, "displayName": d.displayModel()}, &info)
		select {
		case p.opened <- openResult{device: d, id: info.ID, err: err}:
		case <-p.done:
		}
	}()
}

func (p *devicesPane) finishOpen(r openResult) {
	p.opening = false
	if r.err != nil {
		p.err = r.err.Error()
		return
	}
	d := r.device
	p.session, p.streamOf = r.id, d
	p.lines, p.lastSeq, p.hasSeq, p.follow, p.offset = nil, 0, false, true, 0
	p.state = logState{}
	_ = p.c.CallTimeout("events.subscribe", map[string]any{"topics": []string{"logs.lines." + r.id, "logs.state." + r.id}}, nil, syncTimeout)
	// Lines published between logs.open and the subscribe never reach this connection; the
	// daemon's replay has them.
	p.backfill()
}

// closeSession leaves the stream. While the pane keeps running the calls go off the UI loop;
// on exit (wait) they run before the connection closes, each bounded by teardownTimeout.
func (p *devicesPane) closeSession(wait bool) {
	if p.session == "" {
		return
	}
	id := p.session
	p.session = ""
	teardown := func() {
		_ = p.c.CallTimeout("events.unsubscribe", map[string]any{"topics": []string{"logs.lines." + id, "logs.state." + id}}, nil, teardownTimeout)
		_ = p.c.CallTimeout("logs.close", map[string]any{"id": id}, nil, teardownTimeout)
	}
	if wait {
		teardown()
	} else {
		go teardown()
	}
}

func (p *devicesPane) draw() {
	rows, cols := termSize()
	var b strings.Builder
	b.WriteString("\x1b[H\x1b[2J")
	line := func(s string) { b.WriteString(s + "\x1b[K\r\n") }

	if p.session == "" {
		line(sgrDim + "Devices" + sgrReset)
		line("")
		room := rows - 3
		if p.err != "" {
			line(sgrRed + fit(sanitize(p.err), cols) + sgrReset)
			room--
		}
		room = max(1, room)
		switch {
		case !p.loaded:
		case len(p.devices) == 0:
			line("No devices connected")
		default:
			start := 0
			if p.selected >= room {
				start = p.selected - room + 1
			}
			for i := start; i < len(p.devices) && i < start+room; i++ {
				d := p.devices[i]
				marker := "  "
				if i == p.selected {
					marker = sgrRev + " " + sgrReset + " "
				}
				name := sgrBold + fit(sanitize(d.displayModel()), max(10, cols/2-4)) + sgrReset
				state := d.stateLabel()
				if !d.isReady() {
					state = sgrDim + state + sgrReset
				}
				line(marker + name + "  " + fit(d.platformName(), 14) + "  " + state)
			}
		}
		paint(b.String())
		return
	}

	// Header: the device and the stream state. The name gets what the rest leaves.
	name := clip(sanitize(p.streamOf.displayModel()), max(1, cols-16))
	head := sgrBold + name + sgrReset + "  " + sgrDim + "≥" + levelShort[p.minLevel] + sgrReset
	if p.state.IsConnecting {
		head += "  " + sgrDim + "Connecting…" + sgrReset
	} else if !p.state.IsRunning {
		head += "  " + sgrDim + "■" + sgrReset
	}
	line(head)
	if p.state.StatusMessage != nil {
		line(sgrRed + fit(sanitize(*p.state.StatusMessage), cols) + sgrReset)
	}

	visible := make([]logLine, 0, len(p.lines))
	for _, l := range p.lines {
		if p.isVisible(l) {
			visible = append(visible, l)
		}
	}
	room := rows - 2
	if p.state.StatusMessage != nil {
		room--
	}
	room = max(1, room)
	end := len(visible) - p.offset
	if end < 0 {
		end = 0
		p.offset = len(visible)
	}
	start := max(0, end-room)
	for _, l := range visible[start:end] {
		line(renderLogLine(l, cols))
	}
	paint(b.String())
}

func renderLogLine(l logLine, cols int) string {
	msg := sanitize(strings.ReplaceAll(l.Message, "\n", " ⏎ "))
	if l.Marker {
		color := sgrDim
		if l.Critical {
			color = sgrRed + sgrBold
		}
		return color + fit(msg, cols) + sgrReset
	}
	ts := time.Unix(0, int64(l.Time*1e9)).Format("15:04:05.000")
	level := "?"
	if l.Level >= 0 && l.Level < len(levelShort) {
		level = levelShort[l.Level]
	}
	color := ""
	switch {
	case l.Level >= 4:
		color = sgrRed
	case l.Level == 3:
		color = "\x1b[33m"
	case l.Level <= 1:
		color = sgrDim
	}
	prefix := ts + " " + level + " "
	rest := fit(sanitize(l.Tag)+": "+msg, max(0, cols-cellWidth(prefix)))
	return sgrDim + ts + sgrReset + " " + color + level + " " + rest + sgrReset
}
