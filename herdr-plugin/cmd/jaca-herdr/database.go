package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// What the keys act on in the database viewer.
type dbFocus int

const (
	dbGrid   dbFocus = iota // the rows (and the row detail beside them)
	dbEditor                // the SQL box
	dbApps                  // the installed-apps list
	dbMenu                  // the DB or Table menu, or the detail's Copy menu
	dbHelp                  // the keys popup
)

// The row detail's tabs, as in the app's DBRowDetail.
var dbTabs = []string{"Fields", "JSON"}

const (
	dbTabFields = iota
	dbTabJSON
)

// remoteDB mirrors RemoteDB and dbTable DBTable (Sources/Core/Database/DatabaseModels.swift),
// as database.databases and database.pull return them.
type remoteDB struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type dbTable struct {
	Name     string `json:"name"`
	RowCount int    `json:"rowCount"`
}

// The app's texts (DatabaseSessionView and DatabaseSession).
const (
	dbPickApp       = "Pick an app"
	dbPickAppHint   = "Pick an app to browse its local database."
	dbNoDatabase    = "No SQLite database found for this app."
	dbLoading       = "Loading…"
	dbNoTable       = "No table selected."
	dbNoColumns     = "Query returned no columns."
	dbSQLHint       = "SELECT … (read-only)"
	dbNone          = "—"
	dbCopyJSON      = "Copy JSON"
	dbReadOnlyError = "Only read-only queries (SELECT/WITH/PRAGMA/EXPLAIN) are allowed."
)

// dbCellW is the width of a grid column, with one more cell between two columns.
const dbCellW = 24

// dbLongCall is how long a pull or a query may take: the pull copies the file off the device.
const dbLongCall = 2 * time.Minute

// dbClick is a clickable span of the pane as last drawn: rows y0..y1, columns x0..x1 (1-based).
type dbClick struct {
	y0, y1, x0, x1 int
	act            func()
}

// dbViewer is the app's Database tab (DatabaseSessionView on DatabaseSession): pick an app,
// one of its SQLite databases and a table, page through the rows of a pulled snapshot, and
// run read-only SQL on it. The selection lives here; jacad holds the snapshot.
type dbViewer struct {
	snack  snackbar // "Copied"
	p      *pane
	device device
	back   func() // returns to the picker when the viewer runs in its pane; nil in a tab
	left   bool

	// The calls to jacad, where they run and the timer, replaced in tests. spawn runs work off
	// the loop and what it returns on the loop; lost runs instead when the pane has quit
	// meanwhile. after runs f on the loop once d has passed.
	call  func(method string, params, out any, timeout time.Duration) error
	spawn func(work func() func(), lost func())
	after func(d time.Duration, f func())

	id      string // the session's id in jacad
	opened  bool
	opening bool
	// The listing and the pull go to jacad one at a time: it would run two side by side and
	// keep whichever finished last. chain is one of them in flight, want the step to take next.
	chain bool
	want  dbStep

	// DatabaseSession's state. selectedDB and table are indexes, -1 for none.
	apps       appPicker
	appID      string
	databases  []remoteDB
	selectedDB int
	tables     []dbTable
	table      int
	pulled     bool   // a snapshot was pulled, so rows and queries have something to read:
	pulledPath string // this database's
	result     *dbResultSet
	results    int // counts the results shown, for the detail's cache
	err        string
	sql        textArea
	page       int
	reading    bool // a page or a query is in flight
	readGen    int  // counts them: a reply to an earlier one is dropped

	cursor   int  // the highlighted row
	detail   bool // the row detail is open (the app's selectedRow)
	top      int  // the first row drawn
	scrolled bool // the rows were scrolled by hand, so the view doesn't follow the cursor
	leftCol  int  // the first column drawn
	tab      int
	detailContent
	cache dbDetailCache

	focus dbFocus
	menu  *popupMenu

	// The pane as last drawn, for the mouse and the keys.
	clicks             []dbClick
	dbX, tableX        int // the DB and Table controls' columns, 0 when hidden
	barRows            int
	editTop, editRows  int // the SQL box: its first screen row and its rows (0 when hidden),
	editX, editW       int // its first column and its width
	gridTop, gridRows  int // the rows: the first one's screen row and how many fit,
	gridW              int // and the grid's width (0 when hidden)
	maxLeft            int // the last column the grid can start at
	detailX0, detailY0 int // the detail's first column and row (0 when hidden)
}

// dbDetailCache is the detail's content for one row, tab and width, so a long value is
// wrapped once and not on every frame.
type dbDetailCache struct {
	results, row, tab, w int
	lines                []string
	meta                 []detailLine
}

// runDatabasePane is the viewer for the device the picker passed in deviceEnv. Opened without
// one, it shows the picker first.
func runDatabasePane() int {
	var d device
	if json.Unmarshal([]byte(os.Getenv(deviceEnv)), &d) != nil || d.ID == "" {
		return runPicker(false)
	}
	c, err := dial()
	if err != nil {
		fmt.Fprintln(os.Stderr, "jaca:", err)
		return 1
	}
	defer c.Close()
	go sendRightClicksToPane()
	return runPane(c, func(p *pane) screen { return newDBViewer(p, d, nil) })
}

func newDBViewer(p *pane, d device, back func()) *dbViewer {
	v := makeDBViewer(p, d, back)
	v.start()
	return v
}

// makeDBViewer is the viewer before it has asked jacad for anything.
func makeDBViewer(p *pane, d device, back func()) *dbViewer {
	v := &dbViewer{p: p, device: d, back: back, id: newUUID(), selectedDB: -1, table: -1}
	v.call = func(method string, params, out any, timeout time.Duration) error {
		return v.p.c.CallTimeout(method, params, out, timeout)
	}
	v.spawn = func(work func() func(), lost func()) {
		go func() {
			if done := work(); !v.p.post(done) && lost != nil {
				lost()
			}
		}()
	}
	v.after = func(d time.Duration, f func()) {
		go func() {
			time.Sleep(d)
			p.post(f) // nothing to undo when the pane has quit
		}()
	}
	return v
}

// start opens the session and shows the apps, as the app's tab lists them when it opens.
func (v *dbViewer) start() {
	v.open()
	v.openApps()
	v.after(dbKeepAlive, v.touch)
}

