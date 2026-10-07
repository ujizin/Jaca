package main

import "strings"

// formatEditor is the app's Copy format sheet (LogCopyFormatSheet) as a popup: the presets, each
// with what it produces for a sample line, the template and date format to edit by hand, a live
// example, and Save. Nothing is saved until Save.
type formatEditor struct {
	template, date textInput
	focus          int // a preset's index, then the fields and buttons below

	// As last drawn: where the box is, and which focus a click on each of its spans sets.
	top, left int
	hits      []editorHit
}

// The focus stops after the presets.
const (
	editTemplate = iota
	editDate
	editSave
	editCancel
	editStops
)

type editorHit struct {
	row, x0, x1 int // the box row (0-based) and its columns (0-based, inclusive)
	focus       int
}

func newFormatEditor(format logCopyFormat) *formatEditor {
	e := &formatEditor{}
	e.set(format)
	// Starts on the preset in use, or on the template when the format is a custom one.
	e.focus = len(copyPresets) + editTemplate
	for i, preset := range copyPresets {
		if preset.format == format {
			e.focus = i
		}
	}
	return e
}

func (e *formatEditor) set(format logCopyFormat) {
	e.template.set(format.Template)
	e.date.set(format.DateFormat)
}

func (e *formatEditor) format() logCopyFormat {
	return logCopyFormat{Template: e.template.String(), DateFormat: e.date.String()}
}

func (e *formatEditor) stops() int { return len(copyPresets) + editStops }

// move steps the focus, wrapping at the ends.
func (e *formatEditor) move(by int) {
	e.focus = (e.focus + by + e.stops()) % e.stops()
}

// editorAction is what a key or click on the editor asks the viewer to do next.
type editorAction int

const (
	editorStay editorAction = iota
	editorSave
	editorClose
)

// activate is Enter or a click on the focused stop: a preset becomes the format, a field hands
// the focus on, Save and Cancel end the edit.
func (e *formatEditor) activate() editorAction {
	switch at := e.focus - len(copyPresets); {
	case at < 0:
		e.set(copyPresets[e.focus].format)
	case at == editSave:
		return editorSave
	case at == editCancel:
		return editorClose
	default:
		e.move(1)
	}
	return editorStay
}

// key: arrows and Tab move, Enter activates, Ctrl-S saves, Esc cancels; in a field, other keys
// edit it.
func (e *formatEditor) key(k []byte) editorAction {
	switch {
	case len(k) == 1 && k[0] == 0x13, string(k) == "\x1b[115;9u": // Ctrl-S, or ⌘S where the terminal passes it on
		return editorSave
	case isEsc(k):
		return editorClose
	case isArrowUp(k), len(k) == 3 && k[0] == 0x1b && k[1] == '[' && k[2] == 'Z': // Shift-Tab
		e.move(-1)
	case isArrowDown(k), len(k) == 1 && k[0] == '\t':
		e.move(1)
	case isEnter(k):
		return e.activate()
	case e.focus < len(copyPresets):
		if len(k) == 1 && k[0] == ' ' {
			return e.activate()
		}
	case e.focus == len(copyPresets)+editTemplate:
		e.template.handle(k)
	case e.focus == len(copyPresets)+editDate:
		e.date.handle(k)
	}
	return editorStay
}

// click focuses and activates what was drawn at a screen cell (1-based); elsewhere it does nothing.
func (e *formatEditor) click(x, y int) editorAction {
	row, col := y-e.top, x-e.left
	for _, h := range e.hits {
		if h.row == row && col >= h.x0 && col <= h.x1 {
			e.focus = h.focus
			if at := h.focus - len(copyPresets); at == editTemplate || at == editDate {
				return editorStay // a field takes the focus and keeps it
			}
			return e.activate()
		}
	}
	return editorStay
}

