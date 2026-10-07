# Jaca for Herdr

Jaca's areas as Herdr panes and actions. The plugin holds no state of its own: every pane and
action is a client of `jacad`, the Jaca daemon, over `~/.jaca/jacad.sock`. The Jaca app and
these panes see the same data because they read the same daemon. See `docs/daemon-plan.md`.

Experimental. Standard library Go only.

## What it adds

| Kind | Id | What it does |
|---|---|---|
| pane | `devices` | A popup with the device list. `Enter` on a ready device shows its options (logs only for now); `Enter` on an option opens it in a new tab named `Jaca log - <device>` and closes the popup. `j`/`k` or arrows move, `Esc` goes back a step and closes the popup from the device list, `q` quits. |
| pane | `logs` | The log viewer for the device chosen in `devices`, with the app log tab's tools. `1`–`6` set the minimum level (V…F), `/` edits the filter text, `r` toggles regex, `s` toggles system logs (not on Android), `P` edits the package id, `a` lists the installed apps to pick one, `c` clears the view, `C` clears the device buffer (Android), `p` or `space` pauses/resumes the stream, `j`/`k` and `PgUp`/`PgDn` scroll, `Shift+PgDn` (or `G`) follows the tail again, and scrolling up leaves it, `S` exports the lines the filter keeps to the file chosen in the macOS save dialog (named after the device, starting in Downloads), `y` opens the app's Copy format sheet as a popup (presets with an example each, the template and date format to edit, a live example; arrows or `Tab` move, `Enter` chooses, `Ctrl+S` or `⌘S` saves, `Esc` cancels, nothing is saved before Save), `?` shows the keys (any of `Esc`, `Enter`, `q`, `?` or a click closes it), `q` quits. The level chips, `.*`, `System logs`, `Export` and both fields are clickable, as are the arrow at the end of the package field (the installed apps) and `Help` and `Copy format` at the right of the status bar, the apps list too, and the wheel scrolls. A click on a log line selects it, and dragging extends the selection line by line; past the top or bottom of the lines the view scrolls that way. Letting go copies the selected lines whole, in the app's copy format (with `pbcopy`). The view holds still under a selection, and `Esc`, any key or a click outside the lines drops it. A right click opens the app's row menu on the selected lines (`Copy Line`, `Copy Message only`, `Copy Format…`, `Select All`); `Copy Format…` opens the Copy format popup, which saves to `~/.jaca/log-copy-format.json`, the file the app reads. For that menu the pane asks Herdr to send it right clicks (`herdr pane input --right-click pane`), so Herdr's own pane menu doesn't open on a right click in this pane. The pane draws this selection itself because it has the mouse for the toolbar, so the terminal can't select in it. In a field, `Enter` or `Esc` leaves it and `Ctrl-U` empties it; the package id applies on `Enter`. Opened without a device, it shows the picker first and streams in the same pane (`Esc` goes back). |
| pane | `network` | The app's network tab for the in-process agent ("Inspect Network (Agent HTTP)" in the device picker, Android and iOS Simulator), with response overrides. See [Network and overrides](#network-and-overrides). |
| pane | `tools` | A popup listing `Gradle` and `Xcode`. `Enter` or a click opens the chosen pane in a new tab named `Gradle - Jaca` or `Xcode - Jaca` and closes the popup. `j`/`k` or arrows move, `Esc` or `q` closes. Run outside Herdr, the chosen pane shows in place (`Esc` goes back). |
| pane | `gradle` | The app's Gradle area: the folders of `~/.gradle/caches` with their sizes, then the live daemons from the daemon's `gradle.daemons` topic. See [Gradle and Xcode](#gradle-and-xcode). |
| pane | `xcode` | The app's Xcode area: the DerivedData folders with their sizes and kind (`LIVE`, `STALE`, `SHARED`). See [Gradle and Xcode](#gradle-and-xcode). |
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

The picker for the Gradle and Xcode panes takes a binding the same way:

```toml
[[keys.command]]
key = "prefix+shift+b"
type = "shell"
command = "herdr plugin pane open --plugin dev.srsouza.jaca --entrypoint tools"
```

`jacad` is looked up at `$JACAD_PATH`, then `/Applications/Jaca.app` and `~/Applications/Jaca.app`.
`JACA_DAEMON_DIR` points every piece (app, daemon, plugin) at another socket directory, which is
how to try a development build without touching the real one:

```bash
export JACA_DAEMON_DIR=~/.jaca/dev JACAD_PATH=/path/to/Debug/Jaca.app/Contents/MacOS/jacad
bin/jaca-herdr pane gradle
```

Remove it with `herdr plugin unlink dev.srsouza.jaca`.

## Network and overrides

The `network` pane captures one app's requests with the in-process agent and applies response
overrides, the app's "Agent HTTPS debugging". It is a client of jacad's `network.*` and
`overrides.*` methods; nothing was added to the daemon for it.

- **Start**: `a` (or the app button) lists the installed apps; choosing one starts the capture.
  `p` or `space` stops and starts it. On the iOS Simulator starting relaunches the app.
- **List**: `j`/`k` select, `Shift` with `↑`, `↓`, `PgUp` or `PgDn` selects a run of requests,
  `Backspace` deletes the selected ones from the list (here only; jacad keeps them, so they
  are still in an exported HAR), `/` filters by URL, host or method, `c` clears, `S` exports a
  HAR through the macOS save dialog, `C` copies the selected request's response body, `G` goes
  back to following new requests.
- **Timeline**: the strip above the list draws each request from its start to its end.
  Dragging across it shows only the requests in that time range; a click on it, the `×` at
  its right or `Esc` clears the range. It shows in panes of 24 rows or more.
- **Detail**: `Enter` or a click opens the request beside the list, with the app's tabs
  (Overview, Headers, Request, Response, Timing). `Tab` or `1`–`5` switch tabs, `PgUp`/`PgDn`
  scroll, `Esc` closes it. In the Request tab `C` copies the request body.
- **Request menu**: a right click (or `m`) shows the app's menu: `Override response…` (or
  `Edit override “…”` and `Add another override…` when a rule already answers it), `Copy URL`,
  `Copy response body`, `Filter by this host`.
