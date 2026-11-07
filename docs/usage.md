# Using Jaca

How to use each part of Jaca. To build and install it, read the [README](../README.md#get-started).

- [Logs](#logs)
- [Cloud Logging](#cloud-logging)
- [Network traffic](#network-traffic)
- [Mock a response](#mock-a-response)
- [Web apps in Chrome](#web-apps-in-chrome)
- [What each device supports](#what-each-device-supports)
- [App databases](#app-databases)
- [Clean your Mac](#clean-your-mac)
- [The terminal](#the-terminal)

## Logs

1. Click a device in the sidebar and choose **Start Logcat**.
2. Filter the lines:
   - by level, with the chips **V** to **F**;
   - by text, in the search field, or by regex with **.\***;
   - by app, in the app field.

When the app crashes, Jaca adds a red marker after the stack trace. The **1 crash** counter jumps between crashes.

**Prettify JSON** formats the JSON bodies in the log. **Copy format** sets the text that ⌘C copies for a line.

Every session is saved. Open **History** in the title bar to search old sessions.

## Cloud Logging

1. Click the plus button beside **Cloud Logging** in the sidebar and choose **Add GCP project…**. Jaca reads the logs through `gcloud`.
2. Pick a log name and a time range.
3. Filter the rows:
   - by severity;
   - by label: select a row, right-click a label and choose **Filter by this value**;
   - with **SQL**, which runs again as new logs arrive.

## Network traffic

1. Click a device and choose **Inspect Network**.
2. Pick the app.
3. Select a request to see its **Overview**, **Headers**, **Request**, **Response** and **Timing**.

**Copy cURL** copies the request as a command. The download button exports the capture as HAR.

## Mock a response

1. Right-click a request and choose **Override response…**.
2. Set the **Status**, the **Body** or the **Headers**.

The **Overrides** menu in the tab toolbar lists your rules. From a terminal, `jaca overrides` manages them.

## Web apps in Chrome

Click **This Mac** in the sidebar. Jaca opens a separate Chrome window and captures its traffic, `localhost` included. You can mock its responses as for a device.

## What each device supports

| Device | Logs | Network | Mock responses | Databases |
|---|---|---|---|---|
| Android | Yes | Debuggable apps | Debuggable apps | Debuggable apps |
| iOS Simulator | Yes | `URLSession` traffic | Yes | Yes |
| iPhone | Yes | No | No | No |
| This Mac | No | Chrome | Chrome | No |

The Android companion app captures whole-device traffic. It is experimental.

How each capture source works: [network-capture.md](network-capture.md).

## App databases

Click a device and choose **Browse Database**. Jaca opens the app's SQLite databases from a read-only snapshot.

## Clean your Mac

- **Projects**: your Claude Code projects and the folders you add, with their git worktrees and disk sizes. Clean build caches or delete a worktree.
- **Gradle**: stop running daemons and delete caches.
- **Xcode**: delete DerivedData folders. Stale ones are marked.

## The terminal

The `jaca` command line reads the sessions that the Jaca daemon runs:

```bash
jaca devices
jaca logs tail --grep timeout --follow
jaca net requests --failed
jaca overrides add --from REQUEST --status 503
```

Run `jaca --help` for every command. To see which sessions the daemon runs, read [ARCHITECTURE.md](../ARCHITECTURE.md#daemon-areas).

The [Herdr plugin](../herdr-plugin/README.md) shows the same areas as panes in the Herdr terminal workspace.
