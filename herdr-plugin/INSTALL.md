# Installing and uninstalling the Jaca Herdr plugin

The plugin shows Jaca areas as Herdr panes. It holds no data of its own: every pane and action
talks to `jacad`, the Jaca daemon, over `~/.jaca/jacad.sock`. So there are two pieces to install:

1. **A Jaca build that ships `jacad`** (`Jaca.app/Contents/MacOS/jacad`). Builds from `main` don't
   have it yet; builds from `exp/daemon` or `exp/daemon-overrides` do.
2. **The plugin itself** (this folder), linked into Herdr.

Experimental. See `docs/daemon-plan.md` for what the daemon does.

## Requirements

- macOS with Xcode, `xcodegen` and everything else Jaca needs to build (see the repo `README.md`).
- Go 1.22 or newer (`brew install go`).
- Herdr 0.9.0 or newer, with its server running (`herdr status`).

## Install

### 1. Build Jaca from the daemon branch

From the repo root of the branch checkout:

```bash
JACA_SKIP_AGENT=1 ./scripts/build.sh Release
```

Drop `JACA_SKIP_AGENT=1` if the Android toolchain is installed and you want Android agent capture.
To find the built app:

```bash
APP="$(xcodebuild -project Jaca.xcodeproj -scheme Jaca -configuration Release \
  -destination 'platform=macOS' -showBuildSettings 2>/dev/null \
  | awk '/ BUILT_PRODUCTS_DIR =/{d=$3} / FULL_PRODUCT_NAME =/{n=$3} END{print d"/"n}')"
ls "$APP/Contents/MacOS/jacad"
```

### 2. Put the build where the plugin looks for it

The plugin looks for `jacad` in this order:

1. `$JACAD_PATH`, if set in the **Herdr server's** environment (a variable exported in your shell
   doesn't reach panes, which Herdr's server starts)
2. `/Applications/Jaca.app/Contents/MacOS/jacad`
3. `~/Applications/Jaca.app/Contents/MacOS/jacad`

Pick one:

- **Next to your main install (recommended while this is experimental).** `/Applications/Jaca.app`
  stays as it is; the plugin finds the branch build in `~/Applications`:

  ```bash
  mkdir -p ~/Applications
  rm -rf ~/Applications/Jaca.app
  cp -R "$APP" ~/Applications/Jaca.app
  ```

  This only works while `/Applications/Jaca.app` has no `jacad`. Once your main install ships one,
  the plugin uses that instead.

- **As your main install.** Replaces `/Applications/Jaca.app` with the branch build:

  ```bash
  JACA_SKIP_AGENT=1 ./scripts/all.sh --install
  ```

Check that the daemon starts:

```bash
~/Applications/Jaca.app/Contents/MacOS/jacad status     # or /Applications/…
```

It prints a JSON status line (pid, protocol version, connections). `jacad stop` stops it; it also
exits by itself after 5 minutes with no clients and nothing running.

### 3. (Optional) Let the Jaca app use the daemon too

The plugin works on its own. For the app to share the daemon (the same device list, log sessions
and so on as the panes), turn on the areas you want. Run from any shell, then relaunch Jaca:

```bash
defaults write dev.srsouza.Jaca daemonAreas -array gradle xcode projects devices logs cloudLogging network
```

`network` only takes effect while Settings → Network inspection is set to "Agent HTTPS debugging";
with "HTTPS debugging", network capture stays in the app.

### 4. Build and link the plugin

```bash
cd herdr-plugin
go build -o bin/jaca-herdr ./cmd/jaca-herdr
herdr plugin link "$PWD"
herdr plugin list
```

`herdr plugin list` should show `dev.srsouza.jaca`, enabled. The plugin's startup hook starts
`jacad` when the Herdr server starts; before that, the first pane or action starts it.

The plugin stays linked to this folder: keep the checkout where it is, and rebuild `bin/jaca-herdr`
after pulling changes.

### 5. Use it

Open a pane:

```bash
herdr plugin pane open --plugin dev.srsouza.jaca --entrypoint devices    # device picker popup; opens logs in a new tab
herdr plugin pane open --plugin dev.srsouza.jaca --entrypoint gradle     # Gradle daemons
herdr plugin pane open --plugin dev.srsouza.jaca --entrypoint projects   # projects + cache cleanup
```

Run an action:

```bash
herdr plugin action list --plugin dev.srsouza.jaca
herdr plugin action invoke --plugin dev.srsouza.jaca gradle-kill-all
herdr plugin action invoke --plugin dev.srsouza.jaca projects-clear-cache   # uses the focused worktree
```

Keys for each pane are in `README.md`.

## Update

```bash
git pull                                                   # in the branch checkout
JACA_SKIP_AGENT=1 ./scripts/build.sh Release               # then copy the app again (step 2)
cd herdr-plugin && go build -o bin/jaca-herdr ./cmd/jaca-herdr
~/Applications/Jaca.app/Contents/MacOS/jacad stop          # the next pane or app start runs the new one
```

The Jaca app restarts a daemon left over from an older build by itself. The plugin doesn't: if
panes report a protocol mismatch, run `jacad stop`.

## Uninstall

Remove the plugin from Herdr:

```bash
herdr plugin unlink dev.srsouza.jaca
```

(`herdr plugin disable dev.srsouza.jaca` turns it off but keeps it linked.)

Stop the daemon and remove its runtime files (the socket, lock and log; nothing else lives there):

```bash
~/Applications/Jaca.app/Contents/MacOS/jacad stop    # or /Applications/…
rm -f ~/.jaca/jacad.sock ~/.jaca/jacad.lock ~/.jaca/jacad.log ~/.jaca/jacad.log.1
```

Leave the rest of `~/.jaca` alone: it holds Jaca's own data (cloud logging projects, override
rules, logs), which the app uses with or without the daemon.

If you turned on daemon mode for the app (step 3), turn it off and relaunch Jaca:

```bash
defaults delete dev.srsouza.Jaca daemonAreas
```

If you installed the branch build next to your main one:

```bash
rm -rf ~/Applications/Jaca.app
```

If you installed it as your main app, reinstall from `main` with `./scripts/all.sh --install` in
the main checkout.

Finally, delete the build output if you like: `rm -rf herdr-plugin/bin`.

## Troubleshooting

| Symptom | Check |
|---|---|
| `jacad not found: install Jaca.app in /Applications or set JACAD_PATH` | Step 2: the app with `jacad` isn't in either location. |
| A pane exits with `jacad closed the connection` | The daemon stopped. `jacad status` restarts it; `~/.jaca/jacad.log` says why it stopped. |
| `protocol N requested, daemon speaks M` | A daemon from another build is running. `jacad stop`, then reopen the pane. |
| An action seems to do nothing | `herdr plugin log list --plugin dev.srsouza.jaca` shows each command's output and exit code. |
| The startup hook failed | Same log. Usually step 2. |

Two places to look for more:

- `~/.jaca/jacad.log`: the daemon's own log (starts, stops, errors).
- `jacad describe`: every method and event topic the daemon serves; `jacad call METHOD [JSON]` and
  `jacad watch TOPIC` talk to it directly.
