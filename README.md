<div align="center">

<img src="art/logo.png" alt="Jaca logo" width="128">

# Jaca

A native macOS tool for mobile developers: device logs, network capture, response overrides and local build-cache cleanup.

![macOS 14+](https://img.shields.io/badge/macOS-14%2B-blue)
![Swift 5.9](https://img.shields.io/badge/Swift-5.9-orange)

</div>

## What is Jaca

Jaca is a SwiftUI app for macOS. It shows what your Android devices, emulators, iOS simulators and physical iOS devices do: their logs and their network traffic. You can open many tabs at the same time, for several devices or for one device with different filters. Jaca also cleans the local caches that mobile builds leave behind, and it reads Google Cloud Logging.

Jaca runs from the menu bar. A background daemon, `jacad`, serves the same features to the `jaca` command line and to a plugin for the Herdr terminal workspace.

There are no prebuilt releases. Build Jaca from source with the [Quick start](#quick-start).

## Features

### Devices

The sidebar finds Android devices and emulators (`adb`), iOS simulators (`simctl`), physical iOS devices (`devicectl`) and "This Mac". The device menu has "Start Logcat", "Inspect Network" and "Browse Database".

| Feature | What it does |
|---|---|
| **Logs** | Streams `adb logcat` and simulator logs into a filtered, colour-coded list. Physical iOS devices stream structured logs (level, subsystem, category). Filter by level, text, regex or app (the app list comes from the device). Jaca marks crashes inline and counts them. When the app dies or restarts, the stream reconnects and puts a marker in the list. A follow tail holds the scroll position while you read. You can set the copy format, clear the list and export it. |
| **Network capture** | Three capture sources (see below). Each tab has a request list, a timeline with drag-to-filter, headers, bodies with a JSON tree, "Copy as cURL" and "Export HAR". |
| **Response overrides** | Right-click a request and choose "Override response…". A rule matches a method and a URL pattern and answers with your status, headers and body. Rules can go in groups, and one switch pauses all of them. |
| **Database browser** | Opens the SQLite databases of a debuggable Android app or an iOS Simulator app from a read-only snapshot. Shows table pages, runs SQL and shows row detail. |
| **History** | Saves every log session to SQLite with full-text search. Browse it by device, app and session. Settings sets the retention (default 7 days). |

The three network capture sources:

| Source | Targets | How it works |
|---|---|---|
| In-process agent | Debuggable Android apps, iOS Simulator apps | Jaca injects an agent into the app and reads requests before TLS. No proxy and no CA. Works with certificate pinning. Records the call stack on Android. |
| Chrome browser | This Mac | Jaca opens a separate Chrome window with its own profile and sends its traffic through a loopback proxy. |
| Companion stream | Android phones (experimental) | The Jaca companion app captures all device traffic with a `VpnService` and streams it to the Mac over gRPC/TLS. The Mac decrypts TLS. Turn it on in Settings → Network inspection → "HTTPS debugging". |

Details: [docs/network-capture.md](docs/network-capture.md), [docs/agent-https-debugging-android.md](docs/agent-https-debugging-android.md), [docs/agent-https-debugging-ios.md](docs/agent-https-debugging-ios.md), [docs/companion-architecture.md](docs/companion-architecture.md).

### Maintenance areas

| Area | What it does |
|---|---|
| **Projects** | Lists the projects Claude Code ran in (`~/.claude/projects`) and folders you add, each with its git worktrees. Shows disk sizes, cleans build caches (`.gradle/`, `build/`, the matching DerivedData), deletes worktrees and opens a checkout in Finder, Zed or Herdr. |
| **Gradle** | Lists running Gradle daemons (JDK, heap, CPU, uptime) and kills them. Shows the folders of `~/.gradle/caches` with sizes and deletes them. |
| **Xcode** | Lists DerivedData folders with size and kind (live, stale or shared) and deletes them. |
| **Cloud Logging** | Reads Google Cloud Logging through `gcloud`. Session tabs have a time range, severity, filters, live polling and SQL over the loaded entries. |

### Daemon, CLI and Herdr plugin

| Component | What it does |
|---|---|
| **`jacad`** | A background daemon inside `Jaca.app`. It serves each area over newline-delimited JSON-RPC on `~/.jaca/jacad.sock`. The app can run an area in the daemon instead of in-process (opt-in per area). See [docs/daemon-plan.md](docs/daemon-plan.md). |
| **`jaca` CLI** | The same binary, run as `jaca`. It lists devices, tails logs, searches captured requests and manages response overrides from a terminal. |
| **Herdr plugin** | Shows the Jaca areas as Herdr panes. See [herdr-plugin/README.md](herdr-plugin/README.md). |

## Quick start

### Requirements

| Item | Requirement |
|---|---|
| macOS | 14 or later. An Apple Silicon Mac is recommended: the Android agent and the iOS Simulator agent ship arm64 builds only. |
| Xcode | A full Xcode 26 or later (not only the Command Line Tools), with the iOS Simulator platform installed. The build compiles the iOS Simulator agent and fails without that platform. |
| XcodeGen | `brew install xcodegen` |
| Lemonade | The design system, a local Swift package. Clone it next to the Jaca checkout (see below). |
| Android agent toolchain | NDK 27.2.12479018, CMake 3.22.1, platform `android-36`, build-tools (for `d8`), Kotlin (`brew install kotlin`) and a JDK. If you do not have it, build with `--no-agent`. |
| `adb` | Android SDK platform-tools, on your `PATH` or set in Jaca's Settings. Needed for Android devices. |

<details>
<summary>Optional tools</summary>

| Tool | Used for |
|---|---|
| `idevicesyslog` (`brew install libimobiledevice`) | Fallback for physical iOS logs when Apple's LoggingSupport framework does not load. |
| `gcloud` | Cloud Logging. |
| `claude` (Claude Code) | "Ask Claude" in Cloud Logging, and Open in Herdr in Projects. |
| `herdr` | Open in Herdr, and the Herdr plugin. |
| Google Chrome | Network capture on This Mac. |
| Zed | Open in Zed in Projects. |
| JDK 17, NDK 27.2.12479018, CMake 3.22.1 | Building the companion APK (`./scripts/build-mobile.sh`). |
| Go 1.22+ | Building the Herdr plugin. |

</details>

### Build and run

1. Clone Lemonade and Jaca into the same parent folder. The Xcode project expects Lemonade at `../lemonade-design-system`.

   ```bash
   git clone https://github.com/teya-engineering/lemonade-design-system.git
   git clone https://github.com/ujizin/Jaca.git
   cd Jaca
   ```

2. Build and launch:

   ```bash
   ./scripts/all.sh
   ```

`all.sh` generates the Xcode project with XcodeGen, builds the Android agent and the app (Debug), and launches it. Lemonade is not version-pinned. Keep your Lemonade checkout on an up-to-date `main`, because an old checkout can fail to compile.

| Flag | Effect |
|---|---|
| `--release` | Build the Release configuration. |
| `--install` | Build Release, install to `/Applications/Jaca.app`, link the `jaca` CLI, then launch. |
| `--no-agent` | Build without the in-process Android agent. Use it when the Android toolchain is not installed. |
| `--no-run` | Build only. Do not launch. |

The other scripts (`run.sh`, `build.sh`, `gen.sh`, `uitest.sh`, `build-mobile.sh` and more) are listed in [scripts/README.md](scripts/README.md).

## Install to /Applications

```bash
./scripts/all.sh --install
```

This command does these steps:

1. It builds the companion APK, if it can, so that the app can serve it for QR onboarding.
2. It builds Release and quits a running Jaca.
3. It replaces `/Applications/Jaca.app` and links the `jaca` CLI.
4. It launches the app. Add `--no-run` to skip the launch.

Jaca starts as a menu-bar app. To show it in the Dock and in ⌘-Tab, use Settings.

<details>
<summary>Optional: a stable code signature for local builds</summary>

Local builds are ad-hoc signed, so each rebuild gets a new code signature. Run this script once to create a self-signed "Jaca Dev" identity in a separate keychain:

```bash
./scripts/dev-signing.sh
```

After that, `build.sh` re-signs each build with that identity. The separate keychain has a known password, written in the script. To remove it, run the undo command at the top of `scripts/dev-signing.sh`.

</details>

### The in-app updater

The updater acts on your source checkout. Read this before you click "Update".

- **When it is on:** each build writes `~/Library/Application Support/Jaca/build-info.json` with the checkout path and the commit. The last checkout you built, also a worktree, becomes the update source. If that file or that checkout does not exist, the updater is off.
- **How it checks:** it runs `git fetch origin main` in that checkout. It shows "Update available" in the sidebar and an "Update Jaca" item in the menu-bar menu when `origin/main` has new commits.
- **What an update does to the checkout:**
  1. If the working tree has changes, it runs `git stash push -u` (untracked files are included).
  2. If you are not on `main`, it runs `git checkout main`.
  3. It runs `git pull --ff-only origin main`.
  4. It runs `scripts/all.sh --install --no-run`, which replaces `/Applications/Jaca.app`. It adds `--no-agent` when `agent/out` already has files.
  5. It checks out your branch again and runs `git stash pop`.
- **If a step fails:** it checks out your branch and pops the stash, then shows the error.
- **Worktrees:** `main` can be checked out in one worktree only. For a build from a linked worktree, the menu offers "Switch to Main Checkout & Update…".

The updater follows the `origin` remote of the checkout. If you cloned `https://github.com/ujizin/Jaca.git`, it follows this repository.

### The `jaca` CLI

`./scripts/all.sh --install` links `jaca` for you. To link it by hand:

```bash
./scripts/install-cli.sh                  # links /Applications/Jaca.app/Contents/MacOS/jacad as jaca
./scripts/install-cli.sh /path/to/Jaca.app
JACA_BIN_DIR=~/bin ./scripts/install-cli.sh
```

The link goes to `$JACA_BIN_DIR`, else to `/usr/local/bin` when it is writable, else to `~/.local/bin`. If you get `~/.local/bin`, make sure that folder is on your `PATH`.

Some commands:

```bash
jaca devices
jaca logs tail --grep timeout --follow
jaca net requests --failed
jaca net show REQUEST
jaca overrides add --from REQUEST --status 503
jaca overrides disable --group GROUP
```

Run `jaca --help` for the full list. `--json` prints JSON, and `--raw` shows Authorization and cookie values that are hidden by default. The CLI sees log and network sessions only when the app runs those areas in `jacad`, or when Herdr panes opened them. Network sessions also need Settings → Network inspection on "Agent HTTPS debugging" (the default). In "HTTPS debugging" mode, the app captures in-process and the CLI sees no network session.

<details>
<summary>Run app areas in the daemon</summary>

The app runs every area in-process by default. To run areas in `jacad`, so that `jaca` and Herdr see the same sessions as the app:

```bash
defaults write dev.srsouza.Jaca daemonAreas -array gradle xcode projects devices logs cloudLogging network
```

The `JACA_DAEMON_AREAS` environment variable (`all`, or a comma-separated list) overrides this setting. If the app cannot reach the daemon, the area runs in-process. Plan and status: [docs/daemon-plan.md](docs/daemon-plan.md).

</details>

### The Herdr plugin

The plugin shows devices, logs, network, database, Cloud Logging, Gradle, Xcode and Projects as Herdr panes. It needs Go 1.22+, Herdr 0.9.0+ and a `Jaca.app` that contains `jacad`. Follow [herdr-plugin/INSTALL.md](herdr-plugin/INSTALL.md).

## Repository layout

```text
Jaca/
├── project.yml        XcodeGen spec (targets Jaca, jacad, JacaTests, JacaAgentTests, JacaUITests)
├── Sources/
│   ├── App/           Jaca GUI: entry point, menu-bar item, root view, assets
│   ├── Model/         Jaca GUI: @Observable state (AppModel, sessions, area models)
│   ├── Features/      Jaca GUI: SwiftUI views, one folder per feature
│   ├── Core/          Shared engine, no SwiftUI: devices, logs, network, overrides,
│   │                  daemon server (Core/Daemon), the jaca CLI (Core/CLI), areas
│   └── Daemon/        Entry point of jacad (and of jaca, through a symlink)
├── agent/             In-process network agents: Android (native/, java/, kotlin/,
│                      okhttp/, okhttp-stubs/) and iOS Simulator (iOS/)
├── mobile/            Android companion app (Compose Multiplatform)
├── proto/             gRPC contract between the Mac and the companion app
├── herdr-plugin/      Herdr plugin (Go), a client of jacad
├── docs/              Design docs and plans
├── scripts/           Build, run, install and diagnostic scripts
├── Tests/             XCTest unit and live tests (Tests/ObjC tests the iOS agent)
├── UITests/           XCUITest smoke tests
├── Config/            Extra Info.plist keys for the app and jacad
├── Resources/         Build outputs copied into the app (agent, companion APK); not committed
└── art/               logo.png, the app icon at 1024 px
```

The Xcode project is generated from `project.yml` and is not committed. Each component has its own README:

| Component | Read |
|---|---|
| Architecture of the app, the engine and the daemon | [docs/architecture.md](docs/architecture.md) |
| Android and iOS Simulator agents | [agent/README.md](agent/README.md) |
| Companion app | [mobile/README.md](mobile/README.md) |
| gRPC contract | [proto/README.md](proto/README.md) |
| Herdr plugin | [herdr-plugin/README.md](herdr-plugin/README.md) |
| Scripts | [scripts/README.md](scripts/README.md) |

## Documentation

The index of all docs is [docs/README.md](docs/README.md).

## Contributing

Read [CONTRIBUTING.md](CONTRIBUTING.md) for the setup, how to run the tests, and the code rules. Open issues and pull requests on [ujizin/Jaca](https://github.com/ujizin/Jaca).

## Security

Jaca runs a man-in-the-middle proxy, creates a root CA and injects code into apps. Read [SECURITY.md](SECURITY.md) for what that means for your machine and your devices, and for how to report a vulnerability.

## Limitations

- **No releases.** You build Jaca from source.
- **Android agent:** works only with debuggable apps. It captures `HttpURLConnection`, `HttpsURLConnection`, OkHttp3 and OkHttp2, and no other HTTP stacks. It ships for `arm64-v8a` only. Captured bodies stop at 1 MB. Streaming responses (server-sent events, gRPC) show without a body. OkHttp2 responses show without a body.
- **iOS Simulator agent:** captures `URLSession` traffic only. It does not see `WKWebView`, background sessions or raw sockets. It has no call stacks.
- **Physical iOS devices:** no network capture and no database browser.
- **Companion stream (HTTPS debugging):** experimental and Android only. Apps that pin certificates are not decrypted and can stop working. QUIC (HTTP/3) traffic is not decrypted.
- **Private Apple frameworks:** physical iOS logs use the private LoggingSupport framework. A macOS or Xcode update can break them. In that case Jaca falls back to `idevicesyslog`.

## Acknowledgements

Jaca uses these third-party components:

| Component | Where | License |
|---|---|---|
| [Lemonade design system](https://github.com/teya-engineering/lemonade-design-system) | Sibling checkout (`../lemonade-design-system`); `com.teya.foundation:lemonade-ui` in `mobile/` | Apache-2.0 |
| AOSP slicer, from the dexter project | Vendored in `agent/native/third_party/slicer` | Apache-2.0 (file headers; the folder has no license file) |
| `jvmti.h` | `agent/native/include` | GPLv2 with the Classpath exception (file header) |
| zdtun | Vendored in `mobile/composeApp/src/androidMain/jni/zdtun` | LGPL-3.0 or later (`COPYING`) |
| uthash | Vendored in `mobile/composeApp/src/androidMain/jni/zdtun/third_party/uthash.h` | BSD (file header) |
| [swift-nio](https://github.com/apple/swift-nio), [swift-nio-ssl](https://github.com/apple/swift-nio-ssl), [swift-certificates](https://github.com/apple/swift-certificates), [swift-crypto](https://github.com/apple/swift-crypto), [swift-asn1](https://github.com/apple/swift-asn1), [swift-protobuf](https://github.com/apple/swift-protobuf) | Swift packages in `project.yml` | See each repository |
| [grpc-swift](https://github.com/grpc/grpc-swift) | Swift package in `project.yml` | See its repository |
