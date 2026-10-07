package main

import (
	"fmt"
	"strings"
)

// The detail panel beside the list (the app's CloudLogDetailPanel): every field of the open
// entry, each a row the keys can select, with the app's context-menu actions in a menu.

// cloudDetailRow is one selectable row of the panel: its lines in the content, its menu, and
// what Enter or a click does in place of the menu (a disclosure opens or closes).
type cloudDetailRow struct {
	first, count int
	menu         func() []menuItem
	press        func()
}

// cloudDetailLines caps how many lines of one value the panel lays out.
const cloudDetailLines = 400

// openDetail shows an entry of the list in the panel and gives the panel the keys.
func (v *cloudViewer) openDetail(seq uint64) {
	i, ok := v.indexOf(seq)
	if !ok {
		return
	}
	e := v.listed()[i]
	if !v.detailOn || v.detail.Seq != e.Seq {
		// The message folds again for each entry, as in the app.
		v.showMessage, v.detailRow, v.detailTop = false, 0, 0
	}
	v.detail, v.detailOn, v.detailKeep, v.focus = e, true, true, cloudDetail
}

func (v *cloudViewer) closeDetail() {
	v.detailOn, v.detail = false, cloudEntry{}
	if v.focus == cloudDetail {
		v.focus = cloudList
	}
}

// openDetailAtCursor is Enter on the list: the selection's entry, or with nothing selected the
// newest one on screen.
func (v *cloudViewer) openDetailAtCursor() {
	list := v.listed()
	if len(list) == 0 {
		return
	}
	seq := v.sel.cursor
	_, found := v.indexOf(seq)
	if !v.sel.on || v.sel.all || !found {
		newest, ok := v.newestShown()
		if !ok {
			return
		}
		seq = newest
		v.selectEntries(selection{on: true, anchor: seq, cursor: seq})
	}
	v.openDetail(seq)
}

// stepDetail moves the panel to the entry before or after the open one.
func (v *cloudViewer) stepDetail(by int) {
	list := v.listed()
	if len(list) == 0 {
		return
	}
	i, ok := v.indexOf(v.detail.Seq)
	if !ok && by > 0 {
		i-- // the open entry left the list: i is the one after it
	}
	i = clampIndex(i+by, len(list))
	seq := list[i].Seq
	v.selectEntries(selection{on: true, anchor: seq, cursor: seq})
	v.reveal(i)
	v.openDetail(seq)
}

// detailKey handles the panel's own keys and reports whether it took the key: j/k and the
// arrows move between rows, Enter opens a row's menu, [ and ] (or left and right) step to the
// neighboring entries, Tab gives the keys to the list, Esc closes the panel.
func (v *cloudViewer) detailKey(k []byte) bool {
	if !v.detailOn {
		v.focus = cloudList
		return false
	}
	move := func(by int) {
		v.detailRow, v.detailKeep = clampIndex(v.detailRow+by, len(v.detailRows)), true
	}
	nav, isNav := decodeNav(k)
	switch {
	case isEsc(k):
		v.closeDetail()
		v.clearSelection()
	case len(k) == 1 && k[0] == '\t':
		v.focus = cloudList
	case isUp(k):
		move(-1)
	case isDown(k):
		move(1)
	case isPageUp(k):
		move(-max(1, v.detailRoom/2))
	case isPageDown(k) && !isShiftPageDown(k):
		move(max(1, v.detailRoom/2))
	case isEnter(k):
		v.detailActivate()
	case len(k) == 1 && k[0] == '[', isNav && !nav.shift && nav.dir == "left":
		v.stepDetail(-1)
	case len(k) == 1 && k[0] == ']', isNav && !nav.shift && nav.dir == "right":
		v.stepDetail(1)
	default:
		return false
	}
	return true
}

// detailActivate is Enter on the selected row: its disclosure, else its menu under the row.
func (v *cloudViewer) detailActivate() {
	if v.detailRow < 0 || v.detailRow >= len(v.detailRows) {
		return
	}
	row := v.detailRows[v.detailRow]
	switch {
	case row.press != nil:
		row.press()
	case row.menu != nil:
		v.menu = &popupMenu{x: v.detailX0 + 3, y: v.detailFirst + row.first - v.detailTop + 1, items: row.menu()}
	}
}

