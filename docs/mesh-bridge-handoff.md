# Handoff: SIERRA Mesh Bridge (`meshbridge`)

*For a new agent in a new repo. Read the wire spec first: [PROTOCOL.md](../PROTOCOL.md), shared with the relay in this repo. Where this handoff and the protocol disagree, the protocol wins.*

## Where things stand

- **The relay is live** on the SIERRA data server. It implements the bridge and client WebSockets, leases, error codes, and close codes. Build and test against it from day one, not just against a fake.
- **Not built on the relay yet:** pairing, `paused`, leave, and diagnostics. They're specified in the protocol and are next on the relay's list. Until pairing lands, get a bridge token from a SIERRA admin (created through the relay admin API) and put it in the config file by hand. That's fine for M1 and M2.
- The protocol has an implementation-status table at the top. Check it before starting a feature that depends on the relay.

## What we're building

A small, easy-to-install program for a computer a volunteer already leaves on (Windows PC, Mac, Linux box, Raspberry Pi, NAS). It holds the connection to a MeshCore companion radio, over USB or WiFi, and dials **out** to the SIERRA Mesh Relay at `data.sierragridteam.org`. Authorized SIERRA clients can then use that radio remotely.

The host never opens a port, touches their router, opens a terminal, or edits a file.

The same Go binary has a second mode, `meshbridge connect`, that runs on a *user's* machine. It turns a remote gateway into a local `127.0.0.1:5000`, so meshcore-cli and other TCP companion clients work unchanged.

## Who the host is

Design for this real volunteer:

> I'm good with Windows, Mac and Mesh. Not good with Linux and code... at all, in any shape or form.

He can install apps, approve an admin prompt, and plug in a radio. He knows MeshCore well. He shouldn't ever see a terminal, a config file, a token, or a scary security warning. If a step needs any of those on Windows or Mac, that's a bug in the design.

Linux, Pi, and NAS hosts are more technical. CLI and Docker are fine for them.

## The host experience (Windows and Mac)

1. The SIERRA operator creates an invite and texts the host two things: a download link and a pairing link (`sierramesh://pair?passphrase=maple-river-quartz-otter`). The words in the pairing link are the passphrase.
2. The host runs a **signed** installer (`.msi` on Windows, `.pkg` on Mac) and approves one admin prompt. That installs the background service and the tray/menu bar app.
3. A setup window opens: **"Plug your node into this computer with USB."** If the bridge detects a node, it shows **"Found: Heltec V4 · JayNode ✓"**. A WiFi node on the same network also works ("Looking on your network…").
4. **Pair with SIERRA.**
   - **With a passphrase (the normal path).** The host taps the pairing link in the text, which fills in the passphrase, or types the four words into the box. They click **Connect**. Approval is instant.
   - **Without one.** The window shows a pairing code, **`K7M-4QX`**, with "Text this code to your SIERRA contact." The operator approves it.
5. The window turns green: **"Connected to SIERRA. You can close this window."** The tray or menu bar icon stays green.

Target: under 5 minutes. The host types nothing, or only the four words.

Steps 3 and 4 can happen in either order. Pairing can finish before the node is plugged in.

## Architecture

```
 ┌──────────────────────────┐   localhost control API   ┌──────────────────────────────┐
 │ Native app (per OS)      │ <───────────────────────> │ meshbridge service (Go)      │
 │  Mac: Swift menu bar app │    127.0.0.1:8737         │  node link (USB / TCP)       │
 │  Win: C# tray app        │                           │  relay link (WSS, outbound)  │
 │  setup window, status,   │                           │  pairing, pause, diagnostics │
 │  pause, keep-awake       │                           │  runs at boot, before login  │
 └──────────────────────────┘                           └──────────────────────────────┘
```

- **The Go service does all the work.** It's headless and runs as a system service.
- **The native app is thin.** It shows state and sends a handful of commands. If the app crashes or isn't running, bridging continues.
- **Linux has no native app.** It uses the CLI (`meshbridge setup`, `status`, `pause`), plus Docker.

