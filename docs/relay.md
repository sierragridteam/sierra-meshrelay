# Handoff: SIERRA Mesh Relay (data server)

*The relay's service spec and design decisions. Read the wire spec first: [PROTOCOL.md](../PROTOCOL.md). The relay's code is the `relay` package in this repo. The deployed binary (`cmd/mesh-relay`), its `prefab.yaml` config, and the deploy pipeline live in [sierragridteam/sierra-data](https://github.com/sierragridteam/sierra-data), so references below to `prefab.yaml`, `make relay-token`, `make ecs-deploy` and `deploy-handoff.md` are about that repo.*

## What we're building

SIERRA is setting up several MeshCore gateways at volunteers' houses. Each gateway is a stock MeshCore companion radio plus a bridge program. The bridge dials **out** to this relay over WebSocket. Nothing connects in to the houses, so this works behind CGNAT and needs no router changes.

The relay is the meeting point. Bridges attach to it, and authenticated clients reach a node through it. Later, the relay will also poll repeaters for telemetry. That's out of scope now, but the design leaves room for it.

## Hard requirements

1. **No public path to a node.** Every route to a node needs a valid token. No anonymous endpoint, no debug backdoor, no open TCP port that reaches a node.
2. **Separate process.** Run the relay as its own service, routed at `/mesh/relay/*` by the existing reverse proxy. A relay crash or restart must not affect the hazard and news feeds, and vice versa.
3. **Don't log payloads.** Binary frames can contain private messages and channel secrets (`CHANNEL_INFO` returns the 16-byte key). Log metadata only: sizes, counts, timing. A per-gateway debug flag that logs payloads is fine if it's off by default and logged when turned on.
4. **Store tokens hashed.** Show a token once, when it's created, then keep only a hash (SHA-256 is fine for 32 random bytes).
5. **Remote sessions don't get the node's secrets.** The relay applies the node command policy (PROTOCOL.md, package `nodepolicy`) to every lease, including in-process ones (`Relay.Send`): no private key export or import, no factory reset, no `get wifi.pwd`, no contact given remote CLI, no wadamesh firmware update (`ota`), and private key replies are dropped. A refused frame is answered with `RESP_CODE_DISABLED`, logged by reason, and counted in `frames_refused_total`.

## Scope: phase 1 (build now)

### Bridge endpoint

`wss://data.sierragridteam.org/mesh/relay/<gateway>/dial`. Implement it as the protocol spec describes:

- Authenticate the token against `<gateway>`, then require `hello` within 10 s and send `welcome`.
- Allow one active bridge session per gateway. A new valid session replaces the old one (close the old one with 4009).
- Track per gateway: online/offline, last seen, agent version/os/arch, link kind, node state, node pubkey, the last `stats`.
- **Pubkey pinning:** store the first pubkey reported per gateway, or the one set by an admin. On a mismatch, mark the gateway `mismatch` and refuse client leases until an admin accepts the new key.

### Client endpoint

`wss://data.sierragridteam.org/mesh/relay/<gateway>` with a client token.

- **One lease per gateway at a time.** The companion protocol supports only one app session per node, so this is exclusive. If a lease is already held, reject with close 4029.
- Lease length: 30 min by default, renewable with `renew`. It also expires after 10 min with no frames in either direction.
- When the gateway is offline, accept the upgrade, send `gateway_status` (offline), and close. Don't hang.
- Forward binary frames unchanged in both directions between the lease holder and the bridge, except those the node command policy refuses (hard requirement 5). Send `attach`/`detach` to the bridge.
- Admins can preempt a lease: send the holder `lease_ending` with reason `preempted`, then hand the lease over.
- Audit log: who held which gateway's lease, from when to when, frame counts.

### Roles

A client token carries one of two roles:

| Role | Token | Can |
|---|---|---|
| `admin` | `smc_…` | Everything: the API (creating gateways, issuing and revoking tokens, accepting pubkey changes, status), leases on any gateway, preempting leases |
| `host` | `smh_…` | A lease on **one** gateway (`meshbridge connect`), nothing else: no API, no preempting |

- **Host tokens** are named `<gateway>.<label>` (the relay builds the name), expire (180 days by default, at most 730), and a gateway has at most 5 at once. Each is recorded as an `access_issued` event on its gateway. A host token opens its gateway only if it was issued after the gateway was created, so a reused gateway name never inherits old tokens.
- **Revoked automatically** when the gateway's bridge token is rotated (it may have leaked), when the gateway is re-paired (it may now be someone else's), and when the gateway is deleted. Each records an `access_revoked` event; active leases end.
- **The prefix matches the role.** An `smh_` token is always a host token, and an `smc_` token never is. A relay from before host tokens (v0.14.0 and earlier) accepts only `smc_`, so rolling back fails closed instead of letting host tokens open every gateway.
- **Retired roles.** `operator` and `agent` tokens, issued before v0.15.0, are refused everywhere (403, "tokens with the operator role no longer work"). `PATCH /clients/<name> {"role": "admin"}` turns one into an admin token. In-process lease holders (the relay's repeater checks, sierra-data's pollers) still lease with these roles; they pass no token.
- **Expiry and last use.** An expired token gets 401 "this token has expired", which doesn't count toward the IP lockout. The relay records each token's last use, at most hourly.