- **Overrides**: `o` (or the Overrides button) opens the rules: `Enter` edits, `n` is New
  override, `space` enables or disables, `d` duplicates, `x` or `Backspace` deletes, `K`/`J` move a rule up
  or down (the first enabled match wins), `m` pauses or resumes all of them.
- **Override a request**: `O` on a selected request opens the editor filled from it (its URL as
  the pattern, its method, and the response it got), or on the rule that already answers it.
- **Editor**: `Tab`/`Shift+Tab` move between fields, the arrows move inside a group of chips and
  `space` picks, `Ctrl+S` (or `⌘S`) saves, `Esc` closes without saving. In the body `Tab` indents two
  spaces (every selected line, when several are) and `Shift+Tab` takes a level off, so `Esc`
  first hands the keys back to moving between fields (then `Tab` and `Shift+Tab` move again),
  and a second `Esc` closes. The body is a multi-line editor:
  `Shift` with the arrows, `Home`, `End`, `PgUp` or `PgDn` selects, as does dragging with the
  mouse; typing or `Backspace` replaces the selection; `Alt` or `Ctrl` with `←`/`→` moves by
  word and `Alt+Backspace` deletes one; `Ctrl+A` selects everything, `Ctrl+C` copies and
  `Ctrl+X` cuts the selection; `Ctrl+Z` undoes and `Ctrl+Y` redoes. `⌘A`, `⌘C`, `⌘X`, `⌘Z` and `⇧⌘Z` do the
  same where the terminal passes them on (see below). One-line fields edit at a cursor the arrows move. In a
  pane of 120 columns and 30 rows or more the editor is the app's two-column sheet.

**⌘ keys.** A terminal keeps `⌘A`, `⌘C` and `⌘Z` for itself by default, so they never reach
the pane. Herdr forwards a `⌘` key it receives to the focused pane, and the editor reads it, so
the terminal only has to let them through. In Ghostty (`~/.config/ghostty/config`):

