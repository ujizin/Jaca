# Jaca for Herdr

Jaca's areas as Herdr panes and actions. The plugin holds no state of its own: every pane and
action is a client of `jacad`, the Jaca daemon, over `~/.jaca/jacad.sock`. The Jaca app and
these panes see the same data because they read the same daemon. See `docs/daemon-plan.md`.

Experimental. Standard library Go only.

## What it adds

| Kind | Id | What it does |
|---|---|---|
| pane | `devices` | A popup with the device list. `Enter` on a ready device shows its options (logs only for now); `Enter` on an option opens it in a new tab named `Jaca log - <device>` and closes the popup. `j`/`k` or arrows move, `Esc` goes back a step and closes the popup from the device list, `q` quits. |
| pane | `logs` | The log viewer for the device chosen in `devices`, with the app log tab's tools. `1`–`6` set the minimum level (V…F), `/` edits the filter text, `r` toggles regex, `s` toggles system logs (not on Android), `P` edits the package id, `a` lists the installed apps to pick one, `c` clears the view, `C` clears the device buffer (Android), `p` or `space` pauses/resumes the stream, `j`/`k` and `PgUp`/`PgDn` scroll, `Shift+PgDn` (or `G`) follows the tail again, and scrolling up leaves it, `e` exports the lines the filter keeps to the file chosen in the macOS save dialog (named after the device, starting in Downloads), `y` opens the app's Copy format sheet as a popup (presets with an example each, the template and date format to edit, a live example; arrows or `Tab` move, `Enter` chooses, `Esc` cancels, nothing is saved before Save), `?` shows the keys (any of `Esc`, `Enter`, `q`, `?` or a click closes it), `q` quits. The level chips, `.*`, `System logs`, `Export` and both fields are clickable, as are the arrow at the end of the package field (the installed apps) and `Help` and `Copy format` at the right of the status bar, the apps list too, and the wheel scrolls. A click on a log line selects it, and dragging extends the selection line by line; past the top or bottom of the lines the view scrolls that way. Letting go copies the selected lines whole, in the app's copy format (with `pbcopy`). The view holds still under a selection, and `Esc`, any key or a click outside the lines drops it. A right click opens the app's row menu on the selected lines (`Copy Line`, `Copy Message only`, `Copy Format…`, `Select All`); `Copy Format…` opens the Copy format popup, which saves to `~/.jaca/log-copy-format.json`, the file the app reads. For that menu the pane asks Herdr to send it right clicks (`herdr pane input --right-click pane`), so Herdr's own pane menu doesn't open on a right click in this pane. The pane draws this selection itself because it has the mouse for the toolbar, so the terminal can't select in it. In a field, `Enter` or `Esc` leaves it and `Ctrl-U` empties it; the package id applies on `Enter`. Opened without a device, it shows the picker first and streams in the same pane (`Esc` goes back). |
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

To open the device picker with a key, add a binding to `~/.config/herdr/config.toml` (pick a free
key) and run `herdr server reload-config`:

```toml
[[keys.command]]
key = "prefix+d"
type = "shell"
command = "herdr plugin pane open --plugin dev.srsouza.jaca --entrypoint devices"
```

`jacad` is looked up at `$JACAD_PATH`, then `/Applications/Jaca.app` and `~/Applications/Jaca.app`.
`JACA_DAEMON_DIR` points every piece (app, daemon, plugin) at another socket directory, which is
how to try a development build without touching the real one:

```bash
export JACA_DAEMON_DIR=~/.jaca/dev JACAD_PATH=/path/to/Debug/Jaca.app/Contents/MacOS/jacad
bin/jaca-herdr pane gradle
```

Remove it with `herdr plugin unlink dev.srsouza.jaca`.

## Colors

Herdr doesn't pass its theme to plugins, so the panes work it out the way Herdr does: the
built-in theme named under `[theme]` in `~/.config/herdr/config.toml`, with the colors of
`[theme.custom]` on top. Popups use what Herdr's own overlays use (`text` on `panel_bg`, an
`accent` border, and its ` esc close ` button at the top right, clickable), and the log colors are the theme's `red`, `green`, `yellow`, `blue`, `teal`
and `mauve`.

The built-in palettes are copied from Herdr's source into `cmd/jaca-herdr/herdrthemes.go`
(generated from v0.9.1's `src/app/state.rs`); regenerate it when Herdr adds or changes a theme.
A theme the table doesn't have, or `terminal`, leaves the terminal palette's colors.

The config is read when a pane starts, so a theme change shows in panes opened after it.
`auto_switch` and `[theme.custom.light]` / `[theme.custom.dark]` are not applied.

## Protocol

Newline-delimited JSON-RPC 2.0 on the socket. `jacad describe` lists every method and topic;
`jacad call METHOD [PARAMS_JSON]` and `jacad watch TOPIC...` are the quickest way to explore.
`protocolVersion` in `cmd/jaca-herdr/rpc.go` must match `DaemonProtocol.version` in the app.

## Copy

Every string a pane shows is taken from the app (the Gradle, device list, device menu, log and
Projects views, `DeviceState.label`, `LogLevel.short`, the Projects toasts). The log toolbar's field placeholders are the app's with a capital first letter. The `?` popup's labels are the app's where it has one; `Minimum level`, `Filter text`, `Regex`, `Package id`, `Pause / unpause`, `Scroll`, `Help` (also the popup's title and the status bar button) and `Quit` are placeholders that need specified copy. Two things have no app copy
yet and are left out until it's specified: a key legend for the other panes, and the size-scan
approval prompt (so the projects pane shows only sizes already computed). The two action titles
and the `logs` pane title in `herdr-plugin.toml` are placeholders written for this prototype and
need specified copy.