// MARK: the session

// dbKeepAlive is how often the session is touched: jacad closes one left idle for 10 minutes.
const dbKeepAlive = 4 * time.Minute

// How jacad's answers begin when the session is gone, and when it holds no snapshot (or
// another database's).
const (
	dbSessionGone = "No database session "
	dbNoSnapshot  = "No database pulled in session "
)

// dbStep is a step on the way to a snapshot: listing the app's databases, or pulling the
// selected one.
type dbStep int

const (
	dbStepNone dbStep = iota
	dbStepList
	dbStepPull
)

func (v *dbViewer) openParams() map[string]any {
	return map[string]any{"id": v.id, "device": v.device}
}

// open creates the session in jacad. The step that was waiting for it goes out when it is there.
func (v *dbViewer) open() {
	v.opening = true
	id, params, call := v.id, v.openParams(), v.call
	v.spawn(func() func() {
		err := call("database.open", params, nil, callTimeout)
		return func() {
			v.opening = false
			switch {
			case v.left:
				if err == nil {
					v.closeLater()
				}
			case err != nil:
				if v.want != dbStepNone { // shown once something needed the session
					v.want, v.err = dbStepNone, err.Error()
				}
			default:
				v.opened = true
				v.p.setLive("database.close", id)
				v.pump()
			}
		}
	}, v.closeRemote)
}

// touch keeps the session alive: database.open on a session that exists changes nothing but
// when it was last used. One that jacad closed meanwhile comes back empty, so its snapshot is
// pulled again by whatever needs it next.
func (v *dbViewer) touch() {
	if v.left {
		return
	}
	v.after(dbKeepAlive, v.touch)
	if !v.opened {
		return
	}
	params, call := v.openParams(), v.call
	v.spawn(func() func() {
		var info struct {
			Existed bool `json:"existed"`
		}
		err := call("database.open", params, &info, callTimeout)
		return func() {
			switch {
			case err != nil:
			case v.left:
				v.closeLater() // the call may have brought back a session closed meanwhile
			case !info.Existed:
				v.pulled = false
			}
		}
	}, nil)
}

func (v *dbViewer) closeRemote() {
	_ = v.call("database.close", map[string]any{"id": v.id}, nil, teardownTimeout)
}

// leave closes the session, which drops the snapshot jacad pulled for it.
func (v *dbViewer) leave(wait bool) {
	v.left = true
	if !v.opened {
		return
	}
	v.opened = false
	v.p.setLive("", "")
	if wait {
		v.closeRemote()
	} else {
		v.closeLater()
	}
}

// closeLater closes the session off the loop.
func (v *dbViewer) closeLater() {
	v.spawn(func() func() {
		v.closeRemote()
		return func() {}
	}, nil)
}

func (v *dbViewer) handleEvent(event) {}

// loading is the app's flag: something was asked of jacad and has not come back.
func (v *dbViewer) loading() bool { return v.chain || v.want != dbStepNone || v.reading }

// noteLost reads a failed call's message for what jacad no longer holds, so the next thing
// asked opens the session or pulls the database again rather than failing the same way.
func (v *dbViewer) noteLost(err error) {
	if err == nil {
		return
	}
	switch msg := err.Error(); {
	case strings.HasPrefix(msg, dbSessionGone):
		v.opened, v.pulled = false, false
		v.p.setLive("", "")
	case strings.HasPrefix(msg, dbNoSnapshot):
		v.pulled = false
	}
}

// wantStep asks for a step of the chain. While another is in flight only the last one asked
// for is kept; it goes out when that reply arrives, and that reply is dropped.
func (v *dbViewer) wantStep(step dbStep) {
	v.want, v.err = step, ""
	v.pump()
}

// pump sends the wanted step when nothing stands in its way: the session is open and no
// listing or pull is in flight.
func (v *dbViewer) pump() {
	if v.left || v.chain || v.want == dbStepNone {
		return
	}
	if !v.opened {
		if !v.opening {
			v.open()
		}
		return
	}
	step := v.want
	v.want = dbStepNone
	switch {
	case v.appID == "":
	case step == dbStepList:
		var list []remoteDB
		v.chainCall("database.databases", map[string]any{"id": v.id, "package": v.appID}, callTimeout, &list, func() {
			v.databases = list
			if len(list) == 0 {
				v.err = dbNoDatabase
				return
			}
			v.selectDatabase(0)
		})
	case step == dbStepPull && v.selectedDB >= 0 && v.selectedDB < len(v.databases):
		path := v.databases[v.selectedDB].Path
		var tables []dbTable
		params := map[string]any{"id": v.id, "package": v.appID, "database": path}
		v.chainCall("database.pull", params, dbLongCall, &tables, func() {
			v.pulled, v.pulledPath, v.tables = true, path, tables
			v.dropReads() // what was being read was the snapshot before
			if len(tables) > 0 {
				v.selectTable(0)
			}
		})
	}
}

// chainCall runs a step off the loop and applies its result on the loop, unless another step
// was asked for meanwhile: then the reply is dropped and that step goes out.
func (v *dbViewer) chainCall(method string, params map[string]any, timeout time.Duration, out any, apply func()) {
	v.chain = true
	call := v.call
	v.spawn(func() func() {
		err := call(method, params, out, timeout)
		return func() {
			v.chain = false
			if v.left {
				return
			}
			v.noteLost(err)
			switch {
			case v.want != dbStepNone:
				v.pump()
			case err != nil:
				v.err = err.Error()
			default:
				apply()
			}
		}
	}, nil)
}

// read runs a page or a query off the loop, decoding its result into out, and then runs then
// on the loop with the error (nil when it went through). A reply that arrives after another
// read was made, or after the snapshot changed, is dropped.
func (v *dbViewer) read(method string, params map[string]any, timeout time.Duration, out any, then func(err error)) {
	v.readGen++
	gen, call := v.readGen, v.call
	v.reading, v.err = true, ""
	v.spawn(func() func() {
		err := call(method, params, out, timeout)
		return func() {
			if gen != v.readGen || v.left {
				return
			}
			v.reading = false
			v.noteLost(err)
			if err != nil {
				v.err = err.Error()
			}
			then(err)
		}
	}, nil)
}

