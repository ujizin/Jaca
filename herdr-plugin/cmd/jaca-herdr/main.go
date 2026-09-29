// jaca-herdr is Jaca's Herdr plugin binary: Jaca areas as Herdr panes and actions, all
// served by the jacad daemon over its Unix socket. See ../../README.md.
package main

import (
	"encoding/json"
	"fmt"
	"os"
)

const usage = `usage: jaca-herdr ensure
       jaca-herdr pane gradle|devices|projects
       jaca-herdr action gradle-kill-all
       jaca-herdr action projects-clear-cache`

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	switch {
	case args[0] == "ensure":
		if err := ensureDaemon(); err != nil {
			fmt.Fprintln(os.Stderr, "jaca:", err)
			os.Exit(1)
		}
	case len(args) == 2 && args[0] == "pane" && args[1] == "gradle":
		os.Exit(runGradlePane())
	case len(args) == 2 && args[0] == "pane" && args[1] == "devices":
		os.Exit(runDevicesPane())
	case len(args) == 2 && args[0] == "pane" && args[1] == "projects":
		os.Exit(runProjectsPane())
	case len(args) == 2 && args[0] == "action" && args[1] == "gradle-kill-all":
		os.Exit(killAllGradle())
	case len(args) == 2 && args[0] == "action" && args[1] == "projects-clear-cache":
		os.Exit(clearWorktreeCache())
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
}

// killAllGradle kills every running Gradle daemon, reporting each result on stdout (Herdr
// keeps it in the plugin log).
func killAllGradle() int {
	c, err := dial()
	if err != nil {
		fmt.Fprintln(os.Stderr, "jaca:", err)
		return 1
	}
	defer c.Close()
	var list []gradleDaemon
	if err := c.Call("gradle.list", nil, &list); err != nil {
		fmt.Fprintln(os.Stderr, "jaca:", err)
		return 1
	}
	failed := 0
	for _, d := range list {
		var ok bool
		if err := c.Call("gradle.kill", map[string]any{"pid": d.PID}, &ok); err != nil || !ok {
			failed++
			fmt.Printf("Couldn't kill %d\n", d.PID)
			continue
		}
		fmt.Printf("Killed %d\n", d.PID)
	}
	if failed > 0 {
		return 1
	}
	return 0
}

// herdrContext is the part of HERDR_PLUGIN_CONTEXT_JSON this plugin reads.
type herdrContext struct {
	FocusedPaneCwd string `json:"focused_pane_cwd"`
	WorkspaceCwd   string `json:"workspace_cwd"`
	Worktree       *struct {
		CheckoutPath string `json:"checkout_path"`
		RepoRoot     string `json:"repo_root"`
	} `json:"worktree"`
}

type projectCheckout struct {
	Path string `json:"path"`
}

type project struct {
	Path      string            `json:"path"`
	Checkouts []projectCheckout `json:"checkouts"`
}

// clearWorktreeCache clears build caches for the checkout Herdr's context points at: the
// workspace's worktree, else the focused pane's directory. The same work as the Projects
// area's clear-cache action in the app.
func clearWorktreeCache() int {
	var ctx herdrContext
	if raw := os.Getenv("HERDR_PLUGIN_CONTEXT_JSON"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &ctx)
	}
	target := ""
	if ctx.Worktree != nil && ctx.Worktree.CheckoutPath != "" {
		target = ctx.Worktree.CheckoutPath
	} else if ctx.FocusedPaneCwd != "" {
		target = ctx.FocusedPaneCwd
	} else {
		target = ctx.WorkspaceCwd
	}
	if target == "" {
		fmt.Fprintln(os.Stderr, "jaca: no worktree or directory in the Herdr context")
		return 1
	}

	c, err := dial()
	if err != nil {
		fmt.Fprintln(os.Stderr, "jaca:", err)
		return 1
	}
	defer c.Close()
	var state struct {
		Projects []project `json:"projects"`
	}
	if err := c.Call("projects.state", nil, &state); err != nil {
		fmt.Fprintln(os.Stderr, "jaca:", err)
		return 1
	}
	pid, cid := findCheckout(state.Projects, target)
	if cid == "" {
		fmt.Fprintf(os.Stderr, "jaca: %s is not a checkout Jaca knows\n", target)
		return 1
	}
	var outcome *struct {
		Name    string  `json:"name"`
		FreedMB int     `json:"freedMB"`
		Error   *string `json:"error"`
	}
	if err := c.Call("projects.clearCache", map[string]any{"project": pid, "checkout": cid}, &outcome); err != nil {
		fmt.Fprintln(os.Stderr, "jaca:", err)
		return 1
	}
	if outcome == nil {
		fmt.Fprintln(os.Stderr, "jaca: already cleaning, or the checkout is gone")
		return 1
	}
	// The same messages as ProjectsModel.clearCache.
	if outcome.Error != nil {
		msg := *outcome.Error
		if len([]rune(msg)) > 50 {
			msg = string([]rune(msg)[:50])
		}
		fmt.Printf("Clean failed · %s\n", msg)
		return 1
	}
	fmt.Printf("Freed %s · %s\n", formatSize(outcome.FreedMB), outcome.Name)
	return 0
}

// findCheckout returns the project and checkout ids for the checkout containing path
// (the longest matching checkout path wins, so a worktree beats its parent repo).
func findCheckout(projects []project, path string) (string, string) {
	bestP, bestC := "", ""
	for _, p := range projects {
		for _, c := range p.Checkouts {
			if (path == c.Path || len(path) > len(c.Path) && path[:len(c.Path)+1] == c.Path+"/") && len(c.Path) > len(bestC) {
				bestP, bestC = p.Path, c.Path
			}
		}
	}
	return bestP, bestC
}

// formatSize matches formatSize in Sources/Core/Worktrees/Worktree.swift.
func formatSize(mb int) string {
	if mb >= 1024 {
		return fmt.Sprintf("%.2f GB", float64(mb)/1024)
	}
	return fmt.Sprintf("%d MB", mb)
}
