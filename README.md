<p align="center">
  <img src="art/logo.png" alt="Jaca logo" width="112">
</p>

<h1 align="center">Jaca</h1>

<p align="center">
  <b>Logs, network traffic and mocked responses for mobile and web apps.</b>
</p>

<p align="center">
  Android, iOS Simulator, iPhone and Chrome, in one macOS window.
</p>

<p align="center">
  <img src="https://img.shields.io/badge/macOS-14%2B-9BD981?style=flat&labelColor=F3B22A" alt="macOS 14+">
  <img src="https://img.shields.io/badge/Swift-5.9-9BD981?style=flat&labelColor=F3B22A" alt="Swift 5.9">
</p>

<p align="center">
  <img src="docs/images/hero-network-tab.png" alt="Jaca in dark mode: the sidebar with areas and devices, and a network tab with a failed GET /v1/cart request selected and its overview open." width="100%">
</p>

<table>
  <tr>
    <td width="33%" valign="top">
      <b>Logs</b><br>
      Device and Cloud Logging logs, filtered by level, text, regex or app. Crashes are marked.
    </td>
    <td width="33%" valign="top">
      <b>Network</b><br>
      Every request from an app or Chrome, with headers, bodies and timing. Mock any response.
    </td>
    <td width="33%" valign="top">
      <b>Housekeeping</b><br>
      Clean Gradle, Xcode and worktree caches.
    </td>
  </tr>
</table>

## Take the tour

### Logs

Stream device logs and Google Cloud Logging, with filters and crash markers.

<p align="center">
  <img src="docs/images/logs-crash-marker.png" alt="A log tab with colour-coded lines, a crash stack trace with a red marker line and a 1 crash counter." width="100%">
</p>

### Network

See every request from Android, iOS Simulator and Chrome, and mock any response.

<p align="center">
  <img src="docs/images/network-response.png" alt="A network tab with a successful GET /v1/catalog request selected and its JSON response body open as a tree." width="100%">
</p>

### Clean your Mac

Free the space that Gradle, Xcode DerivedData and old worktrees take.

<p align="center">
  <img src="docs/images/projects-area.png" alt="The Projects area with six projects and their sizes, one expanded to show its main checkout and two git worktrees." width="100%">
</p>

### Terminal

Read logs, find failed requests and mock responses with `jaca`, or in Herdr panes.

```bash
jaca net requests --failed
jaca overrides add --from REQUEST --status 503
```

How to use each part: [docs/usage.md](docs/usage.md).

## Get started

You need macOS 14+, Xcode 26+ with the iOS Simulator platform, [XcodeGen](https://github.com/yonaskolb/XcodeGen), and the Android NDK and Kotlin ([details](CONTRIBUTING.md#optional-per-component)).

```bash
git clone https://github.com/teya-engineering/lemonade-design-system.git
git clone https://github.com/ujizin/Jaca.git
cd Jaca
./scripts/all.sh
```

For Android devices, put `adb` on your `PATH` or set it in Settings.

## Install

```bash
./scripts/all.sh --install
```

This replaces `/Applications/Jaca.app` and links `jaca`. There are no prebuilt releases.

> [!NOTE]
> **Update Jaca** pulls `main` into the checkout you last built from and rebuilds. It stashes and restores your changes. [Details](ARCHITECTURE.md#the-in-app-updater).

## Limitations

- Android capture needs a debuggable app.
- iOS Simulator capture sees `URLSession` only.
- iPhones have logs only.
- The Android companion app is experimental.
- iPhone logs use a private Apple framework, so an update can break them.

## Learn more

- [ARCHITECTURE.md](ARCHITECTURE.md): how the parts fit, the repository layout, data on disk, third-party licenses.
- [docs/usage.md](docs/usage.md): how to use each part, and what each device supports.
- [docs/network-capture.md](docs/network-capture.md): how each capture source works.
- [CONTRIBUTING.md](CONTRIBUTING.md): setup, tests, code rules.
- [SECURITY.md](SECURITY.md): the proxy, the CA and the agents, and how to report a vulnerability.
- [docs/README.md](docs/README.md): every doc.