// box draws the editor for a pane rows by cols and records where it sits, centered: the full
// layout where it fits, else a compact one, whose rows are cut from the bottom in a short pane.
func (e *formatEditor) box(rows, cols int) []string {
	if rows >= fullEditorRows && cols >= 64 {
		return e.fullBox(rows, cols)
	}
	return e.compactBox(rows, cols)
}

// fullEditorRows is the height of the full layout.
const fullEditorRows = 25

// fullBox lays the editor out like the app's sheet: the presets in a bordered list, the two
// fields in boxes side by side under their labels, the tokens, the example in a box, and Save.
func (e *formatEditor) fullBox(rows, cols int) []string {
	const pad = 2 // cells between the border and the content
	w := min(cols, 92)
	inner := w - 2 - 2*pad
	e.hits = e.hits[:0]
	rule := strings.Repeat("─", w-2)
	box := []string{titledBorder("Copy format", w)}
	margin := strings.Repeat(" ", pad)
	add := func(content string) { box = append(box, "│"+margin+content+sgrReset+margin+"│") }
	// hit marks columns x0..x1 of the content of the row about to be added.
	hit := func(x0, x1, focus int) {
		e.hits = append(e.hits, editorHit{row: len(box), x0: x0 + 1 + pad, x1: x1 + 1 + pad, focus: focus})
	}
	blank := strings.Repeat(" ", inner)
	current := e.format()
	stop := func(at int) int { return len(copyPresets) + at }

	// The close button at the top right, like Herdr's overlays; underlined while it has the focus.
	closeStyle := ""
	if e.focus == stop(editCancel) {
		closeStyle = sgrUnder
	}
	add(blank)
	hit(inner-len(escClose), inner-1, stop(editCancel))
	add(strings.Repeat(" ", inner-len(escClose)) + closeStyle + closeButton(escClose))

	// The presets, in a bordered list: a radio for the one in use, its name, what it produces.
	add(sgrDim + fit("PRESETS", inner))
	listRule := strings.Repeat("─", inner-2)
	add(sgrDim + "╭" + listRule + "╮")
	nameW := min(30, inner/2)
	exampleW := inner - 2 - 5 - nameW - 3
	for i, preset := range copyPresets {
		radio, radioStyle := "○", sgrDim
		if preset.format == current {
			radio, radioStyle = "◉", sgrGreen
		}
		name, example := fit(preset.name, nameW), fit(sanitize(preset.format.renderSample()), exampleW)
		row := " " + radioStyle + radio + sgrReset + "   " + name + "  " + sgrDim + example + sgrReset + " "
		if e.focus == i { // the focused row is one reversed bar
			row = sgrRev + " " + radio + "   " + sgrBold + name + sgrReset + sgrRev + "  " + example + " " + sgrReset
		}
		hit(0, inner-1, i)
		add(sgrDim + "│" + sgrReset + row + sgrDim + "│")
	}
	add(sgrDim + "╰" + listRule + "╯")
	add(blank)

	// The two fields side by side, each under its label.
	add(sgrDim + fit("CUSTOM", inner))
	dateW := max(20, (inner-2)/3)
	templateW := inner - 2 - dateW
	add(sgrDim + fit("FORMAT", templateW) + "  " + fit("DATE FORMAT", dateW))
	template := boxedField(&e.template, defaultCopyFormat.Template, e.focus == stop(editTemplate), templateW, "")
	date := boxedField(&e.date, defaultCopyFormat.DateFormat, e.focus == stop(editDate), dateW, "")
	for i := range template {
		hit(0, templateW-1, stop(editTemplate))
		hit(templateW+2, inner-1, stop(editDate))
		add(template[i] + "  " + date[i])
	}
	legend := make([]string, len(copyTokens))
	for i, token := range copyTokens {
		legend[i] = "{" + token + "}"
	}
	add(sgrCyan + fit(strings.Join(legend, "  "), inner))

	add(sgrDim + fit("EXAMPLE", inner))
	add(sgrDim + "╭" + listRule + "╮")
	add(sgrDim + "│" + sgrReset + " " + sgrBold + fit(sanitize(current.renderSample()), inner-4) + sgrReset + " " + sgrDim + "│")
	add(sgrDim + "╰" + listRule + "╯")

	// Save, at the right.
	const save = "  Save  "
	saveStyle := sgrGreen + sgrUnder
	if e.focus == stop(editSave) {
		saveStyle = badgeStyle(colorGreen)
	}
	hit(inner-len(save), inner-1, stop(editSave))
	add(strings.Repeat(" ", inner-len(save)) + saveStyle + save)
	box = append(box, "╰"+rule+"╯")
	e.top, e.left = max(0, (rows-len(box))/2)+1, max(0, (cols-w)/2)+1
	return box
}

