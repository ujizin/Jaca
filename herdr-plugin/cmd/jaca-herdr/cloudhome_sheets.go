package main

import "strings"

// The home's sheets: Add project (AddProjectSheet), New session from URL
// (NewSessionFromURLSheet) and Rename (CloudProjectCard's alert). The log-name sheet is in
// cloudhome_lognames.go. Each is a bordered popup with focus stops: Tab and Shift-Tab move
// between them, Enter submits, Esc closes.

// sheetFrame is what the sheets share: where the popup sits and what a click on each of its
// spans does, as last drawn, and the focus stop.
type sheetFrame struct {
	top, left int // 0-based
	hits      []boxHit
	hidden    bool // the pane is too small to draw the sheet: only Esc acts on it
	stop      int
}

// place closes a built popup and centers it in a pane rows by cols. A nil popup, or one the
// pane can't hold, hides the sheet. One too tall loses rows above its last row, which holds
// the buttons, and the spans under the cut move up with it.
func (f *sheetFrame) place(b *popupBox, rows, cols int) (box []string, top, left int) {
	f.hits = nil
	f.hidden = b == nil || rows < 4 || b.w > cols
	if f.hidden {
		return nil, 0, 0
	}
	box = b.close()
	hits := b.hits
	if over := len(box) - rows; over > 0 {
		keep := len(box) - 2
		from := max(1, keep-over)
		box = append(box[:from:from], box[keep:]...)
		hits = nil
		for _, h := range b.hits {
			switch {
			case h.row < from:
				hits = append(hits, h)
			case h.row >= keep:
				h.row -= keep - from
				hits = append(hits, h)
			}
		}
	}
	f.hits = hits
	f.top, f.left = max(0, (rows-len(box))/2), max(0, (cols-b.w)/2)
	return box, f.top, f.left
}

// click presses the span under a left press.
func (f *sheetFrame) click(m mouseEvent) {
	if f.hidden || !m.press || m.button != 0 {
		return
	}
	row, col := m.y-1-f.top, m.x-1-f.left
	for i := len(f.hits) - 1; i >= 0; i-- {
		if h := f.hits[i]; h.row == row && col >= h.x0 && col <= h.x1 {
			h.act(col - h.x0)
			return
		}
	}
}

// tab moves the focus over n stops on Tab and Shift-Tab, and reports whether k was one.
func (f *sheetFrame) tab(k []byte, n int) bool {
	by := 0
	switch string(k) {
	case "\t":
		by = 1
	case "\x1b[Z":
		by = -1
	default:
		return false
	}
	if n > 0 {
		f.stop = ((f.stop+by)%n + n) % n
	}
	return true
}

// sheetField adds a one-line text field across the popup. A click focuses it.
func sheetField(b *popupBox, t *textInput, placeholder string, focused bool, focus func()) {
	b.hit(0, b.inner-1, func(int) { focus() })
	b.add(renderField(t, placeholder, focused, b.inner, sgrUnder))
}

// sheetButton appends a button to a popup row being built.
func sheetButton(b *popupBox, row *string, button homeButton) {
	x := cellWidth(stripSGR(*row))
	if button.act != nil {
		b.hit(x, x+button.width()-1, func(int) { button.act() })
	}
	*row += button.draw()
}

// sheetButtons adds a row with buttons flush right.
func sheetButtons(b *popupBox, buttons []homeButton) {
	buttons = fitButtons(buttons, b.inner)
	row := strings.Repeat(" ", max(0, b.inner-buttonsWidth(buttons)))
	for i, button := range buttons {
		if i > 0 {
			row += " "
		}
		sheetButton(b, &row, button)
	}
	b.add(row)
}

// sheetText adds text wrapped to the popup, at most n lines; what doesn't fit is cut.
func sheetText(b *popupBox, text, style string, n int) {
	for _, part := range wrapCapped(sanitize(text), b.inner, n) {
		b.add(style + part + sgrReset)
	}
}

// wrapCapped is wrapWords held to n lines: the last one takes the rest of the text, cut.
func wrapCapped(text string, w, n int) []string {
	lines := wrapWords(text, w)
	if n <= 0 || len(lines) <= n {
		return lines
	}
	lines[n-1] = clip(strings.Join(lines[n-1:], " "), w)
	return lines[:n]
}