// detailClick is a press on the panel: its header's buttons, a row's own button, or a row,
// which a left click selects (and opens, for a disclosure) and a right click opens the menu of.
func (v *cloudViewer) detailClick(m mouseEvent, right bool) {
	v.focus = cloudDetail
	if m.y < v.detailFirst {
		switch {
		case right:
		case v.closeX0 > 0 && m.x >= v.closeX0 && m.x <= v.closeX1:
			v.closeDetail()
			v.clearSelection()
		case v.copyX0 > 0 && m.x >= v.copyX0 && m.x <= v.copyX1:
			v.copy(v.detail.Raw)
		}
		return
	}
	line, col := v.detailTop+m.y-v.detailFirst, m.x-v.detailX0-1
	if !right {
		for i := len(v.detailHits) - 1; i >= 0; i-- {
			if h := v.detailHits[i]; h.row == line && col >= h.x0 && col <= h.x1 {
				h.act(col)
				return
			}
		}
	}
	for i, row := range v.detailRows {
		if line < row.first || line >= row.first+row.count {
			continue
		}
		v.detailRow, v.detailKeep = i, false
		switch {
		case right && row.menu != nil:
			v.menu = &popupMenu{x: m.x, y: m.y, items: row.menu()}
		case !right && row.press != nil:
			row.press()
		}
		return
	}
}

// filterByLabel is the app's "Filter by this value": any condition on the label gives way to
// one exact match, and the session restarts.
func (v *cloudViewer) filterByLabel(scope, key, value string) {
	v.cfg.Query = v.cfg.Query.withLabelFilter(scope, key, value)
	v.apply()
}

// orLabel is the app's "Add this value (OR)".
func (v *cloudViewer) orLabel(scope, key, value string) {
	v.cfg.Query = v.cfg.Query.orLabelFilter(scope, key, value)
	v.apply()
}

func (v *cloudViewer) filterBySeverity(sev int) {
	v.cfg.Query = v.cfg.Query.withSeverityFilter(sev)
	v.apply()
}

// labelMenu is a label row's context menu (the app's LabelRow.contextMenu).
func (v *cloudViewer) labelMenu(scope, key, value string) []menuItem {
	favorite := fmt.Sprintf("Favorite “%s” (pin to top)", key)
	if v.isFavorite(key) {
		favorite = fmt.Sprintf("Unfavorite “%s”", key)
	}
	return []menuItem{
		{"Copy value", func() { v.copy(value) }},
		{sanitize(fmt.Sprintf("Copy “%s=%s”", key, value)), func() { v.copy(key + "=" + value) }},
		{},
		{"Filter by this value", func() { v.filterByLabel(scope, key, value) }},
		{"Add this value (OR)", func() { v.orLabel(scope, key, value) }},
		{"Open in new session", func() { v.openFork(forkAddingLabel(v.cfg, scope, key, value)) }},
		{},
		{sanitize(favorite), func() { v.toggleFavorite(key) }},
	}
}

// severityMenu is the severity's context menu.
func (v *cloudViewer) severityMenu(sev int) []menuItem {
	name := severityTitle(sev)
	return []menuItem{
		{"Copy", func() { v.copy(severityName(sev)) }},
		{"Filter by " + name, func() { v.filterBySeverity(sev) }},
		{"Open in new session (" + name + ")", func() { v.openFork(forkAddingSeverity(v.cfg, sev)) }},
	}
}