// compactBox is the editor for a pane too small for the full layout: one row per item.
func (e *formatEditor) compactBox(rows, cols int) []string {
	const labelW = 13
	w := min(cols, 84)
	inner := w - 4
	if inner < 30 || rows < 5 {
		return nil
	}
	e.hits = e.hits[:0]
	rule := strings.Repeat("─", w-2)
	box := []string{titledBorder("Copy format", w)}
	add := func(content string) { box = append(box, "│ "+content+sgrReset+" │") }
	hit := func(x0, x1, focus int) {
		e.hits = append(e.hits, editorHit{row: len(box), x0: x0 + 2, x1: x1 + 2, focus: focus})
	}
	button := func(label string, focus int) string {
		if e.focus == focus {
			return sgrBold + sgrRev + " " + label + " " + sgrReset
		}
		return sgrUnder + " " + label + " " + sgrReset
	}
	blank := strings.Repeat(" ", inner)
	current := e.format()

	const save = "Save"
	closeStyle := ""
	if e.focus == len(copyPresets)+editCancel {
		closeStyle = sgrUnder
	}
	hit(inner-len(escClose), inner-1, len(copyPresets)+editCancel)
	add(sgrDim + fit("PRESETS", inner-len(escClose)) + sgrReset + closeStyle + closeButton(escClose))
	nameW := min(30, inner/2)
	for i, preset := range copyPresets {
		radio := "○"
		if preset.format == current {
			radio = sgrGreen + "◉" + sgrReset
		}
		hit(0, inner-1, i)
		add(rowMarker(e.focus == i) + radio + " " + fit(preset.name, nameW) + " " +
			sgrDim + fit(sanitize(preset.format.renderSample()), inner-nameW-5))
	}
	add(blank)
	add(sgrDim + fit("CUSTOM", inner))
	field := func(label string, t *textInput, placeholder string, focus int) {
		hit(0, inner-1, focus)
		add(sgrDim + fit(label, labelW) + sgrReset + renderField(t, placeholder, e.focus == focus, inner-labelW, sgrUnder))
	}
	field("FORMAT", &e.template, defaultCopyFormat.Template, len(copyPresets)+editTemplate)
	field("DATE FORMAT", &e.date, defaultCopyFormat.DateFormat, len(copyPresets)+editDate)
	legend := make([]string, len(copyTokens))
	for i, token := range copyTokens {
		legend[i] = "{" + token + "}"
	}
	add(sgrCyan + fit(strings.Join(legend, " "), inner))
	add(blank)
	add(sgrDim + fit("EXAMPLE", inner))
	add(fit(sanitize(current.renderSample()), inner))
	add(blank)
	hit(inner-len(save)-2, inner-1, len(copyPresets)+editSave)
	add(strings.Repeat(" ", inner-len(save)-2) + button(save, len(copyPresets)+editSave))
	box = append(box, "╰"+rule+"╯")
	if len(box) > rows {
		box = append(box[:rows-1], box[len(box)-1])
	}
	e.top, e.left = max(0, (rows-len(box))/2)+1, max(0, (cols-w)/2)+1
	return box
}
