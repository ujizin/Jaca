package main

import (
	"sort"
	"strings"
)

// logNameSheet is the app's LogNameSheet: a project's log names (from `gcloud logging logs
// list`, cached in jacad and refreshable), one of which is selected for every session of the
// project, and a field to add one by hand. The home and the session viewer both open it.
//
// The owner draws box over its frame, passes it keys and mouse presses while it is open, calls
// setProject when cloud.state changes, and closes it when done is called. It makes its own
// calls to jacad. flash, when the owner sets it, shows a message (a failed refresh).
type logNameSheet struct {
	sheetFrame
	calls   cloudCalls
	project cloudProject
	done    func()
	flash   func(string)

	manual, filter textInput
	focus          string // one of the lnStop values
	cursor         int    // the row of the filtered list under the cursor
	listTop        int    // the first list row drawn
	refreshing     bool
}

// The sheet's focus stops.
const (
	lnManual  = "manual"
	lnAdd     = "add"
	lnFilter  = "filter"
	lnList    = "list"
	lnRefresh = "refresh"
	lnDone    = "done"
)

// logNameFilterAbove is the number of names above which the sheet shows its filter field.
const logNameFilterAbove = 8

// newLogNameSheet opens the sheet for a project. done is called when it should close: Esc,
// Done, the close button, or a name was chosen or added. The names are listed from gcloud at
// once when none are cached.
func newLogNameSheet(p *pane, project cloudProject, done func()) *logNameSheet {
	s := newLogNameSheetWith(newCloudCalls(p), project, done)
	s.opened()
	return s
}

// newLogNameSheetWith is the sheet before it asks jacad for anything.
func newLogNameSheetWith(calls cloudCalls, project cloudProject, done func()) *logNameSheet {
	s := &logNameSheet{calls: calls, project: project, done: done, focus: lnManual}
	if len(project.LogNames) > 0 {
		s.focus = lnList
		for i, name := range project.LogNames {
			if name == project.SelectedLogName {
				s.cursor = i
			}
		}
	}
	return s
}

// opened is LogNameSheet's onAppear: an empty cache is refreshed.
func (s *logNameSheet) opened() {
	if len(s.project.LogNames) == 0 {
		s.refresh()
	}
}

// setProject takes the project's new state. The cursor stays on the name it was on.
func (s *logNameSheet) setProject(p cloudProject) {
	at := ""
	if names := s.filtered(); s.cursor >= 0 && s.cursor < len(names) {
		at = names[s.cursor]
	}
	s.project = p
	names := s.filtered()
	s.cursor = clampIndex(s.cursor, len(names))
	for i, name := range names {
		if name == at {
			s.cursor = i
			break
		}
	}
	s.keepFocus()
}

// showsFilter: the filter field shows only for a long list.
func (s *logNameSheet) showsFilter() bool { return len(s.project.LogNames) > logNameFilterAbove }

// filtered is LogNameSheet.filteredNames: the names whose short id contains the filter text,
// whatever its case.
func (s *logNameSheet) filtered() []string {
	names := s.project.LogNames
	q := strings.ToLower(s.filter.String())
	if q == "" || !s.showsFilter() {
		return names
	}
	var out []string
	for _, name := range names {
		if strings.Contains(strings.ToLower(cloudLogID(name)), q) {
			out = append(out, name)
		}
	}
	return out
}

// stops are the focus stops in Tab order. The filter and the list are stops only while shown.
func (s *logNameSheet) stops() []string {
	stops := []string{lnManual, lnAdd}
	if s.showsFilter() {
		stops = append(stops, lnFilter)
	}
	if len(s.filtered()) > 0 {
		stops = append(stops, lnList)
	}
	return append(stops, lnRefresh, lnDone)
}

// keepFocus moves the focus off a stop that is no longer shown.
func (s *logNameSheet) keepFocus() {
	for _, stop := range s.stops() {
		if stop == s.focus {
			return
		}
	}
	s.focus = lnManual
}

