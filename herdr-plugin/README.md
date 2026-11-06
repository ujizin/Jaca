# Jaca for Herdr

This plugin shows Jaca's areas as Herdr panes and actions.
The plugin holds no state of its own. Every pane and action is a client of `jacad`, the Jaca
daemon, over `~/.jaca/jacad.sock`. The Jaca app and these panes read the same daemon, so they
show the same data. For the daemon, see [docs/daemon-plan.md](../docs/daemon-plan.md).

Status: experimental. The plugin uses only the Go standard library.

- [Requirements](#requirements)
- [Quick start](#quick-start)
- [Panes and actions](#panes-and-actions)
- [Herdr key bindings](#herdr-key-bindings)
- [Keys in every pane](#keys-in-every-pane)
- Panes: [Devices](#devices) · [Logs](#logs) · [Network](#network) · [Tools, Gradle and Xcode](#tools-gradle-and-xcode) · [Cloud Logging](#cloud-logging) · [Projects](#projects) · [Database](#database)
- [Configuration](#configuration)
- [Troubleshooting](#troubleshooting)
- [Development](#development)

## Requirements

| Need | Version |
|---|---|
| macOS | The plugin declares `platforms = ["macos"]`. |
| Herdr | 0.9.0 or newer, with its server running (`herdr status`). |
| Go | 1.22 or newer (`brew install go`). |
| Jaca | A `Jaca.app` that ships `jacad` (`Jaca.app/Contents/MacOS/jacad`). Builds from `main` ship it. |

## Quick start

Do these steps from the repo root. For the full path, updates and uninstall, read
[INSTALL.md](INSTALL.md).

1. Build and install Jaca with `jacad`. Skip this step if `/Applications/Jaca.app/Contents/MacOS/jacad` exists.

   ```bash
   ./scripts/all.sh --install --no-agent
   ```

2. Build the plugin and link it into Herdr.

   ```bash
   cd herdr-plugin
   go build -o bin/jaca-herdr ./cmd/jaca-herdr
   herdr plugin link "$PWD"
   ```

3. Open the device picker.

   ```bash
   herdr plugin pane open --plugin dev.srsouza.jaca --entrypoint devices
   ```

To remove the plugin, run `herdr plugin unlink dev.srsouza.jaca`.

## Panes and actions

Open a pane with `herdr plugin pane open --plugin dev.srsouza.jaca --entrypoint <id>`.

| Kind | Id | Opens as | What it shows |
|---|---|---|---|
| pane | `devices` | popup | The device list. A device opens logs, network or the database in a new tab. |
| pane | `logs` | tab | The log stream of one device, with the app log tab's tools. |
| pane | `network` | tab | The network requests of one app (agent) or of a Chrome window on this Mac, with response overrides. |
| pane | `database` | tab | The local SQLite databases of one app, from a pulled snapshot. |
| pane | `tools` | popup | A picker for the `gradle` and `xcode` panes. |
| pane | `gradle` | tab | The `~/.gradle/caches` folders with their sizes, and the live Gradle daemons. |
| pane | `xcode` | tab | The DerivedData folders with their size and kind (`LIVE`, `STALE`, `SHARED`). |
| pane | `cloud` | popup | Cloud Logging: the home (gcloud sign-in, projects). Log sessions open in tabs. |
| pane | `projects` | tab | The Projects area: Claude Code projects, the folders you added, and their checkouts. |
| action | `gradle-kill-all` | | Kills every running Gradle daemon. |
| action | `projects-clear-cache` | | Clears the build caches of the worktree in the Herdr context. |
| startup | | | `jaca-herdr ensure` starts `jacad` if it does not run. |

Notes:

- `projects-clear-cache` uses the workspace's worktree. If there is none, it uses the directory
  of the focused pane.
- Opened without a device, `logs`, `network` and `database` show the device picker first and
  then show the pane in the same place (`Esc` goes back).

## Herdr key bindings

Add the bindings to `~/.config/herdr/config.toml`, then run `herdr server reload-config`. Pick
keys that are free in your config.

```toml
[[keys.command]]
key = "prefix+d"
type = "shell"
command = "herdr plugin pane open --plugin dev.srsouza.jaca --entrypoint devices"

[[keys.command]]
key = "prefix+shift+b"
type = "shell"
command = "herdr plugin pane open --plugin dev.srsouza.jaca --entrypoint tools"

[[keys.command]]
key = "prefix+l"
type = "shell"
command = "herdr plugin pane open --plugin dev.srsouza.jaca --entrypoint cloud"

[[keys.command]]
key = "prefix+f"
type = "shell"
command = "herdr plugin pane open --plugin dev.srsouza.jaca --entrypoint projects --focus --env JACA_HERDR_OWN_TAB=1"
```

`JACA_HERDR_OWN_TAB=1` tells the `projects` pane that it has a tab of its own. The pane then
names the tab `Projects - Jaca`. Without it, the pane does not change the name of the tab it
runs in.

## Keys in every pane

| Key | Action |
|---|---|
| `?` | Shows the keys of the pane. `Esc`, `Enter`, `q`, `?` or a click closes the list. |
| `q` | Quits the pane. |
| `Ctrl+C` | Quits the pane. In an editor (override body, database SQL, Cloud Logging SQL) it copies the selection. |
| `Help` (click) | The button at the right of the status bar shows the keys. |

The `devices` and `tools` popups have no `?` list. In them, `q` and `Ctrl+C` close the popup.

**Copied notice.** In the logs, network, database and Cloud Logging session panes, every copy
(a button, a key, a menu item, a released selection) shows `Copied` near the bottom of the pane
for a moment. If the copy fails, the pane shows the error.

**Two-press buttons.** Some buttons take two presses, as in the app: Kill, Delete and Clean in
the Gradle and Xcode panes, Remove in the Cloud Logging home and the Projects pane, and Delete
worktree in Projects. The first press arms the button for 3 seconds. A second press in that
time confirms. Each row arms on its own.

<details>
<summary>How the panes tell a second press from a held key</summary>

A confirming press must be a separate press. The repeats of a held key do not confirm what its
first press armed. This applies to every two-press button. In the Projects pane, a held key
acts once: the pane drops its repeats.

- The pane reads how this Mac repeats keys (`InitialKeyRepeat` and `KeyRepeat`) when it starts.
- The pane takes the same key for a repeat when a repeat is due: right after the last one, one
  interval after, or the first delay after it.
- The first delay is 375 ms unless changed. The window goes from 40 ms before it to 60 ms
  after it. A second press that lands in this window must be made again.
- Any other second press of the same key waits a moment (150 ms at the usual repeat rate). The
  pane drops it when a repeat follows it.
- After the wait, the press acts on the row it was pressed on. If that row moved, it does nothing.
- A click on a row that an update moved before the pane drew it is dropped.

One case stays ambiguous: a key held just past its first repeat and released, when that
repeat came more than 40 ms early or 60 ms late.

</details>

## Devices

The `devices` pane is a popup with the device list. `Enter` on a ready device shows its options.
`Enter` on an option opens it in a new tab and closes the popup.

| Option | Shown for | Opens | Tab name |
|---|---|---|---|
| `Start Logcat` | every device except This Mac | `logs` | `Jaca log - <device>` |
| `Inspect Network (Agent HTTP)` | Android, iOS Simulator | `network` | `Jaca network - <device>` |
| `Browse Database` | every device except This Mac | `database` | `Jaca database - <device>` |
| `Inspect Network (Chrome)` | This Mac (always listed) | `network` | `Jaca network - <device>` |

| Key | Action |
|---|---|
| `j` `k` or `↑` `↓` | Move. |
| `Enter` | Show the options of a ready device, or open the selected option. |
| `Esc` | Go back one step. From the device list, close the popup. |
| `q` | Quit. |
| Click on ` esc close ` | Same as `Esc`. |

## Logs

The `logs` pane is the log viewer for the device chosen in `devices`, with the tools of the
app's log tab. The device picker passes the device in `JACA_HERDR_DEVICE`.

| Key | Action |
|---|---|
| `1`–`6` | Set the minimum level (V…F). |
| `/` | Edit the filter text. |
| `r` | Turn regex on or off. |
| `s` | Show or hide system logs (not on Android). |
| `P` | Edit the package id. It applies on `Enter`. |
| `a` | List the installed apps to pick one. |
| `p` or `space` | Pause or resume the stream. |
| `Enter` | Start the stream when the pane shows its start prompt. |
| `c` | Clear the view. |
| `C` | Clear the device buffer (Android). |
| `j` `k`, `↑` `↓`, `PgUp` `PgDn` | Scroll. Scrolling up stops following the tail. |
| `Shift+PgDn` or `G` | Follow the tail again. |
| `S` or `e` | Export the lines the filter keeps. The macOS save dialog opens in Downloads with a file named after the device. |
| `y` | Open the app's Copy format sheet as a popup. |
| `Esc` | Go back to the device picker, when the pane opened without a device. |

**In a field** (filter text, package id): `Enter` or `Esc` leaves the field, `Ctrl-U` empties it.

**Copy format popup** (`y`): it shows presets with an example each, the template and date
format to edit, and a live example. Nothing is saved before Save. It writes
`~/.jaca/log-copy-format.json`, the file the app reads.

| Key | Action |
|---|---|
| arrows or `Tab` | Move. |
| `Enter` | Choose. |
| `Ctrl+S` or `⌘S` | Save. |
| `Esc` | Cancel. |

**Mouse**

- Click the level chips, `.*`, `System logs`, `Export`, both fields, the arrow at the end of
  the package field (the installed apps), the apps list, and `Help` and `Copy format` at the
  right of the status bar. The wheel scrolls.
- Click a log line to select it. Drag to extend the selection line by line. Past the top or
  bottom of the lines, the view scrolls that way.
- Release to copy the selected lines whole, in the app's copy format (with `pbcopy`).
- The view holds still under a selection. `Esc`, any key, or a click outside the lines drops it.
- Right-click to open the app's row menu on the selected lines: `Copy Line`,
  `Copy Message only`, `Copy Format…`, `Select All`. `Copy Format…` opens the Copy format popup.

The pane draws this selection itself because it has the mouse for the toolbar, so the terminal
cannot select in it. For the row menu, the pane asks Herdr to send it right clicks
(`herdr pane input --right-click pane`). Herdr's own pane menu then does not open on a right
click in this pane. That menu's "Send right-clicks to pane" item turns it back.

## Network

The `network` pane is the app's network tab, with response overrides (the app's "Agent HTTPS
debugging"). It uses jacad's `network.*` and `overrides.*` methods. It captures one of:

- **An app, with the in-process agent.** Choose "Inspect Network (Agent HTTP)" in the device
  picker (Android and iOS Simulator). `a` (or the app button) lists the installed apps. Choosing
  one starts the capture. On the iOS Simulator, a start relaunches the app.
- **A Chrome window on this Mac.** Choose "This Mac", then "Inspect Network (Chrome)". The
  capture starts at once. jacad opens a separate Chrome window with its own profile, sends its
  traffic through a proxy on 127.0.0.1 and applies the overrides to it. There is no app to
  choose. Stopping the capture closes the window.

### Request list

| Key | Action |
|---|---|
| `p` or `space` | Start or stop the capture. |
| `a` | List the installed apps to choose one (agent capture). |
| `b` | Open the Chrome window again after it was closed (This Mac). Same as the `Open Chrome` button. |
| `R` | Relaunch the app and attach the agent again. Same as the attach banner's button. |
| `j` `k` or `↑` `↓` | Select a request. |
| `Shift` + `↑` `↓` `PgUp` `PgDn` | Select a run of requests. |
| `PgUp` `PgDn` | Scroll a page. |
| `Backspace` | Delete the selected requests from the list. jacad keeps them, so an exported HAR still has them. |
| `/` | Filter by URL, host or method. `Enter` or `Esc` leaves the filter. |
| `c` | Clear. |
| `S` or `e` | Export a HAR through the macOS save dialog. |
| `C` or `y` | Copy the response body of the selected request. In the Request tab of the detail, copy the request body. |
| `u` | Copy as cURL. |
| `G` | Follow new requests again. |
| `Enter` | Open the detail of the selected request and give it the keys. With no request selected, select the newest. |
| `→` | Give the keys to the open detail. |
| `m` | Open the request menu. |
| `o` | Open the overrides list. |
| `O` | Override the response of the selected request. |
| `P` | Pause or resume all overrides. |
| `Esc` | Close the detail, then clear the time range, then go back to the device picker. |

**List layout.** The path is cut in the middle, as in the app, so the end of an endpoint stays
readable. A narrow list leaves out Size, then Host, then Time, before it cuts the path short.

**Timeline.** The strip above the list draws each request from its start to its end. Drag
across it to show only the requests in that time range. A click on it, the `×` at its right, or
`Esc` clears the range. The strip shows in panes of 24 rows or more.

**Attach banner.** If the app came back without the agent (the user opened it again), a warning
block under the toolbar says so. It has a ` R  Relaunch & re-attach ` button (`R`). On the iOS
Simulator, in Agent HTTPS debugging, a switch beside it turns on the app's
`Re-attach to iOS Simulator apps automatically` setting. The pane writes it with `defaults`, and
jacad reads it each time it decides. With it on, jacad relaunches the app without asking. It is
off by default, as in the app, because a relaunch loses the user's place in their app.

### Request detail

The detail opens beside the list, with the app's tabs: Overview, Headers, Request, Response,
Timing. Opened with `Enter` or `→`, the detail has the keys.

| Key | Action |
|---|---|
| `↑` `↓` or `j` `k` | Scroll. |
| `PgUp` `PgDn` | Scroll a page. |
| `←` `→`, `Tab`, `Shift+Tab`, `1`–`5` | Switch tabs. |
| `C` | Copy the request body (Request tab). |
| `Esc` | Close the detail. The list gets the keys back and keeps the selection. |

**Mouse**

- A click on a request opens its detail and leaves the keys on the list. A click in the detail
  gives it the keys. The side with the keys has its headings in the accent color. The selected
  request is dimmer while the detail has the keys.
- Drag over text to select it. Release to copy it. A value that wrapped over several lines
  comes back in one piece.
- Double-click to copy the whole value under the pointer. Right-click to get `Copy` for it.
- The `Copy` buttons of the bodies and header sections sit at the right of their line.
- The `Copy cURL` button at the right of the tabs copies the request as a curl command.

### Request menu

A right click on a request, or `m`, shows the app's menu:

- `Override response…`. If a rule already answers the request: `Edit override “…”` and
  `Add another override…`.
- `Copy as cURL`: a curl command that runs as-is, as the app's `CurlExport` builds it. The pane
  fetches the request body first if it does not hold it.
- `Copy URL`
- `Copy response body`
- `Filter by this host`

In the menu, `↑` `↓` or `j` `k` move, `Enter` runs the item and `Esc` closes the menu.

### Overrides list

`o` (or the Overrides button) opens the rules. The first enabled rule that matches wins.

| Key | Action |
|---|---|
| `j` `k` or `↑` `↓` | Move. |
| `Enter` | Edit the rule. On a group, open the group's popup. In an empty list, New override. |
| `n` | New override. |
| `space` or `Shift+Enter` | Enable or disable the rule (or the group and all its rules). A click on a rule's switch does the same. |
| `d` | Duplicate the rule. |
| `x` or `Backspace` | Delete the rule. On a group, delete the group. Its rules stay, in no group. |
| `K` `J` | Move the rule (or the group) up or down. |
| `g` | Move the rule into a group or out of one. |
| `N` | New group, then rename it. Same as the New group button. |
| `r` | Rename the group. `Enter` saves, `Esc` cancels. |
| `m` | Pause or resume all overrides. |
| `L` | Show Jaca's log (`~/.jaca/logs/jaca.log`) in Finder. |
| `Esc`, `q` or `o` | Close the list. |

The groups show with a jacad that supports them. Each group is one row above the rules in no
group. A click on a group's name opens the group's popup. A right click on a row shows its
actions as a menu.

**A group's popup.** The popup has the group's name as its title. It has the group's switch
(`m`), its rules with the same keys as the list (`K` `J` move a rule inside the group), and the
New override (makes a rule in the group), Rename group (`r`) and Delete group buttons. `Esc`
goes back to the list. `q` or `o` closes the overrides.

### Rule editor

`O` on a selected request opens the editor filled from it: its URL as the pattern, its method,
and the response it got. If a rule already answers the request, `O` opens that rule. Closing
the editor goes back to where it opened: the rules list or the requests. In a pane of 120
columns and 30 rows or more, the editor is the app's two-column sheet.

| Key | Action |
|---|---|
| `Tab` `Shift+Tab` | Move between fields. |
| `↑` `↓` | Move between fields. In a one-line field, `Enter` also moves to the next field. |
| `←` `→` | Move inside a group of chips. In a one-line field, move the cursor. |
| `space` or `Enter` | Pick a chip or press a button. |
| `Ctrl+S` or `⌘S` | Save. |
| `Esc` | Close without saving. In the body, the first `Esc` leaves the text and the second closes. |

**In the body**, the editor has more keys. `⌘A`, `⌘C`, `⌘X`, `⌘Z` and `⇧⌘Z` work like their
`Ctrl` keys when the terminal passes them on (see [Command keys](#command-keys)).

| Key | Action |
|---|---|
| `Tab` | Indent two spaces (every selected line, when there are several). |
| `Shift+Tab` | Remove one indent level. |
| `Esc` | Give the keys back to moving between fields. `Tab` and `Shift+Tab` move again. `Enter` returns to the text. |
| `Shift` + arrows, `Home`, `End`, `PgUp`, `PgDn` | Select. Dragging with the mouse also selects. |
| typing or `Backspace` | Replace the selection. |
| `Alt` or `Ctrl` + `←` `→` | Move by word. |
| `Alt+Backspace` | Delete a word. |
| `Ctrl+A` | Select all. |
| `Ctrl+C` | Copy the selection. |
| `Ctrl+X` | Cut the selection. |
| `Ctrl+Z` | Undo. |
| `Ctrl+Y` | Redo. |

Rule bodies over 4 KB go to `~/.jaca/network-overrides/bodies/`. The pane writes them, as the
app does, because jacad has no method to upload one.

### Command keys

A terminal keeps `⌘A`, `⌘C` and `⌘Z` for itself by default, so they never reach the pane.
Herdr sends a `⌘` key it receives to the focused pane, and the editor reads it. The terminal
must let these keys through. In Ghostty (`~/.config/ghostty/config`):

```
keybind = super+a=unbind
keybind = super+z=unbind
keybind = super+shift+z=unbind
keybind = performable:super+c=copy_to_clipboard
```

This changes Ghostty everywhere: `⌘A` no longer selects the whole screen and `⌘Z` no longer
undoes a closed tab. `⌘C` still copies a Ghostty selection when there is one.

<details>
<summary>Not carried over from the app (network)</summary>

- The JSON tree view. Bodies show as pretty-printed text.
- The example rows and the shadow count of the match preview.
- Find in the body editor.
- The device proxy section.
- Companion and proxy capture.
- The app's regex engine. Patterns use Go's engine here, so lookaround and backreferences do not
  compile, though they run in the app and in jacad.

</details>

## Tools, Gradle and Xcode

The `tools` pane is a popup that lists `Gradle` and `Xcode`. `Enter` or a click opens the
chosen pane in a new tab named `Gradle - Jaca` or `Xcode - Jaca` and closes the popup. `j` `k`
or the arrows move, and `Esc` or `q` closes. Run outside Herdr, the chosen pane shows in place
(`Esc` goes back).

The `gradle` and `xcode` panes work the same way. One cursor moves over every row. Each row has
a two-press button: `Kill` reads `Confirm kill?` and `Delete` reads `Confirm?` while it is
armed. The result shows on the line above the status bar for 2.6 seconds, as the app's toast
does.

| Key | Action |
|---|---|
| `j` `k` or `↑` `↓` | Move. |
| `PgUp` `PgDn` | Move a page. |
| `Enter`, `x` or `Backspace` | Press the button of the selected row (`Kill` or `Delete`). |
| `r` | Gradle: "Refresh Gradle daemons", which also measures the caches again. Xcode: "Rescan DerivedData". |
| `S` | Xcode: `Clean <n> stale` (two presses). Shown while there are stale entries. |
| `Esc` | Go back to the tools picker, when the pane shows in place. |

**Mouse.** A click selects a row. A click on the button of a row presses it. The wheel
scrolls. `Help` at the right of the status bar shows the keys.

**Gradle.** The pane shows the cache folders (`gradle.caches`) and the daemons (the
`gradle.daemons` topic). It measures the caches with `du` when it opens, so the section reads
`Calculating…` at first. The app measures them each time its area appears. Killing dims the
row of the daemon until jacad answers. A daemon that exited on its own in that time goes
without a toast.

**Xcode.** `xcode.list` sizes every folder with `du`, so the first scan takes a while. `S`, or
the button at the top right, deletes the stale folders one by one and then removes every stale
row, as the app does. The toast counts the deleted folders. A rescan lists any that were not
deleted. jacad has no topic for DerivedData, so the pane removes the rows it deletes and
otherwise changes only on a rescan.

**Narrow panes.** The name is cut first. Then columns go: in Gradle, the JDK, heap and RAM tags,
CPU, uptime, PID and `BUSY`/`IDLE`; in Xcode, the workspace path, kind and size. The button
goes last. While a refresh you asked for runs, the panes show nothing for it, because the app
has no text for it.

## Cloud Logging

The `cloud` pane is the app's Cloud Logging area, on jacad's `cloud.*` methods. It opens on the
home, in a popup (`Esc` closes it). Opening a session closes the popup and switches to the
session's own tab, named `Jaca cloud - <project>`. The session tab runs the same entrypoint with
the session in `JACA_HERDR_CLOUD_SESSION`.

### Home

The header shows what jacad found: `Signed in as …`, `Not signed in` or `gcloud CLI not found`.
When you are signed out, `Open Terminal` opens a Herdr tab that runs `gcloud auth login`. jacad
never runs it, because it has no terminal to prompt in. The home then asks jacad to read the
account again every 3 seconds until the login lands. It does this for 10 minutes after the last
key or click.

| Key | Action |
|---|---|
| `j` `k` or `↑` `↓` | Select a project. |
| `PgUp` `PgDn` | Scroll. |
| `←` `→`, `Tab`, `Shift+Tab` | Select an action of the row. |
| `Enter` | Press the selected action. With no projects, Add project. |
| `n` | New session. |
| `l` | Log names. |
| `e` | Rename. |
| `x` or `Backspace` | Remove (two presses). |
| `a` | Add project. |
| `u` | From URL…. |
| `r` | Re-check (read the account again). |
| `Esc` | Close the popup. |

### Session

| Key | Action |
|---|---|
| `p` or `space` | Start or stop. |
| `c` | Clear the view. |
| `f` | Turn Follow tail on or off. |
| `G` or `Shift+PgDn` | Turn Follow tail on. |
| `l` | Choose the log name. |
| `t` | Time range. |
| `u` | Share menu: `Copy Logs Explorer URL`, `Open in browser`. |
| `1`–`6` | Minimum severity (All, D, I, W, E, C). |
| `F` | Open or close the Filters bar. |
| `/` | Search the loaded logs. `Enter` or `Esc` leaves the box. |
| `↑` `↓`, `PgUp` `PgDn` | Move a selection over the entries. An open details panel follows it. |
| `Shift+↑` `Shift+↓` | Extend the selection. |
| `j` `k` | Scroll without selecting. |
| `C` | Copy the selected entries. |
| `y` | Set the copy format. |
| `S` or `e` | Export. |
| `Enter` | Open the details of the entry. |
| `Tab` | Move the keys between the list and the details. |
| `Q` | Switch between Logs and SQL. |
| `s` | Go to the SQL editor. |
| `Esc` | Close the details, then drop the selection, then go back. |

A click opens the details of an entry beside the list. A right click opens the row menu:
`Copy Entry` (or `Copy <n> Entries`), `Copy Format…`, `Select All`. Scrolling to the top loads
older logs.

**Details panel.**

| Key | Action |
|---|---|
| `j` `k` or `↑` `↓` | Move between fields. |
| `PgUp` `PgDn` | Move half a page. |
| `Enter` | Open the field's actions: filter by a label or severity, open it in a new session, favorite a label. |
| `[` `]` or `←` `→` | Go to the previous or next entry. |
| `Tab` | Give the keys back to the list. |
| `Esc` | Close the details. |

Changes to what goes to gcloud restart a running session, as in the app. These are the log
name, the time range, the severity and the conditions of the Filters bar. The search box only
filters what is loaded.

### SQL

The statement runs over the entries loaded in the session (jacad's `cloud.sessions.query`).
After it has run once, it runs again every 2 seconds, as in the app. The bar has the app's
Schema popup, the Labels menu (inserts a filter on a label), the Templates menu (saved
templates, starters, From current filter) and `Save…`. A pane with fewer than three rows to
spare shows no SQL bar.

| Key | Action |
|---|---|
| `Ctrl+R`, or `⌘↩` if the terminal passes it on | Run the statement, from anywhere in SQL mode. |
| `Tab` | In the editor: indent. On the bar: next stop. |
| `Shift+Tab`, `←` `→`, `↑` `↓` | On the bar: move between the stops. |
| `Enter` or `space` | On the bar: press the stop. |
| `Esc` | In the editor: leave the text. On the bar: give the keys back to the list. |
| `Ctrl+C` or `⌘C` | Copy the selection in the editor. |
| `Ctrl+X` or `⌘X` | Cut the selection in the editor. |

<details>
<summary>Not carried over from the app (Cloud Logging)</summary>

- The Ask Claude buttons (SQL and regex) and the label examples sheet that sets what they send.
- The gcloud debug console. It reads a log kept inside the app, and jacad has no method for it.
- Resizing the detail panel.
- Horizontal scrolling of long lines.
- Syntax highlighting and autocomplete in the SQL editor.
- The tab subtitle (`project · log · range`).
- Selecting part of a value in the detail panel.
- The date pickers of an absolute range. The pane has two text fields read as
  `2006-01-02 15:04`.
- The match mode menu. Press the match mode until the one you want shows.
- Surviving a jacad restart. The pane exits; open it again.

</details>

## Projects

The `projects` pane is the app's Projects area, on jacad's `projects.*` methods and its retained
`projects.state` topic. It opens in a tab. With the [key binding](#herdr-key-bindings), the tab is
named `Projects - Jaca`.

Each row has two lines: the name with its tags and size, then the path. For a checkout, the
second line has the path, `from <base>` or `detached HEAD` / `branch gone`, and its age. The
selected row shows its actions as buttons on the second line. Projects start closed.

| Key | Action |
|---|---|
| `j` `k` or `↑` `↓` | Move. |
| `PgUp` `PgDn` | Move half a page. |
| `Enter` or `space` | Open or close a project. On a checkout, open the row menu. With no projects, add a project folder. |
| `→` | Open a project. |
| `←` | Close a project. On a nested row, go to its parent. |
| `t` | Switch between Tree and List. |
| `o` | Open in Finder. |
| `z` | Open in Zed (when Zed is installed). |
| `y` | Copy name (checkouts). |
| `c` | Cache & worktree actions (checkouts). |
| `h` | Open in Herdr (Claude projects). |
| `x` or `Backspace` | Remove (folders you added, two presses). |
| `m` | Row menu with the same actions. A right click opens it too. |
| `a` | Add a project folder (the macOS folder chooser). |
| `r` | Rescan projects. |
| `s` | Herdr settings: change the command that Open in Herdr runs. |
| `C` / `N` | Calculate / Not now, while the disk usage bar asks. |
| `S` | Stop a running size scan. |

**Mouse.** A click on the name line of a project opens or closes it. `Tree` / `List` in the
header switch the view. A right click opens the row menu.

**Tree and List.** The tree nests a project under the project whose folder contains it. The
list is flat, in jacad's order. The choice is the app's `jaca.projectsViewMode` setting, so the
pane and the app share it.

**Cache & worktree actions.** `Clean cache` runs at once and closes the sheet, as in the app.
`Delete worktree` takes two presses within 3 seconds. `Tab` moves between the buttons and
`Enter` presses the focused one (`Close` at first). `Esc` closes. The main checkout has no
delete. The sheet shows whole or not at all: a pane too short for it does not open it.

**Disk usage.** The bar asks before the pane computes sizes. The answer lasts until the pane
closes. After Calculate, the pane sizes each completed rescan again.

**Rescans.** The pane rescans by itself when it opens and the last scan is missing or older
than 30 seconds.

**Open in Herdr.** The pane asks for the session name. Then it finds or creates the Space of
the project, opens a tab named after the session, and runs the Claude command there.

- On a project root that is a git repo, it fetches, fast-forwards and starts
  `--worktree <name>`.
- In a linked worktree, or a root without git, it runs the command in the folder.
- The first launch also asks for the command. `s` (or `Herdr settings`) changes it later. The
  app's `jaca.herdr.claudeCommand` setting stores it.

<details>
<summary>Not carried over from the app (Projects)</summary>

- The icons and avatars.
- Action buttons on every row. Only the selected row shows them, because the labels are the
  app's tooltips and a terminal has no room for them on each row.
- The hover-only Remove.
- Animations (a worktree that is being deleted is dimmed, a freed size is green).
- The toast icons.
- The multi-line command field. It is one line here.
- The `Add` button label of the folder chooser. The chooser has the app's message, but
  AppleScript's `choose folder` cannot set the button label.
- Locale-aware ages. Ages that jacad does not send are written as `3 min ago`, `2 hr ago` and so
  on, in fixed-length units. The app's formatter follows the system's language and region, so a
  date near a unit boundary can read one unit apart.

</details>

## Database

The `database` pane is the app's Database tab, on jacad's `database.*` methods. It opens on the
installed apps. When you pick one, it lists the app's databases, pulls the first and shows the
first table, 100 rows a page. What it shows is a snapshot: `Refresh` pulls the database again
and goes back to the first table. It only reads: jacad refuses a statement that is not
read-only.

| Key | Action |
|---|---|
| `a` | Pick an app. |
| `d` | DB menu. |
| `t` | Table menu (each table with its row count). |
| `r` | Refresh. |
| `s` or `/` | Go to the SQL box. |
| `Ctrl+R`, or `⌘↩` if the terminal passes it on | Run the statement, from anywhere. |
| `↑` `↓` or `j` `k` | Move the selection. |
| `PgUp` `PgDn` | Move a page. With the row detail open, scroll the detail. |
| `Home` `End` or `g` `G` | Go to the first or last row. |
| `←` `→` or `h` `l` | Scroll by column. |
| `[` or `p` | Prev page. |
| `]` or `n` | Next page. |
| `Enter` | Open or close the row detail. |
| `Tab`, `Shift+Tab`, `1` `2` | Switch between Fields and JSON in the row detail. |
| `C` | Copy JSON. |
| `Esc` | Close the row detail, then go back to the device picker. |

**In the SQL box.** `Enter` runs the statement, as in the app. `Alt+Enter` adds a line break
(the box grows to three rows). `Esc` leaves the box. `Ctrl+C` or `⌘C` copies the selection, and
`Ctrl+X` or `⌘X` cuts it. A query lets go of the table: the Table control reads `—` and the
pagination bar goes.

**Mouse.** A click on the SQL box goes to it. The toolbar buttons are clickable. A click on a
row opens its detail, and a second click closes it. The wheel scrolls; sideways, or with `Shift`, it
scrolls by column. Text in the row detail copies with the mouse, as in the network detail.

**Platforms.** As in the app, every ready device offers the option. A physical iOS device
answers `Database browsing isn't supported on this platform yet.` An Android app must be
debuggable.

**Limits.** jacad bounds what a read returns, which the app does not. A query stops at 50000
rows, any result at 64 MB, and a statement after 30 seconds. The pane asks jacad for one listing
or pull at a time. It opens its session again every 4 minutes so jacad does not reap it, and it
pulls again when jacad says the session or the copy is gone.

<details>
<summary>Not carried over from the app (Database)</summary>

- The tab's name (`DB · <app>`).
- The checkmark on the picked app.
- The cell borders of the grid.
- Selecting text in the grid.
- The `Copied` label on the Copy JSON button. The pane shows its `Copied` notice instead.
- Exact JSON for some numbers and keys. A number between 1e15 and 2^54 is quoted, and the keys
  of a nested object outside Latin-1 are in code point order.

</details>

## Configuration

| Variable | Effect |
|---|---|
| `JACAD_PATH` | The `jacad` binary to start. Set it in the Herdr server's environment: a variable you export in your shell does not reach panes, which the Herdr server starts. |
| `JACA_DAEMON_DIR` | The directory of the socket (default `~/.jaca`). The app, the daemon and the plugin all read it. |

The plugin looks for `jacad` in this order:

1. `$JACAD_PATH`
2. `/Applications/Jaca.app/Contents/MacOS/jacad`
3. `~/Applications/Jaca.app/Contents/MacOS/jacad`
4. The newest Xcode build, `~/Library/Developer/Xcode/DerivedData/Jaca-*/Build/Products/*/Jaca.app/Contents/MacOS/jacad`

Key bindings live in Herdr's config. See [Herdr key bindings](#herdr-key-bindings).

### Colors

Herdr does not pass its theme to plugins, so the panes find it the way Herdr does:

- The built-in theme named under `[theme]` in `~/.config/herdr/config.toml`, with the colors of
  `[theme.custom]` on top.
- Popups use the colors of Herdr's own overlays: `text` on `panel_bg`, an `accent` border, and
  a clickable ` esc close ` button at the top right.
- The log colors are the theme's `red`, `green`, `yellow`, `blue`, `teal` and `mauve`.
- A theme that the table does not have, or `terminal`, keeps the colors of the terminal palette.
- A pane reads the config when it starts, so a theme change shows in panes opened after it.
- `auto_switch` and `[theme.custom.light]` / `[theme.custom.dark]` are not applied.

The built-in palettes are in `cmd/jaca-herdr/herdrthemes.go`, taken from Herdr v0.9.1's
`src/app/state.rs`. The repo has no generator script for this file. When Herdr adds or changes
a theme, update the table from Herdr's source.

## Troubleshooting

| Message or symptom | Fix |
|---|---|
| `jacad not found: install Jaca.app in /Applications or set JACAD_PATH` | Install a Jaca build that ships `jacad`, or set `JACAD_PATH`. See [Configuration](#configuration). |
| `jacad closed the connection` | The daemon stopped. `jacad status` starts it again. `~/.jaca/jacad.log` says why it stopped. |
| `protocol N requested, daemon speaks M` | A daemon from another build runs. Run `jacad stop`, then open the pane again. |
| `<method>: no reply from jacad after <time>` | jacad did not answer in time. Read `~/.jaca/jacad.log`. |
| An action does nothing, or the startup hook failed | Run `herdr plugin log list --plugin dev.srsouza.jaca`. It shows the output and exit code of each command. |

More help:

- `~/.jaca/jacad.log` is the daemon's own log (starts, stops, errors).
- `jacad describe` lists every method and topic. `jacad call METHOD [PARAMS_JSON]` and
  `jacad watch TOPIC...` talk to the daemon directly.

## Development

```bash
cd herdr-plugin
go vet ./...
go test ./...
go build -o bin/jaca-herdr ./cmd/jaca-herdr
```

- Run a pane against a development daemon without touching the real one:

  ```bash
  export JACA_DAEMON_DIR=~/.jaca/dev JACAD_PATH=/path/to/Debug/Jaca.app/Contents/MacOS/jacad
  bin/jaca-herdr pane gradle
  ```

- `jaca-herdr` with no arguments prints the usage: `ensure`, `pane <id>` and `action <id>`.
- The protocol is newline-delimited JSON-RPC 2.0 on the socket. `protocolVersion` in
  `cmd/jaca-herdr/rpc.go` must match `DaemonProtocol.version` in
  `Sources/Core/Daemon/DaemonProtocol.swift`.
- Each screen has a test that draws it at many pane sizes and checks that it fits (for example
  `TestLogViewerRowsFitThePane`). Add one for a new layout.

<details>
<summary>Where the text in the panes comes from</summary>

Every string a pane shows comes from the app: the Gradle, Xcode, device list, device menu, log
and Projects views, the sidebar titles, `DeviceState.label`, `LogLevel.short` and the Projects
toasts. The exceptions below have no app copy. They are placeholders that need specified copy.

**All panes**

- The `?` popup labels `Scroll`, `Help` (also the popup's title and the status bar button) and
  `Quit`.
- The client's own error lines: `<method>: no reply from jacad after <time>`,
  `jacad closed the connection`, and the lines it writes when Herdr or jacad returns something
  it cannot read (`herdr tab create: no pane_id`, and the decode errors in `cloudwire.go`).
- The ` esc close ` and ` esc back ` buttons stand where the app's sheets have `Cancel`.

**Logs**

- The `?` popup labels `Minimum level`, `Filter text`, `Regex`, `Package id` and
  `Pause / unpause`.
- The toolbar's field placeholders are the app's, with a capital first letter.

**Network**

- The `?` popup labels `Delete request`, `Select requests`, `Select request`,
  `Inspect request`, `Switch tab`, `Request menu`, `Follow new requests` and `Back`.
- The `network` pane title.
- The `R` in the ` R  Relaunch & re-attach ` button is the key, like `esc` in ` esc close `. The
  label after it is the app's.
- The switch beside it uses the app's Settings title for the same setting,
  `Re-attach to iOS Simulator apps automatically`.

**Database**

- The `?` popup label `Row details`. The other labels are the app's, or placeholders that the
  other panes use.
- The `database` pane title and the tab name `Jaca database - <device>`.

**Gradle and Xcode**

- The `?` popup label `Select row`. The other labels are the app's buttons and tooltips, or the
  placeholders of the log viewer.
- The `tools` pane title, `Jaca`.

**Cloud Logging**

- The `?` popup labels `Select row`, `Select action`, `Press`, `Start / Stop`, `Time range`,
  `Minimum severity`, `Select entries`, `Details / row actions`, `Previous / next entry`,
  `Switch between list and details`, `Close / back`, `Logs / SQL`, `SQL filter` and
  `Copy Logs Explorer URL / Open in browser`.
- The `Help` button in the status bar and the tab name `Jaca cloud - <project>`.
- The glyphs that stand in for the app's icon buttons (`▶ ■ ⌫ ⤓ ↗ ⟳ ⊕ ⊖ ▾ ▼ ▸ ★ ☆ ✓ ● ○`), the
  `│` gutter of the SQL editor, and the `↓ <path>` note after an export (the app shows nothing
  there).
- The row `… <n> more lines — ⌘C copies all` is the app's string, though the key here is `C`.
  The SQL bar's `⌘↩ to run · …` hint is also the app's, though the key that always works is
  `Ctrl+R`.

**Projects**

- The `?` popup labels `Expand / collapse`, `Row menu` and `Tree / List`.
- The tab name `Projects - Jaca` and the `jaca: ` prefix in front of an error that the app
  would not show.
- The pane uses three app strings where the app does not: `Herdr settings` (the button's
  accessibility label; its tooltip is longer), `Confirm?` as the row menu's label while Remove
  is armed, and the relative ages that jacad does not send, which are approximated.

**Database messages.** jacad's messages for its limits
(`The result has more than 50000 rows. Add a LIMIT to the query.`,
`The result is larger than 64 MB. Select fewer rows or columns.`) and for a session it no longer
has (`No database session …`, `No database pulled in session …`) have no app copy.

**`herdr-plugin.toml`.** The two action titles and the `logs` pane title are placeholders
written for this prototype and need specified copy.

</details>