// addProjectSheet is the app's AddProjectSheet: a project id and an optional display name,
// validated with gcloud before the project is stored.
type addProjectSheet struct {
	sheetFrame
	h          *cloudHome
	id, name   textInput
	validating bool
	err        string
}

// The add sheet's stops.
const (
	addStopID = iota
	addStopName
	addStopSubmit
	addStops
)

func newAddProjectSheet(h *cloudHome) *addProjectSheet { return &addProjectSheet{h: h} }

func (s *addProjectSheet) close() { s.h.closeSheet(s) }

func (s *addProjectSheet) blank() bool { return strings.TrimSpace(s.id.String()) == "" }

// add submits the sheet (AddProjectSheet.add).
func (s *addProjectSheet) add() {
	id := strings.TrimSpace(s.id.String())
	if id == "" || s.validating {
		return
	}
	s.validating, s.err = true, ""
	s.h.addProject(id, s.name.String(), s.added)
}

// added takes the result: a project that was added or was already there closes the sheet, a
// failure shows its message in it.
func (s *addProjectSheet) added(res cloudAddResult) {
	s.validating = false
	if res.Result == cloudAddFailure {
		s.err = res.Message
		return
	}
	if s.h.sheet == s {
		s.h.hush()
	}
	s.close()
}

func (s *addProjectSheet) key(k []byte) {
	switch {
	case isEsc(k):
		s.close()
	case s.hidden:
	case s.tab(k, addStops):
	case isEnter(k):
		s.add()
	case s.stop == addStopID:
		s.id.handle(k)
	case s.stop == addStopName:
		s.name.handle(k)
	}
}

func (s *addProjectSheet) mouse(m mouseEvent) { s.click(m) }

func (s *addProjectSheet) box(rows, cols int) ([]string, int, int) {
	return s.place(s.build(rows, cols), rows, cols)
}

func (s *addProjectSheet) build(rows, cols int) *popupBox {
	w := min(cols, 64)
	if w < 40 || rows < 12 {
		return nil
	}
	b := newPopupBox("Add a GCP project", w)
	b.closeRow(s.close)
	b.add(sgrDim + "PROJECT ID" + sgrReset)
	sheetField(b, &s.id, "my-gcp-project-123", s.stop == addStopID, func() { s.stop = addStopID })
	b.add("")
	b.add(sgrDim + "DISPLAY NAME (OPTIONAL)" + sgrReset)
	sheetField(b, &s.name, "Production", s.stop == addStopName, func() { s.stop = addStopName })
	b.add("")
	if s.err != "" {
		sheetText(b, s.err, sgrRed, 3)
		b.add("")
	}
	submit := homeButton{label: "Add project", kind: buttonPrimary, focused: s.stop == addStopSubmit}
	if s.validating {
		submit.label = "Validating…"
	}
	if !s.blank() && !s.validating {
		submit.act = s.add
	}
	sheetButtons(b, []homeButton{submit})
	return b
}

// urlSheet is the app's NewSessionFromURLSheet: a pasted Logs Explorer URL gives a project and
// a filter; the project is added if it is new, then a session opens on that filter.
type urlSheet struct {
	sheetFrame
	h       *cloudHome
	url     textInput
	working bool
	err     string
}

// The URL sheet's stops.
const (
	urlStopField = iota
	urlStopSubmit
	urlStops
)

func newURLSheet(h *cloudHome) *urlSheet { return &urlSheet{h: h} }

func (s *urlSheet) close() { s.h.closeSheet(s) }

func (s *urlSheet) parsed() (project, query string) { return parseConsoleURL(s.url.String()) }

// start submits the sheet (NewSessionFromURLSheet.start).
func (s *urlSheet) start() {
	if s.working {
		return
	}
	project, query := s.parsed()
	if project == "" {
		return // Start session is disabled until a project is parsed, as in the app
	}
	s.working, s.err = true, ""
	if _, known := s.h.state.project(project); known {
		s.launch(project, query)
		return
	}
	s.h.addProject(project, "", func(res cloudAddResult) {
		if s.h.sheet != s {
			return // closed while the project was validated: nothing opens
		}
		if res.Result == cloudAddFailure {
			s.working, s.err = false, res.Message
			return
		}
		s.h.hush()
		s.launch(project, query)
	})
}

