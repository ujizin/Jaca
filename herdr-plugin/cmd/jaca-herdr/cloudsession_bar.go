package main

import "strings"

// The query builder bar under the toolbar (the app's CloudQueryBar). It edits the session's
// query in place: text conditions, a minimum severity and label conditions, or the raw filter
// of a session opened from a Logs Explorer URL. Nothing reaches gcloud until Apply.

// cloudStop is one focus stop of the bar: what it is, and for a condition's controls which one.
type cloudStop struct {
	kind string
	i    int
}

// The glyphs of the bar's icon buttons (the app's plus.circle.fill, minus.circle and
// chevron.down).
const (
	cloudGlyphAdd     = "⊕"
	cloudGlyphRemove  = "⊖"
	cloudGlyphChevron = "▾"
)

// cloudModeCells is the width of a match mode's label, the widest being "doesn't contain".
const cloudModeCells = 15

// toggleBar shows the bar and gives it the keys, or hides it.
func (v *cloudViewer) toggleBar() {
	if v.showBar {
		v.showBar = false
		if v.focus == cloudBar {
			v.focus = cloudList
		}
		return
	}
	v.showBar, v.focus = true, cloudBar
	v.barStop = v.stops()[0]
}

// labelsDetected is whether the project has label keys for the selected log name. Without any
// the app shows a hint in place of the label conditions.
func (v *cloudViewer) labelsDetected() bool { return len(v.project.labelKeys()) > 0 }

// stops is the bar's focus stops in drawing order.
func (v *cloudViewer) stops() []cloudStop {
	var out []cloudStop
	add := func(kind string, i int) { out = append(out, cloudStop{kind, i}) }
	if v.rawMode {
		add("builder", 0)
		add("raw", 0)
	} else {
		q := v.cfg.Query
		for i := range q.TextConditions {
			add("tmode", i)
			add("tvalue", i)
			add("tdel", i)
			if i < len(q.TextConditions)-1 {
				add("tcomb", i)
			}
		}
		add("tadd", 0)
		for i := 0; i <= len(cloudCommonSeverities); i++ {
			add("sev", i)
		}
		if v.labelsDetected() {
			add("ladd", 0)
			if len(q.LabelConditions) > 1 {
				add("lcomb", 0)
			}
			for i := range q.LabelConditions {
				add("lkey", i)
				add("lmode", i)
				add("lvalue", i)
				add("ldel", i)
			}
		}
	}
	add("apply", 0)
	add("reset", 0)
	add("templates", 0)
	add("query", 0)
	return out
}

// input is the text field of a condition's value, kept by the condition's id. It takes the
// value again when the query was replaced under it (a template, Reset, a detail action).
func (v *cloudViewer) input(id, value string) *textInput {
	in := v.inputs[id]
	if in == nil {
		in = &textInput{}
		in.set(value)
		v.inputs[id] = in
	} else if in.String() != value {
		in.set(value)
	}
	return in
}

// setRaw switches between the raw filter and the builder, and sets the raw filter's text.
func (v *cloudViewer) setRaw(on bool, text string) {
	v.rawMode, v.cfg.RawFilter = on, text
	if v.raw.String() != text {
		v.raw.set(text)
	}
}

// nextMatchMode is the mode after this one, in the app's menu order.
func nextMatchMode(mode string) string {
	for i, m := range cloudMatchModes {
		if m == mode {
			return cloudMatchModes[(i+1)%len(cloudMatchModes)]
		}
	}
	return cloudMatchModes[0]
}

