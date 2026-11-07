# Documentation

Start with the [project README](../README.md). To build and send a change, read [`CONTRIBUTING.md`](../CONTRIBUTING.md). To report a vulnerability, read [`SECURITY.md`](../SECURITY.md).

## Architecture

| Doc | What it covers |
|---|---|
| [`ARCHITECTURE.md`](../ARCHITECTURE.md) | The components, how the processes talk, the `Sources/` tree, the layering rules, the daemon areas and the data on disk. |
| [`scripts/README.md`](../scripts/README.md) | Every build, run, install and diagnostic script, with its flags. |
| [`CLAUDE.md`](../CLAUDE.md) | The full code conventions: the area pattern, cache-first areas, migration-safe `Codable`, shared state. |

## Network capture

| Doc | What it covers |
|---|---|
| [`network-capture.md`](network-capture.md) | The capture modes, how each one works, and their limits. |
| [`agent/README.md`](../agent/README.md) | The Android and iOS Simulator in-process agents: parts, build, toolchain. |
| [`agent-https-debugging-android.md`](agent-https-debugging-android.md) | Response overrides on Android through the agent, the OkHttp layer rule, teardown. |
| [`agent-https-debugging-ios.md`](agent-https-debugging-ios.md) | Response overrides on the iOS Simulator: the same rules over another transport. |
| [`agent-http-contract.md`](agent-http-contract.md) | The route frame and the bounce rules that the desktop and both agents share. |

## Daemon, CLI and Herdr plugin

| Doc | What it covers |
|---|---|
| [`daemon-plan.md`](daemon-plan.md) | `jacad`: status, protocol, per-area design, and the `jaca` command line. |
| [`herdr-plugin/README.md`](../herdr-plugin/README.md) | The Herdr plugin: panes, actions, how it finds `jacad`. |
| [`herdr-plugin/INSTALL.md`](../herdr-plugin/INSTALL.md) | Step-by-step install of the Herdr plugin. |

## Companion app

| Doc | What it covers |
|---|---|
| [`mobile/README.md`](../mobile/README.md) | The Android companion app: what it does, modules, build. |
| [`companion-architecture.md`](companion-architecture.md) | The Mac-to-phone flow from the QR scan to decrypted traffic, with sequence and state diagrams. |
| [`proto/README.md`](../proto/README.md) | The gRPC contract, which side is server and client, and how to regenerate the stubs. |
| [`on-device-store-and-lazy-fetch.md`](on-device-store-and-lazy-fetch.md) | A deferred proposal: keep traffic on the phone and fetch details on demand. Not built. |