// dropReads forgets the page or query in flight: its reply would be for a selection that is
// no longer the one shown.
func (v *dbViewer) dropReads() {
	v.readGen++
	v.reading = false
}

// hasSnapshot reports whether rows and queries have a snapshot to read. When jacad lost it
// (the session was closed, or reopened empty) it is asked for again and the caller does nothing.
func (v *dbViewer) hasSnapshot() bool {
	if v.pulled {
		return true
	}
	if !v.chain && v.want == dbStepNone && !v.opening && v.appID != "" {
		v.refresh()
	}
	return false
}

// MARK: DatabaseSession's flow

func (v *dbViewer) openApps() {
	v.focus = dbApps
	if !v.apps.begin() {
		return
	}
	id, call := v.device.ID, v.call
	v.spawn(func() func() {
		var list []appEntry
		_ = call("devices.apps", map[string]any{"deviceID": id}, &list, callTimeout)
		return func() { v.apps.loaded(list) }
	}, nil)
}

// appLabel is the app button's label (DatabaseAppPicker.buttonLabel).
func (v *dbViewer) appLabel() string {
	if v.appID == "" {
		return dbPickApp
	}
	for _, app := range v.apps.apps {
		if app.ID == v.appID && app.Name != nil {
			return *app.Name
		}
	}
	return v.appID[strings.LastIndex(v.appID, ".")+1:]
}

func (v *dbViewer) selectApp(id string) {
	if id == "" {
		return
	}
	v.appID = id
	v.databases, v.selectedDB, v.tables, v.table, v.pulled = nil, -1, nil, -1, false
	v.dropReads()
	v.setResult(nil)
	v.wantStep(dbStepList)
}

func (v *dbViewer) selectDatabase(i int) {
	if i < 0 || i >= len(v.databases) {
		return
	}
	v.selectedDB, v.tables, v.table, v.pulled = i, nil, -1, false
	v.dropReads()
	v.setResult(nil)
	v.wantStep(dbStepPull)
}

// refresh is the app's Refresh: it pulls the selected database again, or lists the databases
// when none is selected.
func (v *dbViewer) refresh() {
	if v.selectedDB >= 0 && v.selectedDB < len(v.databases) {
		v.wantStep(dbStepPull)
	} else {
		v.wantStep(dbStepList)
	}
}

// tableName is the selected table's name; ok is false when none is selected.
func (v *dbViewer) tableName() (name string, ok bool) {
	if v.table < 0 || v.table >= len(v.tables) {
		return "", false
	}
	return v.tables[v.table].Name, true
}

func (v *dbViewer) selectTable(i int) {
	if i < 0 || i >= len(v.tables) {
		return
	}
	v.table, v.page = i, 0
	v.sql.set(dbTableQuery(v.tables[i].Name))
	v.loadRows()
}

func (v *dbViewer) loadRows() {
	name, ok := v.tableName()
	if !ok || !v.hasSnapshot() {
		return
	}
	var rs dbResultSet
	params := map[string]any{"id": v.id, "database": v.pulledPath, "table": name, "limit": dbPageSize, "offset": v.page * dbPageSize}
	v.read("database.rows", params, callTimeout, &rs, func(err error) {
		if err == nil {
			v.setResult(&rs)
		}
	})
}

// hasNextPage is DatabaseSession.hasNextPage: a full page may have another after it.
func (v *dbViewer) hasNextPage() bool { return v.result != nil && len(v.result.Rows) == dbPageSize }

func (v *dbViewer) nextPage() {
	if _, ok := v.tableName(); ok && v.hasNextPage() {
		v.page++
		v.loadRows()
	}
}

func (v *dbViewer) prevPage() {
	if _, ok := v.tableName(); ok && v.page > 0 {
		v.page--
		v.loadRows()
	}
}

// runQuery runs the SQL box, as DatabaseSession.runQuery does: a statement that is not
// read-only is refused before anything changes, so the result, the table and its pages stay.
// It does nothing while the box isn't showing.
func (v *dbViewer) runQuery() {
	if !v.showsSQL() || !v.hasSnapshot() {
		return
	}
	sql := v.sql.String()
	if !dbIsReadOnly(sql) {
		v.err = dbReadOnlyError
		return
	}
	v.table = -1
	var rs dbResultSet
	params := map[string]any{"id": v.id, "database": v.pulledPath, "sql": sql}
	v.read("database.query", params, dbLongCall, &rs, func(err error) {
		if err == nil {
			v.setResult(&rs)
		}
	})
}

// setResult shows a result (nil for none) from its first row, with the row detail closed.
func (v *dbViewer) setResult(rs *dbResultSet) {
	v.result = rs
	v.results++
	v.cursor, v.detail, v.top, v.scrolled = 0, false, 0, false
	v.detailTop, v.dsel = 0, detailSel{}
}

func (v *dbViewer) rowCount() int {
	if v.result == nil {
		return 0
	}
	return len(v.result.Rows)
}

// showsSQL is whether the pane shows the SQL box: an app is picked and no error has the pane.
func (v *dbViewer) showsSQL() bool {
	return v.appID != "" && !(v.err != "" && v.result == nil && len(v.tables) == 0)
}

// showsGrid is whether the rows are what the pane shows.
func (v *dbViewer) showsGrid() bool {
	return v.appID != "" && v.result != nil && len(v.result.Columns) > 0
}

// moveTo puts the cursor on a row; an open detail then shows that row.
func (v *dbViewer) moveTo(row int) {
	row = clampIndex(row, v.rowCount())
	if row != v.cursor {
		v.cursor, v.detailTop, v.dsel = row, 0, detailSel{}
	}
	v.scrolled = false
}

// toggleRow is a click on a row, as in the app: it opens the row's detail, or closes it when
// that row's is the one open.
func (v *dbViewer) toggleRow(row int) {
	if row < 0 || row >= v.rowCount() {
		return
	}
	if v.detail && row == v.cursor {
		v.detail = false
		return
	}
	v.moveTo(row)
	v.detail = true
}

func (v *dbViewer) setTab(tab int) {
	if v.detail {
		v.tab, v.detailTop, v.dsel = (tab+len(dbTabs))%len(dbTabs), 0, detailSel{}
	}
}

// scrollColumns moves the grid sideways by whole columns.
func (v *dbViewer) scrollColumns(by int) {
	v.leftCol = max(0, min(v.leftCol+by, v.maxLeft))
}

