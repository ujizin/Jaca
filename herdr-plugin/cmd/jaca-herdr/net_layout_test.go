package main

import (
	"strings"
	"testing"
)

// Enter opens the detail with the keys; Esc closes it and leaves the list as it was.
func TestNetDetailFocusAndEscape(t *testing.T) {
	v := testNetViewer(t)
	setTermSize(t, 30, 150)
	v.handleKey([]byte("j"))
	v.handleKey([]byte{'\r'})
	v.draw()
	if !v.detail || !v.detailFocus {
		t.Fatalf("Enter: detail %v, focus %v", v.detail, v.detailFocus)
	}
	// The arrows scroll the detail and leave the selection; Left and Right switch tabs.
	v.handleKey([]byte("\x1b[B"))
	if v.selectedID != "A" {
		t.Errorf("Down in the detail selected %q", v.selectedID)
	}
	v.handleKey([]byte("\x1b[C"))
	if v.tab != tabHeaders {
		t.Errorf("Right in the detail went to tab %d", v.tab)
	}
	v.handleKey([]byte{0x1b})
	if v.detail || v.detailFocus || v.selectedID != "A" {
		t.Fatalf("Esc: detail %v, focus %v, selected %q; want the detail closed and the list as it was",
			v.detail, v.detailFocus, v.selectedID)
	}
	// Back on the list, Down selects the next request.
	v.handleKey([]byte("\x1b[B"))
	if v.selectedID != "B" {
		t.Errorf("Down on the list selected %q", v.selectedID)
	}
}

// A detail opened with a click closes on one Esc too.
func TestNetDetailEscapeAfterAClick(t *testing.T) {
	v := testNetViewer(t)
	setTermSize(t, 30, 150)
	v.draw()
	v.mouse(mouseEvent{button: 0, x: 5, y: v.listTop, press: true})
	v.draw()
	if !v.detail || v.detailFocus {
		t.Fatalf("a click on a row: detail %v, focus %v", v.detail, v.detailFocus)
	}
	v.handleKey([]byte{0x1b})
	if v.detail || v.selectedID == "" {
		t.Errorf("Esc: detail %v, selected %q", v.detail, v.selectedID)
	}
}

// A click in the detail gives it the keys, and the button at its top closes it.
func TestNetDetailClickFocus(t *testing.T) {
	v := testNetViewer(t)
	setTermSize(t, 30, 150)
	v.draw()
	v.mouse(mouseEvent{button: 0, x: 5, y: v.listTop, press: true})
	v.draw()
	v.mouse(mouseEvent{button: 0, x: v.detailX + 2, y: v.detailFirst + 3, press: true})
	v.draw()
	if !v.detailFocus {
		t.Fatal("a click in the detail didn't give it the keys")
	}
	v.mouse(mouseEvent{button: 0, x: v.closeX0, y: v.listTop - 1, press: true})
	if v.detail || v.detailFocus {
		t.Errorf("the close button: detail %v, focus %v", v.detail, v.detailFocus)
	}
}

// Esc in the rule editor goes back to where it was opened from: the overrides list, or the
// request list.
func TestEditorEscapeGoesBack(t *testing.T) {
	v := testNetViewer(t)
	v.openOverridesList()
	v.handleKey([]byte{'\r'})
	if v.focus != netEditor {
		t.Fatalf("Enter on a rule: focus %d", v.focus)
	}
	v.handleKey([]byte{0x1b})
	if v.focus != netOverrides || v.ovui.draft != nil {
		t.Errorf("Esc in an editor opened from the list: focus %d, want the list", v.focus)
	}
	v.handleKey([]byte{0x1b})
	if v.focus != netList {
		t.Errorf("Esc on the overrides list: focus %d", v.focus)
	}
	v.openEditor(blankRule("example.com"), true, "")
	v.handleKey([]byte{0x1b})
	if v.focus != netList {
		t.Errorf("Esc in an editor opened from a request: focus %d, want the request list", v.focus)
	}
}

// A path too long for its column keeps its end, and a narrow list gives up Size and Host
// before it gives up the path.
func TestNetListKeepsThePathReadable(t *testing.T) {
	v := testNetViewer(t)
	v.upsert([]netTransaction{{ID: "D", Method: "GET", Host: "payments.example.com", StatusCode: intPtr(200),
		URL: "https://payments.example.com/v1/customers/12345678-aaaa-bbbb-cccc/card-applications/step"}})
	rows := v.listColumn(67, 8)
	header := stripSGR(rows[0])
	if strings.Contains(header, "HOST") || strings.Contains(header, "SIZE") || !strings.Contains(header, "TIME") {
		t.Errorf("a 67-cell list shows the columns %q; want Path and Time, without Host and Size", header)
	}
	last := stripSGR(rows[4])
	if !strings.Contains(last, "…") || !strings.Contains(last, "applications/step") {
		t.Errorf("the long path reads %q; want its end kept", last)
	}
	wide := stripSGR(v.listColumn(150, 8)[0])
	for _, col := range []string{"HOST", "PATH", "SIZE", "TIME"} {
		if !strings.Contains(wide, col) {
			t.Errorf("a 150-cell list has no %s column: %q", col, wide)
		}
	}
}