func (s *logNameSheet) moveStop(by int) {
	stops := s.stops()
	at := 0
	for i, stop := range stops {
		if stop == s.focus {
			at = i
		}
	}
	s.focus = stops[((at+by)%len(stops)+len(stops))%len(stops)]
}

// moveCursor moves over the list and puts the focus on it.
func (s *logNameSheet) moveCursor(by int) {
	names := s.filtered()
	if len(names) == 0 {
		return
	}
	s.cursor = clampIndex(s.cursor+by, len(names))
	s.focus = lnList
}

// report shows why a call failed, where the owner gave the sheet somewhere to show it.
func (s *logNameSheet) report(err error) {
	if err != nil && s.flash != nil {
		s.flash(err.Error())
	}
}

// refresh lists the project's log names with gcloud (LogNameSheet.refresh). The names arrive
// on cloud.state; the call returns an error message when gcloud failed.
func (s *logNameSheet) refresh() {
	if s.refreshing {
		return
	}
	s.refreshing = true
	id := s.project.ProjectID
	s.calls.spawn(func() func() {
		var message *string
		err := s.calls.call("cloud.refreshLogNames", map[string]any{"id": id}, &message, logNamesTimeout)
		return func() { s.refreshed(message, err) }
	})
}

func (s *logNameSheet) refreshed(message *string, err error) {
	s.refreshing = false
	switch {
	case err != nil:
		s.report(err)
	case message != nil && *message != "" && s.flash != nil:
		s.flash(*message)
	}
}

// choose selects a log name for every session of the project and closes the sheet.
func (s *logNameSheet) choose(name string) {
	id := s.project.ProjectID
	s.calls.spawn(func() func() {
		err := s.calls.call("cloud.setSelectedLogName", map[string]any{"id": id, "logName": name}, nil, callTimeout)
		return func() { s.report(err) }
	})
	s.done()
}

func (s *logNameSheet) chooseAtCursor() {
	if names := s.filtered(); s.cursor >= 0 && s.cursor < len(names) {
		s.choose(names[s.cursor])
	}
}

// addManual is LogNameSheet.addManual: the typed log id becomes a full log name, joins the
// cached names in order when it is new, is selected, and the sheet closes.
func (s *logNameSheet) addManual() {
	raw := strings.TrimSpace(s.manual.String())
	if raw == "" {
		return
	}
	id := s.project.ProjectID
	full := cloudLogNameFull(id, raw)
	names := append([]string(nil), s.project.LogNames...)
	isNew := true
	for _, name := range names {
		if name == full {
			isNew = false
		}
	}
	if isNew {
		names = append(names, full)
		sort.Strings(names)
	}
	s.calls.spawn(func() func() {
		var first error
		if isNew {
			first = s.calls.call("cloud.setLogNames", map[string]any{"id": id, "names": names}, nil, callTimeout)
		}
		err := s.calls.call("cloud.setSelectedLogName", map[string]any{"id": id, "logName": full}, nil, callTimeout)
		if first != nil {
			err = first
		}
		return func() { s.report(err) }
	})
	s.manual.set("")
	s.done()
}

func (s *logNameSheet) key(k []byte) {
	switch {
	case isEsc(k):
		s.done()
	case s.hidden:
	case string(k) == "\t":
		s.moveStop(1)
	case string(k) == "\x1b[Z":
		s.moveStop(-1)
	case isArrowUp(k):
		s.moveCursor(-1)
	case isArrowDown(k):
		s.moveCursor(1)
	case isPageUp(k):
		s.moveCursor(-10)
	case isPageDown(k):
		s.moveCursor(10)
	case isEnter(k):
		switch s.focus {
		case lnManual, lnAdd:
			s.addManual()
		case lnFilter, lnList:
			s.chooseAtCursor()
		case lnRefresh:
			s.refresh()
		case lnDone:
			s.done()
		}
	case s.focus == lnManual:
		s.manual.handle(k)
	case s.focus == lnFilter:
		if s.filter.handle(k) {
			s.cursor, s.listTop = 0, 0
		}
	case s.focus == lnList && len(k) == 1 && k[0] == 'j':
		s.moveCursor(1)
	case s.focus == lnList && len(k) == 1 && k[0] == 'k':
		s.moveCursor(-1)
	}
}