// rowJSON is the cursor's row as the app's Copy JSON writes it; ok is false with no row. It is
// built when asked for: a row can be many megabytes.
func (v *dbViewer) rowJSON() (text string, ok bool) {
	if v.result == nil || v.cursor < 0 || v.cursor >= len(v.result.Rows) {
		return "", false
	}
	return dbRowJSON(v.result.Columns, v.result.Rows[v.cursor]), true
}

func (v *dbViewer) copyRow() {
	if text, ok := v.rowJSON(); ok {
		v.copy(text)
	}
}

func (v *dbViewer) copy(text string) {
	write := copyToClipboard
	v.spawn(func() func() {
		err := write(text)
		return func() {
			if err != nil {
				v.err = err.Error()
			} else {
				v.snack.show(v.p, copiedNotice)
			}
		}
	}, nil)
}

// MARK: the menus

// openDBMenu and openTableMenu are the app's two pickers. An item is chosen by its place in
// the list, so a name is never read back out of its label.
func (v *dbViewer) openDBMenu() {
	if len(v.databases) == 0 {
		return
	}
	items := make([]menuItem, len(v.databases))
	for i, db := range v.databases {
		label := sanitize(db.Name)
		if label == "" {
			label = sanitize(db.Path)
		}
		items[i] = menuItem{dbMenuLabel(label), func() { v.selectDatabase(i) }}
	}
	v.showMenu(items, v.selectedDB, v.dbX)
}

func (v *dbViewer) openTableMenu() {
	if len(v.tables) == 0 {
		return
	}
	items := make([]menuItem, len(v.tables))
	for i, t := range v.tables {
		items[i] = menuItem{dbMenuLabel(fmt.Sprintf("%s (%d)", sanitize(t.Name), t.RowCount)), func() { v.selectTable(i) }}
	}
	v.showMenu(items, v.table, v.tableX)
}

// dbMenuLabel keeps a menu label drawable: an empty one would read as a separator.
func dbMenuLabel(label string) string {
	if label == "" {
		return " "
	}
	return clip(label, 60)
}

func (v *dbViewer) showMenu(items []menuItem, selected, x int) {
	v.menu = &popupMenu{x: max(1, x), y: v.barRows + 1, items: items, selected: clampIndex(selected, len(items))}
	v.focus = dbMenu
}

func (v *dbViewer) menuKey(k []byte) {
	menu := v.menu
	if menu == nil {
		v.focus = dbGrid
		return
	}
	switch {
	case isEsc(k) || (len(k) == 1 && k[0] == 'q'):
		v.menu, v.focus = nil, dbGrid
	case isUp(k):
		menu.move(-1)
	case isDown(k):
		menu.move(1)
	case isEnter(k):
		v.menu, v.focus = nil, dbGrid
		if i := menu.selected; i >= 0 && i < len(menu.items) && menu.items[i].act != nil {
			menu.items[i].act()
		}
	}
}

// MARK: keys and mouse

// handleKey: see helpKeys for the list. In the SQL box the keys edit the statement, Enter
// runs it as the app's field does, Alt+Enter breaks the line and Esc leaves the box.
func (v *dbViewer) handleKey(k []byte) bool {
	// Ctrl-C quits, except in the SQL box, where it copies the selection.
	if len(k) == 1 && k[0] == 0x03 && v.focus != dbEditor {
		return true
	}
	if m, ok := parseMouse(k); ok {
		v.mouse(m)
		return false
	}
	switch v.focus {
	case dbApps:
		switch pkg, done := v.apps.key(k); {
		case pkg != "":
			v.focus = dbGrid
			v.selectApp(pkg)
		case done:
			v.focus = dbGrid
		}
		return false
	case dbMenu:
		v.menuKey(k)
		return false
	case dbHelp:
		if isEsc(k) || isEnter(k) || (len(k) == 1 && (k[0] == 'q' || k[0] == '?')) {
			v.focus = dbGrid
		}
		return false
	}
	if isSQLRunKey(k) {
		v.runQuery()
		return false
	}
	if v.focus == dbEditor && !v.showsSQL() { // the box went with what it was shown over
		v.focus = dbGrid
	}
	if v.focus == dbEditor {
		v.editorKey(k)
		return false
	}
	if isEsc(k) {
		switch {
		case v.detail:
			v.detail = false
		case v.back != nil:
			v.leave(false)
			v.back()
		}
		return false
	}
	if isEnter(k) {
		if v.appID == "" {
			v.openApps()
		} else if v.showsGrid() {
			v.toggleRow(v.cursor)
		}
		return false
	}
	if nav, ok := decodeNav(k); ok {
		switch nav.dir {
		case "up":
			v.moveTo(v.cursor - 1)
		case "down":
			v.moveTo(v.cursor + 1)
		case "left":
			v.scrollColumns(-1)
		case "right":
			v.scrollColumns(1)
		case "home":
			v.moveTo(0)
		case "end":
			v.moveTo(v.rowCount() - 1)
		case "pgup":
			v.pageBy(-1)
		case "pgdn":
			v.pageBy(1)
		}
		return false
	}
	if string(k) == "\x1b[Z" { // Shift-Tab
		v.setTab(v.tab - 1)
		return false
	}
	if len(k) != 1 {
		return false
	}
	switch k[0] {
	case 'q':
		return true
	case 'k':
		v.moveTo(v.cursor - 1)
	case 'j':
		v.moveTo(v.cursor + 1)
	case 'h':
		v.scrollColumns(-1)
	case 'l':
		v.scrollColumns(1)
	case 'g':
		v.moveTo(0)
	case 'G':
		v.moveTo(v.rowCount() - 1)
	case '\t':
		v.setTab(v.tab + 1)
	case '1', '2':
		v.setTab(int(k[0] - '1'))
	case 'a':
		v.openApps()
	case 'd':
		v.openDBMenu()
	case 't':
		v.openTableMenu()
	case 'r':
		if v.appID != "" {
			v.refresh()
		}
	case 's', '/':
		if v.showsSQL() {
			v.focus = dbEditor
		}
	case '[', 'p':
		v.prevPage()
	case ']', 'n':
		v.nextPage()
	case 'C':
		v.copyRow()
	case '?':
		v.focus = dbHelp
	}
	return false
}

