# Network capture

How Jaca captures HTTP(S) traffic: the capture sources, how each one works, how to build the parts, and what each source cannot do.

Related docs:

- [agent-https-debugging-android.md](agent-https-debugging-android.md): response overrides through the Android agent.
- [agent-https-debugging-ios.md](agent-https-debugging-ios.md): the iOS Simulator agent and its overrides.
- [agent-http-contract.md](agent-http-contract.md): the frames that the desktop and the two agents agree on.
- [companion-architecture.md](companion-architecture.md): the companion app, end to end.

## Capture sources

Open a network tab from the device menu ("Inspect Network"). The tab offers the sources that apply to the device (`CaptureSourceRegistry.options(for:)`). Settings → Network inspection controls which sources exist.

| Source (chooser title) | Targets | Network inspection mode |
|---|---|---|
| "In-process agent" | A debuggable Android app, or any iOS Simulator app | Both modes |
| "Chrome browser" | This Mac | Both modes |
| "Companion stream" | An Android phone that runs the Jaca companion app | "HTTPS debugging" only |

The two modes in Settings → Network inspection:

- **"Agent HTTPS debugging"** (the default): the in-process agent and the Chrome browser. Response overrides work in this mode.
- **"HTTPS debugging"** (experimental, off by default): adds the companion stream and the "Connect a device (QR / USB)" button (a plus icon) in the sidebar.

Physical iOS devices have no capture source.

Earlier versions had a standalone MITM proxy mode, where Jaca set the device's proxy and you installed the CA by hand. That mode was removed. `ProxyServer` now terminates TLS only for the companion stream and for the Chrome browser.

### The shared network tab

All sources feed the same tab:

- Method, URL, status and timing for each request.
- Request and response headers, with one-click copy.
- Bodies with a JSON tree and raw toggle.
- A request timeline. Drag on it to filter the list.
- "Copy as cURL" and "Export HAR" (every source).
- "Override response…" in the request's context menu, where the source supports overrides.

## The in-process Android agent

For a **debuggable** Android app, Jaca does not proxy at all. It runs code inside the app and instruments its HTTP stack. It is built from open-source pieces:

