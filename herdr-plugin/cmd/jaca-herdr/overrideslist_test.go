package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFollowWindow(t *testing.T) {
	for _, c := range []struct{ start, selected, room, n, want int }{
		{0, 0, 5, 12, 0},
		{0, 4, 5, 12, 0}, // still in view
		{0, 5, 5, 12, 1}, // one past the end: move by one
		{5, 7, 5, 12, 5}, // a row in view: no move
		{5, 3, 5, 12, 3}, // above the window: it starts there
		{8, 11, 5, 9, 4}, // the list got shorter: show as many as fit
		{3, 0, 5, 2, 0},  // fewer rules than room
		{2, 2, 0, 12, 2}, // no room is one row
	} {
		if got := followWindow(c.start, c.selected, c.room, c.n); got != c.want {
			t.Errorf("followWindow(%d, %d, %d, %d) = %d, want %d", c.start, c.selected, c.room, c.n, got, c.want)
		}
	}
}

// overridesListViewer is the network viewer with the overrides list open on twelve rules.
func overridesListViewer(t *testing.T) *netViewer {
	t.Helper()
	v := testNetViewer(t)
	var rules []overrideRule
	for i := 0; i < 12; i++ {
		r := newOverrideRule()
		r.Name, r.Matcher.Pattern = fmt.Sprintf("Rule %02d", i), fmt.Sprintf("https://api.example.com/%02d", i)
		rules = append(rules, r)
	}
	v.setOverrides(overridesState{Rules: rules, MasterEnabled: true})
	v.openOverridesList()
	return v
}

// listedRules is the names of the rules the list popup draws, in order.
func listedRules(v *netViewer, rows int) []string {
	var names []string
	for _, row := range v.listBox(rows, 120).rows {
		plain := sgr.ReplaceAllString(row, "")
		if i := strings.Index(plain, "Rule "); i >= 0 {
			names = append(names, plain[i:i+7])
		}
	}
	return names
}

// Toggling a rule after scrolling leaves the list where it was (it used to jump back so the
// selected rule sat at the bottom, or to the first rule).
func TestOverridesListKeepsItsScrollWhenARuleIsToggled(t *testing.T) {
	v := overridesListViewer(t)
	const rows = 40 // room for six rules
	listedRules(v, rows)
	for i := 0; i < 9; i++ {
		v.overridesKey([]byte("\x1b[B"))
		listedRules(v, rows)
	}
	want := []string{"Rule 04", "Rule 05", "Rule 06", "Rule 07", "Rule 08", "Rule 09"}
	if got := listedRules(v, rows); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("scrolled to the tenth rule, the list shows %v", got)
	}
	// Up three rows: the selection is still in view, so the list holds still.
	for i := 0; i < 3; i++ {
		v.overridesKey([]byte("\x1b[A"))
		listedRules(v, rows)
	}
	// The toggle's result arrives as a new state with the rule disabled.
	ov := v.overrides
	ov.Rules = append([]overrideRule(nil), ov.Rules...)
	ov.Rules[6].Enabled = false
	v.setOverrides(ov)
	if got := listedRules(v, rows); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("after toggling the seventh rule the list shows %v, want %v", got, want)
	}
	// A click on the switch of a rule in view selects it and keeps the list too.
	v.ovui.selected = 5
	if got := listedRules(v, rows); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("after selecting a rule in view the list shows %v", got)
	}
}

