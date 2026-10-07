package main

import "strings"

// The rows of the Cloud Logging home and the buttons drawn on them.

// homeLine is one drawn row of the home.
type homeLine struct {
	text string
	item int // the project this row draws, -1 for none
	hits []homeHit
}

// homeHit is a clickable span of a row: columns x0..x1 (1-based).
type homeHit struct {
	x0, x1 int
	act    func()
}

func plainLine(text string) homeLine { return homeLine{text: text, item: -1} }

func (l *homeLine) width() int { return cellWidth(stripSGR(l.text)) }

// button appends a drawn button to the row. act is nil for one that can't be pressed.
func (l *homeLine) button(drawn string, act func()) {
	x := l.width()
	l.text += drawn
	if act != nil {
		l.hits = append(l.hits, homeHit{x0: x + 1, x1: l.width(), act: act})
	}
}

// The kinds of button: an ordinary one, the row's main one, a destructive one, and a
// destructive one waiting for its second press.
const (
	buttonPlain = iota
	buttonPrimary
	buttonCritical
	buttonArmed
)

// homeButton is a button of the home or of a sheet. A nil act draws it disabled.
type homeButton struct {
	label   string
	kind    int
	act     func()
	focused bool // Enter presses it
	reserve int  // cells kept free after it, for a label that grows (Remove, Confirm?)
}

func (b homeButton) width() int { return cellWidth(b.label) + 2 + b.reserve }

// draw is the button: its label with a cell either side, underlined when ordinary, a block
// when it is the main one, red when destructive, dim when disabled, reversed under the cursor.
func (b homeButton) draw() string {
	text := " " + b.label + " "
	pad := strings.Repeat(" ", b.reserve)
	switch {
	case b.act == nil:
		return sgrDim + text + sgrReset + pad
	case b.kind == buttonArmed:
		return badgeStyle(colorRed) + text + sgrReset + pad
	case b.kind == buttonCritical && b.focused:
		return "\x1b[7;" + colorRed + "m" + text + sgrReset + pad
	case b.kind == buttonCritical:
		return sgrRed + text + sgrReset + pad
	case b.kind == buttonPrimary && b.focused:
		return sgrBold + sgrRev + sgrUnder + text + sgrReset + pad
	case b.kind == buttonPrimary:
		return sgrBold + sgrRev + text + sgrReset + pad
	case b.focused:
		return sgrRev + text + sgrReset + pad
	}
	return " " + sgrUnder + b.label + sgrReset + " " + pad
}

// buttonsWidth is the width of buttons drawn a cell apart.
func buttonsWidth(buttons []homeButton) int {
	w := max(0, len(buttons)-1)
	for _, b := range buttons {
		w += b.width()
	}
	return w
}

// fitButtons leaves out buttons from the front until the rest fit in w cells. The last one,
// the row's main action, goes last.
func fitButtons(buttons []homeButton, w int) []homeButton {
	for len(buttons) > 0 && buttonsWidth(buttons) > w {
		buttons = buttons[1:]
	}
	return buttons
}

func (l *homeLine) buttons(buttons []homeButton) {
	for i, b := range buttons {
		if i > 0 {
			l.text += " "
		}
		l.button(b.draw(), b.act)
	}
}

// splitLine is a row cols wide with a text at its left and buttons flush right. The text is
// cut to the room the buttons leave, and left out when they leave none.
func splitLine(prefix, text, style string, buttons []homeButton, cols, item int) homeLine {
	line := homeLine{text: prefix, item: item}
	room := cols - line.width()
	buttons = fitButtons(buttons, room)
	bw := buttonsWidth(buttons)
	textW := room - bw
	if bw > 0 {
		textW -= 2
	}
	if textW > 0 {
		line.text += style + fit(text, textW) + sgrReset
	}
	if bw > 0 {
		line.text += strings.Repeat(" ", max(0, cols-line.width()-bw))
		line.buttons(buttons)
	}
	return line
}

// centered is a row with a text in the middle of cols cells.
func centered(text, style string, cols int) homeLine {
	text = clip(text, cols)
	return plainLine(strings.Repeat(" ", max(0, (cols-cellWidth(text))/2)) + style + text + sgrReset)
}

// headLines are the rows that stay put: the header (CloudLoggingHomeView.header), the banner
// for a missing or signed-out gcloud (authBanner), and a blank row.
func (h *cloudHome) headLines(cols int) []homeLine {
	var fromURL, add func()
	if h.available() {
		fromURL, add = h.openURLSheet, h.openAddSheet
	}
	lines := []homeLine{
		splitLine("", "CLOUD LOGGING", sgrDim, []homeButton{
			{label: "Re-check", act: h.detect},
			{label: "From URL…", act: fromURL},
			{label: "Add project", kind: buttonPrimary, act: add},
		}, cols, -1),
		plainLine(clip(h.accountLine(), cols)),
	}
	notice := func(title, text string) {
		lines = append(lines, plainLine(""), plainLine(sgrYellow+sgrBold+clip(title, cols)+sgrReset))
		for _, part := range wrapWords(text, cols) {
			lines = append(lines, plainLine(part))
		}
	}
	switch h.state.AuthState.State {
	case cloudAuthNotInstalled:
		notice("gcloud not found", "The gcloud CLI wasn't found. Install the Google Cloud SDK, then click Re-check.")
	case cloudAuthNotAuthenticated:
		notice("Authentication required", "Sign in to gcloud in a terminal, then click Re-check.")
		lines = append(lines, h.commandLines(cols)...)
	}
	return append(lines, plainLine(""))
}

