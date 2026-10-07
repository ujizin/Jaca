package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The session viewer's small popups: the forms for a custom time range and a template's name
// (alerts and a sheet in the app), the Templates menu, and the label-key picker.

// cloudForm is a popup of text fields over a row of buttons. Enter in a field presses the
// primary button; Esc and the cancel button close it.
type cloudForm struct {
	title   string
	labels  []string
	fields  []textInput
	buttons []string
	primary int
	cancel  int
	focus   int          // a field's index, then the buttons
	bad     map[int]bool // fields whose text didn't read, drawn in red
	// press is a button other than cancel; it reports whether the form closes.
	press func(f *cloudForm, button int) bool
	done  func()

	// As last drawn (0-based).
	top, left int
	hits      []boxHit
}

func (f *cloudForm) value(i int) string {
	if i < 0 || i >= len(f.fields) {
		return ""
	}
	return f.fields[i].String()
}

func (f *cloudForm) activate(button int) {
	if button == f.cancel || f.press == nil || f.press(f, button) {
		f.done()
	}
}

func (f *cloudForm) key(k []byte) {
	n := len(f.fields) + len(f.buttons)
	if n == 0 {
		f.done()
		return
	}
	f.focus = clampIndex(f.focus, n)
	switch {
	case isEsc(k):
		f.done()
	case string(k) == "\x1b[Z", isArrowUp(k):
		f.focus = (f.focus - 1 + n) % n
	case len(k) == 1 && k[0] == '\t', isArrowDown(k):
		f.focus = (f.focus + 1) % n
	case isEnter(k):
		if f.focus < len(f.fields) {
			f.activate(f.primary)
		} else {
			f.activate(f.focus - len(f.fields))
		}
	case f.focus < len(f.fields):
		if f.fields[f.focus].handle(k) {
			delete(f.bad, f.focus)
		}
	case len(k) == 1 && k[0] == ' ':
		f.activate(f.focus - len(f.fields))
	}
}

func (f *cloudForm) mouse(m mouseEvent) {
	if !m.press || m.button != 0 {
		return
	}
	row, col := m.y-1-f.top, m.x-1-f.left
	for i := len(f.hits) - 1; i >= 0; i-- {
		if h := f.hits[i]; h.row == row && col >= h.x0 && col <= h.x1 {
			h.act(col)
			return
		}
	}
}

// box draws the form centered in a pane rows by cols. A pane too small for it shows nothing,
// and Esc still closes it.
func (f *cloudForm) box(rows, cols int) (box []string, top, left int) {
	f.hits = nil
	w := min(cols, 48)
	if w < 30 || rows < 5 {
		return nil, 0, 0
	}
	b := newPopupBox(f.title, w)
	for i := range f.fields {
		label := ""
		if i < len(f.labels) {
			label = f.labels[i]
		}
		b.add(sgrDim + clip(label, b.inner) + sgrReset)
		style := sgrUnder
		if f.bad[i] {
			style = sgrUnder + sgrRed
		}
		b.hit(0, b.inner-1, func(int) { f.focus = i })
		b.add(renderField(&f.fields[i], label, f.focus == i, b.inner, style))
	}
	b.add("")
	// The buttons, at the right in the app's order.
	total := 0
	for _, label := range f.buttons {
		total += cellWidth(label) + 2
	}
	total += 2 * max(0, len(f.buttons)-1)
	row := strings.Repeat(" ", max(0, b.inner-total))
	for j, label := range f.buttons {
		if j > 0 {
			row += "  "
		}
		style := sgrUnder
		if f.focus == len(f.fields)+j {
			style = sgrBold + sgrRev
		}
		b.button(&row, " "+label+" ", style, func() {
			f.focus = len(f.fields) + j
			f.activate(j)
		})
	}
	b.add(row)
	box = b.close()
	if len(box) > rows { // keep the borders, drop the rows that don't fit
		box = append(box[:rows-1], box[len(box)-1])
	}
	for _, h := range b.hits {
		if h.row < len(box)-1 {
			f.hits = append(f.hits, h)
		}
	}
	f.top, f.left = max(0, (rows-len(box))/2), max(0, (cols-w)/2)
	return box, f.top, f.left
}

func (v *cloudViewer) newForm(title string, labels, values, buttons []string, primary, cancel int, press func(f *cloudForm, button int) bool) *cloudForm {
	f := &cloudForm{title: title, labels: labels, buttons: buttons, primary: primary, cancel: cancel,
		bad: map[int]bool{}, press: press, done: v.closeSheet}
	f.fields = make([]textInput, len(labels))
	for i := range f.fields {
		if i < len(values) {
			f.fields[i].set(values[i])
		}
	}
	return f
}