Why native and not one cross-platform GUI: the app is small, it lives in the menu bar or tray where the platform's conventions matter, and the native APIs it needs (power assertions, USB power settings, login items, update frameworks) are platform-specific anyway.

## Go service

### Principles

- **Dumb pipe.** Forward companion frames unchanged, except the few the node command policy refuses (PROTOCOL.md: a check on each frame's first byte, plus one CLI command and one contact flag). Otherwise the only time the service parses MeshCore traffic is the identity check at link-up. All intelligence lives in the relay.
- **One static binary.** Pure Go, `CGO_ENABLED=0`. Targets: windows/amd64, windows/arm64, darwin/amd64, darwin/arm64, linux/amd64, linux/arm64, linux/arm (v6 and v7).
- **Portable core.** Keep session and protocol logic separate from I/O. A future embedded bridge will reimplement the protocol, so keep behaviour simple.

### Node links

Both links sit behind one interface: `NodeLink { Open(ctx), ReadFrame(), WriteFrame(), Close() }`. Frames are companion payloads with the serial/TCP header stripped (see protocol spec).

**USB serial (recommended for PC hosts: no WiFi setup, no LAN scan, no permission prompts)**
- 115200 baud by default.
- **Auto-detect.** List serial ports, filter by likely MeshCore USB VID/PIDs (Silicon Labs CP210x, WCH CH340/CH9102, Espressif native USB, Adafruit/Nordic nRF52), run the identity check, and keep the port whose pubkey matches `expected_pubkey`. With no expected key, take the single candidate that answers.
- **Driver problem detection.** If a likely USB device is present but no serial port appears (typical for CH340 without a driver), report `driver_missing` with the device's VID/PID. The app shows a one-page fix, not "no node found."
- Handle unplug and replug, and re-detect.
- Library: `go.bug.st/serial` (pure Go, enumerates ports with VID/PID).

**TCP (WiFi or Ethernet companion, port 5000)**
- The node accepts one connection at a time and gets its address by DHCP (no static IP or mDNS upstream).
- Resolve the node's address in this order:
  1. Configured host
  2. Last known good address (cached)
  3. Scan private (RFC 1918) local subnets for port 5000, then run the identity check on each hit. Accept a pubkey match only.
- Use connect timeouts of about 500 ms and limited concurrency.
- **macOS:** test whether LAN scanning triggers the Local Network privacy prompt for the service. If it does, the app must explain it before it appears.

**Identity check.** Send `APP_START` (app name `sierra-meshbridge`), read `SELF_INFO`, and extract the pubkey and name. The serial/TCP framing has been verified against MeshCore 1.18 (see protocol, Frames). The `SELF_INFO` field layout still needs checking against `examples/companion_radio/MyMesh.cpp`.

**Frame size.** The firmware maximum is 176 bytes. Treat anything over the protocol's 512-byte limit as a broken link: log it and relink. Never forward it, because the relay closes with 1009 or `frame_too_large`.

### Relay link

Implement the bridge side of the protocol spec: pairing, `hello`/`welcome`, `node_status` (including `paused`), `stats`, `attach`/`detach`, `relink`, pings, close codes, backoff, `gateway/leave`, `gateway/diagnostics`.

The relay link and node link are independent. Stay connected to the relay when the node is missing (the relay sees "bridge online, node offline"), and keep retrying the node when the relay is unreachable.

Behaviour the live relay has that the bridge must handle:
- **Backpressure, not drops.** The relay throttles each connection to about 50 messages/s, burst 100, by reading more slowly. A node dumping its contact list will see the WebSocket write block. Let it block, and keep a bounded buffer between the node reader and the WebSocket writer, so a slow relay slows the node read instead of growing memory. Never drop frames to keep up.
- **Close codes 1001 (relay restarting) and 1009 (text over 8 KiB).** Reconnect with backoff. For 1009, also log it as a bug, since the bridge's control messages are tiny.
- **`error` messages.** Log the `code`. Never display or depend on `message`, which is free text. Codes the bridge may see: `hello_required`, `unsupported_protocol`, `frame_too_large`, `pubkey_mismatch`, `pubkey_unverified`.
- **Pubkey pinning.** The relay pins a node pubkey per gateway. A pubkey mismatch means the wrong node is plugged in, or the node was reflashed with a new identity. Report it in status as "This isn't the node this bridge is set up for" with a plain next step: if the host replaced the node, `meshbridge change-node`. Never switch nodes without the host asking.

Library: `github.com/coder/websocket`.

### Pause

- Pausing closes the node link (releasing the port for the host's own use) and reports `node_status: paused` with `until`.
- The host picks a duration in the app: 1 hour, until tomorrow, or until resumed. It resumes automatically when the time runs out. It also resumes on "Resume" or reboot, unless "until resumed" was chosen.
- Why this matters: the node allows only one connection. Without pause, a host who also uses that node with their own MeshCore app will eventually just quit the bridge.

### Keep-awake

- When enabled (on by default for PC hosts, with explanation), hold a "system must stay awake" assertion while bridging:
  - **macOS:** IOKit power assertion. The display can still sleep.
  - **Windows:** `SetThreadExecutionState(ES_CONTINUOUS | ES_SYSTEM_REQUIRED)`.
- The service holds the assertion, so it works even when the app isn't running. This needs a quick spike: on Windows, check that a service-held execution state actually prevents sleep. If it doesn't, the app holds it instead.
- **Windows USB:** when a serial node is found, disable USB selective suspend for that device. This happens through the installer or service with admin rights, never by asking the host to open Device Manager.
- **Laptops:** if on battery or the lid is closed, report it as a warning. Don't fight it.

### Diagnostics

"Send diagnostics" builds a zip containing logs, redacted config (no token), a status snapshot, OS and version, a serial port list, and a network interface summary. It must not contain frame payloads. The service uploads it to `gateway/diagnostics` and returns the reference ID to show the host.

### Local control API

The native app talks to the service over HTTP on `127.0.0.1:8737`, never bound to any other address.

- **Auth:** a random secret in a file readable only by admins and the installing user, sent as a bearer header. No firewall prompts, since it's loopback only.

| Endpoint | Does |
|---|---|
| `GET /status` | Overall state (green / amber / red + reason), relay connection, node (name, model, link, pubkey), last mesh traffic time, pause state, keep-awake state, warnings (driver missing, on battery, update ready) |
| `POST /pair/start` / `GET /pair` | Start pairing. Current code and pairing state for the setup window. |
| `POST /pause` `{until}` / `POST /resume` | Pause and resume |
| `POST /keep-awake` `{enabled}` | Toggle keep-awake |
| `POST /diagnostics` | Build and send diagnostics, returns the reference |
| `POST /leave` | Leave the SIERRA network (calls `gateway/leave`, wipes the token) |
| `POST /find-node` | Re-run detection now |

The CLI uses the same API, so `meshbridge status` and the app always agree.

### Config

Hosts on Windows and Mac never see the config file. Pairing writes it. One TOML schema on every platform:

```toml
gateway = "jay-murphys"
relay   = "wss://data.sierragridteam.org/mesh/relay"
token   = "smb_…"

[node]
link = "auto"                # auto | serial | tcp
expected_pubkey = "a1b2…"
serial_port = ""             # optional override
baud = 115200
tcp_host = ""                # optional override
tcp_port = 5000

[host]
keep_awake = true

[log]
level = "info"
```

Locations:
- **Windows:** `%ProgramData%\SIERRA\meshbridge\`
- **macOS:** `/Library/Application Support/SIERRA/meshbridge/`
- **Linux:** `/etc/meshbridge/`

The file is readable only by the service account and admins.

### CLI (mainly for Linux, Pi, NAS, and operators)

| Command | Does |
|---|---|
| `meshbridge setup` | Pair interactively in the terminal: prompts for a passphrase, or press Enter to show a device code. Then installs the service. |
| `meshbridge setup --passphrase "maple-river-quartz-otter"` | Non-interactive setup for Pi images, scripts, and fleet installs. A multi-use invite works for a batch of Pis. |
| `meshbridge install` / `uninstall` | Register or remove the OS service |
| `meshbridge run` | Run in the foreground (what the service runs) |
| `meshbridge status` | Same information as the tray menu |
| `meshbridge pause [1h\|until-resumed]` / `resume` | Pause and resume |
| `meshbridge reconnect` | Look for the node now (`POST /find-node`) |
| `meshbridge find-node` | Run detection and print what it finds |
| `meshbridge diagnostics` | Send diagnostics |
| `meshbridge connect <gateway>` | Client mode (below) |
| `meshbridge version` | Version and build info |

Service registration: `github.com/kardianos/service` (Windows service, systemd, launchd). On Linux, `setup` checks serial access (`dialout` or `uucp` group) and explains the fix in plain words.

## Native apps

### What they show

Tray or menu bar menu:
```
● Connected to SIERRA            (green / amber / red)
  Node: JayNode (Heltec V4) · USB
  Last mesh traffic: 2 min ago
  ─────────────
  Pause sharing                ▸  1 hour / Until tomorrow / Until I resume
  Keep this computer awake  ✓
  Send diagnostics to SIERRA…
  ─────────────
  Open setup…
  Leave SIERRA network…
```

Colors mean:
- **Green:** relay connected and node connected.
- **Amber:** paused, or relay connected but no node yet, or a warning that needs the host.
- **Red:** can't reach the relay, or a node identity mismatch.

Every amber or red state has one plain sentence and, where possible, one button. For example: "Your node isn't plugged in." / "Your computer is about to sleep: [Keep awake]" / "A driver is needed for this radio: [Show me how]".

### Setup window

A few screens only:
- **Find your node.**
- **Pair with SIERRA.** The passphrase box comes first, with "Got a passphrase from SIERRA? Type it here." Typing is forgiving: case, spaces, and hyphens don't matter. Below it is a link, "Don't have one?", which shows the big device code with a copy button and "Text this to your SIERRA contact."
- **Done.**

The window reopens automatically if the bridge isn't paired.

If the passphrase is wrong, say so plainly: "That passphrase didn't work. Check the words, or ask your SIERRA contact for a new one." Then offer the device code. Never show the relay's reason, because it deliberately gives none.

**Deep link.** Register the `sierramesh://` URL scheme in both installers: Info.plist `CFBundleURLTypes` on Mac, a registry protocol handler on Windows. `sierramesh://pair?passphrase=…` opens the setup window with the passphrase filled in. **The host must still click Connect.** A link must never pair a machine on its own, and an already-paired bridge ignores pairing links unless the host chooses "Re-pair."

The passphrase is redeemed once and never stored. Only the resulting token is saved.

### Platform notes

- **Mac:** Swift/SwiftUI menu bar app (`MenuBarExtra`). Register as a login item with `SMAppService`. Ships inside the signed, notarized `.pkg` alongside the service binary and launchd plist.
- **Windows:** C# (WinUI 3 or WinForms) tray app. Starts at login for the installing user. Ships in the signed `.msi` alongside the service.

## Packaging, signing, updates

- **Code signing is required before the first non-technical host,** not later. That means Apple Developer ID signing and notarization, and Windows Authenticode, ideally with an EV or Azure Trusted Signing certificate so SmartScreen trusts it quickly. An unsigned build triggers warnings that stop exactly the hosts we're designing for.
- **Auto-update, silent by default:**
  - Mac: Sparkle (EdDSA-signed appcast) updates the app, and the app updates the service binary.
  - Windows: a signed updater, or MSI upgrade, run by the service.
  - The host can see "Updated to 0.5.0" in the menu but never has to act.
  - Linux: packages through the package manager, or Docker image tags.
- **Release pipeline:** goreleaser for the Go binaries (all targets, checksums, `.deb`/`.rpm`, Docker image). Separate pipelines build and sign the Mac `.pkg` and Windows `.msi`, which embed those binaries.

## Client mode: `meshbridge connect`

```
meshbridge connect avery-ridge --token smh_… [--listen 127.0.0.1:5000]
```

- Listens on localhost only, unless `--listen` explicitly says otherwise (warn loudly).
- Accepts one local TCP connection and opens a client session at `wss://<relay>/mesh/relay/<gateway>` (subprotocol `sierra-mesh-client.v1`).
- Translates companion TCP framing to and from binary WebSocket messages.
- Prints lease status and `lease_ending` warnings.
- Follows the live relay's lease rules:
  - Leases are 30 min.
  - Send `renew` automatically when the 60-second `lease_ending` warning arrives, while the local client is still active.
  - Renewals stop at 8 h, after which `renew_denied` comes back. Tell the user and let the lease end.
- Turns relay errors into plain messages:
  - `busy` / 4029: "Someone else is using this gateway."
  - `gateway_offline`: "Gateway is offline."
  - `node_paused`: "The host has paused sharing until <time>."
  - `pubkey_mismatch` / `pubkey_unverified`: "Gateway's node needs checking by a SIERRA admin."
- Supports `--preempt` for admins (adds `?preempt=1`).

CLI only for now. A native "Connect to a gateway" feature can come later.

## Testing

- **Fake node:** speaks companion framing over TCP and a virtual serial pair, answers `APP_START` with a configurable `SELF_INFO`.
- **Real relay first.** The relay is live. Ask a SIERRA admin for a test gateway and token, and run the conformance tests against it in CI or nightly.
- **Fake relay:** still useful, for offline unit tests and for pairing and pause until the relay ships them. Implement the relay side of the protocol, including a CLI to approve pairing codes. Publish it as `cmd/fakerelay`. Match the live relay's behaviour where the protocol pins it down: backpressure rather than drops, error codes, and close codes.
- **Conformance tests** from the protocol spec:
  - `hello`/`welcome`
  - Close codes, including 1001 and 1009
  - Backoff
  - Frame limit
  - Backpressure under a burst of 1,000 frames: none dropped, memory stays bounded
  - Relink
  - Pubkey mismatch
  - Pairing, with the token delivered exactly once
  - Pause and resume
- **Host-experience test.** Before the pilot, have someone non-technical (ideally the volunteer quoted above) install on a fresh Windows PC and a fresh Mac with no help, while we watch. Every place they hesitate becomes a fix.
- **Hardware checklist:**
  - Heltec V4 over USB and WiFi
  - RAK4631 over USB
  - A CH340 board with no driver installed
  - Unplug and replug
  - Node reboot
  - Router reboot
  - Laptop sleep and wake
  - Relay restart
  - Pause and resume while a client holds a lease

## Milestones

1. **M1, pipe works.** Service `run` with TCP link + relay link + identity check, using a hand-written config file and an admin-issued token. Get meshcore-cli working end to end through the **live relay** against a real WiFi node. For this milestone, a client session can use a simple `wscat`-style test client, or `meshbridge connect` built early as a test tool.
2. **M2, serial.** USB link with auto-detect, replug handling, and driver-missing detection.
3. **M3, pairing and service.** Pairing, service install on all three OSes, local control API, CLI `status`/`pause`/`resume`. Linux hosts can do the full setup from here. Depends on the relay shipping pairing and `paused`. Build against `cmd/fakerelay` until it does, and check the protocol's status table.
4. **M4, native apps.** Mac menu bar app and Windows tray app with the setup window, pause, keep-awake, and diagnostics. Signed and notarized installers.
5. **M5, updates and pilot.** Auto-update, the host-experience test, a one-page install guide with screenshots, and a pilot with 2–3 hosts including at least one Windows and one Mac.
6. **M6, client mode.** `meshbridge connect`.

## Out of scope

Polling and telemetry (the relay does that). Messaging UI. Changing node settings. Firmware flashing. Configuring WiFi on nodes (hosts use the MeshCore app for that; recommend USB for PC hosts to avoid it entirely).

## Future (not this repo yet): embedded bridge

A dedicated low-cost bridge device, possibly a 3D-printed box with a companion node inside, implementing the same protocol, probably on an ESP32-class chip. Pairing works the same way. A tiny display or LED blink pattern shows the code. Keep the protocol repo the source of truth.
