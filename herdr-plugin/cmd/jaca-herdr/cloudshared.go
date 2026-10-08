package main

import (
	"encoding/json"
	"os"
)

// cloudSessionEnv carries a session to open (a cloudSessionSpec as JSON) to the cloud pane a
// new tab runs. Without it the pane shows the Cloud Logging home.
const cloudSessionEnv = "JACA_HERDR_CLOUD_SESSION"

// cloudSessionSpec is a Cloud Logging session to open: what to stream, the tab's name, and
// whether to start at once (the app opens a new session stopped, and a forked one running).
type cloudSessionSpec struct {
	Config    cloudStreamConfig `json:"config"`
	Name      string            `json:"name,omitempty"`
	AutoStart bool              `json:"autoStart,omitempty"`
}

// underHerdr reports whether the pane runs inside Herdr, where a session opens in its own tab.
func underHerdr() bool {
	return os.Getenv("HERDR_BIN_PATH") != "" || os.Getenv("HERDR_ENV") != ""
}

// launchCloudSession opens a session: in a new Herdr tab named after it, or, with no Herdr to
// open one, in this pane (back returns from it). fail gets why a tab didn't open, and opened
// (when not nil) runs once one did.
func launchCloudSession(p *pane, spec cloudSessionSpec, back func(), fail func(error), opened func()) {
	if !underHerdr() {
		p.screen = newCloudSession(p, spec, back)
		return
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		fail(err)
		return
	}
	name := spec.Name
	if name == "" {
		name = spec.Config.ProjectID
	}
	name = sanitize(name) // a fork is named after a label value, which is the log's text
	go func() {
		err := openTab("cloud", "Jaca cloud - "+name, cloudSessionEnv+"="+string(raw))
		p.post(func() {
			if err != nil {
				fail(err)
			} else if opened != nil {
				opened()
			}
		})
	}()
}