// detailContent lays the open entry out w cells wide: its lines, the selectable rows among
// them, and the clickable spans (by line and 0-based column). The sections are the app's, in
// its order; one whose value is missing is left out.
func (v *cloudViewer) detailContent(w int) (lines []string, rows []cloudDetailRow, hits []boxHit) {
	e := v.detail
	textW := max(1, w-2) // after the two cells of a row's marker
	marker := func() string {
		return rowMarker(v.focus == cloudDetail && len(rows) == v.detailRow)
	}
	// A section's title stands out from its values: bold in the theme's accent, with a rule
	// to the panel's edge.
	titled := func(title string) string { return sectionStyle + clip(title, textW) + sgrReset }
	rule := func(n int) string {
		if n < 2 {
			return strings.Repeat(" ", max(0, n))
		}
		return " " + sgrDim + strings.Repeat("─", n-1) + sgrReset
	}
	heading := func(title string) {
		line := "  " + titled(title)
		lines = append(lines, line+rule(w-cellWidth(stripSGR(line))))
	}
	row := func(text []string, style string, menu func() []menuItem, press func()) {
		lead := marker()
		rows = append(rows, cloudDetailRow{first: len(lines), count: len(text), menu: menu, press: press})
		for _, t := range text {
			lines = append(lines, lead+style+t+sgrReset)
		}
	}
	gap := func() { lines = append(lines, "") }
	wrapped := func(text string) []string {
		out := wrapCells(text, textW)
		if len(out) > cloudDetailLines {
			out = out[:cloudDetailLines]
		}
		if len(out) == 0 {
			out = []string{""}
		}
		return out
	}
	copyMenu := func(label, text string) func() []menuItem {
		return func() []menuItem { return []menuItem{{label, func() { v.copy(text) }}} }
	}
	field := func(title, value string) {
		heading(title)
		row(wrapped(value), "", copyMenu("Copy", value), nil)
		gap()
	}
	// disclosure is a section's heading that opens and closes it, with a button at its right.
	disclosure := func(title string, open bool, toggle func(), action, text string) {
		chevron := "▸"
		if open {
			chevron = "▾"
		}
		line := marker() + titled(chevron+" "+title)
		if room := w - cellWidth(stripSGR(line)) - cellWidth(action); room >= 2 {
			hits = append(hits, boxHit{row: len(lines), x0: w - cellWidth(action), x1: w - 1, act: func(int) { v.copy(text) }})
			line += rule(room-1) + " " + sgrUnder + action + sgrReset
		} else {
			line += rule(w - cellWidth(stripSGR(line)))
		}
		rows = append(rows, cloudDetailRow{first: len(lines), count: 1, menu: copyMenu(action, text), press: toggle})
		lines = append(lines, line)
	}

	// The message: one line until it is opened.
	disclosure("MESSAGE", v.showMessage, func() { v.showMessage = !v.showMessage }, "Copy message", e.Message)
	if v.showMessage {
		message := e.Message
		if message == "" {
			message = "—"
		}
		row(wrapped(message), "", copyMenu("Copy", e.Message), nil)
	} else {
		preview, _, _ := strings.Cut(e.Message, "\n")
		if preview == "" {
			preview = "—"
		}
		row([]string{clip(sanitize(preview), textW)}, sgrDim, copyMenu("Copy", e.Message), func() { v.showMessage = true })
	}
	gap()

	heading("SEVERITY")
	row([]string{cloudSeverityBadge(e.Severity) + " " + severityName(e.Severity) + " "}, "",
		func() []menuItem { return v.severityMenu(e.Severity) }, nil)
	gap()

	// The app's abbreviated date with the standard time, and the time alone for RECEIVED.
	field("TIME", e.time().Format("Jan 2, 2006 at 3:04:05 PM"))

	labels := func(title string, values map[string]string, scope string) {
		if len(values) == 0 {
			return
		}
		heading(title)
		keys := make([]string, 0, len(values))
		for key := range values {
			keys = append(keys, key)
		}
		keyW := max(4, min(24, textW/3))
		// Favorite keys are pinned to the top here too.
		for _, key := range orderedLabelKeys(keys, v.project.favoriteLabelKeys()) {
			value := values[key]
			shown := sanitize(value)
			if shown == "" {
				shown = "—"
			}
			star := "  "
			if v.isFavorite(key) {
				star = sgrYellow + "★" + sgrReset + " "
			}
			// A long value wraps under itself, so a UUID or a URL can be read whole.
			valueW := max(1, textW-keyW-3)
			var lines []string
			if most := cloudDetailLines * valueW; len(shown) > most {
				shown = string([]rune(shown)[:min(most, len([]rune(shown)))]) // no more than is laid out
			}
			for i, part := range wrapCells(shown, valueW) {
				if i == cloudDetailLines {
					break
				}
				head := strings.Repeat(" ", keyW+3)
				if i == 0 {
					head = star + sgrCyan + fit(sanitize(key), keyW) + sgrReset + " "
				}
				lines = append(lines, head+part)
			}
			row(lines, "", func() []menuItem { return v.labelMenu(scope, key, value) }, nil)
		}
		gap()
	}
	labels("LABELS", e.Labels, labelScopeEntry)
	labels("RESOURCE LABELS", e.ResourceLabels, labelScopeResource)

	if e.ReceiveTimestamp != nil {
		field("RECEIVED", epochTime(*e.ReceiveTimestamp).Format("3:04:05 PM"))
	}
	field("LOG", e.LogID)
	optional := func(title, value string) {
		if value != "" {
			field(title, value)
		}
	}
	optional("RESOURCE TYPE", e.ResourceType)
	optional("HTTP REQUEST", e.HTTPRequestSummary)
	optional("TRACE", e.Trace)
	optional("SPAN", e.SpanID)
	optional("INSERT ID", e.InsertID)

	disclosure("RAW JSON", v.showRaw, func() { v.showRaw = !v.showRaw }, "Copy JSON", e.Raw)
	if v.showRaw {
		row(wrapped(e.Raw), "", copyMenu("Copy JSON", e.Raw), nil)
	}
	return lines, rows, hits
}