func (s *urlSheet) launch(project, query string) {
	s.working = false
	s.close()
	s.h.startSession(project, query)
}

func (s *urlSheet) key(k []byte) {
	switch {
	case isEsc(k):
		s.close()
	case s.hidden:
	case s.tab(k, urlStops):
	case isEnter(k):
		s.start()
	case s.stop == urlStopField:
		if s.url.handle(k) {
			s.err = ""
		}
	}
}

func (s *urlSheet) mouse(m mouseEvent) { s.click(m) }

func (s *urlSheet) box(rows, cols int) ([]string, int, int) {
	return s.place(s.build(rows, cols), rows, cols)
}

func (s *urlSheet) build(rows, cols int) *popupBox {
	w := min(cols, 84)
	if w < 40 || rows < 12 {
		return nil
	}
	b := newPopupBox("New session from URL", w)
	b.closeRow(s.close)
	sheetText(b, "Paste a Logs Explorer URL (console.cloud.google.com/logs/query…).", sgrDim, 2)
	sheetField(b, &s.url, "https://console.cloud.google.com/logs/query;query=…?project=…",
		s.stop == urlStopField, func() { s.stop = urlStopField })
	b.add("")
	project, query := s.parsed()
	if project != "" {
		b.add(sgrDim + "PROJECT" + sgrReset)
		sheetText(b, project, "", 1)
	}
	if query != "" {
		b.add(sgrDim + "FILTER" + sgrReset)
		sheetText(b, query, "", 3)
	}
	if project != "" || query != "" {
		b.add("")
	}
	if s.err != "" {
		sheetText(b, s.err, sgrRed, 3)
		b.add("")
	}
	submit := homeButton{label: "Start session", kind: buttonPrimary, focused: s.stop == urlStopSubmit}
	if s.working {
		submit.label = "Starting…"
	}
	if project != "" && !s.working {
		submit.act = s.start
	}
	sheetButtons(b, []homeButton{submit})
	return b
}

// renameSheet is the app's "Rename project" alert: the display name, Save and Cancel.
type renameSheet struct {
	sheetFrame
	h         *cloudHome
	projectID string
	name      textInput
}

// The rename sheet's stops.
const (
	renameStopField = iota
	renameStopSave
	renameStopCancel
	renameStops
)

func newRenameSheet(h *cloudHome, p cloudProject) *renameSheet {
	s := &renameSheet{h: h, projectID: p.ProjectID}
	s.name.set(p.DisplayName)
	return s
}

func (s *renameSheet) close() { s.h.closeSheet(s) }

func (s *renameSheet) save() {
	s.close()
	s.h.rename(s.projectID, s.name.String())
}

func (s *renameSheet) key(k []byte) {
	switch {
	case isEsc(k):
		s.close()
	case s.hidden:
	case s.tab(k, renameStops):
	case isEnter(k):
		if s.stop == renameStopCancel {
			s.close()
		} else {
			s.save()
		}
	case s.stop == renameStopField:
		s.name.handle(k)
	}
}

func (s *renameSheet) mouse(m mouseEvent) { s.click(m) }

func (s *renameSheet) box(rows, cols int) ([]string, int, int) {
	return s.place(s.build(rows, cols), rows, cols)
}

func (s *renameSheet) build(rows, cols int) *popupBox {
	w := min(cols, 56)
	if w < 40 || rows < 8 {
		return nil
	}
	b := newPopupBox("Rename project", w)
	b.closeRow(s.close)
	sheetField(b, &s.name, "Display name", s.stop == renameStopField, func() { s.stop = renameStopField })
	b.add("")
	sheetButtons(b, []homeButton{
		{label: "Save", kind: buttonPrimary, act: s.save, focused: s.stop == renameStopSave},
		{label: "Cancel", act: s.close, focused: s.stop == renameStopCancel},
	})
	return b
}
