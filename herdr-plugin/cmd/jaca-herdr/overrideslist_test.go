package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"reflect"
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

// P pauses or resumes all overrides from the request list and its detail.
func TestOverridesShortcuts(t *testing.T) {
	v := overridesListViewer(t)
	c, calls := pipeClient(t)
	v.p.c = c
	v.focus = netList
	v.handleKey([]byte("P"))
	v.detail, v.detailFocus = true, true
	v.handleKey([]byte("P"))
	deadline := time.Now().Add(2 * time.Second)
	for len(calls()) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	got := calls()
	if len(got) != 2 {
		t.Fatalf("calls: %v", got)
	}
	for _, call := range got {
		if call["method"] != "overrides.setMaster" {
			t.Errorf("P sent %v, want overrides.setMaster", call["method"])
		}
	}
}

// groupedViewer is the network viewer with the overrides list open on two groups (Checkout
// with rules 0 and 1, Login, off, with rule 2) and two rules in no group (3, and 4, whose
// groupID names no listed group).
func groupedViewer(t *testing.T) *netViewer {
	t.Helper()
	v := overridesListViewer(t)
	rules := v.overrides.Rules[:5]
	for i, group := range []string{groupA, groupA, groupB, "", "C0000000-0000-4000-8000-00000000000C"} {
		rules[i].GroupID = group
	}
	v.setOverrides(overridesState{Rules: rules, MasterEnabled: true, HasGroups: true, Groups: []overrideGroup{
		{ID: groupA, Name: "Checkout", Enabled: true}, {ID: groupB, Name: "Login", Enabled: false}}})
	v.openOverridesList()
	return v
}

// describeRows is the list's rows as text: "Name(count)" for a group and the name for a rule.
func describeRows(v *netViewer) []string {
	var out []string
	for _, row := range v.ovRows() {
		if row.group != nil {
			out = append(out, fmt.Sprintf("%s(%d)", row.group.displayName(), row.count))
			continue
		}
		out = append(out, v.overrides.Rules[row.rule].Name)
	}
	return out
}

// boxText is the popup as drawn, without styles, one string per row.
func boxText(v *netViewer) []string {
	var out []string
	for _, row := range v.listBox(60, 120).rows {
		out = append(out, sgr.ReplaceAllString(row, ""))
	}
	return out
}

// The list shows each group as one row; Enter opens the group in its own popup, under its name,
// with its rules, its switch and its actions, and Esc goes back.
func TestOverrideGroupPopup(t *testing.T) {
	v := groupedViewer(t)
	want := "Checkout(2),Login(1),Rule 03,Rule 04"
	if got := strings.Join(describeRows(v), ","); got != want {
		t.Errorf("rows = %s\nwant   %s", got, want)
	}

	// A group that is off is dim; the group row has its marker and count.
	plain := map[string]string{}
	for _, row := range v.listBox(60, 120).rows {
		text := sgr.ReplaceAllString(row, "")
		for _, name := range []string{"Login", "Checkout"} {
			if strings.Contains(text, name) {
				plain[name] = row
			}
		}
	}
	if !strings.Contains(sgr.ReplaceAllString(plain["Checkout"], ""), "▸ Checkout") ||
		!strings.Contains(sgr.ReplaceAllString(plain["Checkout"], ""), "(2)") {
		t.Errorf("group row = %q", sgr.ReplaceAllString(plain["Checkout"], ""))
	}
	if !strings.Contains(plain["Login"], tertiaryStyle+"Login") {
		t.Errorf("a group that is off isn't dim: %q", plain["Login"])
	}

	// Enter on Checkout opens its popup; nothing is sent and the editor stays shut.
	v.ovui.selected = 0
	v.overridesKey([]byte("\r"))
	if v.focus != netOverrides || v.ovui.draft != nil || v.ovui.group != groupA {
		t.Fatalf("Enter on a group: focus %v, group %q", v.focus, v.ovui.group)
	}
	if got := strings.Join(describeRows(v), ","); got != "Rule 00,Rule 01" {
		t.Errorf("group rows = %s", got)
	}
	text := strings.Join(boxText(v), "\n")
	for _, part := range []string{"Checkout", "Group: ON", "Rule 00", "Rule 01", "New override", "Rename group", "Delete group"} {
		if !strings.Contains(text, part) {
			t.Errorf("the group popup lacks %q:\n%s", part, text)
		}
	}
	if strings.Contains(text, "Rule 03") || strings.Contains(text, "New group") || strings.Contains(text, "Overrides: ON") {
		t.Errorf("the group popup shows the list of everything:\n%s", text)
	}
	if !strings.Contains(boxText(v)[0], "Checkout") {
		t.Errorf("title row = %q", boxText(v)[0])
	}

	// Enter on a rule edits it, and closing the editor comes back to the group's popup.
	v.overridesKey([]byte("\x1b[B"))
	v.overridesKey([]byte("\r"))
	if v.focus != netEditor || v.ovui.draft == nil || v.ovui.draft.rule.Name != "Rule 01" {
		t.Fatal("Enter on a rule in the group didn't edit it")
	}
	v.closeEditor()
	if v.focus != netOverrides || v.ovui.group != groupA {
		t.Errorf("after the editor: focus %v, group %q", v.focus, v.ovui.group)
	}

	// A new override made here goes in the group.
	v.overridesKey([]byte("n"))
	if d := v.ovui.draft; d == nil || d.rule.GroupID != groupA {
		t.Errorf("a new override from the group's popup isn't in it: %+v", v.ovui.draft)
	}
	v.closeEditor()

	// Esc goes back to the list, with the group selected; Esc again closes the popup.
	v.overridesKey([]byte("\x1b"))
	if v.focus != netOverrides || v.ovui.group != "" || v.ovui.selected != 0 {
		t.Errorf("after Esc: focus %v, group %q, selected %d", v.focus, v.ovui.group, v.ovui.selected)
	}
	v.overridesKey([]byte("\x1b"))
	if v.focus != netList {
		t.Error("Esc on the list didn't close it")
	}

	// A group deleted elsewhere closes its popup.
	v.openOverridesList()
	v.openGroup(groupB)
	s := v.overrides
	s.Groups = s.Groups[:1]
	v.setOverrides(s)
	v.listBox(60, 120)
	if v.ovui.group != "" {
		t.Error("the popup of a group that is gone stayed open")
	}
}

