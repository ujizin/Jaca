package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The Projects area's wire types (ProjectsState, Project and ProjectCheckout in
// Sources/Core/Projects) and the values the app derives from them. Nothing here draws.

const projectsTopic = "projects.state"

// wireTime is a date from jacad: ISO-8601, with or without fractional seconds. A value that
// is missing or can't be read stays zero, which stands for no date.
type wireTime struct{ time.Time }

func (t *wireTime) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) != nil {
		return nil
	}
	if parsed, err := time.Parse(time.RFC3339Nano, s); err == nil {
		t.Time = parsed
	}
	return nil
}

// checkoutRow mirrors ProjectCheckout: the main working tree or a linked worktree.
type checkoutRow struct {
	Path              string          `json:"path"`
	IsMain            bool            `json:"isMain"`
	Branch            string          `json:"branch"`
	Base              string          `json:"base"`
	Age               string          `json:"age"`
	Orphan            bool            `json:"orphan"`
	OrphanKind        json.RawMessage `json:"orphanKind"`
	IsClaudeManaged   bool            `json:"isClaudeManaged"`
	HasClaudeSessions bool            `json:"hasClaudeSessions"`
	ClaudeLastActive  wireTime        `json:"claudeLastActive"`
	LastCommit        wireTime        `json:"lastCommit"`
	SizeMB            int             `json:"sizeMB"`
	CacheMB           int             `json:"cacheMB"`
	SizeComputed      bool            `json:"sizeComputed"`
	Cleaning          bool            `json:"cleaning"`
	Dropped           bool            `json:"dropped"`
	Removing          bool            `json:"removing"`
}

// name matches ProjectCheckout.name.
func (c checkoutRow) name() string {
	if c.Branch != "" {
		return c.Branch
	}
	if c.IsMain {
		return "main checkout"
	}
	return filepath.Base(c.Path)
}

func (c checkoutRow) displayPath() string { return tildePath(c.Path) }

// lastModified is the date a checkout is ordered by: its last commit, else its last Claude
// session (ProjectCheckout.lastModified).
func (c checkoutRow) lastModified() time.Time {
	if !c.LastCommit.IsZero() {
		return c.LastCommit.Time
	}
	return c.ClaudeLastActive.Time
}

// detached reports the orphan kind. OrphanKind is a Swift enum with a synthesized encoding
// whose shape is not pinned down ({"detached":{}} or "detached"), so the raw JSON is searched
// for the case's name; anything else is the other kind, a branch that is gone.
func (c checkoutRow) detached() bool {
	return bytes.Contains(c.OrphanKind, []byte(`"detached"`))
}

// typeTag is the row's first tag (ProjectCheckoutRow.typeTag).
func (c checkoutRow) typeTag() string {
	if c.IsMain {
		return "folder"
	}
	return "worktree"
}

// subtitle is the row's second line (ProjectCheckoutRow.subtitle).
func (c checkoutRow) subtitle(now time.Time) string {
	parts := []string{c.displayPath()}
	switch {
	case c.Orphan && c.detached():
		parts = append(parts, "detached HEAD")
	case c.Orphan:
		parts = append(parts, "branch gone")
	case c.Base != "":
		parts = append(parts, "from "+c.Base)
	}
	if c.Age != "" {
		parts = append(parts, c.Age)
	} else if !c.ClaudeLastActive.IsZero() {
		parts = append(parts, relativeAge(c.ClaudeLastActive.Time, now))
	}
	return strings.Join(parts, " · ")
}

// sizeText is the row's size column (ProjectCheckoutRow.sizeText).
func (c checkoutRow) sizeText() string {
	if c.Cleaning {
		return "cleaning…"
	}
	if c.SizeComputed {
		return formatSize(c.SizeMB)
	}
	return "—"
}

// projectRow mirrors Project: a folder and its checkouts, the main one first.
type projectRow struct {
	Path         string        `json:"path"`
	IsGitRepo    bool          `json:"isGitRepo"`
	Source       string        `json:"source"`
	SessionCount int           `json:"sessionCount"`
	LastActive   wireTime      `json:"lastActive"`
	Checkouts    []checkoutRow `json:"-"`
}

// name matches Project.name.
func (p projectRow) name() string {
	if n := filepath.Base(p.Path); n != "" && n != "/" && n != "." {
		return n
	}
	return p.Path
}

func (p projectRow) displayPath() string { return tildePath(p.Path) }

// isUser reports a folder the user added. A source jacad didn't send reads as one, as in the
// app's decoder.
func (p projectRow) isUser() bool { return p.Source != "claude" }