// detailColumn draws the panel w cells wide and n rows tall: its header with Copy JSON and the
// close button, then the content, scrolled to keep the selected row in view.
func (v *cloudViewer) detailColumn(w, n int) []string {
	out := make([]string, 0, n)
	blank := strings.Repeat(" ", max(0, w))
	v.detailW, v.closeX0, v.closeX1, v.copyX0, v.copyX1 = w, 0, 0, 0, 0
	v.detailRows, v.detailHits = nil, nil
	v.detailFirst, v.detailRoom = v.listTop+1, max(0, n-1)
	if w < len(escClose)+4 || n < 2 {
		for len(out) < n {
			out = append(out, blank)
		}
		return out
	}
	const title, copyJSON = "DETAILS", "Copy JSON"
	titleW := w - len(escClose) - 2
	action := ""
	if titleW-len(copyJSON)-1 >= len(title)+1 {
		titleW -= len(copyJSON) + 1
		action = sgrUnder + copyJSON + sgrReset + " "
		v.copyX0 = v.detailX0 + 1 + titleW
		v.copyX1 = v.copyX0 + len(copyJSON) - 1
	}
	v.closeX0 = v.detailX0 + w - 1 - len(escClose)
	v.closeX1 = v.closeX0 + len(escClose) - 1
	out = append(out, " "+sgrDim+fit(title, titleW)+sgrReset+action+closeButton(escClose)+" ")

	lines, rows, hits := v.detailContent(w - 1)
	if at := clampIndex(v.detailRow, len(rows)); at != v.detailRow {
		v.detailRow = at
		lines, rows, hits = v.detailContent(w - 1)
	}
	room := n - 1
	if v.detailKeep && v.detailRow < len(rows) {
		r := rows[v.detailRow]
		if r.first <= v.detailTop {
			v.detailTop = max(0, r.first-1) // with its heading
		}
		if last := r.first + min(r.count, room); last > v.detailTop+room {
			v.detailTop = last - room
		}
	}
	v.detailTop = max(0, min(v.detailTop, len(lines)-room))
	for i := v.detailTop; i < len(lines) && len(out) < n; i++ {
		line := lines[i]
		if cellWidth(stripSGR(line)) > w-1 { // a line the panel is too narrow for, plain and cut
			line = clip(stripSGR(line), w-1)
		}
		out = append(out, " "+padStyled(line, w-1))
	}
	for len(out) < n {
		out = append(out, blank)
	}
	v.detailRows, v.detailHits = rows, hits
	return out
}