// barActivate is Enter or a click on the focused stop.
func (v *cloudViewer) barActivate() {
	q := &v.cfg.Query
	s := v.barStop
	switch s.kind {
	case "tmode":
		if s.i < len(q.TextConditions) {
			c := &q.TextConditions[s.i]
			c.Mode = nextMatchMode(orDefault(c.Mode, cloudMatchContains))
		}
	case "tdel":
		if s.i < len(q.TextConditions) {
			kept := append([]textCondition(nil), q.TextConditions[:s.i]...)
			q.TextConditions = append(kept, q.TextConditions[s.i+1:]...)
			v.barStop = cloudStop{"tadd", 0}
		}
	case "tcomb":
		q.TextCombineOr = !q.TextCombineOr
	case "tadd":
		q.TextConditions = append(append([]textCondition(nil), q.TextConditions...), newTextCondition())
		v.barStop = cloudStop{"tvalue", len(q.TextConditions) - 1}
	case "sev":
		// Unlike the toolbar's chips this waits for Apply.
		q.SeveritySet, q.MinSeverity = nil, nil
		if s.i >= 1 && s.i <= len(cloudCommonSeverities) {
			sev := cloudCommonSeverities[s.i-1]
			q.MinSeverity = &sev
		}
	case "ladd":
		keys := v.project.labelKeys()
		if len(keys) == 0 {
			return
		}
		c := newLabelCondition()
		c.Key = keys[0]
		q.LabelConditions = append(append([]labelCondition(nil), q.LabelConditions...), c)
		v.barStop = cloudStop{"lvalue", len(q.LabelConditions) - 1}
	case "lcomb":
		q.LabelCombineOr = !q.LabelCombineOr
	case "lkey":
		if s.i < len(q.LabelConditions) {
			v.openKeyPicker(q.LabelConditions[s.i].ID)
		}
	case "lmode":
		if s.i < len(q.LabelConditions) {
			c := &q.LabelConditions[s.i]
			c.Mode = nextMatchMode(orDefault(c.Mode, cloudMatchExact))
		}
	case "ldel":
		if s.i < len(q.LabelConditions) {
			kept := append([]labelCondition(nil), q.LabelConditions[:s.i]...)
			q.LabelConditions = append(kept, q.LabelConditions[s.i+1:]...)
			v.barStop = cloudStop{"ladd", 0}
		}
	case "apply":
		v.apply()
	case "reset":
		v.cfg.Query = newCloudQuery()
		v.setRaw(false, "")
		v.apply()
	case "builder":
		v.setRaw(false, "")
		v.barStop = cloudStop{"apply", 0}
		v.apply()
	case "templates":
		v.openTemplatesMenu(v.anchor("templates"))
	case "query":
		v.showQuery = !v.showQuery
	}
}

// stopInput is the text field under the focused stop and where its text goes, nil for a stop
// that isn't a value field.
func (v *cloudViewer) stopInput() (in *textInput, value *string) {
	q := &v.cfg.Query
	s := v.barStop
	switch {
	case s.kind == "tvalue" && s.i < len(q.TextConditions):
		c := &q.TextConditions[s.i]
		return v.input(c.ID, c.Value), &c.Value
	case s.kind == "lvalue" && s.i < len(q.LabelConditions):
		c := &q.LabelConditions[s.i]
		return v.input(c.ID, c.Value), &c.Value
	}
	return nil, nil
}

// barKey: Tab, Shift-Tab and the arrows move between the stops, Enter or space presses one, and
// in a field other keys edit it. Esc gives the keys back to the list.
func (v *cloudViewer) barKey(k []byte) {
	stops := v.stops()
	at := 0
	for i, s := range stops {
		if s == v.barStop {
			at = i
		}
	}
	v.barStop = stops[at]
	move := func(by int) { v.barStop = stops[(at+by+len(stops))%len(stops)] }
	kind := v.barStop.kind
	nav, isNav := decodeNav(k)
	switch {
	case isEsc(k):
		v.focus = cloudList
	case string(k) == "\x1b[Z": // Shift-Tab
		move(-1)
	case len(k) == 1 && k[0] == '\t':
		move(1)
	case kind == "raw":
		// The arrows move inside the filter's text, so only Tab leaves it.
		v.raw.handle(k, 5)
		v.cfg.RawFilter = v.raw.String()
	case isArrowUp(k):
		move(-1)
	case isArrowDown(k):
		move(1)
	case kind == "tvalue" || kind == "lvalue":
		if isEnter(k) {
			// Stays in the field: the next stop is the condition's remove control, and a second
			// Enter would delete what was just typed.
			return
		}
		if in, value := v.stopInput(); in != nil && in.handle(k) {
			*value = in.String()
		}
	case isEnter(k) || (len(k) == 1 && k[0] == ' '):
		v.barActivate()
	case isNav && nav.dir == "left":
		move(-1)
	case isNav && nav.dir == "right":
		move(1)
	}
}