func (p projectRow) worktreeCount() int {
	n := 0
	for _, c := range p.Checkouts {
		if !c.IsMain {
			n++
		}
	}
	return n
}

// isClaudeProject matches Project.isClaudeProject, which shows the Claude tag.
func (p projectRow) isClaudeProject() bool {
	if p.Source == "claude" || p.SessionCount > 0 {
		return true
	}
	for _, c := range p.Checkouts {
		if c.HasClaudeSessions || c.IsClaudeManaged {
			return true
		}
	}
	return false
}

func (p projectRow) totalSizeMB() int {
	total := 0
	for _, c := range p.Checkouts {
		total += c.SizeMB
	}
	return total
}

// sizesComputed matches Project.sizesComputed: there are checkouts and each has a size.
func (p projectRow) sizesComputed() bool {
	for _, c := range p.Checkouts {
		if !c.SizeComputed {
			return false
		}
	}
	return len(p.Checkouts) > 0
}

// effectiveLastActive is the latest activity of the root and its checkouts, zero for none.
func (p projectRow) effectiveLastActive() time.Time {
	latest := p.LastActive.Time
	for _, c := range p.Checkouts {
		if t := c.lastModified(); t.After(latest) {
			latest = t
		}
	}
	return latest
}

func (p projectRow) checkout(path string) (checkoutRow, bool) {
	for _, c := range p.Checkouts {
		if c.Path == path {
			return c, true
		}
	}
	return checkoutRow{}, false
}

// worktreesTag and projectsTag are ProjectNodeView's count tags.
func worktreesTag(n int) string {
	if n == 1 {
		return "1 worktree"
	}
	return fmt.Sprintf("%d worktrees", n)
}

func projectsTag(n int) string {
	if n == 1 {
		return "1 project"
	}
	return fmt.Sprintf("%d projects", n)
}

// projectsState mirrors ProjectsState.
type projectsState struct {
	Projects         []projectRow
	IsRefreshing     bool
	IsComputingSizes bool
	HasCompletedScan bool
	LastRefresh      time.Time // zero when jacad has not scanned yet
	ScanGeneration   int
}

func (s projectsState) project(path string) (projectRow, bool) {
	for _, p := range s.Projects {
		if p.Path == path {
			return p, true
		}
	}
	return projectRow{}, false
}

// totalWorktrees and sizableCheckouts are ProjectsModel's: the header's count, and the
// checkouts a size scan walks.
func (s projectsState) totalWorktrees() int {
	n := 0
	for _, p := range s.Projects {
		n += p.worktreeCount()
	}
	return n
}

func (s projectsState) sizableCheckouts() int {
	n := 0
	for _, p := range s.Projects {
		if p.IsGitRepo {
			n += len(p.Checkouts)
		}
	}
	return n
}

// lenient drops the error of a field that had another type than expected: the decoder skips
// that field and reads the rest, which is what a record from another version of jacad needs.
func lenient(err error) error {
	var mismatch *json.UnmarshalTypeError
	if errors.As(err, &mismatch) {
		return nil
	}
	return err
}

// decodeProjectsState reads a projects.state. Every field but a path is optional, and a
// project or checkout that can't be read is left out rather than failing the others.
func decodeProjectsState(data []byte) (projectsState, error) {
	var wire struct {
		Projects         []json.RawMessage `json:"projects"`
		IsRefreshing     bool              `json:"isRefreshing"`
		IsComputingSizes bool              `json:"isComputingSizes"`
		HasCompletedScan bool              `json:"hasCompletedScan"`
		LastRefresh      wireTime          `json:"lastRefresh"`
		ScanGeneration   int               `json:"scanGeneration"`
	}
	if err := lenient(json.Unmarshal(data, &wire)); err != nil {
		return projectsState{}, err
	}
	st := projectsState{
		IsRefreshing:     wire.IsRefreshing,
		IsComputingSizes: wire.IsComputingSizes,
		HasCompletedScan: wire.HasCompletedScan,
		LastRefresh:      wire.LastRefresh.Time,
		ScanGeneration:   wire.ScanGeneration,
	}
	for _, raw := range wire.Projects {
		var p projectRow
		var nested struct {
			Checkouts []json.RawMessage `json:"checkouts"`
		}
		if lenient(json.Unmarshal(raw, &p)) != nil || p.Path == "" {
			continue
		}
		_ = json.Unmarshal(raw, &nested)
		for _, rawCheckout := range nested.Checkouts {
			var c checkoutRow
			if lenient(json.Unmarshal(rawCheckout, &c)) == nil && c.Path != "" {
				p.Checkouts = append(p.Checkouts, c)
			}
		}
		st.Projects = append(st.Projects, p)
	}
	return st, nil
}

