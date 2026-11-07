# Scripts

Run every script from the repository root. The build scripts set `DEVELOPER_DIR` to `/Applications/Xcode.app/Contents/Developer` when it is not set, because `xcode-select` can point at the Command Line Tools.

| Script | What it does | Arguments and environment | In-app updater |
|---|---|---|---|
| `all.sh` | Generates the project, builds the app with both agents, and launches it. | `--release`, `--debug` (default), `--install` (Release build, companion APK first, copy to `/Applications/Jaca.app`, then `install-cli.sh`), `--no-agent` (sets `JACA_SKIP_AGENT=1`), `--no-run`, `-h`/`--help`. Needs `xcodegen` and a full Xcode. | Calls it: `all.sh --install --no-run`, plus `--no-agent` when `agent/out` is not empty |
| `build.sh [Debug\|Release]` | Builds the Android agent when it is missing or stale and copies it into `Resources/`. Then runs `xcodegen generate` and `xcodebuild` (ad-hoc signing), adds `NSBonjourServices` (`_jaca._tcp`) to the built Info.plist, and runs `dev-resign.sh`. | `JACA_SKIP_AGENT=1` builds without the Android agent. Without it, a missing Android toolchain fails the build. | Through `all.sh` |
| `run.sh [Debug\|Release]` | Runs `build.sh`, quits a running Jaca (waits up to 15 s), and opens the new build. | Configuration, default `Debug` | No |
| `gen.sh` | Regenerates `Jaca.xcodeproj` from `project.yml`. | None | No |
| `uitest.sh` | Kills stray Jaca and test processes, then runs the `JacaUITests` suite. | None | No |
| `install.sh` | Runs `all.sh --install`. | Passes its arguments to `all.sh` | No |
| `install-cli.sh [APP]` | Links `jaca` to `APP/Contents/MacOS/jacad`. | `APP` defaults to `/Applications/Jaca.app`. `JACA_BIN_DIR` sets the link folder; else `/usr/local/bin` when writable, else `~/.local/bin` (add it to `PATH` if needed). | Through `all.sh --install` |
| `build-mobile.sh` | Builds the companion APK and copies it to `Resources/jaca-mobile.apk`, with the commit hash in `Resources/jaca-mobile.apk.version`. See [`mobile/README.md`](../mobile/README.md). | `ANDROID_HOME` (default `~/Library/Android/sdk`), `JAVA_HOME` (default from `java_home`; use JDK 17) | Through `all.sh --install` (a failure there does not stop the install) |
| `proto-gen.sh` | Regenerates the Swift gRPC stubs in `Sources/Core/Companion/Generated/` from `proto/companion.proto`. See [`proto/README.md`](../proto/README.md). | `GRPC_SWIFT_PLUGIN` (default `/tmp/grpc-swift-1x/.build/release/protoc-gen-grpc-swift`) | No |
| `dev-signing.sh` | Optional, one time. Creates a self-signed "Jaca Dev" code-signing identity in `~/Library/Keychains/jaca-dev.keychain-db` and trusts it (one macOS prompt). | None. To undo: `security delete-keychain "$HOME/Library/Keychains/jaca-dev.keychain-db"` | No |
| `dev-resign.sh [CONFIG]` | Re-signs the built app with "Jaca Dev" if that identity exists. Otherwise it does nothing. | Configuration, default `Debug` | Through `build.sh` |
| `companion-diag.sh [serial]` | Uses adb to check the tun interface, the companion process, DNS and HTTPS on a device. Run it once with capture on and once with capture off, then compare. | Device serial | No |

The dedicated keychain that `dev-signing.sh` creates has a fixed password (`jaca-dev`, in the script). Any local process can unlock it and sign code as "Jaca Dev". Delete the keychain if you do not need a stable signature.

The Android agent has its own scripts in `agent/` (`build.sh`, `build-dex.sh`, `deploy.sh`). See [`agent/README.md`](../agent/README.md).

## In-app updater

The updater (`Sources/Core/Update/UpdateService.swift`) works only when `~/Library/Application Support/Jaca/build-info.json` exists. An Xcode post-build phase writes that file with the checkout path and the commit. The updater compares the commit with `origin/main` of that checkout. To update, it stashes local changes, checks out `main`, runs `git pull --ff-only origin main`, and runs `all.sh --install --no-run` in a login shell. Then it restores the branch and the stash. Keep `all.sh`, its `--install`, `--no-run` and `--no-agent` flags, and the `agent/out` path stable: the updater depends on them.
