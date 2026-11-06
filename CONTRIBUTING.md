# Contributing to Jaca

Jaca is built from source. This guide tells you how to set up a checkout, build it, test it and
send a change.

- Read [docs/architecture.md](docs/architecture.md) before you change the app.
- Report a security problem as [SECURITY.md](SECURITY.md) tells you, not in a public issue.
- Send issues and pull requests to [ujizin/Jaca](https://github.com/ujizin/Jaca).

## Quick start

```bash
mkdir jaca-dev && cd jaca-dev
git clone https://github.com/teya-engineering/lemonade-design-system.git
git clone https://github.com/ujizin/Jaca.git
cd Jaca
brew install xcodegen
./scripts/all.sh --no-agent     # generate, build and launch, without the Android agent
```

`--no-agent` lets you build the app without the Android toolchain. Network capture of Android
apps does not work in that build.

## Set up

### Required

| Item | Why |
|---|---|
| macOS 14 or later, Apple silicon | The deployment target is macOS 14. The Android and iOS Simulator agents are built for arm64 only. |
| Xcode 26 or later, at `/Applications/Xcode.app` | The scripts set `DEVELOPER_DIR` to this path. The Command Line Tools alone are not sufficient. |
| The iOS Simulator platform (Xcode > Settings > Components) | A post-build phase compiles `JacaNetAgent.dylib` with the iOS Simulator SDK. The build fails without it. |
| [XcodeGen](https://github.com/yonaskolb/XcodeGen) (`brew install xcodegen`) | `project.yml` is the source of the Xcode project. `Jaca.xcodeproj/` is generated and ignored by git. |
| [Lemonade](https://github.com/teya-engineering/lemonade-design-system), cloned next to the Jaca checkout | `project.yml` reads it as a local Swift package at `../lemonade-design-system`. It is not pinned to a version, so keep it on an up-to-date `main`. |

Lemonade is a third-party design system under the Apache-2.0 license. The folder layout must be:

```text
<parent folder>/
├── Jaca/
└── lemonade-design-system/
```

Quit Xcode before you build from the command line. A running Xcode can hold the Swift package
locks and make `xcodebuild` stop responding.

### Optional, per component

| Component | Tools |
|---|---|
| Android agent (`agent/`) | Android SDK at `~/Library/Android/sdk` (or `$ANDROID_HOME`), NDK 27.2.12479018, CMake 3.22.1, platform `android-36`, build-tools (for `d8`), Kotlin from Homebrew (`brew install kotlin`, read from `/opt/homebrew/opt/kotlin`), a JDK that supports `javac --release 11`. |
| Companion app (`mobile/`) | JDK 17, Android SDK, NDK 27.2.12479018, CMake 3.22.1. A newer JDK can fail the Gradle build. |
| Herdr plugin (`herdr-plugin/`) | Go 1.22 or later. Herdr 0.9.0 or later to run it. |
| gRPC stubs (`proto/`) | `brew install protobuf swift-protobuf`, plus the grpc-swift 1.27.5 `protoc-gen-grpc-swift` plugin. See `scripts/proto-gen.sh`. |
| Runtime features | `adb` (Android), `idevicesyslog` (fallback for physical iOS logs), `gcloud` (Cloud Logging), `claude` (Ask Claude, Open in Herdr), `herdr` (Open in Herdr), Google Chrome (This Mac capture). Install a tool only for the feature that uses it. |

## Build and run

All scripts run from the repository root. See [scripts/README.md](scripts/README.md) for the full
list.

| Goal | Command |
|---|---|
| Generate, build and launch | `./scripts/all.sh` |
| Build without the Android agent | `./scripts/all.sh --no-agent`, or `JACA_SKIP_AGENT=1 ./scripts/build.sh` |
| Build, then relaunch | `./scripts/run.sh [Debug\|Release]` |
| Build only | `./scripts/build.sh [Debug\|Release]` |
| Regenerate `Jaca.xcodeproj` | `./scripts/gen.sh` |
| Install to `/Applications` and link the `jaca` CLI | `./scripts/all.sh --install` |

Source files are found by glob from `Sources/`, `Tests/` and `UITests/`. To add a file, create it
and run `./scripts/gen.sh`. You do not need to change `project.yml`.

Do not put a `README.md` or other loose file in `Sources/`, `Tests/`, `UITests/` or `Resources/`.
XcodeGen adds the files in those folders to the app bundle.

## Test

### Run one test

Generate the project first (`./scripts/gen.sh`). Then run:

```bash
DEVELOPER_DIR=/Applications/Xcode.app/Contents/Developer TEST_RUNNER_JACA_DAEMON_AREAS=none \
xcodebuild -project Jaca.xcodeproj -scheme Jaca -configuration Debug -destination 'platform=macOS' \
  CODE_SIGN_IDENTITY="-" CODE_SIGNING_REQUIRED=NO CODE_SIGNING_ALLOWED=YES \
  -only-testing:JacaTests/ProjectsGroupingTests/test_isWorktreePath_andParentExtraction test
```

To run a whole class, give only the class: `-only-testing:JacaTests/ProjectsGroupingTests`.

Keep `TEST_RUNNER_JACA_DAEMON_AREAS=none` in the command:

- The app hosts the unit tests.
- If your Mac has `daemonAreas` set, the test host connects to the real daemon socket,
  `~/.jaca/jacad.sock`.
- If that daemon is older than the build under test, the test host stops it and starts its own.
- `none` matches no area, so the test host stays in-process and your running daemon is not touched.

### Live tests

Some test classes need a real device, simulator or tool. Some of them fail, not skip, when the
device or tool is missing.

| Class | Needs | Without it |
|---|---|---|
| `LiveAndroidTests` | An adb device or emulator | Skips |
| `LiveSimulatorTests` | A booted simulator | Skips |
| `LiveAgentCaptureTests` | adb, the built agent, the debuggable app `com.example.app.dev` | Can fail |
| `LiveCompanionGrpcTests` | The companion app on `127.0.0.1:8889` (`adb forward tcp:8889 tcp:8889`) | Fails |
| `CrashDetectorLiveTests` | adb and an Android device | Fails without adb |
| `LiveChromeOverrideTests` | Google Chrome | Skips |
| `AppLevelE2ETests` | `emulator-5554` with `com.example.app.dev` | Skips |
| `AppModelIntegrationTests` | A device or simulator | Skips. It can be flaky. |

To check logic without devices, skip the live classes:

```bash
-skip-testing:JacaTests/LiveAgentCaptureTests \
-skip-testing:JacaTests/LiveCompanionGrpcTests \
-skip-testing:JacaTests/CrashDetectorLiveTests
```

If a test fails and your change does not touch its area, run it again on `main` before you look
for a cause in your diff.

### Other test suites

| Suite | Command |
|---|---|
| UI tests | `./scripts/uitest.sh` |
| iOS Simulator agent (host tests of `agent/iOS/JacaAgentHTTP.m`) | The one-test command above with `-only-testing:JacaAgentTests` |
| Herdr plugin | See [Herdr plugin](#herdr-plugin) |

## Work in a git worktree

`project.yml` finds Lemonade at `../lemonade-design-system`, relative to the checkout root. A
worktree under `.claude/worktrees/<name>` is two levels lower, so XcodeGen fails with
"Invalid local package Lemonade".

Before you build in such a worktree, add a symlink. Replace the path with the path of your
Lemonade checkout:

```bash
ln -sf "<path to your lemonade-design-system checkout>" \
  "$(git rev-parse --show-toplevel)/../lemonade-design-system"
```

The symlink is necessary only at build time. If a clean removes it, create it again.

## Stable dev signing (optional)

Debug builds are ad-hoc signed, so each build has a new code signature. Run this command once to
give every build the same signature:

```bash
./scripts/dev-signing.sh
```

The script creates a self-signed "Jaca Dev" identity in a dedicated keychain. macOS asks you to
trust it one time. After that, `build.sh` re-signs each build with it. If you do not run the
script, builds stay ad-hoc signed. [SECURITY.md](SECURITY.md) explains the risk of this keychain.

## The in-app updater acts on your checkout

> **Warning:** Commit or push your work before you click an update in a build that you made
> yourself. The updater changes the branch and the files of your checkout.

Each build writes `~/Library/Application Support/Jaca/build-info.json` with the checkout path and
the commit. The last checkout that you built becomes the update source. If that file exists, the
app compares the commit with `origin/main`. An update does these steps in that checkout:

1. If the tree has changes, `git stash push -u` (untracked files too).
2. If the branch is not `main`, `git checkout main`.
3. `git pull --ff-only origin main`.
4. `scripts/all.sh --install --no-run`, which replaces `/Applications/Jaca.app`.
5. `git checkout <your branch>`, then `git stash pop`.

If a step fails, the updater tries to restore the branch and the stash. If the build came from a
linked worktree, the app offers to switch to the main checkout and update there.

## Architecture rules

[docs/architecture.md](docs/architecture.md) has the full text. These rules apply to every change:

| Rule | What to do | Reference |
|---|---|---|
| Layers | `Sources/Core` has no SwiftUI, AppKit or Lemonade. `Model` holds `@Observable @MainActor` state. `Features` holds thin SwiftUI views. Core must never reference Model or Features: the `jacad` target compiles Core alone and fails if it does. | `project.yml` (`jacad` target) |
| One owner for shared state | Put state that several screens read in one observable owner. Views read it directly. Do not poll model state on a timer. | `Sources/Model/CompanionRegistry.swift` |
| Cache-first areas | Load the last result from disk in `init`. Block only the first scan. Refresh in the background, behind a staleness TTL. | `Sources/Model/ProjectsModel.swift`, `Sources/Core/Projects/ProjectsCache.swift` |
| Migration-safe `Codable` | For each persisted model, write a tolerant `init(from:)` with `decodeIfPresent(...) ?? default`. Load arrays with `CloudPersistence.decodeArray`. Add a test that decodes JSON of the old schema. | `Sources/Core/CloudLogging/CloudProjectStore.swift`, `Sources/Core/CloudLogging/CloudPersistence.swift`, `Tests/CloudMigrationTests.swift`, `Tests/OverrideMigrationTests.swift` |
| Design system | Build UI from `LemonadeUi.*`, `LemonadeTheme.colors.*`, `LemonadeTypography.shared.*` and `GroveIcon(glyph:)`. Do not hardcode colors or fonts. | Neighboring views in `Sources/Features/` |
| Animation | Animate each visible state change with a short `.easeInOut` (about 0.15 to 0.3 s) and a fade or slide transition. | Neighboring views in `Sources/Features/` |
| Testable logic | Put pure logic in free functions or enums, and unit-test them. Do not test logic through the UI. | `LogcatParser`, `ProjectsGrouping`, `WorktreePorcelainParser` |
| No sandbox | Do not add the App Sandbox entitlement. The app starts `adb`, `xcrun` and `git` as processes. | `project.yml` |

To add a top-level area, read one area end to end first (Projects or Xcode). The steps are in
[docs/architecture.md](docs/architecture.md).

### Contracts and generated code

- If you change `proto/companion.proto`, run `./scripts/proto-gen.sh`. Do not edit
  `Sources/Core/Companion/Generated/` by hand.
- If you change the companion flow or its wire protocol, update
  [docs/companion-architecture.md](docs/companion-architecture.md).
- If you change the daemon protocol, its areas or the `jaca` CLI, update
  [docs/daemon-plan.md](docs/daemon-plan.md).
- If you change the agent route frame, change all three sides and
  [docs/agent-http-contract.md](docs/agent-http-contract.md).
- Keep `protocolVersion` in `herdr-plugin/cmd/jaca-herdr/rpc.go` equal to `DaemonProtocol.version`
  in `Sources/Core/Daemon/DaemonProtocol.swift`.

## Component workflows

### Herdr plugin

The plugin is a Go module with no dependencies. Run these checks in `herdr-plugin/` before you
send a change. CI runs the same checks.

```bash
cd herdr-plugin
test -z "$(gofmt -l .)"
go vet ./...
go test ./...
```

[herdr-plugin/README.md](herdr-plugin/README.md) and [herdr-plugin/INSTALL.md](herdr-plugin/INSTALL.md)
tell you how to build and link the plugin.

### Android agent

`./scripts/build.sh` builds the agent when its output is missing or older than its sources. To
build it by hand, or to try it on a device:

```bash
cd agent
./build.sh && ./build-dex.sh       # writes agent/out/
./deploy.sh [package] [serial]     # build, push, attach and follow the log
```

`deploy.sh` uses `com.example.app.dev` on `emulator-5554` by default. If you build with
`--no-agent` (or `JACA_SKIP_AGENT=1`) and `agent/out` has the `.so` but not the dex files, the copy
step in `scripts/build.sh` stops the build. Run `rm -rf agent/out` and build again. See
[agent/README.md](agent/README.md).

### Companion app

```bash
./scripts/build-mobile.sh
```

The script builds the APK with Gradle and copies it to `Resources/`. Use JDK 17. Only
`./scripts/all.sh --install` runs it for you. See [mobile/README.md](mobile/README.md).

## Commits

Use [Conventional Commits](https://www.conventionalcommits.org/) with a scope, as in the history:

```text
feat(overrides): group overrides in folders
fix(proxy): let the app and jacad forward plain http://
docs(cli): overrides and the CLI's network commands need agent mode
```

Common types are `feat`, `fix`, `docs`, `test`, `perf` and `chore`. Common scopes are
`herdr-plugin`, `worktrees`, `network`, `overrides`, `daemon`, `agent`, `cli`, `proxy`,
`projects`, `logs` and `cloud-logs`.

## Pull requests

1. Fork [ujizin/Jaca](https://github.com/ujizin/Jaca).
2. Create a branch from `main`.
3. Make the change, with tests for new logic.
4. Run the tests for each component that you changed.
5. Open a pull request against `main` of `ujizin/Jaca`. GitHub can select a different base
   repository for a fork, so check the base before you submit.
6. Fill in the pull request template.

Keep a pull request to one topic. Do not commit `Jaca.xcodeproj/`, build output or files in
`Resources/` that the scripts generate.

## Text in the UI

Take each user-visible string from the issue that asks for it. If the issue does not give the text,
ask in the issue. Do not reword an existing string as part of an unrelated change. Open an issue
for it.

The same rule applies to text that the `jaca` CLI prints. Its table columns are the JSON field
names.