// projectNode is a project and the projects nested under it in the Tree view.
type projectNode struct {
	project  projectRow
	children []projectNode
}

// recencyBefore orders by date, the latest first; an entry with no date comes after the dated
// ones, and equal dates fall back to the name (ProjectsGrouping.recency).
func recencyBefore(ad time.Time, an string, bd time.Time, bn string) bool {
	switch {
	case !ad.IsZero() && !bd.IsZero():
		if ad.Equal(bd) {
			return an < bn
		}
		return ad.After(bd)
	case !ad.IsZero():
		return true
	case !bd.IsZero():
		return false
	}
	return an < bn
}

func sortNodes(nodes []projectNode) []projectNode {
	sort.SliceStable(nodes, func(i, j int) bool {
		a, b := nodes[i].project, nodes[j].project
		return recencyBefore(a.effectiveLastActive(), a.name(), b.effectiveLastActive(), b.name())
	})
	return nodes
}

// projectTree nests projects by path for the Tree view (ProjectsGrouping.tree): a project
// goes under the project whose path is its longest ancestor directory, and each level is
// ordered by recency.
func projectTree(projects []projectRow) []projectNode {
	paths := map[string]bool{}
	for _, p := range projects {
		paths[p.Path] = true
	}
	nearestParent := func(path string) string {
		best := ""
		for q := range paths {
			if q != path && strings.HasPrefix(path, q+"/") && len(q) > len(best) {
				best = q
			}
		}
		return best
	}
	childrenOf := map[string][]projectRow{}
	var roots []projectRow
	for _, p := range projects {
		if parent := nearestParent(p.Path); parent != "" {
			childrenOf[parent] = append(childrenOf[parent], p)
		} else {
			roots = append(roots, p)
		}
	}
	// A child's path is longer than its parent's, so the walk ends.
	var build func(p projectRow) projectNode
	build = func(p projectRow) projectNode {
		node := projectNode{project: p}
		for _, child := range childrenOf[p.Path] {
			node.children = append(node.children, build(child))
		}
		node.children = sortNodes(node.children)
		return node
	}
	nodes := make([]projectNode, 0, len(roots))
	for _, p := range roots {
		nodes = append(nodes, build(p))
	}
	return sortNodes(nodes)
}

// projectFlat is the List view: every project on its own, in jacad's order.
func projectFlat(projects []projectRow) []projectNode {
	nodes := make([]projectNode, len(projects))
	for i, p := range projects {
		nodes[i] = projectNode{project: p}
	}
	return nodes
}

// homeDir is the user's home directory, for the paths drawn with a tilde.
var homeDir = func() string {
	home, _ := os.UserHomeDir()
	return home
}()

// tildePath is NSString.abbreviatingWithTildeInPath: the home directory reads as ~.
func tildePath(path string) string {
	switch {
	case homeDir == "" || homeDir == "/":
		return path
	case path == homeDir:
		return "~"
	case strings.HasPrefix(path, homeDir+"/"):
		return "~" + path[len(homeDir):]
	}
	return path
}

// relativeAge is a date as the app's RelativeDateTimeFormatter writes it with abbreviated
// units in English ("3 min. ago", "2 hr. ago", "4 days ago"). The app's formatter counts in
// calendar units; this counts in fixed lengths (30 days to a month, 365 to a year), so a date
// near a boundary can read one unit apart from the app's.
func relativeAge(t, now time.Time) string {
	d := now.Sub(t)
	future := d < 0
	if future {
		d = -d
	}
	secs := int64(d / time.Second)
	const minute, hour, day = 60, 3600, 86400
	var n int64
	var unit string
	switch {
	case secs < minute:
		n, unit = secs, "sec"
	case secs < hour:
		n, unit = secs/minute, "min"
	case secs < day:
		n, unit = secs/hour, "hr"
	case secs < 7*day:
		n, unit = secs/day, "days"
		if n == 1 {
			unit = "day"
		}
	case secs < 30*day:
		n, unit = secs/(7*day), "wk"
	case secs < 365*day:
		n, unit = secs/(30*day), "mo"
	default:
		n, unit = secs/(365*day), "yr"
	}
	if future {
		return fmt.Sprintf("in %d %s", n, unit)
	}
	return fmt.Sprintf("%d %s ago", n, unit)
}
