# Jaca companion app

An Android app (`dev.srsouza.jaca`) that captures the traffic of the whole device and streams it to Jaca on the Mac. Use it when the app under test is not debuggable, so the in-process agent cannot attach.

- **Capture.** A `VpnService` and a userspace TCP/IP stack (zdtun, through JNI) see every connection. The app tags each flow with the app that opened it.
- **Link.** The app runs a gRPC server over TLS (port 8889, or a free port if 8889 is in use) and advertises it over mDNS as `_jaca._tcp`. Jaca on the Mac is the gRPC client.
- **Decryption.** The phone sends intercepted TLS to the Mac's MITM proxy. TLS is decrypted on the Mac, so the CA private key never reaches the phone.
- **CA install.** The Mac pushes its CA (`InstallCa`), and the app guides the user to install it.

The Mac side shows "Connect a device" only when Settings → Network inspection is "HTTPS debugging". The full flow, with sequence and state diagrams, is in [`docs/companion-architecture.md`](../docs/companion-architecture.md).

## Modules

| Module | What it is |
|---|---|
| `composeApp` | The Compose Multiplatform app. `androidMain` has the `VpnService`, the gRPC server, the TLS tunnel and the zdtun JNI code (`androidMain/jni/`). `commonMain` has the shared UI. `iosMain` is a placeholder: `IosCaptureEngine.isSupported` is `false`. |
| `companion-grpc` | A plain JVM library with the Java (lite) protobuf and gRPC stubs, generated from [`../proto/companion.proto`](../proto/README.md). |

## Build

```bash
./scripts/build-mobile.sh
```

The script runs `./gradlew :composeApp:assembleDebug` with the short commit hash, then copies the APK to `Resources/jaca-mobile.apk` and the hash to `Resources/jaca-mobile.apk.version`. Jaca bundles that APK, serves it for QR onboarding, and compares the hash to find a phone that runs an older build.

- `./scripts/all.sh --install` runs this script for you. A plain `./scripts/build.sh` does not, so the app has no APK unless one is already in `Resources/`.
- The script sets `ANDROID_HOME` to `~/Library/Android/sdk` and `JAVA_HOME` from `/usr/libexec/java_home` when they are not set.

### Requirements

| Tool | Version |
|---|---|
| JDK | 17. Point `JAVA_HOME` at a JDK 17. A newer JDK can fail in the Gradle build (seen with JDK 26). |
| Android SDK | compileSdk and targetSdk 36, minSdk 29 |
| Android NDK | 27.2.12479018 |
| CMake | 3.22.1 |
| Gradle | 9.3.1 (wrapper, no install needed) |

Versions of AGP, Kotlin, Compose Multiplatform and gRPC are in `gradle/libs.versions.toml`. The UI uses the Lemonade design system for Android (`com.teya.foundation:lemonade-ui`), which Gradle downloads from Maven Central. The APK is built for `arm64-v8a` only.

## License note

zdtun (`composeApp/src/androidMain/jni/zdtun/`) carries its own license in `COPYING` (LGPL-3.0 or later).