// A daemon with no groups shows the list flat, as before; a groupID it sends anyway is ignored.
func TestOverrideRowsWithoutGroups(t *testing.T) {
	v := overridesListViewer(t)
	rules := v.overrides.Rules
	rules[0].GroupID = groupA
	v.setOverrides(overridesState{Rules: rules, MasterEnabled: true})
	rows := describeRows(v)
	if len(rows) != 12 || rows[0] != "Rule 00" {
		t.Errorf("rows = %v", rows)
	}
	c, calls := pipeClient(t)
	v.p.c = c
	for _, key := range []string{"N", "g", "r"} {
		v.overridesKey([]byte(key))
	}
	if v.ovui.menu != nil || v.ovui.rename != nil {
		t.Error("a group key acted on a daemon without groups")
	}
	time.Sleep(50 * time.Millisecond)
	if got := calls(); len(got) != 0 {
		t.Errorf("calls: %v", got)
	}

	// An empty state: nothing to select, and no key panics.
	v.setOverrides(overridesState{MasterEnabled: true, HasGroups: true})
	for _, key := range []string{"\x1b[B", "\r", " ", "x", "K", "J", "g", "r", "d"} {
		v.openOverridesList()
		v.overridesKey([]byte(key))
	}
	v.closeOverrides()
	v.openOverridesList()
	v.listBox(40, 120)
}

// waitCalls waits for n calls to reach the pipe's jacad.
func waitCalls(t *testing.T, calls func() []map[string]any, n int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for len(calls()) < n && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	got := calls()
	if len(got) != n {
		t.Fatalf("%d calls, want %d: %v", len(got), n, got)
	}
	return got
}