// pageBy scrolls the row detail when it is open, else moves the cursor a page.
func (v *dbViewer) pageBy(dir int) {
	if v.detail {
		v.detailTop = max(0, v.detailTop+dir*max(1, v.detailRoom-1))
		return
	}
	v.moveTo(v.cursor + dir*max(1, v.gridRows))
}

func (v *dbViewer) editorKey(k []byte) {
	switch {
	case isEsc(k):
		v.focus = dbGrid
	case isEnter(k):
		v.runQuery()
	case string(k) == "\x1b\r", string(k) == "\x1b\n": // Alt+Enter: a line break
		v.sql.handle([]byte{'\n'}, 1)
	case len(k) == 1 && k[0] == 0x03, string(k) == "\x1b[99;9u": // Ctrl-C or ⌘C copies the selection
		if text := v.sql.selectedText(); text != "" {
			v.copy(text)
		}
	case len(k) == 1 && k[0] == 0x18, string(k) == "\x1b[120;9u": // Ctrl-X or ⌘X cuts it
		if text := v.sql.cut(); text != "" {
			v.copy(text)
		}
	default:
		v.sql.handle(k, max(1, v.editRows-1))
	}
}

func (v *dbViewer) mouse(m mouseEvent) {
	const (
		wheelUp, wheelDown, wheelLeft, wheelRight = 64, 65, 66, 67
		shift, wheelLines                         = 4, 3
	)
	// A drag over the detail runs until the button is released (the report's final byte in SGR
	// mode; some terminals send button 3 instead).
	released := !m.press || (m.button&mouseDrag == 0 && m.button&3 == 3)
	if v.dsel.dragging && (m.button&mouseDrag != 0 || released) {
		v.detailDrag(m, released, v.copy)
		return
	}
	if !m.press {
		return
	}
	switch v.focus {
	case dbHelp:
		if m.button == 0 {
			v.focus = dbGrid
		}
		return
	case dbMenu:
		menu := v.menu
		v.menu, v.focus = nil, dbGrid
		if menu == nil || m.button != 0 {
			return
		}
		if i := menu.itemAt(m.x, m.y); i >= 0 && i < len(menu.items) && menu.items[i].act != nil {
			menu.items[i].act()
		}
		return
	case dbApps:
		switch pkg, done := v.apps.mouse(m); {
		case pkg != "":
			v.focus = dbGrid
			v.selectApp(pkg)
		case done:
			v.focus = dbGrid
		}
		return
	}
	inDetail := v.detailX0 > 0 && m.x >= v.detailX0 && m.y >= v.detailY0
	inEditor := v.editRows > 0 && m.y >= v.editTop && m.y < v.editTop+v.editRows
	switch m.button {
	case wheelUp, wheelDown:
		by := wheelLines
		if m.button == wheelUp {
			by = -wheelLines
		}
		switch {
		case inDetail:
			v.detailTop = max(0, v.detailTop+by)
		case inEditor:
			v.sql.ensure()
			v.sql.row = max(0, min(v.sql.row+by, len(v.sql.lines)-1))
			v.sql.col = min(v.sql.col, len(v.sql.lines[v.sql.row]))
		default:
			v.top, v.scrolled = max(0, v.top+by), true
		}
		return
	case wheelLeft, wheelUp | shift: // the wheel sideways, or with Shift
		v.scrollColumns(-1)
		return
	case wheelRight, wheelDown | shift:
		v.scrollColumns(1)
		return
	}
	if m.button != 0 && m.button != 2 {
		return
	}
	v.focus = dbGrid
	if m.button == 0 {
		for i := len(v.clicks) - 1; i >= 0; i-- {
			if c := v.clicks[i]; m.y >= c.y0 && m.y <= c.y1 && m.x >= c.x0 && m.x <= c.x1 {
				c.act()
				return
			}
		}
	}
	row := m.y - v.gridTop + v.top
	switch {
	case inEditor && m.button == 0:
		if m.x >= v.editX && m.x < v.editX+v.editW {
			v.focus = dbEditor
			v.sql.click(m.y-v.editTop, m.x-v.editX)
		}
	case inDetail && m.button == 0:
		v.detailPress(m, v.copy)
	case inDetail:
		if menu := v.detailMenu(m, v.copy); menu != nil {
			v.menu, v.focus = menu, dbMenu
		}
	case m.button == 0 && v.appID == "" && m.y > v.barRows:
		v.openApps()
	case m.button == 0 && v.gridW > 0 && m.x <= v.gridW && m.y >= v.gridTop && m.y < v.gridTop+v.gridRows && row < v.rowCount():
		v.toggleRow(row)
	}
}

// helpKeys are the viewer's keys for the ? popup. The labels are the app's where it has one;
// "Row details" has none, and the rest are the placeholders the other panes use.
func (v *dbViewer) helpKeys() [][2]string {
	return [][2]string{
		{"a", dbPickApp},
		{"d", "DB"},
		{"t", "Table"},
		{"r", "Refresh"},
		{"s", dbSQLHint},
		{"Ctrl+R  ⌘↩", "Run"},
		{"↑ ↓ PgUp PgDn Home End", "Select row"},
		{"← →", "Scroll"},
		{"Enter", "Row details"},
		{"Tab  1-2", "Switch tab"},
		{"C", dbCopyJSON},
		{"[  p", "Prev"},
		{"]  n", "Next"},
		{"Esc", "Close / back"},
		{"?", "Help"},
		{"q", "Quit"},
	}
}

// MARK: drawing

func (v *dbViewer) draw() {
	rows, cols := termSize()
	frame := v.screenRows(rows, cols)
	switch {
	case v.focus == dbHelp:
		if box := keysBox(v.helpKeys(), rows, cols); len(box) > 0 {
			frame = overlay(frame, box, max(0, (rows-len(box))/2), max(0, (cols-cellWidth(stripSGR(box[0])))/2))
		}
	case v.focus == dbMenu && v.menu != nil:
		if box := v.menu.box(rows, cols); len(box) > 0 {
			frame = overlay(frame, box, v.menu.top-1, v.menu.left-1)
		}
	}
	paintRows(v.snack.over(frame, rows, cols))
}