// commandLines is the sign-in command in its own box with Copy, Open Terminal and Re-check
// beside it, or under it in a pane too narrow for both.
func (h *cloudHome) commandLines(cols int) []homeLine {
	buttons := []homeButton{
		{label: "Copy", act: h.copyCommand},
		{label: "Open Terminal", act: h.login},
		{label: "Re-check", kind: buttonPrimary, act: h.detect},
	}
	boxW := len(cloudAuthCommand) + 4
	if cols < boxW {
		row := plainLine("")
		row.buttons(fitButtons(buttons, cols))
		return []homeLine{plainLine(clip(cloudAuthCommand, cols)), row}
	}
	rule := strings.Repeat("─", boxW-2)
	mid := plainLine(sgrDim + "│" + sgrReset + " " + cloudAuthCommand + " " + sgrDim + "│" + sgrReset)
	lines := []homeLine{plainLine(sgrDim + "╭" + rule + "╮" + sgrReset), mid, plainLine(sgrDim + "╰" + rule + "╯" + sgrReset)}
	if boxW+2+buttonsWidth(buttons) <= cols {
		lines[1].text += "  "
		lines[1].buttons(buttons)
		return lines
	}
	row := plainLine("")
	row.buttons(fitButtons(buttons, cols))
	return append(lines, row)
}

// cloudLogLine is a project's second row (CloudProjectCard.logLine).
func cloudLogLine(p cloudProject) string {
	if p.SelectedLogName != "" {
		return p.ProjectID + " · " + cloudLogID(p.SelectedLogName)
	}
	return p.ProjectID + " · no log name selected"
}

// projectActions are a project row's buttons, in the order of the action constants.
func (h *cloudHome) projectActions(i int, p cloudProject) []homeButton {
	remove := homeButton{label: "Remove", kind: buttonCritical, reserve: 2}
	if h.isArmed(removeKey(p.ProjectID)) {
		remove = homeButton{label: "Confirm?", kind: buttonArmed}
	}
	buttons := []homeButton{
		{label: "Log names"},
		{label: "Rename"},
		remove,
		{label: "New session", kind: buttonPrimary},
	}
	for n := range buttons {
		n := n
		buttons[n].focused = i == h.selected && n == h.action
		buttons[n].act = func() {
			h.selected, h.action = i, n
			h.press(i, n)
		}
	}
	return buttons
}

// bodyLines are the rows that scroll: one card per project (CloudProjectCard) with a blank
// row between them, or the empty state. Before the first state arrives there is nothing to
// show, so nothing is drawn.
func (h *cloudHome) bodyLines(cols int) []homeLine {
	if !h.loaded {
		return nil
	}
	if len(h.state.Projects) == 0 {
		return h.emptyLines(cols)
	}
	var lines []homeLine
	for i, p := range h.state.Projects {
		if i > 0 {
			lines = append(lines, plainLine(""))
		}
		marker := rowMarker(i == h.selected)
		actions := h.projectActions(i, p)
		title := sanitize(p.title())
		// The actions sit beside the title where that leaves the title room, else under the card.
		beside := cols >= 2+16+2+buttonsWidth(actions)
		if beside {
			lines = append(lines, splitLine(marker, title, sgrBold, actions, cols, i))
		} else {
			lines = append(lines, homeLine{text: marker + sgrBold + clip(title, max(0, cols-2)) + sgrReset, item: i})
		}
		lines = append(lines, homeLine{text: marker + sgrDim + clipMiddle(sanitize(cloudLogLine(p)), max(0, cols-2)) + sgrReset, item: i})
		if !beside {
			row := homeLine{text: marker, item: i}
			row.buttons(fitButtons(actions, max(0, cols-2)))
			lines = append(lines, row)
		}
	}
	return lines
}

// emptyLines is the home with no projects (CloudLoggingHomeView.emptyState).
func (h *cloudHome) emptyLines(cols int) []homeLine {
	lines := []homeLine{plainLine(""), centered("No GCP projects yet", sgrBold, cols)}
	for _, part := range wrapWords("Add a project id to start streaming its Cloud Logging.", cols) {
		lines = append(lines, centered(part, sgrDim, cols))
	}
	var add func()
	if h.available() {
		add = h.openAddSheet
	}
	button := homeButton{label: "Add project", kind: buttonPrimary, act: add, focused: true}
	row := plainLine("")
	if button.width() <= cols {
		row.text = strings.Repeat(" ", (cols-button.width())/2)
		row.button(button.draw(), button.act)
	}
	return append(lines, plainLine(""), row)
}
