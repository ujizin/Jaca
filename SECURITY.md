# Security

Jaca is a developer tool that reads device logs and intercepts network traffic. It runs with more
access than a normal Mac app. This page tells you what Jaca does that affects security, and how to
report a problem.

## Report a vulnerability

Use GitHub private vulnerability reporting:

1. Go to the [Security tab of ujizin/Jaca](https://github.com/ujizin/Jaca/security).
2. Click **Report a vulnerability**.
3. Give the Jaca commit, your macOS version and the steps to reproduce.

Do not open a public issue for a vulnerability. There are no released versions, so a fix goes to
`main`.

## Security model

### The MITM certificate authority

Jaca creates its own root certificate, "Jaca Proxy CA", the first time it needs one. Each Mac has
its own CA.

| File | Contents |
|---|---|
| `~/Library/Application Support/Jaca/ca/rootCA.pem` | The CA certificate |
| `~/Library/Application Support/Jaca/ca/rootCA.key.pem` | The CA private key, file mode `0600` |

> **Warning:** Keep the private key on your Mac. A person with the key can read and change the
> HTTPS traffic of each device that trusts the CA.

Older builds kept the key in the macOS Keychain (service `dev.srsouza.Jaca`, account
`ca-private-key`). Jaca reads that item one time, moves the key to the file, and then uses only
the file.

Where the CA is trusted:

- **Android device, no root:** you install the CA in the user certificate store yourself. Jaca
  copies the certificate to `/storage/emulated/0/Download/JacaProxyCA.crt` and opens the security
  settings.
- **Rooted Android device or emulator:** Jaca mounts a `tmpfs` over the system CA store and adds
  the CA there. A reboot removes it.
- **Companion app:** the desktop sends the CA to the phone over gRPC (`InstallCa`), and the app
  guides you to install it in the user store.
- **This Mac (Chrome):** Jaca starts Chrome with its own profile and
  `--ignore-certificate-errors-spki-list` for the CA key. Jaca does not add the CA to the macOS
  trust store.

### Remove the CA

On each Android device that trusts the CA:

1. Open **Settings** and search for "Trusted credentials" (the path differs between devices).
2. Open the **User** tab and remove "Jaca Proxy CA".
3. Delete `JacaProxyCA.crt` (or `JacaProxyCA.pem`) from the **Download** folder.

On a rooted device or emulator, restart it.

On the Mac, delete the CA files to make Jaca create a new CA the next time it needs one:

```bash
rm -rf "$HOME/Library/Application Support/Jaca/ca"
```

After this, devices that trust the old CA no longer match. Remove the old CA from them too. If an
older build left the key in the Keychain, delete that item:

```bash
security delete-generic-password -s dev.srsouza.Jaca -a ca-private-key
```

### Network listeners

| Listener | Bind address | When it runs |
|---|---|---|
| MITM proxy for the companion app | `0.0.0.0` (all interfaces), random port | While a companion capture runs. A phone on the LAN must reach it. |
| MITM proxy for This Mac Chrome | `127.0.0.1`, random port | While a This Mac capture runs |
| Agent HTTP server (response overrides) | `127.0.0.1`, random port | While overrides route a host through the Mac |
| APK download server | All interfaces, port 8890 (or a random port), plain HTTP | While the "Connect a device" sheet is open |

Other hosts on your network can connect to the companion proxy and to the APK server while they
run. Use companion capture only on a network that you trust.

The desktop connects to the companion app over gRPC with TLS. The phone presents a self-signed
certificate, and the desktop does not verify it. The TLS gives the link confidentiality, not
authentication of the phone.

### The daemon socket

`jacad` listens on a Unix socket, `~/.jaca/jacad.sock` (mode `0600`), in `~/.jaca` (mode `0700`).
The daemon refuses to start if another user owns that folder. The socket has no other
authentication: each process that runs as your user can call the daemon. That includes reading
captured traffic and logs, and changing response overrides.

`jaca net show` hides the values of `Authorization`, `Proxy-Authorization`, `Cookie` and
`Set-Cookie` headers. `--raw` shows them.

### App signing and entitlements

- The app is not sandboxed, because it starts `adb`, `xcrun`, `git` and other tools as processes.
- Hardened runtime is off. Builds are ad-hoc signed (`CODE_SIGN_IDENTITY: "-"`).
- `NSAllowsArbitraryLoads` is on for the app and for `jacad` (`Config/ProxyTransport-Info.plist`),
  because the proxy forwards plain `http://` requests.

### Private Apple frameworks and code injection

- For physical iOS device logs, Jaca loads the private frameworks `LoggingSupport` and
  `MobileDevice` with `dlopen`.
- For iOS Simulator network capture, Jaca relaunches the app with
  `DYLD_INSERT_LIBRARIES` set to `JacaNetAgent.dylib`. The agent runs inside the app and connects
  to `127.0.0.1`.
- For Android network capture, Jaca attaches a JVMTI agent to a debuggable app with
  `cmd activity attach-agent`. The agent runs inside the app.

### Claude Code with bypassed permissions

The **Open in Herdr** action in the Projects area and in the Herdr plugin runs this command in the
selected checkout by default:

```bash
claude --permission-mode bypassPermissions
```

In that mode, Claude Code runs tools without a prompt for each one. To change the command, click
the gear in the Projects header and edit "Claude command". The Herdr plugin has the same setting.
Both store it in the `jaca.herdr.claudeCommand` default.

### The dev-signing keychain

`scripts/dev-signing.sh` is optional. It creates `~/Library/Keychains/jaca-dev.keychain-db` with a
self-signed "Jaca Dev" code-signing identity that your user trusts. The keychain password is
`jaca-dev`, and it is in the script. Each local process can unlock that keychain and sign code as
"Jaca Dev".

To remove the keychain:

```bash
security delete-keychain "$HOME/Library/Keychains/jaca-dev.keychain-db"
```

### Captured data on disk

| Path | Contents |
|---|---|
| `~/Library/Application Support/Jaca/history.sqlite` | Log history. Settings sets the retention (default 7 days). |
| `~/Library/Caches/Jaca/net-bodies/`, `net-bodies-jacad/` | Request and response bodies of open captures. Each process clears its folder when it starts. |
| `~/.jaca/network-overrides/` | Response override rules and bodies |
| `~/.jaca/logs/`, `~/.jaca/jacad.log` | Diagnostic logs |

Captured traffic and logs can contain tokens and personal data. Remove these files before you
share a log or a screen recording.
