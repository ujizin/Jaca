<p align="center">
  <img src="art/logo.png" alt="Jaca logo" width="128">
</p>

<h1 align="center">Jaca</h1>

<p align="center">
  A native macOS app for mobile developers: see your device logs and network traffic, mock responses, and clean up build caches.
</p>

<p align="center">
  <img src="https://img.shields.io/badge/macOS-14%2B-blue" alt="macOS 14+">
  <img src="https://img.shields.io/badge/Swift-5.9-orange" alt="Swift 5.9">
</p>

## What is Jaca

Connect an Android phone or emulator, an iOS simulator or an iPhone, and Jaca shows what it does: its logs and its network traffic. Open as many tabs as you need, for several devices or for one device with different filters.

Jaca also lists the caches that mobile builds leave behind (Gradle, Xcode DerivedData, git worktrees) and cleans them. It runs from the menu bar.

## Features

**Devices**

- **Logs:** live, colour-coded device logs. Filter by level, text, regex or app. Crashes are marked in the list.
- **Network:** see every request of an app, with headers, bodies, a JSON tree and a timeline. Copy a request as cURL or export HAR.
- **Response overrides:** right-click a request and choose "Override response…" to answer it with your own status and body.
- **Database browser:** open the SQLite databases of a debuggable Android app or an iOS Simulator app.
- **History:** every log session is saved and searchable.

**Your Mac**

- **Projects:** your Claude Code projects and the folders you add, with their git worktrees and disk sizes. Clean build caches or delete a worktree.
- **Gradle:** running Gradle daemons and the Gradle caches. Stop daemons and delete caches.
- **Xcode:** DerivedData folders by size, and which ones are stale.
- **Cloud Logging:** read Google Cloud Logging through `gcloud`.

**From the terminal**

- **`jaca` CLI:** read logs, find failed requests and add response overrides from a terminal.
- **Herdr plugin:** the same areas as panes in the Herdr terminal workspace. See [herdr-plugin/README.md](herdr-plugin/README.md).

## Quick start

You need macOS 14 or later, Xcode 26 or later with the iOS Simulator platform, and [XcodeGen](https://github.com/yonaskolb/XcodeGen) (`brew install xcodegen`).

1. Clone the Lemonade design system and Jaca into the same folder:

   ```bash
   git clone https://github.com/teya-engineering/lemonade-design-system.git
   git clone https://github.com/ujizin/Jaca.git
   cd Jaca
   ```

2. Build and launch:

   ```bash
   ./scripts/all.sh --no-agent
   ```

`--no-agent` skips the Android network agent, which needs the Android NDK and Kotlin. If you have them (see [CONTRIBUTING.md](CONTRIBUTING.md#optional-per-component)), run `./scripts/all.sh` with no flag.

For Android devices, install the Android SDK platform-tools so that `adb` is on your `PATH`, or set its path in Jaca's Settings.

### Install to /Applications

```bash
./scripts/all.sh --install
```

This builds Release, replaces `/Applications/Jaca.app`, links the `jaca` command line and launches the app. There are no prebuilt releases.

> [!NOTE]
> The in-app "Update Jaca" pulls `main` into the checkout you last built from, then rebuilds. It stashes and restores your local changes. Read [ARCHITECTURE.md](ARCHITECTURE.md#the-in-app-updater) for the exact steps.

## Use the `jaca` command line

```bash
jaca devices
jaca logs tail --grep timeout --follow
jaca net requests --failed
jaca overrides add --from REQUEST --status 503
```

Run `jaca --help` for every command. The CLI reads the sessions that the Jaca daemon runs. To find out which sessions those are, read [ARCHITECTURE.md](ARCHITECTURE.md#daemon-areas).

## Limitations

- The Android network agent works only with debuggable apps.
- The iOS Simulator network agent sees `URLSession` traffic only.
- Physical iOS devices have logs, but no network capture and no database browser.
- Capture over the Android companion app is experimental.
- Physical iOS logs use a private Apple framework, so a macOS or Xcode update can break them.

## Learn more

| Read | For |
|---|---|
| [ARCHITECTURE.md](ARCHITECTURE.md) | How the app, the daemon, the agents and the plugin fit together, the repository layout, and where Jaca keeps its data. |
| [docs/network-capture.md](docs/network-capture.md) | How each network capture source works. |
| [docs/README.md](docs/README.md) | The index of all docs. |
| [CONTRIBUTING.md](CONTRIBUTING.md) | Setup, tests and the code rules. Issues and pull requests go to [ujizin/Jaca](https://github.com/ujizin/Jaca). |
| [SECURITY.md](SECURITY.md) | What the MITM proxy, the root CA and the injected agents mean for your machine, and how to report a vulnerability. |

Jaca uses third-party components. The list with their licenses is in [ARCHITECTURE.md](ARCHITECTURE.md#third-party-components).