1. A small **native JVMTI agent** (`agent/native`, built with the NDK) attaches to the running app over adb with `cmd activity attach-agent`. This works on debuggable apps **without root**.
2. It uses the vendored [AOSP **slicer**](https://android.googlesource.com/platform/tools/dexter) (`agent/native/third_party/slicer`) to rewrite `java.net.URL.openConnection()` at load time, so the method calls Jaca's hook on return (a JVMTI `ClassFileLoadHook` + `RetransformClasses`).
3. The hook returns a transparent **wrapper** around the returned connection. It wraps `HttpsURLConnection` and plain (cleartext) `HttpURLConnection`. The wrapper tees the request and response streams and records method, URL, headers, bodies, timing and the call stack that made the request.
4. For OkHttp, the agent also hooks `OkHttpClient.networkInterceptors()` (an app class, found with `GetLoadedClasses`/`RetransformClasses`) and **injects a capture interceptor**. Both **okhttp3** and **okhttp2** (`com.squareup.okhttp`) are hooked. The declaring class in the exit-hook signature routes each call to the right handler.
   - **okhttp3:** the interceptor is a real class, `SqueezeOkHttp3Interceptor` (`agent/okhttp`). It is compiled against compile-only OkHttp stubs (`agent/okhttp-stubs`) and loaded on a child of the app's class loader, so it binds to the app's own `okhttp3.Interceptor`.
   - **okhttp2:** the interceptor is a reflective `java.lang.reflect.Proxy` of the `Interceptor` interface.
   - Request bodies are captured by writing the `RequestBody` into a fresh okio `Buffer`.
5. Each transaction streams as one JSON line over an adb-forwarded `localabstract` socket back to Jaca.

### Transparency to the app

The interceptor must not change what the app sees:

- It never consumes a one-shot or duplex request body.
- On a cancelled or failed call it rethrows the *real* exception, so a `StreamResetException` cannot crash the app.
- The okhttp3 interceptor is a real class and not a `Proxy`, because a `Proxy` rewraps a checked exception as `UndeclaredThrowableException` when the proxied method has no `throws` clause. R8 strips okhttp's `throws` clauses. A cancelled call's `IOException` then escaped okhttp's `catch (IOException)` on the async dispatcher and crashed the app.

Response bodies are captured with a **lazy tee**, never an eager `peekBody`:

- The response body's `okio.Source` is wrapped in a reflective proxy. The proxy copies bytes (up to 1 MB) *as the app reads them* and reports the transaction at EOF or close.
- An eager read-ahead on a live HTTP/2 stream can block, time out and reset the stream with `CANCEL`. The old eager peek did this for any response without a `Content-Length` (common with Ktor/OkHttp over HTTP/2), and the reset then crashed the app's own read. The tee never advances the stream past the app, so this cannot happen.
- The tee unwraps reflection's `InvocationTargetException`, so a real read error reaches the app as its original `IOException` and not as an `UndeclaredThrowableException`.
- Streaming content types (`text/event-stream`, `application/grpc` and any `x-mixed-replace` type) are reported at once with no body, because a tee on a stream that never ends would hold the transaction until the stream closes.
- okhttp2 responses are reported with headers only, because okhttp2 has no compatible `ResponseBody.create(…, BufferedSource)`.

### The classloader split

The hook trampoline has to live on the **bootstrap** classloader, so that an instrumented boot class like `URL` can resolve it. Kotlin's stdlib cannot go there, because it would shadow the host app's stdlib. So the agent is split in two layers:

- `agent/java`: a small **Java** trampoline and bridge interface on the bootstrap loader. No Kotlin, so nothing to shadow.
- `agent/kotlin`: the capture logic in **Kotlin**, loaded on a separate `DexClassLoader` with its own bundled stdlib, isolated from the app's.

### The "Squeeze" names

"Squeeze" is an earlier name of the project. The Android agent still uses it: `libsqueezeagent.so`, `squeezeagent-boot.dex`, `squeezeagent-capture.dex`, the `com.squeeze.agent` and `com.squeeze.capture` packages, and `squeeze_*` socket names. The app loads these names at runtime, so they stay as they are.

### Response overrides through the agent

With an enabled override rule, the agent sends only the hosts that the rule names through the Mac, over an `adb reverse` tunnel to a loopback HTTP server in Jaca. All other traffic stays on the device's own network. Jaca removes the tunnel when capture stops. Full design and traps: [agent-https-debugging-android.md](agent-https-debugging-android.md).

### Build the Android agent

`scripts/build.sh` (and so `scripts/all.sh`) builds the agent when `agent/out` is missing or older than its sources. It then copies the `.so` and the two dexes into `Resources/`, and the app loads them only from its own bundle. Without the Android toolchain the build fails. Pass `--no-agent` to `all.sh`, or set `JACA_SKIP_AGENT=1`, to build the app without the agent.

Toolchain: NDK 27.2.12479018, CMake 3.22.1, platform `android-36`, build-tools (for `d8`), Kotlin (`brew install kotlin`; `build-dex.sh` reads it from `/opt/homebrew/opt/kotlin`) and a JDK with `javac`.

To work on the agent itself:

```bash
cd agent
./build.sh                   # native JVMTI .so, via the NDK and the vendored slicer
./build-dex.sh               # boot dex (javac) + capture dex (kotlinc + d8)
./deploy.sh [pkg] [serial]   # build, attach to a debuggable app, tail the log
```

- `build.sh` builds `arm64-v8a` only by default. Set `ABIS` (for example `ABIS="arm64-v8a x86_64"`) to build others, and `API` (default 28) for the API level. The app ships `arm64-v8a` only.
- `deploy.sh` defaults to `com.example.app.dev` on `emulator-5554`. Override them with the two arguments. It does not read `ANDROID_SERIAL`.
- `scripts/build.sh` copies the agent into `Resources/` when `agent/out` has the `.so`. If you build with `--no-agent` and `agent/out` has the `.so` but not the two dexes, the copy fails and the build stops. Run `rm -rf agent/out` and build again.

More: [agent/README.md](../agent/README.md).

## The in-process iOS Simulator agent

For an iOS Simulator app, Jaca relaunches the app with `JacaNetAgent.dylib` injected (`SIMCTL_CHILD_DYLD_INSERT_LIBRARIES`). The dylib registers an `NSURLProtocol` that taps the URL-loading system. It connects back to Jaca on `127.0.0.1` (the simulator shares the Mac's loopback) and reports each request as one JSON line, in the same schema as the Android agent.

- The Xcode post-build phase compiles `agent/iOS/*.m` with the iOS Simulator SDK into `Jaca.app/Contents/Resources`. The build fails when the iOS Simulator platform is not installed.
- Settings has a toggle to re-attach the agent when you relaunch the app yourself.
- Response overrides work as on Android, with no tunnel.

Details: [agent-https-debugging-ios.md](agent-https-debugging-ios.md).

## Chrome on This Mac

For "This Mac", Jaca starts a `ProxyServer` on `127.0.0.1` and opens a separate Chrome window with its own profile (`~/Library/Application Support/Jaca/chrome-profile`). The window uses the proxy and trusts Jaca's CA through a launch flag (`--ignore-certificate-errors-spki-list`), so you do not install the CA on the Mac. QUIC is off in that window, so every request goes through the proxy. Response overrides work here too.

## The proxy

`Sources/Core/Network/ProxyServer.swift` is a swift-nio MITM proxy:

- For `CONNECT`, it terminates TLS with a per-host leaf certificate that it generates on the fly (swift-certificates) and signs with Jaca's root CA.
- It reads the plaintext HTTP and runs it through an `InterceptPipeline`: the override rules first, then the origin. The origin request goes out through `URLSession` (`UpstreamClient`).
- A tunnel that does not start with TLS (for example a `ws://` WebSocket through `CONNECT`) is relayed to its target unread.
- It binds every interface by default, for a phone on the LAN. The Chrome capture binds loopback only.

The root CA is `~/Library/Application Support/Jaca/ca/rootCA.pem`, and its private key is `ca/rootCA.key.pem` (mode 0600) in the same folder. Older builds kept the key in the macOS Keychain. Jaca reads the Keychain once to move the key into the file.

Jaca no longer changes a device's proxy settings. The companion app gets its proxy address from the desktop over gRPC (`SetProxy`).

## The companion app

The companion is a Compose Multiplatform app (`mobile/`) for Android:

- It captures *all* device traffic with a `VpnService` and a userspace TCP/IP stack (zdtun, through JNI).
- It attributes each flow to the app that owns it and streams it to Jaca over gRPC/TLS on the LAN.
- TLS is decrypted on the **Mac**. The CA private key never leaves the Mac: the phone tunnels intercepted TLS to the desktop proxy.
- It works over Wi-Fi without adb, also on emulators.
- The desktop pushes its CA over the link, and the app guides you through installing it. You do not download the CA by hand.

To use it:

1. In Settings → Network inspection, choose "HTTPS debugging".
2. Click the plus button in the sidebar ("Connect a device (QR / USB)") and scan the QR code with the phone, or run `adb install Resources/jaca-mobile.apk`.
3. Open the companion app and start capture. The phone shows up in Jaca's device list.

Build and bundle the APK:

```bash
./scripts/build-mobile.sh    # assembles the APK and copies it into Resources/
./scripts/proto-gen.sh       # regenerates the gRPC stubs after you edit proto/companion.proto
```

The APK build needs a JDK 17, NDK 27.2.12479018 and CMake 3.22.1. `./scripts/all.sh --install` runs `build-mobile.sh` for you. A plain build has no APK unless one is already in `Resources/`.

Full flow, channels and state diagrams: [companion-architecture.md](companion-architecture.md). Build details: [mobile/README.md](../mobile/README.md). The contract: [proto/README.md](../proto/README.md).

## Limitations

- **Android agent:**
  - Works only with debuggable apps (a debug build). A release build that you re-sign to attach trips signature and attestation checks, so it is out of scope.
  - Captures `HttpURLConnection`, `HttpsURLConnection`, OkHttp3 and OkHttp2 (including Ktor over OkHttp). Other stacks are not captured.
  - Captured bodies stop at 1 MB each. Streaming responses and okhttp2 responses show without a body.
  - Ships for `arm64-v8a` only.
  - It is the only source that records the request call stack.
- **iOS Simulator agent:** sees `URLSession` traffic only. It does not see `WKWebView`, background sessions or raw sockets. No call stack.
- **Companion stream:** experimental. It installs a CA on the device. Apps that pin certificates, or that do not trust user CAs, are not decrypted and can stop working. QUIC (HTTP/3) is not decrypted. No call stack, because the capture is outside the app process.
- **Physical iOS devices:** no network capture.