// A rule is its name, its pattern and a padding row; a divider row (the margin) separates it from
// the next one, with none after the last. The selection covers the padding row, and so do clicks.
func TestOverridesListSeparatesRules(t *testing.T) {
	v := overridesListViewer(t)
	v.ovui.selected = 1
	b := v.listBox(40, 120)
	var plain []string
	for _, row := range b.rows {
		plain = append(plain, strings.TrimRight(strings.Trim(sgr.ReplaceAllString(row, ""), "│"), " "))
	}
	dividers := 0
	for i, row := range plain {
		if strings.TrimSpace(row) == "" || strings.Trim(strings.TrimSpace(row), "─") != "" {
			continue
		}
		dividers++
		if strings.TrimSpace(plain[i-1]) != "" || !strings.Contains(plain[i-2], "https://api.example.com/") {
			t.Errorf("divider at row %d does not follow a rule's pattern and padding: %q / %q", i, plain[i-2], plain[i-1])
		}
		if !strings.Contains(plain[i+1], "Rule ") {
			t.Errorf("divider at row %d is not followed by the next rule: %q", i, plain[i+1])
		}
	}
	if dividers != 5 {
		t.Errorf("%d dividers between six drawn rules, want 5:\n%s", dividers, strings.Join(plain, "\n"))
	}
	// The second rule's rows: its name, pattern and padding all carry the selection.
	name := -1
	for i, row := range plain {
		if strings.Contains(row, "Rule 01") {
			name = i
		}
	}
	for row := name; row < name+3; row++ {
		if !strings.Contains(b.rows[row], selectedRowStyle) {
			t.Errorf("row %d of the selected rule is not highlighted: %q", row-name, plain[row])
		}
		if n, ok := v.ruleAt(row); !ok || n != 1 {
			t.Errorf("row %d of the selected rule maps to rule %d (%v)", row-name, n, ok)
		}
	}
	if _, ok := v.ruleAt(name + 3); ok {
		t.Error("the divider row belongs to a rule")
	}
}

// pipeClient is a client whose jacad is the other end of a pipe; it records each method called.
func pipeClient(t *testing.T) (*client, func() []map[string]any) {
	t.Helper()
	near, far := net.Pipe()
	t.Cleanup(func() { near.Close(); far.Close() })
	c := &client{conn: near, pending: map[int]chan inbound{}, Events: make(chan event), Closed: make(chan struct{}),
		dropped: map[string]int{}}
	c.qcond = sync.NewCond(&c.qmu)
	go c.readLoop()
	go c.pump()
	var mu sync.Mutex
	var calls []map[string]any
	go func() {
		in := bufio.NewScanner(far)
		for in.Scan() {
			var req map[string]any
			if json.Unmarshal(in.Bytes(), &req) != nil {
				continue
			}
			mu.Lock()
			calls = append(calls, req)
			mu.Unlock()
			reply, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": map[string]any{}})
			far.Write(append(reply, '\n'))
		}
	}()
	return c, func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), calls...)
	}
}

func TestOverridesListShiftEnterToggles(t *testing.T) {
	v := overridesListViewer(t)
	c, calls := pipeClient(t)
	v.p.c = c
	v.listBox(40, 120)
	v.overridesKey([]byte("\x1b[B"))
	v.overridesKey([]byte("\x1b[B"))

	for _, seq := range []string{"\x1b[27;2;13~", "\x1b[13;2u"} {
		if got := string(keyFor(v, []byte(seq))); got != seq {
			t.Fatalf("the list popup got %q for Shift+Enter %q", got, seq)
		}
	}
	v.overridesKey(keyFor(v, []byte("\x1b[27;2;13~")))
	if v.focus != netOverrides || v.ovui.draft != nil {
		t.Fatal("Shift+Enter opened the editor")
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(calls()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	got := calls()
	if len(got) != 1 || got[0]["method"] != "overrides.setEnabled" {
		t.Fatalf("calls: %v", got)
	}
	params := got[0]["params"].(map[string]any)
	if params["id"] != v.overrides.Rules[2].ID || params["enabled"] != false {
		t.Errorf("setEnabled params %v, want the third rule turned off", params)
	}

	// Plain Enter still edits.
	v.overridesKey(keyFor(v, []byte("\r")))
	if v.focus != netEditor {
		t.Error("Enter didn't open the editor")
	}
}

// Everywhere but the list popup Shift+Enter is Enter, as it was before the terminal reported it.
func TestShiftEnterIsEnterElsewhere(t *testing.T) {
	v := testNetViewer(t)
	if got := keyFor(v, []byte("\x1b[27;2;13~")); string(got) != "\r" {
		t.Errorf("the request list got %q", got)
	}
	v = overridesListViewer(t)
	v.openEditor(v.overrides.Rules[0], false, "")
	if got := keyFor(v, []byte("\x1b[13;2u")); string(got) != "\r" {
		t.Errorf("the editor got %q", got)
	}
	if got := keyFor(v, []byte("a")); string(got) != "a" {
		t.Errorf("another key changed: %q", got)
	}
}
