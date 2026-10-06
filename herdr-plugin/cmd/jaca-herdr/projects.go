package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// Mirrors of ProjectsState / Project / ProjectCheckout (Sources/Core/Projects).
type checkoutRow struct {
	Path         string  `json:"path"`
	IsMain       bool    `json:"isMain"`
	Branch       *string `json:"branch"`
	SizeMB       int     `json:"sizeMB"`
	CacheMB      int     `json:"cacheMB"`
	SizeComputed bool    `json:"sizeComputed"`
	Cleaning     bool    `json:"cleaning"`
}

// name matches ProjectCheckout.name.
func (c checkoutRow) name() string {
	if c.Branch != nil && *c.Branch != "" {
		return *c.Branch
	}
	if c.IsMain {
		return "main checkout"
	}
	return filepath.Base(c.Path)
}

type projectRow struct {
	Path      string        `json:"path"`
	Checkouts []checkoutRow `json:"checkouts"`
}

func (p projectRow) name() string {
	if n := filepath.Base(p.Path); n != "" && n != "/" {
		return n
	}
	return p.Path
}

type projectsState struct {
	Projects         []projectRow `json:"projects"`
	IsRefreshing     bool         `json:"isRefreshing"`
	HasCompletedScan bool         `json:"hasCompletedScan"`
}

const projectsTopic = "projects.state"

// A flattened row: a project header, or one of its checkouts.
type projRowRef struct {
	project  int
	checkout int // -1 = the project header
}

type projectsPane struct {
	c        *client
	state    projectsState
	loaded   bool
	selected int
	toast    string
	toastAt  time.Time
}

// runProjectsPane: j/k move, c cleans the selected checkout's build caches, r rescans, q quits.
func runProjectsPane() int {
	c, err := dial()
	if err != nil {
		fmt.Fprintln(os.Stderr, "jaca:", err)
		return 1
	}
	defer c.Close()
	if err := c.Subscribe(projectsTopic); err != nil {
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
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	toasts := make(chan string, 4)
	done := make(chan struct{})
	defer close(done)

	p := &projectsPane{c: c}
	p.draw()
	for {
		select {
		case ev := <-c.Events:
			switch ev.Topic {
			case projectsTopic:
				var st projectsState
				if json.Unmarshal(ev.Data, &st) == nil {
					p.setState(st)
				}
			case "events.dropped":
				// The skipped update may have been the latest state: fetch it.
				var note droppedNote
				// Fetched on the UI loop, so a newer live update can't be overwritten by this
				// reply (readLoop never blocks on the pane, so the reply can't wait on this loop).
				if json.Unmarshal(ev.Data, &note) != nil || note.Topic != projectsTopic {
					continue
				}
				var st projectsState
				if c.CallTimeout("projects.state", nil, &st, syncTimeout) != nil {
					continue
				}
				p.setState(st)
			}
		case <-c.Closed:
			restore()
			fmt.Fprintln(os.Stderr, "jaca: jacad closed the connection")
			return 1
		case k, ok := <-keys:
			if !ok || (len(k) == 1 && (k[0] == 'q' || k[0] == 0x03)) {
				return 0
			}
			p.handleKey(k, toasts, done)
		case t := <-toasts:
			p.toast, p.toastAt = t, time.Now()
		case <-resize:
			refreshTermSize()
		case <-tick.C:
			if p.toast == "" || time.Since(p.toastAt) <= 2600*time.Millisecond {
				continue
			}
			p.toast = ""
		}
		p.draw()
	}
}

func (p *projectsPane) setState(st projectsState) {
	p.state, p.loaded = st, true
	p.selected = clampIndex(p.selected, len(p.rows()))
}

func (p *projectsPane) rows() []projRowRef {
	var out []projRowRef
	for i, pr := range p.state.Projects {
		out = append(out, projRowRef{i, -1})
		for j := range pr.Checkouts {
			out = append(out, projRowRef{i, j})
		}
	}
	return out
}

func (p *projectsPane) handleKey(k []byte, toasts chan<- string, done <-chan struct{}) {
	rows := p.rows()
	switch {
	case isUp(k):
		p.selected = clampIndex(p.selected-1, len(rows))
	case isDown(k):
		p.selected = clampIndex(p.selected+1, len(rows))
	case len(k) == 1 && k[0] == 'r':
		go p.c.Call("projects.refresh", nil, nil)
	case len(k) == 1 && k[0] == 'c':
		if p.selected < 0 || p.selected >= len(rows) || rows[p.selected].checkout < 0 {
			return
		}
		ref := rows[p.selected]
		pr := p.state.Projects[ref.project]
		co := pr.Checkouts[ref.checkout]
		go func() {
			var outcome *struct {
				Name    string  `json:"name"`
				FreedMB int     `json:"freedMB"`
				Error   *string `json:"error"`
			}
			var toast string
			// The failure lines are the projects-clear-cache action's (main.go); the outcome
			// toasts are ProjectsModel.clearCache's.
			switch err := p.c.CallTimeout("projects.clearCache", map[string]any{"project": pr.Path, "checkout": co.Path}, &outcome, 10*time.Minute); {
			case err != nil:
				toast = "jaca: " + err.Error()
			case outcome == nil:
				toast = "jaca: already cleaning, or the checkout is gone"
			case outcome.Error != nil:
				msg := []rune(*outcome.Error)
				if len(msg) > 50 {
					msg = msg[:50]
				}
				toast = "Clean failed · " + string(msg)
			default:
				toast = fmt.Sprintf("Freed %s · %s", formatSize(outcome.FreedMB), outcome.Name)
			}
			select {
			case toasts <- toast:
			case <-done:
			}
		}()
	}
}

func (p *projectsPane) draw() {
	rows, cols := termSize()
	var frame []string
	line := func(s string) { frame = append(frame, s) }

	head := sgrDim + "PROJECTS" + sgrReset
	if p.state.IsRefreshing {
		head += "  " + sgrDim + "Refreshing…" + sgrReset
	}
	line(head)
	line("")
	refs := p.rows()
	switch {
	case !p.loaded:
	case len(refs) == 0 && p.state.HasCompletedScan:
		line("No projects found")
	default:
		room := max(1, rows-3)
		start := 0
		if p.selected >= room {
			start = p.selected - room + 1
		}
		for i := start; i < len(refs) && i < start+room; i++ {
			ref := refs[i]
			marker := "  "
			if i == p.selected {
				marker = sgrRev + " " + sgrReset + " "
			}
			pr := p.state.Projects[ref.project]
			if ref.checkout < 0 {
				line(marker + sgrBold + fit(sanitize(pr.name()), cols-2) + sgrReset)
				continue
			}
			co := pr.Checkouts[ref.checkout]
			size := ""
			if co.Cleaning {
				size = "Cleaning…"
			} else if co.SizeComputed {
				size = formatSize(co.SizeMB)
			}
			nameW := max(10, cols-cellWidth(size)-8)
			line(marker + "  " + fit(sanitize(co.name()), nameW) + "  " + sgrDim + size + sgrReset)
		}
	}
	if p.toast != "" && rows > 4 {
		frame = rowAt(frame, rows, sgrBold+fit(sanitize(p.toast), cols)+sgrReset)
	}
	paintRows(frame)
}
