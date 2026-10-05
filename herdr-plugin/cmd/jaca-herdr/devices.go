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

// devicesPane lists devices; Enter streams the selected device's logs in the same pane.
type devicesPane struct {
	c        *client
	devices  []device
	loaded   bool
	selected int

	// Streaming (session non-empty).
	session  string
	streamOf device
	lines    []logLine
	state    logState
	minLevel int
	follow   bool
	offset   int // lines scrolled up from the tail when not following
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

	p := &devicesPane{c: c, follow: true}
	defer p.closeSession()
	dirty := true
	for {
		if dirty {
			p.draw()
			dirty = false
		}
		select {
		case ev := <-c.Events:
			p.handleEvent(ev)
			dirty = true
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
			dirty = true
		case <-resize:
			dirty = true
		case <-tick.C:
		}
	}
}

func (p *devicesPane) handleEvent(ev event) {
	switch {
	case ev.Topic == devicesTopic:
		var list []device
		if json.Unmarshal(ev.Data, &list) == nil {
			p.devices, p.loaded = list, true
			if p.selected >= len(list) {
				p.selected = max(0, len(list)-1)
			}
		}
	case p.session != "" && ev.Topic == "logs.lines."+p.session:
		var batch []logLine
		if json.Unmarshal(ev.Data, &batch) == nil {
			p.lines = append(p.lines, batch...)
			if len(p.lines) > maxLines {
				p.lines = p.lines[len(p.lines)-maxLines:]
			}
		}
	case p.session != "" && ev.Topic == "logs.state."+p.session:
		_ = json.Unmarshal(ev.Data, &p.state)
	}
}

func (p *devicesPane) handleKey(k []byte) bool {
	switch {
	case len(k) == 1 && (k[0] == 'q' || k[0] == 0x03):
		return true
	case len(k) == 1 && k[0] == 0x1b: // Esc
		if p.session != "" {
			p.closeSession()
		}
	case isUp(k):
		if p.session == "" {
			p.selected = max(0, p.selected-1)
		} else {
			p.follow = false
			p.offset++
		}
	case isDown(k):
		if p.session == "" {
			p.selected = min(len(p.devices)-1, p.selected+1)
		} else if p.offset > 0 {
			p.offset--
		}
	case len(k) == 1 && (k[0] == '\r' || k[0] == '\n'):
		if p.session == "" && len(p.devices) > 0 {
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

func (p *devicesPane) openSession(d device) {
	var info struct {
		ID string `json:"id"`
	}
	if err := p.c.Call("logs.open", map[string]any{"device": d, "autoStart": true, "displayName": d.displayModel()}, &info); err != nil {
		return
	}
	p.session, p.streamOf = info.ID, d
	p.lines, p.follow, p.offset = nil, true, 0
	_ = p.c.Subscribe("logs.lines."+info.ID, "logs.state."+info.ID)
}

func (p *devicesPane) closeSession() {
	if p.session == "" {
		return
	}
	id := p.session
	p.session = ""
	_ = p.c.Call("events.unsubscribe", map[string]any{"topics": []string{"logs.lines." + id, "logs.state." + id}}, nil)
	_ = p.c.Call("logs.close", map[string]any{"id": id}, nil)
}

func (p *devicesPane) draw() {
	rows, cols := termSize()
	var b strings.Builder
	b.WriteString("\x1b[H\x1b[2J")
	line := func(s string) { b.WriteString(s + "\x1b[K\r\n") }

	if p.session == "" {
		line(sgrDim + "Devices" + sgrReset)
		line("")
		switch {
		case !p.loaded:
		case len(p.devices) == 0:
			line("No devices connected")
		default:
			for i, d := range p.devices {
				marker := "  "
				if i == p.selected {
					marker = sgrRev + " " + sgrReset + " "
				}
				name := sgrBold + fit(d.displayModel(), max(10, cols/2-4)) + sgrReset
				state := d.stateLabel()
				if !d.isReady() {
					state = sgrDim + state + sgrReset
				}
				line(marker + name + "  " + fit(d.platformName(), 14) + "  " + state)
			}
		}
		fmt.Print(b.String())
		return
	}

	// Header: the device and the stream state.
	head := sgrBold + p.streamOf.displayModel() + sgrReset + "  " + sgrDim + "≥" + levelShort[p.minLevel] + sgrReset
	if p.state.IsConnecting {
		head += "  " + sgrDim + "Connecting…" + sgrReset
	} else if !p.state.IsRunning {
		head += "  " + sgrDim + "■" + sgrReset
	}
	line(head)
	if p.state.StatusMessage != nil {
		line(sgrRed + fit(*p.state.StatusMessage, cols) + sgrReset)
	}

	visible := make([]logLine, 0, len(p.lines))
	for _, l := range p.lines {
		if l.Marker || l.Level >= p.minLevel {
			visible = append(visible, l)
		}
	}
	room := rows - 2
	if p.state.StatusMessage != nil {
		room--
	}
	end := len(visible) - p.offset
	if end < 0 {
		end = 0
		p.offset = len(visible)
	}
	start := max(0, end-room)
	for _, l := range visible[start:end] {
		line(renderLogLine(l, cols))
	}
	fmt.Print(b.String())
}

func renderLogLine(l logLine, cols int) string {
	msg := strings.ReplaceAll(l.Message, "\n", " ⏎ ")
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
	rest := fit(l.Tag+": "+msg, max(0, cols-len(prefix)))
	return sgrDim + ts + sgrReset + " " + color + level + " " + rest + sgrReset
}