### Admin API

JSON over HTTPS under `/mesh/relay/api/`. Requires an `admin` token. A host token is refused (403).

| Method & path | Does |
|---|---|
| `POST /gateways` `{name, expected_pubkey?, notes?}` | Create a gateway. Returns its bridge token **once**. |
| `GET /gateways` | List with status: online, last seen, agent version, link, node state and pubkey, current lease |
| `GET /gateways/<name>` | Detail, including recent connect/disconnect events and last `stats` |
| `GET /gateways/<name>/logs?after=&limit=` | The bridge's recent log lines (`log` messages; newest 1,000 kept) and the relay's events for it. Admins only: they hold LAN addresses and host names. |
| `POST /gateways/<name>/rotate-token` | New bridge token (returned once). Old one stops working, and the live session is closed with 4001. |
| `POST /gateways/<name>/accept-pubkey` | Accept the node's current pubkey after a mismatch (`meshbridge admin accept-node`). A host can also switch their own gateway's node with `POST gateway/node` (`meshbridge change-node`); see PROTOCOL.md. |
| `POST /gateways/<name>/relink` | Send `relink` to the bridge |
| `GET /gateways/<name>/repeaters/nearby` | The repeaters in that gateway's node's contacts, as the bridge route `GET gateway/repeaters/nearby` returns them. For an admin sharing a repeater through any host's gateway. Takes a short lease on the node (client `repeater-list`); recorded as a `repeaters_listed` event on the gateway. |
| `DELETE /gateways/<name>` | Revoke and remove |
| `POST /clients` `{name, role: "admin", expires_in_days?, note?}` | Issue an admin token. Returns the token **once**. |
| `POST /clients` `{role: "host", gateway, label, expires_in_days?, note?}` | Issue a host token for `gateway`, named `<gateway>.<label>`. Returns `{client, role, gateway, expires_at, token}`, the token **once**. 404 for an unknown gateway, 409 at the cap. |
| `PATCH /clients/<name>` `{role: "admin"}` | Turn an `operator` or `agent` token into an admin token. The only role change allowed. |
| `GET /clients` / `DELETE /clients/<name>` | List tokens (role, gateway, expiry, last use, issuer, note; never the hashes); revoke one, ending its leases |
| `GET /leases` | Current and recent leases (audit) |
| `GET /pairings` | Device codes waiting for approval: code, host label, source IP, the node the bridge reported |
| `POST /pairings/<code>/approve` `{gateway?, expected_pubkey?, notes?, repair?}` | Approve a code. Without `gateway`, the gateway is named after the reported node, or else the computer (`SIERRA Elmer` becomes `sierra-elmer`, then `-2`, `-3`… if taken). `GET /pairings` shows that name as `suggested_gateway`. Creates the gateway, or with `repair: true` re-pairs an existing one (rotating its token and closing its session with 4001). The pin is `expected_pubkey` or else the node the bridge reported. The code is case- and hyphen-insensitive. |
| `POST /pairings/<code>/deny` | Refuse a code; the bridge's next poll reads `denied` |
| `POST /invites` `{gateway? \| name_prefix?, max_uses?, expires_in_hours? \| expires_at?, expected_pubkey?, note?}` | Create a passphrase invite. Returns the passphrase and a `sierramesh://` link **once**. `gateway` names it (single use). `name_prefix` names gateways `<prefix>-1`, `-2`… (up to 100 uses). With neither, each gateway is named after its node when redeemed. Expires in 7 days by default, 30 at most. |
| `GET /invites` / `DELETE /invites/<id>` | List invites (never the passphrase) / revoke one. Revoking doesn't touch gateways already created from it. |
| `GET /bridge-update` / `PUT /bridge-update` `{version, gateways?}` | The release bridges should run, offered with `update_available` (see PROTOCOL.md). `gateways` limits it (a canary first); `version: ""` stops offering. `PUT` answers with the gateways it was sent to right away. Config `ReleaseBaseURL` says where releases live. |
| `GET /repeaters` | Shared repeaters (never a password): name, access (`guest`/`admin`), who shared it, last successful read, the gateway it came through, the last problem |
| `POST /repeaters` `{pubkey, gateway, password, name?}` | Share a repeater. The relay logs in to it through `gateway`'s node and reads its status first; the password is stored only if that works. Recorded as a `repeater_check` event on the gateway. |
| `POST /repeaters/stream` | The same, answered as a stream of steps: see `POST gateway/repeaters/stream` in [PROTOCOL.md](../PROTOCOL.md). Each step is recorded as a `repeater_add` event on the gateway. |
| `DELETE /repeaters/<pubkey>` | Stop sharing a repeater and delete its password |