```
keybind = super+a=unbind
keybind = super+z=unbind
keybind = super+shift+z=unbind
keybind = performable:super+c=copy_to_clipboard
```

This changes Ghostty everywhere: `⌘A` no longer selects the whole screen and `⌘Z` no longer
undoes a closed tab; `⌘C` still copies a Ghostty selection when there is one.

Not carried over from the app: the JSON tree view (bodies show as
pretty-printed text), the match preview's example rows and shadow count, find in the body
editor, the device proxy section, and companion or proxy capture. Regex patterns use Go's
engine, not the app's: lookaround and backreferences don't compile here though they run in the
app and in jacad.

Rule bodies over 4 KB are written to `~/.jaca/network-overrides/bodies/` by the pane, as the
app does; jacad has no method to upload one.

## Gradle and Xcode

The `gradle` and `xcode` panes work alike. One cursor moves over every row, and each row has a
button that takes two presses, as in the app: the first arms it for 3 seconds (`Kill` reads
`Confirm kill?`, `Delete` reads `Confirm?`), the second within that time confirms. Each row is
armed on its own. The result shows on the line above the status bar for 2.6 seconds, as the
app's toast does.

- **Keys**: `j`/`k` or arrows move, `PgUp`/`PgDn` move a page, `Enter`, `x` or `Backspace`
  press the selected row's button, `r` refreshes, `?` shows the keys (any of `Esc`, `Enter`,
  `q`, `?` or a click closes it), `q` quits.
- **Mouse**: a click selects a row, a click on a row's button presses it, the wheel scrolls,
  and `Help` at the right of the status bar opens the keys.
- **Gradle**: the cache folders (`gradle.caches`, measured with `du` when the pane opens, so
  the section reads `Calculating…` at first) and the daemons. Killing dims the daemon's row
  until jacad answers; a daemon that exited on its own meanwhile goes without a toast. `r` is
  the app's "Refresh Gradle daemons" and also measures the cache again, which the app does
  each time its area appears.
- **Xcode**: `xcode.list` sizes every folder with `du`, so the first scan takes a while. `r`
  is the app's "Rescan DerivedData". `S` (or the button at the top right, shown while there
  are stale entries) is the app's `Clean <n> stale`, two presses too: it deletes the stale
  folders one by one and then removes every stale row, as the app does; the toast counts the
  folders that were deleted, and a rescan lists any that weren't. jacad has no topic for
  DerivedData, so the pane removes the rows it deletes and otherwise changes only on a rescan.

In a narrow pane the name is cut first, then columns are left out: the JDK, heap and RAM tags, CPU,
uptime, PID and `BUSY`/`IDLE` in the Gradle pane, the workspace path, kind and size in the Xcode pane. The button goes
last.

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

Every string a pane shows is taken from the app (the Gradle, Xcode, device list, device menu, log and
Projects views, the sidebar titles, `DeviceState.label`, `LogLevel.short`, the Projects toasts). The log toolbar's field placeholders are the app's with a capital first letter. In the network pane's `?` popup, `Delete request`, `Select requests`, `Select request`, `Inspect request`, `Switch tab`, `Request menu` and `Follow new requests` are placeholders too, as is the `network` pane title. In the Gradle and Xcode panes' `?` popups, `Select row` is a placeholder (the other labels are the app's buttons and tooltips, or the placeholders the log viewer already uses), as is the `tools` pane title, `Jaca`. Those two panes show nothing while a refresh the user asked for is running, because the app has no text for it. The `?` popup's labels are the app's where it has one; `Minimum level`, `Filter text`, `Regex`, `Package id`, `Pause / unpause`, `Scroll`, `Help` (also the popup's title and the status bar button) and `Quit` are placeholders that need specified copy. Two things have no app copy
yet and are left out until it's specified: a key legend for the projects pane, and the size-scan
approval prompt (so the projects pane shows only sizes already computed). The two action titles
and the `logs` pane title in `herdr-plugin.toml` are placeholders written for this prototype and
need specified copy.