// screenRows is the pane without its popups: at most rows rows, none wider than cols.
func (v *dbViewer) screenRows(rows, cols int) []string {
	if rows < 1 || cols < 1 {
		return nil
	}
	var frame []string
	if v.focus == dbApps {
		frame = v.apps.frame(rows, cols)
	} else {
		frame = v.frame(rows, cols)
	}
	if len(frame) > rows {
		frame = frame[:rows]
	}
	// Whatever a row was built from, it never reaches past the pane.
	for i, row := range frame {
		if plain := stripSGR(row); cellWidth(plain) > cols {
			frame[i] = clip(plain, cols)
		}
	}
	return frame
}

// frame draws the viewer: the toolbar, what the app shows under it, and the status bar.
func (v *dbViewer) frame(rows, cols int) []string {
	v.clicks = v.clicks[:0]
	v.dbX, v.tableX, v.editRows, v.gridW, v.gridRows, v.detailX0, v.detailY0 = 0, 0, 0, 0, 0, 0, 0
	v.barRows = toolbarRows(rows)
	frame := v.toolbar(cols, v.barRows)
	body := max(0, rows-len(frame)-1)
	frame = append(frame, v.content(cols, body, len(frame)+1)...)
	return append(frame, v.statusBar(cols, rows))
}

// toolbar is the app's: the app button, the DB and Table pickers, and at the right the
// loading mark, "snapshot" and Refresh.
func (v *dbViewer) toolbar(cols, height int) []string {
	b := &barBuilder{cols: cols, tall: height >= 3}
	b.chip(clip(sanitize(v.appLabel()), 28)+" ▼", sgrBold, v.openApps)
	// picker adds a titled control and returns the column its menu opens at, 0 when it
	// doesn't fit.
	picker := func(title, selection string, act func()) int {
		label := clip(selection, 28) + " ▼"
		if b.used+min(b.used, 1)+len(title)+1+b.chipWidth(label) > cols {
			return 0
		}
		blank := strings.Repeat(" ", len(title))
		b.add(len(title), blank, sgrDim+title+sgrReset, blank, nil)
		b.chip(label, "", act)
		return b.used - b.chipWidth(label) + 1
	}
	if len(v.databases) > 0 {
		selection := dbNone
		if v.selectedDB >= 0 && v.selectedDB < len(v.databases) {
			selection = sanitize(v.databases[v.selectedDB].Name)
		}
		v.dbX = picker("DB", selection, v.openDBMenu)
	}
	if len(v.tables) > 0 {
		selection := dbNone
		if name, ok := v.tableName(); ok {
			selection = sanitize(name)
		}
		v.tableX = picker("Table", selection, v.openTableMenu)
	}

	// The right end, flush right: what doesn't fit is left out, the note first.
	const snapshot, refresh = "snapshot", "Refresh"
	type part struct {
		text, style string
		chip        bool
	}
	var parts []part
	if v.loading() || v.apps.loading {
		parts = append(parts, part{text: cloudGlyphLoading, style: sgrDim})
	}
	if v.selectedDB >= 0 {
		parts = append(parts, part{text: snapshot, style: sgrDim}, part{text: refresh, chip: true})
	}
	width := func(p part) int {
		if p.chip {
			return b.chipWidth(p.text)
		}
		return cellWidth(p.text)
	}
	room := func() int {
		room := cols - b.used - min(b.used, 1)
		for _, p := range parts {
			room -= 1 + width(p)
		}
		return room
	}
	for len(parts) > 0 && room() < 0 {
		drop := 0
		for i, p := range parts {
			if p.text == snapshot {
				drop = i
			}
		}
		parts = append(parts[:drop:drop], parts[drop+1:]...)
	}
	if len(parts) > 0 {
		lead := strings.Repeat(" ", room())
		b.add(len(lead), lead, lead, lead, nil)
		for _, p := range parts {
			if p.chip {
				b.chip(p.text, "", v.refresh)
				continue
			}
			blank := strings.Repeat(" ", width(p))
			b.add(width(p), blank, p.style+p.text+sgrReset, blank, nil)
		}
	}
	for _, h := range b.hits {
		v.clicks = append(v.clicks, dbClick{y0: 1, y1: height, x0: h.x0, x1: h.x1, act: h.act})
	}
	return b.rows()
}

// content is what the app shows under the toolbar, w cells wide and h rows tall, its first row
// on screen row top: a message, or the SQL box over the rows.
func (v *dbViewer) content(w, h, top int) []string {
	switch {
	case h <= 0:
		return nil
	case v.appID == "":
		return dbMessage(dbPickAppHint, false, w, h)
	case v.err != "" && v.result == nil && len(v.tables) == 0:
		return dbMessage(v.err, true, w, h)
	}
	out := v.sqlBar(w, h, top)
	// A failed call under the box, on at most two rows; what was shown before stays.
	if parts := wrapWords(sanitize(v.err), w-2); len(parts) > 0 {
		if len(parts) > 2 {
			parts = []string{parts[0], clip(strings.Join(parts[1:], " "), w-2)}
		}
		for _, part := range parts {
			if len(out) < h-1 || (len(out) < h && h <= 2) {
				out = append(out, " "+sgrRed+part+sgrReset)
			}
		}
	}
	rest, restTop := h-len(out), top+len(out)
	switch {
	case rest <= 0:
	case v.result == nil && v.loading():
		out = append(out, dbMessage(dbLoading, false, w, rest)...)
	case v.result == nil:
		out = append(out, dbMessage(dbNoTable, false, w, rest)...)
	case len(v.result.Columns) == 0:
		out = append(out, dbMessage(dbNoColumns, false, w, rest)...)
	default:
		out = append(out, v.rowsAndDetail(w, rest, restTop)...)
	}
	return out
}

// dbMessage is the app's messageState: a text in the middle of its area, red when critical.
func dbMessage(text string, critical bool, w, h int) []string {
	style := sgrDim
	if critical {
		style = sgrRed
	}
	lines := wrapWords(sanitize(text), max(1, w-2))
	if len(lines) > h {
		lines = lines[:h]
	}
	out := make([]string, 0, h)
	for lead := (h - len(lines)) / 2; len(out) < lead; {
		out = append(out, "")
	}
	for _, line := range lines {
		row, _, _ := centeredSpan(line, style, w)
		out = append(out, row)
	}
	for len(out) < h {
		out = append(out, "")
	}
	return out
}

