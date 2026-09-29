package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
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

	p := &projectsPane{c: c}
	for {
		p.draw()
		select {
		case ev := <-c.Events:
			if ev.Topic == projectsTopic && json.Unmarshal(ev.Data, &p.state) == nil {
				p.loaded = true
			}
		case <-c.Closed:
			restore()
			fmt.Fprintln(os.Stderr, "jaca: jacad closed the connection")
			return 1
		case k, ok := <-keys:
			if !ok || (len(k) == 1 && (k[0] == 'q' || k[0] == 0x03)) {
				return 0
			}
			p.handleKey(k, toasts)
		case t := <-toasts:
			p.toast, p.toastAt = t, time.Now()
		case <-resize:
		case <-tick.C:
			if p.toast != "" && time.Since(p.toastAt) > 2600*time.Millisecond {
				p.toast = ""
			}
		}
	}
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

func (p *projectsPane) handleKey(k []byte, toasts chan<- string) {
	rows := p.rows()
	switch {
	case isUp(k):
		p.selected = max(0, p.selected-1)
	case isDown(k):
		p.selected = min(len(rows)-1, p.selected+1)
	case len(k) == 1 && k[0] == 'r':
		go p.c.Call("projects.refresh", nil, nil)
	case len(k) == 1 && k[0] == 'c':
		if p.selected >= len(rows) || rows[p.selected].checkout < 0 {
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
			if p.c.Call("projects.clearCache", map[string]any{"project": pr.Path, "checkout": co.Path}, &outcome) != nil || outcome == nil {
				return
			}
			// The same toasts as ProjectsModel.clearCache.
			if outcome.Error != nil {
				msg := []rune(*outcome.Error)
				if len(msg) > 50 {
					msg = msg[:50]
				}
				toasts <- "Clean failed · " + string(msg)
			} else {
				toasts <- fmt.Sprintf("Freed %s · %s", formatSize(outcome.FreedMB), outcome.Name)
			}
		}()
	}
}

func (p *projectsPane) draw() {
	rows, cols := termSize()
	var b strings.Builder
	b.WriteString("\x1b[H\x1b[2J")
	line := func(s string) { b.WriteString(s + "\x1b[K\r\n") }

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
		room := rows - 3
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
				line(marker + sgrBold + fit(pr.name(), cols-2) + sgrReset)
				continue
			}
			co := pr.Checkouts[ref.checkout]
			size := ""
			if co.Cleaning {
				size = "Cleaning…"
			} else if co.SizeComputed {
				size = formatSize(co.SizeMB)
			}
			nameW := max(10, cols-len([]rune(size))-8)
			line(marker + "  " + fit(co.name(), nameW) + "  " + sgrDim + size + sgrReset)
		}
	}
	if p.toast != "" && rows > 4 {
		b.WriteString(fmt.Sprintf("\x1b[%d;1H%s%s%s\x1b[K", rows, sgrBold, fit(p.toast, cols), sgrReset))
	}
	fmt.Print(b.String())
}