// A rule's switch is a click target on each of its rows: the name, the pattern and the padding.
func TestRuleSwitchToggles(t *testing.T) {
	v := testNetViewer(t)
	v.openOverridesList()
	box, top, left := v.overridesBox(40, 150)
	if len(box) == 0 {
		t.Fatal("no overrides popup drawn")
	}
	var rows []int
	for row := range v.ovui.ruleRows {
		rows = append(rows, row)
	}
	if len(rows) != 3 {
		t.Fatalf("the rule is drawn on %d rows", len(rows))
	}
	for _, row := range rows {
		found := false
		for _, h := range v.ovui.hits {
			if h.row == row && h.x1 == len([]rune(stripSGR(box[row])))-3 && h.x1-h.x0 >= toggleCells {
				found = true
			}
		}
		if !found {
			t.Errorf("box row %d has no switch target at its right: %q", row, stripSGR(box[row]))
		}
	}
	_, _ = top, left
}

// The banner for an app that came back without the agent: two rows when tall, the button on
// the second, and on the iOS Simulator the setting to relaunch without asking.
func TestAttachBanner(t *testing.T) {
	v := testNetViewer(t)
	v.device.Platform = "iosSimulator"
	v.state.AttachState = armingState{State: "detached", AppID: "com.example.app"}
	writes := make(chan bool, 1)
	saved := writeBoolDefault
	writeBoolDefault = func(key string, value bool) error {
		if key == autoReattachKey {
			writes <- value
		}
		return nil
	}
	t.Cleanup(func() { writeBoolDefault = saved })

	rows := v.banner(150, true, 4)
	if len(rows) != 2 || !strings.Contains(stripSGR(rows[1]), "Relaunch & re-attach") ||
		!strings.Contains(stripSGR(rows[1]), "Re-attach to iOS Simulator apps automatically") {
		t.Fatalf("tall banner: %q", rows)
	}
	for _, row := range rows {
		if got := plainWidth(row); got != 150 {
			t.Errorf("banner row is %d cells in 150", got)
		}
	}
	if len(v.bannerHits) != 2 {
		t.Fatalf("%d banner buttons, want 2", len(v.bannerHits))
	}
	sw := v.bannerHits[1]
	if !v.bannerHit(mouseEvent{x: sw.x0, y: sw.row}) || !v.autoReattach || !<-writes {
		t.Error("a click on the switch didn't turn the setting on")
	}

	// Short panes keep it to one row with the button at the right; Android has no such setting.
	v.bannerHits = nil
	v.device.Platform = "android"
	short := v.banner(120, false, 4)
	if len(short) != 1 || plainWidth(short[0]) != 120 || !strings.HasSuffix(strings.TrimRight(stripSGR(short[0]), " "), "Relaunch & re-attach") {
		t.Errorf("short banner: %q", short)
	}
	v.bannerHits = nil
	if rows := v.banner(150, true, 4); strings.Contains(stripSGR(rows[1]), "automatically") {
		t.Errorf("the Android banner offers the simulator setting: %q", rows[1])
	}
}

// The network toolbar keeps its filter at 80 columns, at either height: the chips lose their
// padding, then Export HAR goes, before the filter is left out. A long app name is clipped, and
// no row is wider than the pane.
func TestNetToolbarKeepsTheFilter(t *testing.T) {
	v := testNetViewer(t)
	for _, height := range []int{1, 3} {
		got := stripSGR(strings.Join(v.toolbar(80, height), "\n"))
		if !strings.Contains(got, "Filter by URL") {
			t.Errorf("a %d-row toolbar at 80 columns has no filter: %q", height, got)
		}
	}
	v.state.TargetPackage = "com.example." + strings.Repeat("myapplication", 4)
	if got := stripSGR(strings.Join(v.toolbar(120, 1), "\n")); !strings.Contains(got, "…") {
		t.Errorf("a long app name isn't clipped: %q", got)
	}
	for cols := 1; cols <= 200; cols++ {
		for _, height := range []int{1, 3} {
			for _, row := range v.toolbar(cols, height) {
				if w := cellWidth(stripSGR(row)); w > cols {
					t.Fatalf("a %d-row toolbar is %d cells in a %d-cell pane", height, w, cols)
				}
			}
		}
	}
}

// A detail too narrow for every tab keeps the active tab in view, with a click target on it.
func TestNetDetailKeepsTheActiveTab(t *testing.T) {
	v := testNetViewer(t)
	setTermSize(t, 30, 80)
	v.handleKey([]byte{'\r'})
	v.draw()
	w := 80 - v.listW - 1
	for _, tab := range []int{tabResponse, tabTiming, tabOverview} {
		v.setTab(tab)
		row := stripSGR(v.detailColumn(w, 20)[1])
		name := netTabs[tab]
		at := strings.Index(row, name+" ")
		if at < 1 {
			t.Errorf("at 80 columns the active tab %s isn't drawn: %q", name, row)
			continue
		}
		// The detail starts past the list and its divider; the target starts on the tab's leading space.
		x0 := v.listW + 2 + len([]rune(row[:at])) - 1
		found := false
		for _, h := range v.tabHits {
			if h.x0 == x0 && h.x1 == x0+len(name)+1 {
				found = true
			}
		}
		if !found {
			t.Errorf("at 80 columns no click target is on the %s tab (x %d): %+v", name, x0, v.tabHits)
		}
	}
}