// barClick is a click on the bar at a screen cell.
func (v *cloudViewer) barClick(x, y int) {
	row, col := y-v.barTop, x-1
	v.focus = cloudBar
	if v.rawFirst >= 0 && row >= v.rawFirst && row < v.rawFirst+v.rawHeight && col >= 1 {
		v.barStop = cloudStop{"raw", 0}
		v.raw.click(row-v.rawFirst, col-1)
		return
	}
	for i := len(v.barHits) - 1; i >= 0; i-- {
		if h := v.barHits[i]; h.row == row && col >= h.x0 && col <= h.x1 {
			h.act(col)
			return
		}
	}
}

// barLines draws the bar w cells wide in at most budget rows. When it is taller than that the
// rows around the focused stop are the ones shown.
func (v *cloudViewer) barLines(w, budget int) []string {
	c := &formColumn{w: w}
	focusLine, templatesLine, templatesX := 0, 0, 0
	v.rawFirst = -1
	// on reports whether a stop has the focus, noting the line it is on.
	on := func(kind string, i int) bool {
		focused := v.focus == cloudBar && v.barStop == cloudStop{kind, i}
		if focused {
			focusLine = len(c.lines)
		}
		return focused
	}
	// button appends a stop's label to a row: style, or focusStyle while it has the focus.
	button := func(row *string, label, style, focusStyle, kind string, i int) {
		if on(kind, i) {
			style = focusStyle
		}
		c.button(row, label, style, func() {
			v.focus, v.barStop = cloudBar, cloudStop{kind, i}
			v.barActivate()
		})
	}
	field := func(row *string, t *textInput, placeholder string, fw int, kind string, i int) {
		x := cellWidth(stripSGR(*row))
		c.hit(1, x, x+fw-1, func(int) { v.focus, v.barStop = cloudBar, cloudStop{kind, i} })
		*row += renderField(t, placeholder, on(kind, i), fw, sgrUnder)
	}
	header := func(title string) string { return " " + sgrDim + title + sgrReset }
	// trailing puts a button at the right end of a row.
	trailing := func(row *string, label, style, focusStyle, kind string, i int) {
		gap := max(2, w-cellWidth(stripSGR(*row))-cellWidth(label)-1)
		*row += strings.Repeat(" ", gap)
		button(row, label, style, focusStyle, kind, i)
	}
	const (
		plain   = sgrUnder
		focused = sgrBold + sgrRev
		chipOn  = sgrRev
		chipAt  = sgrRev + sgrBold + sgrUnder
	)
	combine := func(or bool) string {
		if or {
			return " OR "
		}
		return " AND "
	}
	// The value field takes what the row's other controls leave, up to the app's width.
	valueWidth := func(used int) int { return max(8, min(40, w-used-3)) }

	q := v.cfg.Query
	if v.rawMode {
		row := header("RAW FILTER (from URL)")
		trailing(&row, "Use builder instead", plain, focused, "builder", 0)
		c.add(row)
		height := max(2, min(6, len(v.raw.lines)))
		active := on("raw", 0)
		v.rawFirst, v.rawHeight = len(c.lines), height
		view := v.raw.view(w-2, height, active)
		if len(view) > 0 && v.raw.String() == "" && !active {
			view[0] = sgrDim + fit("Cloud Logging filter…", w-2) + sgrReset
		}
		for _, line := range view {
			c.add(" " + line)
		}
	} else {
		c.add(header("TEXT PAYLOAD"))
		for i, cond := range q.TextConditions {
			row := " "
			button(&row, fit(matchModeLabel(orDefault(cond.Mode, cloudMatchContains)), cloudModeCells), plain, focused, "tmode", i)
			row += " "
			field(&row, v.input(cond.ID, cond.Value), "text…", valueWidth(cellWidth(stripSGR(row))), "tvalue", i)
			row += " "
			button(&row, cloudGlyphRemove, sgrDim, focused, "tdel", i)
			c.add(row)
			// The combiner sits between conditions, as in the app.
			if i < len(q.TextConditions)-1 {
				row := "  "
				button(&row, combine(q.TextCombineOr), chipOn, chipAt, "tcomb", i)
				hint := "match all of these"
				if q.TextCombineOr {
					hint = "match any of these"
				}
				c.add(row + " " + sgrDim + hint + sgrReset)
			}
		}
		add := "Add a text filter"
		switch {
		case len(q.TextConditions) == 0:
		case q.TextCombineOr:
			add = "Add another (OR)"
		default:
			add = "Add another (AND)"
		}
		row := " "
		button(&row, cloudGlyphAdd+" "+add, sgrGreen, focused, "tadd", 0)
		c.add(row)

		c.add(header("MIN SEVERITY"))
		row = " "
		for i := 0; i <= len(cloudCommonSeverities); i++ {
			label, selected := "Any", q.MinSeverity == nil && len(q.SeveritySet) == 0
			if i > 0 {
				sev := cloudCommonSeverities[i-1]
				label = severityTitle(sev)
				selected = len(q.SeveritySet) == 0 && q.MinSeverity != nil && *q.MinSeverity == sev
			}
			style, at := "", sgrBold+sgrUnder
			if selected {
				style, at = chipOn, chipAt
			}
			button(&row, " "+label+" ", style, at, "sev", i)
			row += " "
		}
		c.add(row)

		row = header("LABELS") + " "
		if v.labelsDetected() {
			button(&row, cloudGlyphAdd, sgrGreen, focused, "ladd", 0)
			if len(q.LabelConditions) > 1 {
				trailing(&row, combine(q.LabelCombineOr), chipOn, chipAt, "lcomb", 0)
			}
			c.add(row)
			keyW := max(6, min(24, w/4))
			for i, cond := range q.LabelConditions {
				row := " "
				key, keyStyle := sanitize(cond.Key), plain
				if key == "" {
					key, keyStyle = "key…", sgrDim+sgrUnder
				}
				button(&row, fit(key, keyW)+" "+cloudGlyphChevron, keyStyle, focused, "lkey", i)
				row += " "
				button(&row, fit(matchModeLabel(orDefault(cond.Mode, cloudMatchExact)), cloudModeCells), plain, focused, "lmode", i)
				row += " "
				field(&row, v.input(cond.ID, cond.Value), "value…", valueWidth(cellWidth(stripSGR(row))), "lvalue", i)
				row += " "
				button(&row, cloudGlyphRemove, sgrDim, focused, "ldel", i)
				c.add(row)
			}
		} else {
			c.add(row + sgrDim + cloudGlyphAdd + sgrReset)
			const hint = "No labels detected yet — run a session once so Jaca can auto-detect this log's label keys."
			for _, part := range wrapWords(hint, w-2) {
				c.add(" " + sgrDim + part + sgrReset)
			}
		}
	}

	// The footer: Apply, Reset, Templates, and the query at the right.
	apply := " Apply "
	if v.active() {
		apply = " Apply (restart) "
	}
	row := " "
	button(&row, apply, sgrGreen+sgrUnder, badgeStyle(colorGreen), "apply", 0)
	row += "  "
	button(&row, "Reset", plain, focused, "reset", 0)
	row += "  "
	templatesLine, templatesX = len(c.lines), cellWidth(stripSGR(row))
	button(&row, "Templates "+cloudGlyphChevron, plain, focused, "templates", 0)
	show := "Show query"
	if v.showQuery {
		show = "Hide query"
	}
	trailing(&row, show, plain, focused, "query", 0)
	c.add(row)
	if v.showQuery {
		// What the app shows: the filter the first query would run now, time clause included.
		filter := buildCloudFilter(v.cfg, v.now())
		if filter == "" {
			filter = "(no filter)"
		}
		for _, part := range wrapCells(filter, w-2) {
			c.add(" " + sgrDim + part + sgrReset)
		}
	}

	lines, start := c.lines, 0
	if len(lines) > budget {
		if focusLine >= budget {
			start = focusLine - budget + 1
		}
		start = max(0, min(start, len(lines)-budget))
		lines = lines[start : start+budget]
	}
	v.barHits = v.barHits[:0]
	for _, h := range c.hits {
		if h.row -= start; h.row >= 0 && h.row < len(lines) {
			v.barHits = append(v.barHits, h)
		}
	}
	if v.rawFirst >= 0 {
		v.rawFirst -= start
	}
	v.anchors["templates"] = [2]int{templatesX + 1, v.barTop + templatesLine - start + 1}
	return lines
}
