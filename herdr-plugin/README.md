# Jaca for Herdr

Jaca's areas as Herdr panes and actions. The plugin holds no state of its own: every pane and
action is a client of `jacad`, the Jaca daemon, over `~/.jaca/jacad.sock`. The Jaca app and
these panes see the same data because they read the same daemon. See `docs/daemon-plan.md`.

Experimental. Standard library Go only.

## What it adds

| Kind | Id | What it does |
|---|---|---|
| pane | `devices` | The device list; `Enter` streams the selected device's logs in the same pane. While streaming: `space` stops/starts, `1`–`6` set the minimum level (V…F), `j`/`k` scroll, `G` follows the tail again, `Esc` goes back (and closes the stream), `q` quits. |
| pane | `gradle` | Live Gradle daemons from the daemon's `gradle.daemons` topic. `j`/`k` or arrows move, `x` twice kills, `r` refreshes, `q` quits. |
| pane | `projects` | Projects and their checkouts with cached sizes. `c` cleans the selected checkout's build caches, `r` rescans, `q` quits. |
| action | `gradle-kill-all` | Kills every running Gradle daemon. |
| action | `projects-clear-cache` | Clears build caches for the worktree in the Herdr context (the workspace's worktree, else the focused pane's directory). |
| startup | | `jaca-herdr ensure`: starts `jacad` if it isn't running. |

## Install

Full steps, including uninstalling: **[INSTALL.md](INSTALL.md)**. The short version:

Needs Go 1.22+ and a Jaca build that ships `jacad` (`Jaca.app/Contents/MacOS/jacad`).

```bash
cd herdr-plugin
go build -o bin/jaca-herdr ./cmd/jaca-herdr
herdr plugin link "$PWD"
herdr plugin pane open --plugin dev.srsouza.jaca --entrypoint devices
```

`jacad` is looked up at `$JACAD_PATH`, then `/Applications/Jaca.app` and `~/Applications/Jaca.app`.
`JACA_DAEMON_DIR` points every piece (app, daemon, plugin) at another socket directory, which is
how to try a development build without touching the real one:

```bash
export JACA_DAEMON_DIR=~/.jaca/dev JACAD_PATH=/path/to/Debug/Jaca.app/Contents/MacOS/jacad
bin/jaca-herdr pane gradle
```

Remove it with `herdr plugin unlink dev.srsouza.jaca`.

## Protocol

Newline-delimited JSON-RPC 2.0 on the socket. `jacad describe` lists every method and topic;
`jacad call METHOD [PARAMS_JSON]` and `jacad watch TOPIC...` are the quickest way to explore.
`protocolVersion` in `cmd/jaca-herdr/rpc.go` must match `DaemonProtocol.version` in the app.

## Copy

Every string a pane shows is taken from the app (the Gradle, device list, log and Projects
views, `DeviceState.label`, `LogLevel.short`, the Projects toasts). Two things have no app copy
yet and are left out until it's specified: a key legend for the panes, and the size-scan
approval prompt (so the projects pane shows only sizes already computed). The two action titles
in `herdr-plugin.toml` are placeholders written for this prototype and need specified copy.