// openCustomMinutes is the app's Custom range alert. As there, Apply with anything but a
// positive whole number changes nothing.
func (v *cloudViewer) openCustomMinutes() {
	v.sheet = v.newForm("Custom range (minutes)", []string{"Minutes"}, []string{"15"}, []string{"Apply", "Cancel"}, 0, 1,
		func(f *cloudForm, _ int) bool {
			if m, err := strconv.Atoi(strings.TrimSpace(f.value(0))); err == nil && m > 0 {
				v.setRange(cloudTimeRange{Minutes: m})
			}
			return true
		})
}

// cloudDateLayout is how the absolute range's fields read a local time.
const cloudDateLayout = "2006-01-02 15:04"

// openAbsoluteRange is the app's Absolute time range sheet, its date pickers as text fields in
// local time. A field that doesn't read as a time is marked and the form stays open.
func (v *cloudViewer) openAbsoluteRange() {
	now := v.now()
	values := []string{now.Add(-time.Hour).Format(cloudDateLayout), now.Format(cloudDateLayout)}
	v.sheet = v.newForm("Absolute time range", []string{"Start", "End"}, values, []string{"Cancel", "Apply"}, 1, 0,
		func(f *cloudForm, _ int) bool {
			start, errStart := time.ParseInLocation(cloudDateLayout, strings.TrimSpace(f.value(0)), time.Local)
			end, errEnd := time.ParseInLocation(cloudDateLayout, strings.TrimSpace(f.value(1)), time.Local)
			if errStart != nil {
				f.bad[0] = true
			}
			if errEnd != nil {
				f.bad[1] = true
			}
			if errStart != nil || errEnd != nil {
				return false
			}
			v.setRange(cloudTimeRange{Start: start, End: end})
			return true
		})
}

// openSaveTemplate is the app's Save query template alert.
func (v *cloudViewer) openSaveTemplate() {
	v.sheet = v.newForm("Save query template", []string{"Template name"}, nil, []string{"Save", "Cancel"}, 0, 1,
		func(f *cloudForm, _ int) bool {
			params := map[string]any{"name": f.value(0), "query": v.cfg.Query}
			if v.rawMode {
				params["rawFilter"] = v.cfg.RawFilter
			}
			v.call("cloud.saveQueryTemplate", params)
			return true
		})
}

// openTemplatesMenu is the bar's Templates menu: the saved templates under their heading, then
// saving the current query as one.
func (v *cloudViewer) openTemplatesMenu(x, y int) {
	var items []menuItem
	if len(v.templates) > 0 {
		items = append(items, menuItem{label: "Saved"})
		for _, t := range v.templates {
			items = append(items, menuItem{sanitize(t.Name), func() { v.applyTemplate(t) }})
		}
	}
	items = append(items, menuItem{}, menuItem{"Save current as template…", v.openSaveTemplate})
	menu := &popupMenu{x: x, y: y, items: items}
	menu.selected = -1
	menu.move(1)
	if len(v.templates) > 0 {
		menu.move(1) // past the heading
	}
	v.menu = menu
}

// applyTemplate makes a saved template the session's query, or its raw filter, and applies it.
func (v *cloudViewer) applyTemplate(t cloudQueryTemplate) {
	q := t.Query
	// The conditions are edited in place from here on, so they can't be the template's own.
	q.TextConditions = append([]textCondition(nil), q.TextConditions...)
	q.LabelConditions = append([]labelCondition(nil), q.LabelConditions...)
	q.SeveritySet = append([]int(nil), q.SeveritySet...)
	v.cfg.Query = q
	v.setRaw(t.RawFilter != "", t.RawFilter)
	v.apply()
}

// cloudKeyPicker is the app's LabelKeyPicker popover for one label condition: the detected keys
// under a search field, favorites first. Enter or a click picks a key; Tab or a click on its
// star pins it.
type cloudKeyPicker struct {
	v        *cloudViewer
	cond     string // the condition's id
	query    textInput
	selected int

	// As last drawn (0-based).
	top, left, width, height int
	hits                     []boxHit
}

func (v *cloudViewer) openKeyPicker(cond string) {
	v.sheet = &cloudKeyPicker{v: v, cond: cond}
}