func TestOverrideGroupActions(t *testing.T) {
	v := groupedViewer(t)
	c, calls := pipeClient(t)
	v.p.c = c
	rules := v.overrides.Rules

	v.ovui.selected = 1         // Login
	v.overridesKey([]byte(" ")) // toggle the group on
	v.overridesKey([]byte("K")) // move it up
	if v.ovui.selected != 0 {
		t.Errorf("after moving Login up the selection is on row %d, want 0", v.ovui.selected)
	}
	v.ovui.selected = 1
	v.overridesKey([]byte("r")) // rename
	for _, k := range []string{"\x15", "A", "u", "t", "h", "\r"} {
		v.overridesKey([]byte(k))
	}
	v.ovui.selected = 1
	v.overridesKey([]byte("x")) // delete the group

	v.ovui.selected = 2 // Rule 03, in no group
	v.overridesKey([]byte("g"))
	if v.ovui.menu == nil || len(v.ovui.menu.items) != 3 || v.ovui.menu.items[0].label != "Move to group" ||
		v.ovui.menu.items[1].label != "Checkout" || v.ovui.menu.items[2].label != "Login" {
		t.Fatalf("picker for a rule in no group: %+v", v.ovui.menu)
	}
	v.overridesKey([]byte("\x1b[B")) // Login
	v.overridesKey([]byte("\r"))
	v.ovui.selected = 0
	v.overridesKey([]byte("\r")) // open Checkout
	v.ovui.selected = 0          // Rule 00
	v.overridesKey([]byte("g"))
	labels := []string{}
	for _, it := range v.ovui.menu.items {
		labels = append(labels, it.label)
	}
	if strings.Join(labels, ",") != "Move to group,Login,,No group" {
		t.Fatalf("picker for a grouped rule: %v", labels)
	}
	v.overridesKey([]byte("\x1b[B")) // over the separator to No group
	v.overridesKey([]byte("\r"))

	// K on the first rule of a group doesn't cross out of it; J moves inside it.
	v.ovui.selected = 0
	v.overridesKey([]byte("K"))
	v.overridesKey([]byte("J"))
	if v.ovui.selected != 1 {
		t.Errorf("after J the selection is on row %d, want 1", v.ovui.selected)
	}

	type call struct {
		method string
		params map[string]any
	}
	want := []call{
		{"overrides.saveGroup", map[string]any{"group": map[string]any{"id": groupB, "name": "Login", "enabled": true}}},
		{"overrides.moveGroup", map[string]any{"id": groupB, "offset": float64(-1)}},
		{"overrides.saveGroup", map[string]any{"group": map[string]any{"id": groupB, "name": "Auth", "enabled": false}}},
		{"overrides.removeGroup", map[string]any{"id": groupB}},
		{"overrides.setGroup", map[string]any{"id": rules[3].ID, "group": groupB}},
		{"overrides.setGroup", map[string]any{"id": rules[0].ID}},
		{"overrides.move", map[string]any{"id": rules[0].ID, "offset": float64(1)}},
	}
	// K on the first rule of a group sent nothing. Each call goes out on its own goroutine, so they
	// are compared in any order. A group's createdAt is left out of the comparison.
	got := waitCalls(t, calls, len(want))
	for _, c := range got {
		params := c["params"].(map[string]any)
		if g, ok := params["group"].(map[string]any); ok {
			if _, has := g["createdAt"]; !has {
				t.Errorf("%s: a group without createdAt: %v", c["method"], g)
			}
			delete(g, "createdAt")
		}
		found := false
		for i, w := range want {
			if c["method"] == w.method && reflect.DeepEqual(params, w.params) {
				want = append(want[:i], want[i+1:]...)
				found = true
				break
			}
		}
		if !found {
			t.Errorf("unexpected call %s %v", c["method"], params)
		}
	}
	for _, w := range want {
		t.Errorf("missing call %s %v", w.method, w.params)
	}
}

// N creates "Untitled group" at the end of the groups, selects it and renames it in place.
func TestOverrideNewGroup(t *testing.T) {
	v := groupedViewer(t)
	c, calls := pipeClient(t)
	v.p.c = c
	v.overridesKey([]byte("N"))
	if v.ovui.rename == nil || v.ovui.rename.name.String() != "Untitled group" {
		t.Fatalf("rename = %+v", v.ovui.rename)
	}
	rows := v.ovRows()
	if sel := rows[v.ovui.selected]; sel.group == nil || sel.group.ID != v.ovui.rename.group.ID || v.ovui.selected != 2 {
		t.Errorf("selected row %d: %+v", v.ovui.selected, sel)
	}
	drawn := false
	for _, row := range v.listBox(60, 120).rows {
		if strings.Contains(sgr.ReplaceAllString(row, ""), "Untitled group") {
			drawn = true
		}
	}
	if !drawn {
		t.Error("the new group isn't drawn before the daemon lists it")
	}
	id := v.ovui.rename.group.ID
	for _, k := range []string{"\x15", "C", "a", "r", "t", "\r"} {
		v.overridesKey([]byte(k))
	}
	// The create and the rename, in either order (each call has its own goroutine).
	names := map[string]bool{}
	for _, c := range waitCalls(t, calls, 2) {
		g := c["params"].(map[string]any)["group"].(map[string]any)
		if c["method"] != "overrides.saveGroup" || g["id"] != id || g["enabled"] != true {
			t.Errorf("call %v", c)
		}
		names[fmt.Sprint(g["name"])] = true
	}
	if !names["Untitled group"] || !names["Cart"] {
		t.Errorf("saved names %v, want Untitled group and Cart", names)
	}
	// Esc while renaming keeps the old name and sends nothing.
	v.ovui.selected = 0
	v.overridesKey([]byte("r"))
	v.overridesKey([]byte("x"))
	v.overridesKey([]byte("\x1b"))
	if v.ovui.rename != nil || v.focus != netOverrides {
		t.Error("Esc didn't end renaming")
	}
	time.Sleep(50 * time.Millisecond)
	if len(calls()) != 2 {
		t.Errorf("Esc sent %v", calls()[2:])
	}
}
