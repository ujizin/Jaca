# Install the Jaca Herdr plugin

The plugin shows Jaca areas as Herdr panes. It holds no data of its own: every pane and action
talks to `jacad`, the Jaca daemon, over `~/.jaca/jacad.sock`. You install two pieces:

1. **A Jaca build that ships `jacad`** (`Jaca.app/Contents/MacOS/jacad`). Builds from `main` ship it.
2. **The plugin** (this folder), linked into Herdr.

For the plugin's status, panes and keys, read [README.md](README.md). For the daemon, read
[docs/daemon-plan.md](../docs/daemon-plan.md).

> If `/Applications/Jaca.app/Contents/MacOS/jacad` already exists, go to
> [step 4](#4-build-and-link-the-plugin).

## Requirements

| Need | Notes |
|---|---|
| Jaca build tools | Xcode, `xcodegen` and the rest of what Jaca needs to build. See the repo [README.md](../README.md). |
| Go | 1.24 or newer (`brew install go`). |
| Herdr | 0.9.0 or newer, with its server running (`herdr status`). |

## Install

### 1. Build and install Jaca

From the repo root:

```bash
./scripts/all.sh --install --no-agent
```

This builds a Release app, copies it to `/Applications/Jaca.app` and launches it. Remove
`--no-agent` if the Android toolchain is installed and you want Android agent capture.

To build without installing, run `JACA_SKIP_AGENT=1 ./scripts/build.sh Release`. Then find the
built app:

```bash
APP="$(xcodebuild -project Jaca.xcodeproj -scheme Jaca -configuration Release \
  -destination 'platform=macOS' -showBuildSettings 2>/dev/null \
  | awk '/ BUILT_PRODUCTS_DIR =/{d=$3} / FULL_PRODUCT_NAME =/{n=$3} END{print d"/"n}')"
ls "$APP/Contents/MacOS/jacad"
```

### 2. Check where the plugin finds `jacad`

The plugin looks for `jacad` in this order and uses the first that exists:

1. `$JACAD_PATH`, if it is set in the **Herdr server's** environment. A variable you export in
   your shell does not reach panes, because the Herdr server starts them.
2. `/Applications/Jaca.app/Contents/MacOS/jacad`
3. `~/Applications/Jaca.app/Contents/MacOS/jacad`
4. The newest Xcode build:
   `~/Library/Developer/Xcode/DerivedData/Jaca-*/Build/Products/*/Jaca.app/Contents/MacOS/jacad`

After step 1 with `--install`, location 2 has it. A build from `./scripts/run.sh` or
`./scripts/build.sh` is found at location 4.

Check that the daemon starts:

```bash
/Applications/Jaca.app/Contents/MacOS/jacad status
```

It prints a JSON status line (pid, protocol version, connections). `jacad stop` stops it. The
daemon also exits by itself after 5 minutes with no clients and nothing running.

### 3. (Optional) Let the Jaca app use the daemon

The plugin works on its own. To make the app share the daemon (the same device list, log
sessions and so on as the panes), turn on the areas you want. Run this from any shell, then
relaunch Jaca:

```bash
defaults write dev.srsouza.Jaca daemonAreas -array gradle xcode projects devices logs cloudLogging network
```

`network` takes effect only while Settings → Network inspection is set to "Agent HTTPS
debugging". With "HTTPS debugging", network capture stays in the app.

### 4. Build and link the plugin

```bash
cd herdr-plugin
go build -o bin/jaca-herdr ./cmd/jaca-herdr
herdr plugin link "$PWD"
herdr plugin list
```

`herdr plugin list` shows `dev.srsouza.jaca`, enabled. The startup hook of the plugin starts
`jacad` when the Herdr server starts. Before that, the first pane or action starts it.

The plugin stays linked to this folder. Keep the checkout where it is, and build
`bin/jaca-herdr` again after you pull changes.

### 5. Use it

Open a pane with `herdr plugin pane open --plugin dev.srsouza.jaca --entrypoint <id>`:

| Id | Opens as | What it does |
|---|---|---|
| `devices` | popup | The device picker. It opens logs, network or the database in a new tab. On This Mac, it opens network (Chrome). |
| `logs` | tab | Device logs. Opened directly, it shows the device picker first. |
| `network` | tab | Network requests and response overrides. Opened directly, it shows the device picker first. |
| `database` | tab | An app's SQLite databases. Opened directly, it shows the device picker first. |
| `tools` | popup | The Gradle / Xcode picker. It opens the choice in a new tab. |
| `gradle` | tab | Gradle caches and daemons. |
| `xcode` | tab | Xcode DerivedData. |
| `cloud` | popup | Cloud Logging home. Log sessions open in tabs. |
| `projects` | tab | Projects and cache cleanup. |

Run an action:

```bash
herdr plugin action list --plugin dev.srsouza.jaca
herdr plugin action invoke --plugin dev.srsouza.jaca gradle-kill-all
herdr plugin action invoke --plugin dev.srsouza.jaca projects-clear-cache   # uses the focused worktree
```

To open panes with a key, read [Herdr key bindings](README.md#herdr-key-bindings). The keys of
each pane are in [README.md](README.md).

## Update

From the repo root:

```bash
git pull
./scripts/all.sh --install --no-agent
cd herdr-plugin && go build -o bin/jaca-herdr ./cmd/jaca-herdr
/Applications/Jaca.app/Contents/MacOS/jacad stop   # the next pane or app start runs the new one
```

The Jaca app replaces a daemon from an older build by itself. The plugin does not. If panes
report a protocol mismatch, run `jacad stop`.

## Uninstall

1. Remove the plugin from Herdr:

   ```bash
   herdr plugin unlink dev.srsouza.jaca
   ```

   `herdr plugin disable dev.srsouza.jaca` turns it off but keeps it linked.

2. Stop the daemon and remove its runtime files (its socket, lock and logs):

   ```bash
   /Applications/Jaca.app/Contents/MacOS/jacad stop
   rm -f ~/.jaca/jacad.sock ~/.jaca/jacad.lock ~/.jaca/jacad.log ~/.jaca/jacad.log.1
   ```

   Do not remove the rest of `~/.jaca`. It holds Jaca's own data (Cloud Logging projects,
   override rules, logs), which the app uses with or without the daemon.

3. If you turned on daemon mode for the app (install step 3), turn it off and relaunch Jaca:

   ```bash
   defaults delete dev.srsouza.Jaca daemonAreas
   ```

4. Optional: delete the build output with `rm -rf herdr-plugin/bin`.

## Troubleshooting

| Symptom | Check |
|---|---|
| `jacad not found: install Jaca.app in /Applications or set JACAD_PATH` | No location in [step 2](#2-check-where-the-plugin-finds-jacad) has `jacad`. |
| The startup hook failed | Run `herdr plugin log list --plugin dev.srsouza.jaca`. The cause is usually step 2. |

For messages that panes show while they run, read [Troubleshooting](README.md#troubleshooting)
in the README.