// sqlBar is the app's SQL box and its Run button: one to three rows, as many as the statement
// has lines, fewer in a pane with no room for them.
func (v *dbViewer) sqlBar(w, h, top int) []string {
	const run = " Run "
	v.sql.ensure()
	rows := max(1, min(len(v.sql.lines), 3, h-2))
	buttonW := 0
	if w >= len(run)+12 {
		buttonW = len(run) + 1
	}
	editW := w - 1 - buttonW
	if editW < 1 {
		return []string{""}
	}
	focused := v.focus == dbEditor
	gutter := sgrDim
	if focused {
		gutter = sgrBold
	}
	var lines []string
	if v.sql.String() == "" {
		// The placeholder, behind the cursor when the box has the keys.
		first := sgrDim + fit(dbSQLHint, editW) + sgrReset
		if focused {
			first = sgrRev + " " + sgrReset + sgrDim + fit(dbSQLHint, editW-1) + sgrReset
		}
		lines = append(lines, first)
		for len(lines) < rows {
			lines = append(lines, strings.Repeat(" ", editW))
		}
	} else {
		for _, line := range v.sql.view(editW, rows, focused) {
			lines = append(lines, padStyled(dbDropUnsafe(line), editW))
		}
	}
	v.editTop, v.editRows, v.editX, v.editW = top, len(lines), 2, editW
	out := make([]string, 0, len(lines))
	for i, line := range lines {
		row := gutter + "│" + sgrReset + line
		if i == 0 && buttonW > 0 {
			row += " " + sgrGreen + sgrUnder + run + sgrReset
			v.clicks = append(v.clicks, dbClick{y0: top, y1: top, x0: w - len(run) + 1, x1: w, act: v.runQuery})
		}
		out = append(out, row)
	}
	return out
}

// rowsAndDetail is the grid with the pagination bar under it and, when a row is open, its
// detail at the right. Too narrow to split, the detail takes the pane.
func (v *dbViewer) rowsAndDetail(w, n, top int) []string {
	open := v.detail && v.cursor >= 0 && v.cursor < v.rowCount()
	listW := w
	if open {
		listW = max(40, w*9/20)
		if w-listW < 30 {
			listW = 0
		}
		if listW == 0 && w < len(escClose)+4 { // no room for the detail either: the rows stay
			open, listW = false, w
		}
	}
	list := v.gridColumn(listW, n, top)
	if !open {
		return list
	}
	x := listW + 1 // the detail's first column on screen
	if listW > 0 {
		x++
	}
	detail := v.detailColumn(w-x+1, n, x, top)
	out := make([]string, 0, n)
	for i := 0; i < n && i < len(detail) && i < len(list); i++ {
		left := ""
		if listW > 0 {
			left = list[i] + borderStyle + "│" + sgrReset
		}
		out = append(out, left+detail[i])
	}
	return out
}

// gridColumn draws the rows w cells wide and n rows tall, its first row on screen row top: the
// column names, the rows, and the pagination bar while a table is selected.
func (v *dbViewer) gridColumn(w, n, top int) []string {
	out := make([]string, 0, n)
	rs := v.result
	if w <= 0 || n <= 0 || rs == nil {
		for len(out) < n {
			out = append(out, "")
		}
		return out
	}
	_, paged := v.tableName()
	paged = paged && n >= 3
	room := n - 1
	if paged {
		room--
	}
	// As many whole columns as fit; the grid scrolls sideways until the last one is whole.
	v.maxLeft = max(0, len(rs.Columns)-max(1, (w+1)/(dbCellW+1)))
	v.leftCol = max(0, min(v.leftCol, v.maxLeft))
	// cells lays one row out: text gives a column's cell, already styled, for its width.
	cells := func(text func(col, width int) string) string {
		var b strings.Builder
		used := 0
		for col := v.leftCol; col < len(rs.Columns) && used < w; col++ {
			cw := min(dbCellW, w-used)
			b.WriteString(text(col, cw))
			if used += cw; used < w {
				b.WriteString(" ")
				used++
			}
		}
		return b.String() + strings.Repeat(" ", max(0, w-used))
	}
	header := cells(func(col, cw int) string { return dbCellText(rs.Columns[col], cw) })
	out = append(out, onPanel(sgrBold+header+sgrReset, w))

	limit := max(0, len(rs.Rows)-room)
	v.cursor = clampIndex(v.cursor, len(rs.Rows))
	switch {
	case v.scrolled:
	case v.cursor < v.top:
		v.top = v.cursor
	case v.cursor >= v.top+room:
		v.top = v.cursor - room + 1
	}
	v.top = max(0, min(v.top, limit))
	v.gridTop, v.gridRows, v.gridW = top+1, max(0, room), w
	for i := v.top; i < len(rs.Rows) && len(out) < 1+room; i++ {
		row := rs.Rows[i]
		selected := i == v.cursor
		line := cells(func(col, cw int) string {
			switch {
			case col >= len(row):
				return strings.Repeat(" ", cw)
			case row[col] == nil && !selected:
				return sgrDim + fit("NULL", cw) + sgrReset
			case row[col] == nil:
				return fit("NULL", cw)
			}
			return dbCellText(*row[col], cw)
		})
		if selected {
			line = sgrRev + sgrBold + line + sgrReset
		}
		out = append(out, line)
	}
	for len(out) < 1+room {
		out = append(out, strings.Repeat(" ", w))
	}
	if paged {
		out = append(out, v.paginationBar(w, top+len(out)))
	}
	for len(out) < n {
		out = append(out, strings.Repeat(" ", w))
	}
	return out[:n]
}

