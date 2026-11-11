# SIERRA Mesh Relay — Wire Protocol v1

*The canonical spec. The relay, the bridge, and client libraries all implement this. It lives in [sierragridteam/sierra-meshrelay](https://github.com/sierragridteam/sierra-meshrelay) alongside the relay, the bridge and the Go types (package `meshproto`); other repos link here instead of keeping a copy. Change the spec, the types and both implementations in the same pull request.*

## Implementations

| Component | Language | Repo | Role |
|---|---|---|---|
| **Relay** | Go | `dpup/sierra-meshrelay` (`relay/`), deployed by `sierragridteam/sierra-data` | Accepts bridges and clients, leases, pairing, admin API |
| **meshbridge** | Go | `dpup/sierra-meshrelay` (`cmd/meshbridge`) | Bridge daemon at host sites. Also `meshbridge connect`, a localhost TCP shim for TCP-only clients (official MeshCore app, meshcore-cli). |
| **meshproto** | Go | `dpup/sierra-meshrelay` (`meshproto/`) | Message types, close codes, error codes and constants. Used by both the relay and meshbridge, and tested against this spec's examples so they can't drift. |
| **meshcore-ts relay transport** | TypeScript | `dpup/meshcore-ts` | `MeshCoreClient.relay(…)`: a native client of this protocol, so any meshcore-ts app can use a gateway with no shim |
| **meshcore-mcp** | TypeScript | `dpup/meshcore-mcp` | Uses the meshcore-ts relay transport, so Elmer and other MCP clients can work any gateway remotely |
| **meshcore-sim** | TypeScript | `@dpup/meshcore-sim` | Simulated companion node for testing bridges and clients without hardware |

Why this split:
- **Bridges and the relay are Go.** The bridge only frames bytes and does one identity check, so it doesn't need a MeshCore client library. It needs static binaries for every OS, a pure-Go serial library, and native service management, and sharing a module with the relay keeps the protocol from drifting.
- **Clients are TypeScript.** They need the full companion protocol, which meshcore-ts already provides.

## Implementation status (Nov 5, 2026)

| Area | Relay | meshbridge | meshcore-ts |
|---|---|---|---|
| Bridge and client WebSockets, leases, errors, close codes | Done | Bridge side done (M1: TCP node link) | n/a |
| Pairing (device code) | Done (admin API; no approval page yet) | Done (`meshbridge setup`) | n/a |
| Passphrase invites | Done | Done | n/a |
| `paused` | Done | Done (`meshbridge pause`/`resume`, local control API) | n/a |
| Shared repeaters (`gateway/repeaters`) | Done | Done (`meshbridge repeater`) | n/a |
| Bridge logs (`log`) | Done | Done (`host.send_logs`) | n/a |
| Updates (`update_available`) | Done (`PUT /bridge-update`) | Done (signed releases; `host.auto_update`) | n/a |
| Leave, diagnostics bundle | Not yet built | Not started | n/a |
| Client tickets (browser auth) | Not yet built | n/a | Not started |
| Client relay transport | n/a | `connect` shim done | Not started |
| Shared Go types (`meshproto`) | Uses them | Uses them | n/a |

Update this table as things land.

## Overview

Three parties:

- **Node**: a stock MeshCore companion radio (upstream firmware ≥ 1.18). Speaks the MeshCore companion protocol over USB serial or TCP port 5000.
- **Bridge**: software at a host's house. Holds the one connection to the node and dials *out* to the relay.
- **Relay**: runs at `data.sierragridteam.org`. Accepts bridges, and lets authenticated clients talk to a node through its bridge.

```
 node <--serial/TCP--> bridge ==WSS (bridge dials out)==> relay <==WSS== client
```

Nothing ever connects in to a host's house. Nothing on the relay gives unauthenticated access to a node.

## Endpoints

| Path | Used by | Subprotocol |
|---|---|---|
| `wss://data.sierragridteam.org/mesh/relay/<gateway>/dial` | Bridge | `sierra-mesh-bridge.v1` |
| `wss://data.sierragridteam.org/mesh/relay/<gateway>` | Client | `sierra-mesh-client.v1` |
| `https://data.sierragridteam.org/mesh/relay/api/pair/...` | Unpaired bridge | n/a (HTTP, see Pairing) |
| `https://data.sierragridteam.org/mesh/relay/api/gateway/...` | Paired bridge | n/a (HTTP, see Bridge self-service) |
| `https://data.sierragridteam.org/mesh/relay/api/tickets` | Browser-client backends | n/a (HTTP, see Client tickets) |
| `https://data.sierragridteam.org/mesh/relay/api/...` | Admin tools | n/a (see relay spec) |

`<gateway>` is a lowercase name, `[a-z0-9-]{3,40}`, e.g. `avery-ridge`.

## Authentication

- Both bridge and client send `Authorization: Bearer <token>` on the WebSocket upgrade request.
- Bridge tokens are per gateway. The relay rejects a bridge whose token doesn't belong to `<gateway>`.
- Client tokens are per person or per agent. They carry a role (see relay spec).
- Tokens are opaque: 32 random bytes, base64url, prefixed `smb_` (bridge) or `smc_` (client) so they're recognizable if leaked.
- The relay rejects the upgrade with HTTP 401 for a bad token, 404 for an unknown gateway, and 426 for an unsupported subprotocol.

### Client tickets (for browsers)

Browsers can't set an `Authorization` header on a WebSocket. Browser clients use a short-lived ticket instead. Never put a long-lived token in a URL, because URLs end up in logs and browser history.

1. With the client token, `POST https://data.sierragridteam.org/mesh/relay/api/tickets` `{ "gateway": "avery-ridge" }` → `{ "ticket": "smt_…", "expires_in": 60 }`.
   - The browser app gets the ticket from its own backend, which holds the client token. A browser should never hold an `smc_` token itself.
2. Connect to `wss://data.sierragridteam.org/mesh/relay/avery-ridge?ticket=smt_…` with subprotocol `sierra-mesh-client.v1`.

Ticket rules:
- Single use, valid for 60 s.
- Bound to one gateway and to the issuing client's identity and role.
- Using it consumes it. A reused or expired ticket gets HTTP 401.
- The relay must strip `ticket` from any URL it logs.

Non-browser clients (meshcore-ts on Node, meshbridge, agents) keep using the `Authorization` header.

## Frames

Each WebSocket message is one of two kinds:

- **Binary message** = exactly one MeshCore companion protocol frame *payload*, without the serial/TCP header.
  - On the node link, companion frames are wrapped as `[0x3c][len_lo][len_hi][payload]` toward the node and `[0x3e][len_lo][len_hi][payload]` from the node. The bridge strips the header when reading from the node and adds it when writing to the node.
  - Direction is implied by who sent the message, so no header is needed on the WebSocket.
  - Max payload: 512 bytes. Anything larger is a protocol error. (Current firmware max frame is 176; this leaves headroom.)
  - Verified against MeshCore v1.18.0: `ArduinoSerialInterface.cpp` (`checkRecvFrame` / `writeFrame`) and `SerialWifiInterface.cpp` both use `'<'` (0x3c) toward the node and `'>'` (0x3e) from it, each followed by a 2-byte little-endian length. `MAX_FRAME_SIZE` is 176 (`src/helpers/BaseSerialInterface.h`); the firmware drops oversized outgoing frames and truncates oversized incoming ones.
- **Text message** = one JSON control object with a `type` field. Unknown `type` values are ignored (forward compatibility).

The relay forwards binary messages between the bridge and the client holding the lease, unchanged. It doesn't parse them, except where the relay spec says otherwise, and except for the node command policy below.

**Node command policy.** Companion firmware trusts whoever holds the companion link as the node's owner, sitting next to it. A lease holder isn't, so a few frames are refused for every lease, admins and the node's owner included (package `nodepolicy`):

| Direction | Frame | Why |
|---|---|---|
| to node | `CMD_EXPORT_PRIVATE_KEY` (23) | returns the node's private key |
| to node | `CMD_IMPORT_PRIVATE_KEY` (24) | replaces the node's identity |
| to node | `CMD_FACTORY_RESET` (51) | wipes the node |
| to node | `CMD_RUN_CLI_COMMAND` (66) with `get wifi.pwd` | returns the WiFi password (usually the host's home network). Matched after the firmware's own normalization (leading spaces, an `xx\|` prefix) and more loosely (case, whitespace, trailing text). |
| to node | `CMD_ADD_UPDATE_CONTACT` (9) setting contact flag `0x10` | would let that contact run the node's CLI over the radio, past the bridge |
| to node | wadamesh `ota ...`: `CMD_SEND_RAW_DATA` (25) with an empty path, or `CMD_SEND_TXT_MSG` (2) to the `MESHCM` pseudo-contact | an HTTP firmware update, which wadamesh accepts from a WiFi TCP session; new firmware could read out every secret above |
| from node | `RESP_CODE_PRIVATE_KEY` (14) | second layer: dropped |

A refused command never reaches the node. The lease holder gets `RESP_CODE_DISABLED` (15), the firmware's own "not available" reply, and the lease continues. The bridge enforces this on the host's machine, and the relay enforces it again, so an older bridge or a relay mistake doesn't let these through. Both count and log each refusal by reason, never its contents. Everything else passes, including commands neither side knows. Known gaps, by decision (2026-11-11):

- The CLI rule matches one command. A firmware fork could add another way to read a secret.
- Signing (`CMD_SIGN_START`/`DATA`/`FINISH`, 33–35) passes: the key stays on the node, but a lease holder can have the node sign data while the lease lasts.
- Commands that can strand or lock out the owner pass: `set pin`, `set wifi.*`, and wadamesh's `tcp off`/`ble off`. None of them reveals a secret.
- wadamesh's on-device backup export holds the private key and WiFi password, but only the device's own screen can start it.

**Rate limit.** The relay throttles each connection to about 50 messages/s (burst 100). Throttling is backpressure: the relay reads more slowly. It does not drop frames or disconnect, so a node that sends its whole contact list at once is slowed down, not cut off.

## Bridge ↔ relay control messages

### `hello` (bridge → relay, first message, required)

```json
{
  "type": "hello",
  "protocol": 1,
  "gateway": "avery-ridge",
  "agent": { "name": "meshbridge", "version": "0.3.1", "os": "linux", "arch": "arm64" },
  "link": { "kind": "serial", "detail": "/dev/ttyACM0" },
  "node": { "state": "connected", "pubkey": "a1b2…64 hex", "name": "SIERRA-GW-avery-ridge", "fw": "1.18.0", "model": "Heltec V4" }
}
```

- `link.kind`: `serial` or `tcp`. `detail` is informational (port name or `host:port`).
- `node.state` is one of:
  - `connected`
  - `disconnected`
  - `mismatch`: the node's pubkey doesn't match the bridge's configured `expected_pubkey`.
  - `paused`: the host paused sharing, and the bridge has released the node. Include `"until": "<RFC 3339 time>"`.
- When `node.state` isn't `connected`, `pubkey`, `name`, `fw`, and `model` may be omitted.
- The relay closes the socket with 4003 if `hello` doesn't arrive within 10 seconds.

### `welcome` (relay → bridge)

```json
{ "type": "welcome", "session": "s_7f3c…", "ping_interval_s": 25, "server_time": "2026-11-03T22:51:00Z" }
```

The bridge must not send binary messages before `welcome`.

### `node_status` (bridge → relay, on any change)

```json
{ "type": "node_status", "state": "disconnected", "error": "serial port closed" }
```

Same `state` values and node fields as `hello.node`. Send it when the node link drops, reconnects, or fails the pubkey check, and when the host pauses or resumes sharing.

```json
{ "type": "node_status", "state": "paused", "until": "2026-11-04T01:30:00Z" }
```

When it receives `paused`:
- The bridge has already closed its node link.
- The relay ends any active lease with `lease_ending` (reason `host_paused`, `in_s: 0`).
- New clients get the "gateway unavailable" treatment with code `node_paused`, which follows the existing `node_<state>` pattern.
- The relay shows the gateway as intentionally unavailable, not failed.
- Leases resume when the bridge reports `connected`.

Pausing is the host's right. The relay never overrides it.

### `stats` (bridge → relay, every 60 s)

```json
{ "type": "stats", "uptime_s": 86400, "node_link_reconnects": 2, "frames_to_node": 1204, "frames_from_node": 3388, "relay_reconnects": 1 }
```

Counters run from bridge start. `frames_from_node` counts every frame read from the node after the identity check, including those dropped because no client was attached. `frames_to_node` counts frames written to the node.

### `attach` / `detach` (relay → bridge)

```json
{ "type": "attach", "lease": "l_91ad…", "client": "elmer" }
{ "type": "detach", "lease": "l_91ad…" }
```

Informational: tells the bridge a client started or stopped using the node. The bridge logs these. If no client is attached, the bridge drops frames that arrive from the node, except for its own identity check.

### `relink` (relay → bridge)

```json
{ "type": "relink" }
```

The bridge closes and reopens its connection to the node, then re-runs the identity check and sends `node_status`. Used to clear a stuck node session.

### `update_available` (relay → bridge, optional)

```json
{ "type": "update_available", "version": "v0.7.0", "url": "https://github.com/sierragridteam/sierra-meshrelay/releases/download/v0.7.0", "sha256": "", "sig": "" }
```

The relay offers the release an admin chose (`PUT /bridge-update`) to each bridge running something else, when it connects and when the choice changes. `url` is the release's download directory. The relay doesn't vouch for it: the bridge downloads `SHA256SUMS` and `SHA256SUMS.sig` from there and installs only if the signature verifies against a release key built into the bridge, and the archive for its platform matches. The signature is ed25519 over `meshbridge release <version>\n` followed by `SHA256SUMS`, base64. `sha256` and `sig` are unused (empty).

meshbridge installs only newer versions, only as an installed service, and only while no client is attached. A bridge may ignore the message (`host.auto_update = false`, or a bridge run by hand), and older bridges just log it.

### `log` (bridge → relay, optional)

```json
{ "type": "log", "entries": [ { "seq": 41, "t": "2026-11-06T17:19:11Z", "level": "WARN", "msg": "node not found; scanning the LAN", "attrs": { "addr": "192.168.1.50:5000" } } ], "lost": 2 }
```

The bridge's recent log lines (Info and above), so SIERRA admins can see why a bridge is stuck without reaching the host's computer. The bridge sends lines logged while it was offline once it connects, and `lost` counts lines it dropped before it could send them.

- Never frame payloads, and never secrets: a bridge redacts attributes named like `token`, `password`, `passphrase`, `secret`, `pin` or `key`.
- A message stays under 7 KiB. The relay keeps at most 100 entries per message, 120 per minute per session, and the newest 1,000 per gateway, and shows them only to admins (`GET /gateways/<name>/logs`).
- Hosts can turn it off with `host.send_logs = false`.

### `error` (either direction)

```json
{ "type": "error", "code": "frame_too_large", "message": "…" }
```

Codes the relay sends: `hello_required`, `unsupported_protocol`, `frame_too_large`, `busy`, `gateway_offline`, `pubkey_mismatch`, `pubkey_unverified`, `node_<state>`, `renew_denied`, `unknown_lease`. Treat `message` as free text and don't rely on its content. The relay logs only the `code` and the length of `message` for an `error` that a bridge sends.

## Client ↔ relay control messages

### Getting a lease

A client gets a lease by connecting. The relay grants one only when the bridge is online, the node is `connected`, and the node's pubkey matches the gateway's pinned key. Otherwise:

- **Gateway busy:** the relay sends `error` with `code: "busy"` and closes with 4029.
- **Gateway unavailable:** the relay sends `gateway_status`, then `error` with a `code` (`gateway_offline`, `pubkey_mismatch`, `pubkey_unverified`, or `node_<state>`), then closes with 1000.

An `admin` can append `?preempt=1` to the client URL. That takes the lease from its current holder, who gets `lease_ending` with reason `preempted`. A non-admin who asks for preemption gets HTTP 403.

### `lease` (relay → client, after a successful upgrade)

```json
{ "type": "lease", "lease": "l_91ad…", "gateway": "avery-ridge", "expires_at": "2026-11-03T23:21:00Z", "node": { "state": "connected", "pubkey": "…", "name": "…" } }
```

The client may send binary frames after `lease`. The client is responsible for the normal companion handshake (`APP_START`, etc.).

### `lease_ending` (relay → client)

```json
{ "type": "lease_ending", "lease": "l_91ad…", "in_s": 60, "reason": "expiry" }
```

`reason` is one of the following. A client must treat any reason it doesn't recognize as "the lease is ending."

- `expiry`: the lease ran out.
- `preempted`: an admin took over.
- `gateway_offline`: the bridge disconnected.
- `idle`: no frames in either direction for 10 min.
- `revoked`: the client's token was revoked, the gateway was deleted, or the host left the network.
- `pubkey_mismatch`: the node started reporting a pubkey that doesn't match the pin.
- `relay_shutdown`: the relay is restarting.
- `host_paused`: the host paused sharing. Always `in_s: 0`.

With `in_s` > 0 it's a warning: the relay sends one 60 s before expiry, and a `renew` cancels it. With `in_s: 0` the lease has already ended, and the relay closes the socket with 1000.

`renew` extends the lease by another 30 min. Renewals stop at 8 h from the start of the lease. After that the relay answers `renew` with `error` `renew_denied`. A `renew` for a different lease id gets `error` `unknown_lease`.

### `renew` (client → relay)

```json
{ "type": "renew", "lease": "l_91ad…" }
```

Extends the lease if policy allows. The relay answers with a new `lease` message.

### `gateway_status` (relay → client, on change)

```json
{ "type": "gateway_status", "bridge": "offline", "node": { "state": "disconnected" } }
{ "type": "gateway_status", "bridge": "online", "node": { "state": "paused", "until": "2026-11-04T01:30:00Z" } }
```

## Pairing (HTTP)

A freshly installed bridge has no token. There are two ways to get one, and both end with the relay delivering the token straight to the bridge, so nobody copies a secret.

- **Passphrase (pre-approved).** An admin creates an invite ahead of time and gives the host a passphrase, for example `maple-river-quartz-otter`. The host types it into setup, and the bridge is approved immediately with no waiting.
- **Device code (approve on the spot).** The bridge shows a short code, the host texts it to an admin, and the admin approves it. This is the same pattern as signing a TV app into an account, and it's the fallback when the host has no passphrase.

All JSON over HTTPS, under `https://data.sierragridteam.org/mesh/relay/api/`.

### `POST pair/start` (bridge, no auth)

Request:
```json
{
  "agent": { "name": "meshbridge", "version": "0.3.1", "os": "windows", "arch": "amd64" },
  "host_label": "Jay's PC",
  "node": { "state": "connected", "pubkey": "…", "name": "JayNode", "fw": "1.18.0", "model": "Heltec V4", "link": "serial" }
}
```
- `host_label` is optional and comes from the computer name. It helps the admin recognize the request.
- `node` may be `{ "state": "disconnected" }` if no node has been found yet. Pairing can happen first.
- `passphrase` is optional. If present, the relay redeems the matching invite (see Passphrase invites below).

Response `200` with a valid passphrase. The response is approved immediately, with no polling:
```json
{
  "status": "approved",
  "gateway": "jay-murphys",
  "token": "smb_…",
  "relay": "wss://data.sierragridteam.org/mesh/relay",
  "expected_pubkey": "…"
}
```

With a bad passphrase, the response is HTTP `403` `{ "error": "invalid_passphrase" }`. That one status covers wrong, expired, used-up, and revoked passphrases, so a guesser learns nothing. The bridge says "That passphrase didn't work" and offers the device-code flow instead.

Response `200` without a passphrase (device-code flow):
```json
{
  "pair_id": "p_3c9e…",
  "poll_secret": "random 32 bytes, base64url",
  "user_code": "K7M-4QX",
  "verification_url": "https://data.sierragridteam.org/mesh/pair?code=K7M-4QX",
  "expires_in": 600,
  "interval": 5
}
```
- `user_code`: 6 characters from an unambiguous alphabet (no `0 O 1 I L`), shown as `XXX-XXX`.
- The bridge shows `user_code` large, plus the URL and a "text this code to your SIERRA contact" hint.
- The relay rate-limits `pair/start` per source IP and caps pending pairs. It returns HTTP 429 when either limit is hit.

### `POST pair/poll` (bridge)

Request: `{ "pair_id": "…", "poll_secret": "…" }`. Poll every `interval` seconds.

| Response | Meaning |
|---|---|
| `{"status":"pending"}` | Keep polling |
| `{"status":"approved","gateway":"jay-murphys","token":"smb_…","relay":"wss://data.sierragridteam.org/mesh/relay","expected_pubkey":"…"}` | Done. Delivered **exactly once**; the pairing record is then consumed. |
| `{"status":"denied"}` | Admin rejected it. Show a friendly message. |
| `{"status":"expired"}` | Code timed out, or it was already consumed. Offer "Get a new code." |
| HTTP 429 | Polling too fast. Back off. |

- `poll_secret` is what protects the token. Knowing `user_code` alone can't collect it. The relay compares it in constant time.
- `expected_pubkey` is the pubkey the admin approved, usually the one reported in `pair/start`. The bridge saves it to config. The relay pins the same key for the gateway, so the bridge's first `hello` passes the pin check.

### Approval (admin side)

Admins approve through the relay admin API or the admin page at `https://data.sierragridteam.org/mesh/pair`; see the relay spec. Approval either creates a new gateway or **re-pairs an existing one** (for example, the host replaced their PC). Re-pairing rotates that gateway's token. Any old bridge session is closed with 4001.

### Passphrase invites

An invite is an approval created ahead of time. An admin creates it through the admin API, and the relay returns the passphrase **once**. The admin sends it to the host by text, email, or printed card.

**Passphrase format**
- 4 words from a fixed list of 2,048 short, common, easy-to-spell English words (≈44 bits).
- Joined by hyphens, all lowercase, e.g. `maple-river-quartz-otter`.

**Matching is forgiving.** The relay lowercases the input and treats spaces, hyphens, commas, and runs of them as separators. So `Maple River quartz  otter` matches. Hosts type passphrases from text messages on phones, and autocorrect and capitalization shouldn't make them fail.

**Invite fields**
| Field | Meaning |
|---|---|
| `gateway` | Gateway name the redeemer gets. If the gateway already exists, redeeming re-pairs it (rotating the token). Omit it for a multi-use invite. |
| `name_prefix` | For multi-use invites: gateways are named `<prefix>-<n>`, e.g. `workshop-1`, `workshop-2`. Gateways can't be renamed: to change a name, an admin removes the gateway and sends a new invite. |
| `max_uses` | Default 1. Above 1 is for setup sessions where several hosts install at once. |
| `expires_at` | Default 7 days, max 30. |
| `expected_pubkey` | Optional. If set, the bridge's reported node pubkey must match or the redemption fails with `invalid_passphrase`. If unset, the node pubkey reported at redemption is pinned. If no node was connected at redemption, the first pubkey in `hello` is pinned. |
| `note` | Free text for admins, e.g. "Jay, Murphys, Heltec V4". |

**Security**
- The passphrase is a bearer secret with limited uses. The relay stores only a hash. Use a slow hash (argon2id or bcrypt), because 44 bits isn't enough against an offline attack on a fast hash.
- Rate-limit `pair/start` attempts that include a passphrase: per source IP (e.g. 10 per hour) and globally. With that limit, guessing 44 bits online is impractical.
- Every redemption is logged and notifies admins: gateway, invite, source IP, host label, node pubkey.
- Invites can be revoked before use. Revoking doesn't affect gateways already created from it. Revoke those separately.

**Deep link (optional, recommended)**
`sierramesh://pair?passphrase=maple-river-quartz-otter`
On a machine with meshbridge installed, opening this link opens setup with the passphrase filled in. The admin can text the link instead of the bare passphrase, so the host just taps it. The bridge still shows the passphrase and asks the host to confirm before redeeming it. A link alone must never pair a machine silently.

## Bridge self-service (HTTP)

Authenticated with the bridge token: `Authorization: Bearer smb_…`.

### Shared repeaters: `gateway/repeaters`

A host shares a MeshCore repeater with SIERRA by giving its password (preferably the guest one). The relay then logs in to it over the mesh, through this gateway or any other in range, to read its status. Before sharing, the bridge must tell the host that any SIERRA gateway in range may be used to log in to the repeater.

| Method & path | Does |
|---|---|
| `GET gateway/repeaters/nearby` | The repeaters in this gateway's node's contacts: `{repeaters: [{pubkey, name, last_advert, shared}]}`. Takes a short lease on the node; 409 if it's busy or paused. |
| `POST gateway/repeaters` `{pubkey, password, name?}` | Log in to the repeater through this gateway's node, read its status, and if that works, store the password. Answers `{repeater, check: {name, access, status, telemetry, gateway}}`. 403 for a wrong password, 504 if the repeater doesn't answer, 409 if the node is busy or paused or another gateway already shared it, 503 if the relay can't store passwords. Can take up to a minute. |
| `GET gateway/repeaters` | The repeaters this gateway shared (never the password) |
| `DELETE gateway/repeaters/<pubkey>` | Stop sharing one this gateway shared; the password is deleted |

`access` is `guest` or `admin`: what the password logged in as. The node's contacts are left as they were: a repeater added as a contact for the check is removed after.

### `POST gateway/leave`

The host clicked "Leave SIERRA network." The relay revokes the token, marks the gateway `left` (keeping its history), closes the live session with 4001, and notifies admins. Active leases end with reason `revoked`. The bridge then deletes its token and, if asked, uninstalls its service.

### `POST gateway/diagnostics`

Upload a diagnostics bundle the host chose to send.
- Body: `application/zip`, at most 5 MB (HTTP 413 if larger).
- Contents: bridge logs, redacted config (no token), a status snapshot, OS and version, serial port list, network interface summary.
- Must not contain companion frame payloads.
- Response: `{ "id": "d_…", "received_at": "…" }`. The bridge shows the host "Sent. Reference d_…".
- The relay keeps bundles for 30 days, visible to admins only.

## Identity check (bridge)

When the bridge opens the node link, it sends `APP_START` with app name `sierra-meshbridge`, reads `SELF_INFO`, and takes the node's public key and name from it. It then sends `DEVICE_QUERY` and reads `DEVICE_INFO` for the firmware version and board model (`hello.node.fw` and `model`). That step is best effort: a node that doesn't answer still passes. `DEVICE_INFO` also carries the node's BLE PIN, so the bridge reads only those two fields and never logs the frame.

- If `expected_pubkey` is configured and doesn't match, the node is `mismatch`. The bridge reports it and forwards nothing.
- If `expected_pubkey` isn't configured, the bridge reports the key it found. The relay pins it on first sight and flags any later change.

This is the only time the bridge parses companion frames.

A node in `mismatch` is still reported with its `pubkey` and `name`, so an admin can see which node is plugged in. The relay only pins keys from a `connected` node.

## Keepalive and close

- Both sides send WebSocket pings every `ping_interval_s` (default 25 s). This keeps proxies with an idle timeout of about 100 s from dropping the socket.
- Either side closes after 3 missed pongs.

Close codes:

| Code | Meaning | Bridge reaction |
|---|---|---|
| 1000 | Normal | Reconnect with backoff |
| 1001 | Going away: the relay is restarting | Reconnect with backoff |
| 1009 | Message too big (text over 8 KiB) | Reconnect with backoff, and fix the sender |
| 4001 | Bad or revoked token | Back off 15 min, log loudly |
| 4002 | Gateway name doesn't match token | Back off 15 min, log loudly |
| 4003 | Protocol error or missing `hello` | Reconnect with backoff |
| 4004 | Unsupported protocol version | Back off 1 h, log "update required" |
| 4009 | Replaced: a newer session for this gateway connected | Reconnect with backoff (2 bridges with the same token will flap; that's visible in relay logs) |
| 4029 | (Client) Gateway busy, lease held by someone else | n/a |

Bridge reconnect backoff: start at 1 s, double up to 60 s, add ±20% jitter. Reset after 5 minutes connected.

## Client libraries

Any client of `sierra-mesh-client.v1` is a normal MeshCore companion app, connected through the relay instead of TCP or serial. It must:

- Send each companion frame payload as one binary message, with no `<` header and no length. Received binary messages are payloads too.
- Wait for `lease` before sending frames, then do the normal companion handshake (`APP_START` etc.).
- Handle `lease_ending`. When `in_s > 0` and the app is still in use, send `renew` automatically. Treat `in_s: 0`, or any unknown `reason`, as disconnected.
- Turn relay `error` codes into clear messages: `busy`, `gateway_offline`, `node_paused`, `pubkey_mismatch`, `pubkey_unverified`, `renew_denied`. Never show the `message` field as if it were authoritative.
- Reconnect only when the user or app asks to. Don't auto-reconnect in a loop on 4029, because someone else holds the lease.

### meshcore-ts

Add a relay transport next to `tcp()` and `serial()`:

```ts
const client = await MeshCoreClient.relay({
  url: "wss://data.sierragridteam.org/mesh/relay",
  gateway: "avery-ridge",
  token: process.env.SIERRA_MESH_TOKEN,      // Node: bearer header
  // or: ticket: "smt_…"                     // browser: one-time ticket
});
```

- Implement it as a meshcore.js connection class whose send and receive work on frame payloads. That's a small adapter, since meshcore.js already frames payloads for its TCP and serial connections.
- Expose lease events (`lease`, `leaseEnding`, `gatewayStatus`) as typed events. Expose relay errors as typed errors.
- Node gets the `Authorization` header through the WebSocket library's headers option. In the browser, accept only `ticket`.

### meshcore-mcp

Add a third transport option next to `MESHCORE_HOST` and `MESHCORE_SERIAL_PATH`. Exactly one of the three must be set.

```
MESHCORE_RELAY_URL=wss://data.sierragridteam.org/mesh/relay
MESHCORE_RELAY_GATEWAY=avery-ridge
MESHCORE_RELAY_TOKEN=smc_…
```

- Use an `agent`-role client token.
- Show lease state in the MCP server's status output. That way an agent can tell "someone else has this gateway" apart from "the gateway is down."

## Testing

- **End-to-end tests** (`e2e/` in `dpup/sierra-meshrelay`) run the real relay and the real bridge in one process, with a fake node and a real client. Each side also has its own tests: the relay against a scripted bridge and client, the bridge against a scripted relay.
- **meshcore-sim** is the reference fake node. meshbridge's tests run it as a TCP companion (if it can't listen on TCP yet, that's a small addition to make there). This tests the bridge against an independent implementation, not one written alongside it.
- **End-to-end:** meshcore-sim → meshbridge → relay → meshcore-ts relay transport. A meshcore-ts client should get identical results whether it connects to meshcore-sim over TCP directly or through the relay. That's the main end-to-end test.

## Versioning

- The subprotocol name carries the major version. Breaking changes mean `v2` and a new subprotocol name.
- Additive changes (new optional fields, new message types) keep `v1`. Receivers ignore unknown fields and types.
- `hello.protocol` carries the same major version as a sanity check.

## Design constraint

Keep the protocol small enough for a future embedded bridge (ESP32-class): JSON control messages are tiny and infrequent, binary frames are passed through unchanged apart from the stateless node command policy, and nothing requires the bridge to keep companion-protocol state beyond the identity check.