A minimal read-only status page (gateways, online/offline, last seen) is a nice-to-have and is useful during the pilot.

### Pairing (implementation)

- **Bridge endpoints** `POST /api/pair/start` and `POST /api/pair/poll` have no auth: a bridge being paired has no token. They answer in the spec's exact shapes (`{"error":"invalid_passphrase"}` with 403, 429 when rate-limited) rather than the admin API's error envelope.
- **Device codes live in memory.** A relay restart drops pending codes, and the bridge's next poll reads `expired` (`meshbridge setup` offers a new code). Approval mints the gateway's token at once and holds it in memory until the bridge's next poll collects it, exactly once. The store only ever has its hash.
- **A poll needs the `poll_secret`.** Unknown, consumed, expired and wrong-secret polls all read `expired`. Polling faster than half the interval gets 429.
- **Passphrases** are four words from the BIP-39 English list (2,048 words, 44 bits), stored as bcrypt hashes in the `invites` table. Redemption compares against every active invite, which is fine at SIERRA's scale. Every failure (wrong, expired, used up, revoked, or a node that doesn't match `expected_pubkey`) is the same 403.
- **Limits:** `pair/start` 20 per IP per 10 min; passphrase attempts 10 per IP and 200 in total per hour; at most 50 pending codes. Tune with `Config.Pairing`.
- **Notifications:** each redemption or approval adds a `paired` or `repaired` gateway event and logs at warn level with the source IP and host label, so admins can spot one they didn't expect.
- **`Config.PublicBaseURL`** (default `https://data.sierragridteam.org`) builds the `verification_url` and the `relay` URL handed to a paired bridge.
- **Not built yet:** the approval page at `/mesh/pair`, which `verification_url` points to. Admins approve with `meshbridge admin` (below) or the API.
- **`meshbridge admin`** wraps these endpoints for admins: `pairings`, `approve <code> [gateway]`, `deny`, `invite [gateway]` (which prints a message to send the host, with the passphrase and the install one-liner), `invites`, `revoke`, `gateways` and `remove <gateway>` (deletes it: revokes its token and disconnects its bridge; there's no rename, so remove and re-invite instead). `update-bridges [vX.Y.Z|off] [-gateway a,b]` sets the bridge release; with no argument it only shows the current offer. `logs <gateway> [-n 50] [-f]` prints a bridge's recent log lines merged with the relay's connects and disconnects. It also covers shared repeaters: `repeaters`, `repeater add <pubkey> <gateway>`, `repeater remove`. It reads the admin token from `$MESHBRIDGE_ADMIN_TOKEN` or `~/.config/meshbridge/admin.token`.

### Shared repeaters (implementation)

A host shares a repeater with SIERRA so the relay can log in to it over the mesh and read its status (battery, airtime, packet counts, temperature). Hosts use `meshbridge repeater add|list|remove`, which calls the bridge self-service routes in [PROTOCOL.md](../PROTOCOL.md#bridge-self-service-http) with the bridge's own token. Admins use `meshbridge admin repeaters` and `admin repeater add|remove`. `admin repeater add <gateway>` lists what any host's gateway has heard and shares one through it; the host's gateway events record both uses of its node, and the repeater is the admin's, not in the host's `meshbridge repeater list`.

- **Checked before stored.** Sharing takes a lease on the gateway's node at normal priority (so it waits for nobody and yields to nobody), logs in, and reads the status. A wrong password, a repeater that doesn't answer, or a busy or paused node is reported and nothing is stored.
- **Host's node left as it was.** If the node doesn't have the repeater as a contact, it's added for the check and removed after. The node's clock is set if it's more than 5 minutes off, as every companion app does, because a repeater refuses a login whose timestamp isn't newer than the last one from that node.
- **Stale routes are reset.** A node keeps the direct route it last learned to a contact, and a route that stopped working (the repeater can't hear this node, or a hop moved) is never answered. If a login or status request over a stored route gets no reply, the check resets that route once (`CMD_RESET_PATH`, the apps' "Reset path") and floods; the node learns a working route from the reply. `RepeaterCheck.PathReset` says it happened.
- **Passwords** are sealed with AES-256-GCM under `Config.SecretKey` (sierra-data: `PF__MESH_RELAY__SECRET_KEY`, 32 bytes base64), with the repeater's pubkey as additional data. No API returns them; only `Relay.RepeaterCredentials`, for the in-process poller, opens them. Without the key, sharing answers 503.
- **Ownership.** The gateway that shared a repeater, or an admin, can remove it. Another gateway can't overwrite or remove it. Removing deletes the password.
- **Hosts are warned** that any SIERRA gateway in range may be used to log in, and are asked for the guest password first (a guest can read status; MeshCore repeaters answer `GET_STATUS` to guests).
- **Attempts and links.** Every check is recorded (`repeater_attempts`, kept 7 days): gateway, client, outcome (`ok`, `no_reply`, `refused`, or one that sent nothing: `not_known`, `unavailable`, `error`), the step it stopped at, whether the login flooded, and how long it took. The repeater listings (`GET /repeaters`, and a host's own) add `links`, each gateway's last 24 hours (tries over the mesh, successes, floods, median time of a success), which `meshbridge admin repeaters` and `meshbridge repeater list` print. `CheckRequest.FloodFirst` resets a stored route before logging in, for a link whose route just went unanswered.
- **Poll state.** The poller saves each repeater's schedule through `Relay.SetRepeaterPollState` (`repeater_poll_state`): its primary gateway, next cycle, consecutive failed cycles, whether it's hibernating (polled daily) or stopped on a refused password, whether it's waiting (cycles ended with no gateway able to try, and why), and its last reading (kept from the listings). `RepeaterCredentials` returns it, so a restart resumes the schedule, and the listings show it as `poll`. The policy itself lives in the poller (sierra-data `internal/relaytelemetry`). Removing a repeater deletes its attempts and state.
- **Adverts for islands.** A check also exports the repeater's last signed advert from the gateway's node (`CMD_EXPORT_CONTACT`, `RepeaterCheck.Advert`). sierra-data's poller forwards it to the Grid's `mesh.packet` stream, which verifies the signature, so a repeater whose adverts never reach the wider mesh can still be established.
- **Package `meshops`** holds the companion operations (contacts, login, status, telemetry, clock), usable over any transport; `meshops/meshsim` is a fake node with repeaters for tests. `Relay.Use` runs them under a lease, and `Relay.CheckRepeater` is the whole check.

### Pause (implementation)

`node_status` with `state: "paused"` ends any lease at once with `lease_ending` reason `host_paused` (`in_s: 0`), and new clients get `gateway_status` (carrying `until`) then `error` `node_paused`. The public status reports `node_state: "paused"` and `ready: false`. Leases are available again as soon as the bridge reports `connected`.

### Operational details

- **Proxy:** WebSocket upgrade must work through the existing reverse proxy. Idle timeout has to be well over 25 s (the ping interval). If the site is behind Cloudflare, WebSockets are supported there, with a ~100 s idle limit that the pings keep us under.
- **Limits:** Max binary message 512 bytes. Per-connection rate limit, for example 50 frames/s. Lock out repeated auth failures by source IP.
- **Restarts:** Bridges reconnect on their own with backoff. Leases don't survive a relay restart; clients reconnect.
- **Metrics:** gateways online, sessions, leases, frames/s, auth failures.

## Phase 2 (design for it, don't build yet): remote access from any MeshCore client

Goal: someone with the official MeshCore app or meshcore-cli can use a remote gateway as if it were their own companion radio. Those clients speak raw TCP to port 5000 with **no authentication**, so we can't just open a port.

Two options. Leave room for both in the design.

1. **Local shim (works with phase 1 as-is).** The bridge program has a client mode, `meshbridge connect <gateway>`. It listens on `127.0.0.1:5000` on the user's machine and tunnels to the client endpoint with their token. meshcore-cli and desktop apps connect to localhost. No relay changes needed.
2. **Pinned TCP lease (for phones).** An authenticated user requests a session through the API or the status page. The relay opens a TCP port from a small pool, for example 40000–40099. It accepts connections **only from the requester's current public IP**, allows one connection, closes after 10 min idle or 2 h max, and is fully audited. Before committing to this, check:
   - Can the data server expose a raw TCP port range? (Cloudflare's normal proxy doesn't carry raw TCP. It needs direct origin exposure or Cloudflare Spectrum.)
   - Does the official MeshCore app accept an arbitrary `host:port` for a TCP companion?

   Please report back on both questions.

## Phase 3 (later): polling and telemetry

The relay will run its own internal client that takes short leases to poll repeaters for status, telemetry, and neighbours, and stores the results. Build the lease manager so an internal client can hold a lease just like an external one, with a priority flag so polling can wait for a human session. Nothing else is needed for this now. (Built: an in-process holder implements `LeaseHolder`, takes the lease with `Acquire` at `PriorityBackground`, and sends frames with `Relay.Send`. sierra-data's `cmd/mesh-relay` runs the first such poller.)

## Acceptance tests for phase 1

1. A bridge with a valid token connects, sends `hello`, and gets `welcome`. The gateway shows online in `GET /gateways` within 2 s.
2. Bad token → 401. A valid token for a different gateway → 401, or 4002 after upgrade.
3. A second bridge with the same token takes over, and the first gets 4009.
4. A client with an admin token gets a lease and exchanges binary frames end to end with a fake node behind a test bridge. A second client gets 4029.
5. Lease idle timeout and admin preemption both send `lease_ending` and close cleanly.
6. A pubkey change marks the gateway `mismatch` and blocks leases until `accept-pubkey`, unless the host announced it first (`POST gateway/node`).
7. Kill the relay process: the data server's other endpoints are unaffected. After restart, the bridge is back online within its backoff window.
8. No payload bytes appear in logs at the default log level.

A fake bridge and fake node for testing will exist in the bridge repo (`meshbridge`). Until then, a small test client that speaks the protocol is enough.

## Open questions for you

- What stack does the data server use, and does the relay fit as a sibling service there?
- Where should gateway, token, and lease state live: the existing database or the relay's own store?
- The phase 2 feasibility questions above.

---

## Implementation (data server, 2026-11-04)

Phase 1 is built: `cmd/mesh-relay` (binary) and `internal/meshrelay` (package). The acceptance tests are `internal/meshrelay/relay_test.go`. They run a real HTTP server with a fake bridge and a fake echo node, and they cover tests 1–6 and 8, plus token rotation, deletion, revocation, frame limits, auth lockout, and restart.

### Answers to the open questions

- **Stack.** Go on prefab, like the Grid, but as a separate binary, image target (`docker build --target relay`) and process. `cmd/server` doesn't import the relay package, so a crash in one process can't take down the other. Prefab owns the relay's HTTP server, config, logging, errors and lifecycle: the relay is a prefab plugin, and the admin API runs on prefab JSON handlers. Prefab has no websocket support, so the two websocket endpoints are plain handlers on prefab's mux, built on gorilla/websocket. Prefab's gzip wrapper passes the connection hijack through, and the tests dial with `Accept-Encoding: gzip` to cover that.
- **State.** The relay has its own SQLite file (`meshRelay.dbPath`; `PF__MESH_RELAY__DB_PATH=/data/relay.db` in the image). It never writes to `grid.db`. The file holds gateways (bridge token hash, pinned or mismatched pubkey, last hello and stats, last seen), gateway events, client token hashes, and the lease audit log. Columns added to a table after its first release are added on open (`addedColumns` in `relay/store.go`), with defaults, so an older relay still reads and writes the file. It uses the TRUNCATE journal mode and takes an exclusive flock, the same as the grid store. During a rolling deploy, a second instance waits up to 90 s for the first to exit. A coordinated deploy can instead hand `relay.db` over explicitly: start the replacement with `--standby` and quiesce the old relay on its control port (8091) before activating the new one. See [deploy-handoff.md](deploy-handoff.md). A hosted relay can keep its state in DynamoDB instead (`relay/dynamostore`), with an instance lease in place of the flock; see [dynamodb.md](dynamodb.md).
- **Phase 2 feasibility:**
  - **Raw TCP port range.** An ALB carries only HTTP(S). An NLB listener covers one port, an NLB allows at most 50 listeners, and an ECS service can attach at most 5 target groups. Neither limit can be raised, so an NLB can't reach 40000–40099 on one service. Cloudflare's normal proxy doesn't carry raw TCP. Spectrum does, including port ranges, but only on the Enterprise plan as a paid add-on. The workable option is a Fargate task with a public IP and a security group that opens the range. Fargate can't hold an Elastic IP, so the task needs a DNS-only (grey-cloud) record that it updates on every start.
  - **The official app.** It has a Wi-Fi/TCP connection type, and guides tell users to type in the radio's IP. No source confirms that it accepts a hostname or a port other than 5000, or says which platforms support it. Someone needs to test this on iOS and Android before we commit to option 2. meshcore-cli does accept any host and port (`-t host -p port`), so option 1 (the local shim) works for meshcore-cli now.
  - **Recommendation.** Ship option 1 first. Treat option 2 as blocked until the app test is done.

### Decisions the spec left open

- **Admin bootstrap.** Bootstrap admins are listed in `meshRelay.admins` in `prefab.yaml` as a name plus the token's SHA-256. They exist so there's an admin before the clients table has any rows, and the API can't revoke them. Mint one with `make relay-token NAME=<you>`. It works the same way as push-ingest reporters: the token is printed once, only its hash is committed, and a malformed entry stops the relay at startup.
- **Configuration.** The rest of the config lives in `prefab.yaml` under `meshRelay:` too: port, db path, client IP header, lease, ping, rate and lockout timings, and the payload-debug list. Override any key with `PF__MESH_RELAY__*`. A misspelled override stops the relay at startup with a "did you mean" message.
- **API status codes.** The admin API is served through prefab JSON handlers, so a successful call returns 200, including creates and deletes. Errors use the `/api/v1` envelope `{code, codeName, message}` and map to 401, 403, 404, 409 or 429.
- **Roles on the API.** Every route needs `admin`, reads included (`GET /gateways`, `/gateways/<name>`, `/leases`, `/metrics`). Host tokens only take leases.
- **Leases need a verified node.** A lease is granted only when the node is `connected` and reports the pinned key. The first key is pinned when the relay first sees it, or at create time with `expected_pubkey`. A mismatch reported mid-lease ends the lease. If the node goes back to the pinned key, the mismatch clears on its own.
- **A bridge drop ends the lease** (`gateway_offline`). When a session is replaced (4009), the lease moves to the new session instead.
- **Preemption.** An admin connects with `?preempt=1`. The lease manager also has a `Priority` field, so the phase 3 poller (`PriorityBackground`) gives way to any human or agent session.
- **The rate limit throttles.** See the wire spec. Disconnecting would cut off a healthy bridge whenever its node dumps a contact list.
- **Auth lockout.** 10 failures from one IP within 10 min lock that IP out for 15 min (HTTP 429) on every relay route. Behind the proxy, set `meshRelay.clientIpHeader` to `X-Forwarded-For`. The relay image sets this by default. The relay uses the rightmost entry, which is the one the proxy appended. Without this setting, every client looks like the proxy and one bad bridge would lock everyone out.
- **Payload debug.** `meshRelay.debugPayloadGateways` logs frames as hex at debug level for the gateways listed. It's off by default, and the relay logs a warning at startup for each gateway listed.
- **Public status.** `GET /mesh/relay/api/status` needs no token and feeds the public site's `/relay` page. For each gateway it reports name, whether it's online, node state, whether a session could start now, whether one is in use and by what kind of client (the holder's role, never its name; a host's lease shows as `operator`, so the page never says the host is on their own node), bridge version and link kind, and last seen. It never includes pubkeys, node names, notes, lease holder names, the bridge's OS, or link detail. Gateway names are public through it, so choose names that don't pinpoint a house.
- **Metrics.** `GET /mesh/relay/api/metrics` returns JSON counters, and the relay logs a `relay metrics` line every minute with per-minute rates.

### Deployment requirements

These apply wherever the relay runs.

- **Run it as its own process,** separate from the Grid server. If both run in one orchestration unit (one pod or task), make sure a relay failure can't stop or restart the Grid. In particular, don't let a failing relay health check mark the shared unit unhealthy.
- **Run exactly one instance.** Bridge sessions and leases live in memory, so two relays would split bridges from their clients.
- **Give it persistent storage for `relay.db`** (`PF__MESH_RELAY__DB_PATH`). This must be its own file, never `grid.db`.
- **Route `/mesh/relay` and `/mesh/relay/*` to the relay** at the reverse proxy, without stripping the prefix. The proxy must support WebSocket upgrades and have an idle timeout well over the 25 s ping interval.
- **Set `meshRelay.clientIpHeader`** to the header your proxy uses to pass the client IP. `X-Forwarded-For` (rightmost entry) is the image default.
- **Health endpoint:** `/mesh/relay/healthz`.
- **Set `PF__MESH_RELAY__SECRET_KEY`** (32 random bytes, base64: `openssl rand -base64 32`) to turn on shared repeaters. Keep it stable: a new key makes every stored repeater password unreadable, and they must be shared again.
- **Commit at least one admin hash** to `meshRelay.admins` (`make relay-token NAME=<you>`) before the first deploy. Then create gateways with `POST /mesh/relay/api/gateways` and hand each bridge token to its host.

`make ecs-deploy` always builds and pushes both images, then waits for the AWS
coordinator to hand database ownership and traffic to the replacement task
(see the README). AWS credentials and this repository are sufficient.