// paginationBar is the app's: Prev, the page, Next, and the row count at the right. A button
// that has nowhere to go is dim.
func (v *dbViewer) paginationBar(w, y int) string {
	var b strings.Builder
	used := 0
	add := func(text, style string, act func()) {
		if used+1+cellWidth(text) > w {
			return
		}
		b.WriteString(" " + style + text + sgrReset)
		if act != nil {
			v.clicks = append(v.clicks, dbClick{y0: y, y1: y, x0: used + 2, x1: used + 1 + cellWidth(text), act: act})
		}
		used += 1 + cellWidth(text)
	}
	button := func(label string, enabled bool, act func()) {
		if enabled {
			add(label, sgrUnder, act)
		} else {
			add(label, sgrDim, nil)
		}
	}
	button("Prev", v.page > 0, v.prevPage)
	add(fmt.Sprintf("page %d", v.page+1), sgrDim, nil)
	button("Next", v.hasNextPage(), v.nextPage)
	count := fmt.Sprintf("%d rows", v.rowCount())
	if room := w - used - cellWidth(count) - 1; room >= 2 {
		return b.String() + strings.Repeat(" ", room) + sgrDim + count + sgrReset + " "
	}
	return b.String() + strings.Repeat(" ", max(0, w-used))
}

// detailLines is the open row for the open tab, w cells wide: each column's name over its
// value, or the row as JSON, a line of it to a value. No value is laid out over more than
// cloudDetailLines lines, so a huge cell costs what a long one does; what is copied is whole.
func (v *dbViewer) detailLines(w int) (lines []string, meta []detailLine) {
	c := &v.cache
	if c.lines != nil && c.results == v.results && c.row == v.cursor && c.tab == v.tab && c.w == w {
		return c.lines, c.meta
	}
	lines, meta = []string{}, []detailLine{}
	add := func(line, value string, cont bool, group int) {
		lines = append(lines, line)
		meta = append(meta, detailLine{value: value, cont: cont, group: group})
	}
	// wrapped adds a value over the lines it needs, up to the cap. Its line breaks stay line
	// breaks; with group, the lines are one value to a double click.
	wrapped := func(value string, group int) {
		n := 0
		for _, para := range strings.Split(dbHead(value, cloudDetailLines*(w+1)), "\n") {
			for i, part := range wrapCells(para, w) {
				if n == cloudDetailLines {
					return
				}
				add(part, value, i > 0, group)
				n++
			}
		}
	}
	rs := v.result
	if rs != nil && v.cursor >= 0 && v.cursor < len(rs.Rows) {
		row := rs.Rows[v.cursor]
		switch v.tab {
		case dbTabJSON:
			most := cloudDetailLines * (len(rs.Columns) + 2)
			for _, line := range strings.Split(dbRowJSON(rs.Columns, row), "\n") {
				if len(lines) >= most {
					break
				}
				wrapped(line, 0)
			}
		default:
			for i := 0; i < len(rs.Columns) && i < len(row); i++ {
				add(sgrDim+clip(sanitize(rs.Columns[i]), w)+sgrReset, "", false, 0)
				if row[i] == nil {
					add(sgrDim+"NULL"+sgrReset, "NULL", false, 0)
				} else {
					wrapped(*row[i], i+1)
				}
				add("", "", false, 0)
			}
		}
	}
	*c = dbDetailCache{results: v.results, row: v.cursor, tab: v.tab, w: w, lines: lines, meta: meta}
	return lines, meta
}

// detailColumn draws the row detail w cells wide and n rows tall, its first cell at screen
// column x and row top: the close button, the tabs and Copy JSON, then the open tab's content.
func (v *dbViewer) detailColumn(w, n, x, top int) []string {
	out := make([]string, 0, n)
	v.detailMeta, v.detailDrawn, v.detailText = nil, nil, nil
	if w < len(escClose)+4 || n < 1 {
		for len(out) < n {
			out = append(out, "")
		}
		return out
	}
	v.detailX0, v.detailY0, v.detailX = x, top, x+1
	title := ""
	if name, ok := v.tableName(); ok {
		title = sanitize(name)
	}
	titleW := w - len(escClose) - 2
	v.clicks = append(v.clicks, dbClick{y0: top, y1: top, x0: x + 1 + titleW, x1: x + titleW + len(escClose), act: func() { v.detail = false }})
	out = append(out, " "+sgrBold+fit(title, titleW)+sgrReset+closeButton(escClose)+" ")

	// The tabs at the left of the second row and Copy JSON at its right.
	const copyLabel = " " + dbCopyJSON + " "
	copyW := 0
	if w >= len(copyLabel)+2 {
		copyW = len(copyLabel) + 1
	}
	var tabs strings.Builder
	used := 1
	tabs.WriteString(" ")
	for i, name := range dbTabs {
		if used+len(name)+3 > w-copyW {
			break
		}
		style := ""
		if i == v.tab {
			style = sgrBold + sgrRev
		}
		tabs.WriteString(style + " " + name + " " + sgrReset + " ")
		if n < 2 {
			continue
		}
		v.clicks = append(v.clicks, dbClick{y0: top + 1, y1: top + 1, x0: x + used, x1: x + used + len(name) + 1, act: func() { v.setTab(i) }})
		used += len(name) + 3
	}
	second := tabs.String() + strings.Repeat(" ", max(0, w-used-copyW))
	if copyW > 0 {
		second += closeButton(copyLabel) + " "
		if n >= 2 {
			v.clicks = append(v.clicks, dbClick{y0: top + 1, y1: top + 1, x0: x + w - copyW, x1: x + w - 2, act: v.copyRow})
		}
	}
	out = append(out, second, strings.Repeat(" ", w))
	if len(out) > n {
		out = out[:n]
	}

	v.detailFirst = top + len(out)
	lines, meta := v.detailLines(w - 2)
	out = append(out, v.detailRows(lines, meta, w, n-len(out))...)
	for len(out) < n {
		out = append(out, strings.Repeat(" ", w))
	}
	return out
}

// statusBar is the other viewers': the device at the right, with Help beside it.
func (v *dbViewer) statusBar(cols, rows int) string {
	name := clip(sanitize(v.device.displayModel()), cols)
	const help, gap = "Help", 3
	button := ""
	if cols-cellWidth(name)-len(help)-gap >= 1 {
		button = sgrUnder + help + sgrReset + strings.Repeat(" ", gap)
		x0 := cols - cellWidth(name) - gap - len(help) + 1
		v.clicks = append(v.clicks, dbClick{y0: rows, y1: rows, x0: x0, x1: x0 + len(help) - 1, act: func() { v.focus = dbHelp }})
	}
	pad := strings.Repeat(" ", max(0, cols-cellWidth(name)-cellWidth(stripSGR(button))))
	return pad + button + sgrDim + name + sgrReset
}
