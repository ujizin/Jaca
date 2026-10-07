package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
)

// openLoginTerminal is the home's Open Terminal. Inside Herdr it opens a new tab and runs the
// sign-in command there, so the login starts without leaving Herdr. Elsewhere it does what the
// app does: it opens Terminal, where the user runs the command.
func openLoginTerminal() error {
	if !underHerdr() {
		return exec.Command("open", "-a", "Terminal").Run()
	}
	herdr := envOr("HERDR_BIN_PATH", "herdr")
	args := []string{"tab", "create", "--focus", "--label", cloudAuthCommand}
	// A plugin pane gets the context JSON; a popup opened by a custom keybinding gets the id alone.
	ws := envOr("HERDR_ACTIVE_WORKSPACE_ID", readHerdrContext().WorkspaceID)
	if ws == "" {
		ws = os.Getenv("HERDR_WORKSPACE_ID")
	}
	if ws != "" {
		args = append(args, "--workspace", ws)
	}
	out, err := exec.Command(herdr, args...).CombinedOutput()
	paneID, err := newTabPane(out, err)
	if err != nil {
		return err
	}
	out, err = exec.Command(herdr, "pane", "run", paneID, cloudAuthCommand).CombinedOutput()
	return herdrError(out, err)
}

// newTabPane reads the id of a new tab's pane from what `herdr tab create` printed
// (.result.root_pane.pane_id), or why the tab didn't open.
func newTabPane(out []byte, runErr error) (string, error) {
	if err := herdrError(out, runErr); err != nil {
		return "", err
	}
	var reply struct {
		Result struct {
			RootPane struct {
				PaneID string `json:"pane_id"`
			} `json:"root_pane"`
		} `json:"result"`
	}
	_ = json.Unmarshal(out, &reply)
	if reply.Result.RootPane.PaneID == "" {
		// The tab is open but can't be addressed, so the command can't be run in it.
		return "", errors.New("herdr tab create: no pane_id")
	}
	return reply.Result.RootPane.PaneID, nil
}

// herdrError is why a herdr command failed: the message of its error reply, else the first
// line it printed, else the error of running it. Nil when it succeeded.
func herdrError(out []byte, runErr error) error {
	var reply struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(out, &reply)
	switch {
	case reply.Error != nil && reply.Error.Message != "":
		return errors.New(reply.Error.Message)
	case runErr != nil:
		if text := firstLine(out); text != "" {
			return errors.New(text)
		}
		return runErr
	}
	return nil
}

func firstLine(out []byte) string {
	return strings.TrimSpace(string(bytes.SplitN(bytes.TrimSpace(out), []byte("\n"), 2)[0]))
}