func (s *logNameSheet) mouse(m mouseEvent) {
	if s.hidden || !m.press {
		return
	}
	switch m.button {
	case 64:
		s.moveCursor(-1)
	case 65:
		s.moveCursor(1)
	default:
		s.click(m)
	}
}

// box draws the sheet for a pane rows by cols and says where to put it (0-based top and left).
// It is empty when the pane is too small to hold the sheet.
func (s *logNameSheet) box(rows, cols int) (box []string, top, left int) {
	return s.place(s.build(rows, cols), rows, cols)
}

func (s *logNameSheet) build(rows, cols int) *popupBox {
	w := min(cols, 76)
	if w < 40 || rows < 14 {
		return nil
	}
	s.keepFocus()
	b := newPopupBox("Log names", w)
	b.closeRow(s.done)
	sheetText(b, "Selecting a log name applies it to every session for "+s.project.title()+".", sgrDim, 2)
	b.add("")

	add := homeButton{label: "Add", kind: buttonPrimary, focused: s.focus == lnAdd}
	if strings.TrimSpace(s.manual.String()) != "" {
		add.act = s.addManual
	}
	fieldW := b.inner - add.width() - 1
	b.hit(0, fieldW-1, func(int) { s.focus = lnManual })
	row := renderField(&s.manual, "Add a log name manually (e.g. stdout)…", s.focus == lnManual, fieldW, sgrUnder) + " "
	sheetButton(b, &row, add)
	b.add(row)
	b.add("")

	if s.showsFilter() {
		sheetField(b, &s.filter, "Filter…", s.focus == lnFilter, func() { s.focus = lnFilter })
	}
	// The list takes the rows left above the blank row, the buttons and the border.
	room := max(1, rows-len(b.rows)-3)
	names := s.filtered()
	if len(names) == 0 {
		text := "No log names found yet — Refresh, or add one manually above."
		if s.refreshing {
			text = "Loading log names…"
		}
		sheetText(b, text, sgrDim, room)
	} else {
		room = min(room, len(names), 16)
		s.cursor = clampIndex(s.cursor, len(names))
		switch {
		case s.cursor < s.listTop:
			s.listTop = s.cursor
		case s.cursor >= s.listTop+room:
			s.listTop = s.cursor - room + 1
		}
		s.listTop = max(0, min(s.listTop, len(names)-room))
		for i := s.listTop; i < s.listTop+room; i++ {
			name := names[i]
			mark, markStyle := "○", sgrDim
			if name == s.project.SelectedLogName {
				mark, markStyle = "●", sgrGreen
			}
			id := clip(sanitize(cloudLogID(name)), b.inner-2)
			n := i
			b.hit(0, b.inner-1, func(int) { s.cursor = n; s.choose(name) })
			switch {
			case i == s.cursor && s.focus == lnList:
				b.add(sgrRev + sgrBold + fit(mark+" "+id, b.inner) + sgrReset)
			case i == s.cursor:
				b.add(markStyle + mark + sgrReset + " " + sgrBold + id + sgrReset)
			default:
				b.add(markStyle + mark + sgrReset + " " + id)
			}
		}
	}
	b.add("")
	refresh := homeButton{label: "Refresh", focused: s.focus == lnRefresh}
	if !s.refreshing {
		refresh.act = s.refresh
	}
	sheetButtons(b, []homeButton{refresh, {label: "Done", act: s.done, focused: s.focus == lnDone}})
	return b
}