// keys is the detected keys, favorites first, narrowed by the search.
func (p *cloudKeyPicker) keys() []string {
	all := orderedLabelKeys(p.v.project.labelKeys(), p.v.project.favoriteLabelKeys())
	q := strings.ToLower(strings.TrimSpace(p.query.String()))
	if q == "" {
		return all
	}
	var out []string
	for _, key := range all {
		if strings.Contains(strings.ToLower(key), q) {
			out = append(out, key)
		}
	}
	return out
}

// current is the key the condition has now.
func (p *cloudKeyPicker) current() string {
	for _, c := range p.v.cfg.Query.LabelConditions {
		if c.ID == p.cond {
			return c.Key
		}
	}
	return ""
}

func (p *cloudKeyPicker) pick(key string) {
	conds := p.v.cfg.Query.LabelConditions
	for i := range conds {
		if conds[i].ID == p.cond {
			conds[i].Key = key
		}
	}
	p.v.closeSheet()
}

func (p *cloudKeyPicker) key(k []byte) {
	keys := p.keys()
	p.selected = clampIndex(p.selected, len(keys))
	switch {
	case isEsc(k):
		p.v.closeSheet()
	case isArrowUp(k):
		p.selected = clampIndex(p.selected-1, len(keys))
	case isArrowDown(k):
		p.selected = clampIndex(p.selected+1, len(keys))
	case isEnter(k):
		if p.selected < len(keys) {
			p.pick(keys[p.selected])
		}
	case len(k) == 1 && k[0] == '\t':
		if p.selected < len(keys) {
			p.v.toggleFavorite(keys[p.selected])
		}
	default:
		if p.query.handle(k) {
			p.selected = 0
		}
	}
}

func (p *cloudKeyPicker) mouse(m mouseEvent) {
	const wheelUp, wheelDown = 64, 65
	if !m.press {
		return
	}
	keys := p.keys()
	switch m.button {
	case wheelUp:
		p.selected = clampIndex(p.selected-1, len(keys))
	case wheelDown:
		p.selected = clampIndex(p.selected+1, len(keys))
	case 0:
		row, col := m.y-1-p.top, m.x-1-p.left
		if row < 0 || row >= p.height || col < 0 || col >= p.width {
			p.v.closeSheet() // a click outside closes the popover
			return
		}
		for i := len(p.hits) - 1; i >= 0; i-- {
			if h := p.hits[i]; h.row == row && col >= h.x0 && col <= h.x1 {
				h.act(col)
				return
			}
		}
	}
}

func (p *cloudKeyPicker) box(rows, cols int) (box []string, top, left int) {
	p.hits, p.width, p.height = nil, 0, 0
	w := min(cols, 44)
	if w < 24 || rows < 6 {
		return nil, 0, 0
	}
	b := &popupBox{w: w, inner: w - 4, rows: []string{"╭" + strings.Repeat("─", w-2) + "╮"}}
	message := func(text string) {
		for _, part := range wrapWords(text, b.inner) {
			b.add(sgrDim + part + sgrReset)
		}
	}
	if !p.v.labelsDetected() {
		message("No labels detected yet — run a session once.")
	} else {
		b.add(renderField(&p.query, "Search labels…", true, b.inner, sgrUnder))
		b.add("")
		keys := p.keys()
		p.selected = clampIndex(p.selected, len(keys))
		if len(keys) == 0 {
			message(fmt.Sprintf("No labels match “%s”.", sanitize(p.query.String())))
		}
		current := p.current()
		room := max(1, min(10, rows-6))
		start := listWindow(p.selected, room)
		for i := start; i < len(keys) && i < start+room; i++ {
			key := keys[i]
			star, starStyle := "☆", sgrDim
			if p.v.isFavorite(key) {
				star, starStyle = "★", sgrYellow
			}
			check := " "
			if key == current {
				check = "✓"
			}
			name := fit(sanitize(key), max(1, b.inner-4))
			b.hit(2, b.inner-1, func(int) { p.pick(key) })
			b.hit(0, 1, func(int) { p.selected = i; p.v.toggleFavorite(key) })
			if i == p.selected {
				b.add(sgrRev + sgrBold + star + " " + name + " " + check + sgrReset)
			} else {
				b.add(starStyle + star + sgrReset + " " + name + " " + sgrGreen + check + sgrReset)
			}
		}
	}
	box = b.close()
	if len(box) > rows {
		box = append(box[:rows-1], box[len(box)-1])
	}
	for _, h := range b.hits {
		if h.row < len(box)-1 {
			p.hits = append(p.hits, h)
		}
	}
	p.width, p.height = w, len(box)
	p.top, p.left = max(0, (rows-len(box))/2), max(0, (cols-w)/2)
	return box, p.top, p.left
}
