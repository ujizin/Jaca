# In-process network agents

Jaca captures HTTP traffic from inside a debug app. It needs no CA and no proxy, and it works with certificate pinning. This folder holds two agents:

| Agent | Target | Source | Output |
|---|---|---|---|
| Android agent | A debuggable Android app (arm64-v8a) | `native/`, `java/`, `kotlin/`, `okhttp/`, `okhttp-stubs/` | `out/arm64-v8a/libsqueezeagent.so`, `out/squeezeagent-boot.dex`, `out/squeezeagent-capture.dex` |
| iOS Simulator agent | An app on the iOS Simulator (arm64) | `iOS/` | `JacaNetAgent.dylib` in `Jaca.app/Contents/Resources` |

"Squeeze" is an old internal name. It stays in the Android package names, the file names and the socket name prefix (`squeeze_`), because the app loads these names at runtime and on the device.

## Android agent

### How it attaches

1. Jaca runs `cmd activity attach-agent <pid> '<so>=<boot dex>,<capture dex>,<socket>'` on a debuggable app.
2. ART loads `libsqueezeagent.so` and calls `Agent_OnAttach`.
3. The native agent adds the boot dex to the bootstrap class loader search and calls `com.squeeze.agent.SqueezeAgent.attach`.
4. The agent listens on an abstract socket with the name from the attach spec. Jaca picks a new name for each attach (`squeeze_<random>`; the agent falls back to `squeeze_agent`) and reaches it with `adb forward tcp:0 localabstract:<socket>`.
5. The agent sends one JSON line for each completed request.

The desktop side is `Sources/Core/Network/AgentController.swift`.

### Parts

| Folder | Language | What it does |
|---|---|---|
| `native/` | C++17 | The JVMTI agent (`squeeze_agent.cc`) and the bytecode transform (`squeeze_transform.cc`). The transform adds exit hooks to methods with the vendored AOSP slicer. |
| `native/third_party/slicer/` | C++ | AOSP dex IR and instrumentation library (Apache-2.0). |
| `native/include/jvmti.h` | C | The JVMTI header (GPLv2 with the Classpath exception). |
| `java/` | Java 11 | The trampoline on the bootstrap class loader (`com.squeeze.agent`). Pure Java, so it never shadows the app's Kotlin stdlib. |
| `kotlin/` | Kotlin | The capture logic (`com.squeeze.capture`): the OkHttp hook, tracked `HttpURLConnection`/`HttpsURLConnection`, tee streams, the reporter socket, and `AgentHttp.kt` (response-override routing). It loads on its own class loader with a bundled Kotlin stdlib. |
| `okhttp/` | Java | `SqueezeOkHttp3Interceptor`, bundled into the capture dex. |
| `okhttp-stubs/` | Java | Compile-only OkHttp3 stubs. They are never bundled: the app provides the real OkHttp at runtime. |

### Build

`scripts/build.sh` builds the agent when an output is missing or older than its sources. It then copies the outputs into `Resources/`, so the app loads the agent from its own bundle. To build by hand:

```bash
cd agent
./build.sh        # native .so  -> out/arm64-v8a/libsqueezeagent.so
./build-dex.sh    # two dexes   -> out/squeezeagent-boot.dex, out/squeezeagent-capture.dex
```

| Script | Inputs |
|---|---|
| `build.sh` | `ABIS` (default `arm64-v8a`), `API` (default `28`), `ANDROID_HOME` (default `~/Library/Android/sdk`), `ANDROID_NDK` (default `$ANDROID_HOME/ndk/27.2.12479018`) |
| `build-dex.sh` | `ANDROID_HOME`. It uses `platforms/android-36/android.jar`, the newest `build-tools/*/d8`, `kotlinc` on `PATH` and the Kotlin stdlib at `/opt/homebrew/opt/kotlin`. |
| `deploy.sh [package] [serial]` | Builds, pushes and attaches the agent, then prints the agent log. Defaults: `com.example.app.dev` on `emulator-5554`. Use it to iterate on the agent without the app. |

If the toolchain is missing, `scripts/build.sh` fails. To build the app without the Android agent, run `./scripts/all.sh --no-agent` or `JACA_SKIP_AGENT=1 ./scripts/build.sh`.

If `JACA_SKIP_AGENT=1` is set and `out/` holds the `.so` but not the dexes, the copy step fails. The in-app updater sets `--no-agent` when `agent/out` is not empty, so it can hit this case. Run `rm -rf agent/out` and build again.

### Toolchain

| Tool | Version |
|---|---|
| Android NDK | 27.2.12479018 |
| CMake (with its ninja) | 3.22.1, under `$ANDROID_HOME/cmake/3.22.1` |
| Android platform | `android-36` |
| Android build-tools | any; the newest `d8` is used (the hint in `scripts/build.sh` names 34.0.0) |
| Kotlin | `brew install kotlin` |
| JDK | one whose `javac` supports `--release 11` |

```bash
sdkmanager "ndk;27.2.12479018" "cmake;3.22.1" "platforms;android-36" "build-tools;34.0.0"
brew install kotlin
```

The `.so` links with `-Wl,-z,max-page-size=16384` (`native/CMakeLists.txt`). Do not remove it: without it the agent does not load on 16 KB-page devices and emulators.

## iOS Simulator agent

`iOS/` holds the Objective-C sources of `JacaNetAgent.dylib`:

| File | Role |
|---|---|
| `JacaNetAgent.m` | The tap (an `NSURLProtocol`) and the route client |
| `JacaNetChannel.{h,m}` | The transport back to Jaca |
| `JacaAgentHTTP.{h,m}` | The routing state for response overrides, the twin of `AgentHttp.kt` |

- **Build.** A post-build phase of the `Jaca` target in `project.yml` compiles every `iOS/*.m` with `xcrun -sdk iphonesimulator clang` for `arm64-apple-ios15.0-simulator`. The build fails if the iOS Simulator platform is not installed in Xcode.
- **Injection.** Jaca relaunches the app with `SIMCTL_CHILD_DYLD_INSERT_LIBRARIES` and `SIMCTL_CHILD_JACA_NET_PORT`. The agent connects back to `127.0.0.1` on that port.
- **Tests.** `Tests/ObjC` is the `JacaAgentTests` target. It tests `JacaAgentHTTP.m` on the Mac.

## Related docs

- [`docs/agent-https-debugging-android.md`](../docs/agent-https-debugging-android.md): response overrides on Android, the OkHttp layer rule, teardown.
- [`docs/agent-https-debugging-ios.md`](../docs/agent-https-debugging-ios.md): the iOS Simulator transport.
- [`docs/agent-http-contract.md`](../docs/agent-http-contract.md): the route frame that the desktop and both agents share.
- [`docs/architecture.md`](../docs/architecture.md): where the agents sit in Jaca.
