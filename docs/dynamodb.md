# The relay's DynamoDB store

`relay/dynamostore` is a `relay.Storage` on one DynamoDB table, for a hosted relay
that should own no filesystem. The SQLite store (`relay.OpenStore`) stays the
default for local and self-hosted relays and for `make test`. Both pass the same
conformance suite (`relay/storetest`), and `make test-dynamo` also runs every
local e2e test on the DynamoDB store.

The relay is still a single process (live sessions and leases are in memory).
`dynamostore.Open` takes an instance lease in the table in place of the SQLite
store's flock, and a deploy must stop the old relay before the new one can open
the table. On ECS, that means a minimum healthy percent of 0 and a maximum of
100.

## Table

| Setting | Value |
|---|---|
| Keys | `pk` (string, partition), `sk` (string, sort) |
| Capacity | On-demand |
| TTL | Attribute `expires` (epoch seconds) |
| Indexes | None |
| Production extras | Point-in-time recovery and deletion protection, both set in Terraform |

`dynamostore.CreateTable` creates the same shape for development, without
point-in-time recovery or deletion protection.

## Items

| `pk` | `sk` | Holds |
|---|---|---|
| `GW` | gateway name | the gateway row (token hash, pins, notes, last hello/stats/seen) |
| `CL` | client name | the client row (role, token hash, created; for a scoped token also `gateway`, `expiresAt`, `lastUsedAt`, `createdBy`, `note`). Deleting a gateway deletes the clients scoped to it, with their `TOK` pointers. |
| `TOK` | `B#<hash>` / `C#<hash>` | token pointer to a gateway or client name |
| `EV#<gateway>` | ID, 20 digits | gateway event |
| `LOG#<gateway>` | ID, 20 digits | bridge log line |
| `SEQ` | `EV#<gateway>` / `LOG#<gateway>` | counter for those IDs |
| `LEASE` | `<started ms>#<lease id>` | lease audit row |
| `LEASEOPEN` | lease ID | marker for an open lease |
| `INV` | invite ID | passphrase invite |
| `REP` | repeater pubkey | shared repeater with its sealed password (binary) |
| `RPS` | repeater pubkey | the poller's saved schedule (JSON) |
| `RA#<pubkey>` | `<ms>#<seq>` | repeater attempt (TTL) |
| `SET` | key | setting (JSON) |
| `LOCK` | `relay` | the instance lease: owner, heartbeat, nonce |

Each partition's sort order matches what the relay asks for, so listings are
single queries. Every read the relay depends on is a strongly consistent
`GetItem` or `Query`.

- **Tokens.** Token lookups go through the `TOK` pointer items rather than a
  secondary index. A pointer is written in the same transaction as its gateway
  or client, so token hashes stay unique, and a new or rotated token works on
  the next request. A GSI can't guarantee either.
- **Caps.** Events and bridge logs take their IDs from per-gateway atomic
  counters. Because the IDs are contiguous, the 500-event and 1000-line caps
  stay exact: each write deletes the ID that falls off, with no read.
  The log IDs are the API's `after` cursor.
- **TTL.** Repeater attempts expire by TTL a day after the week they are kept,
  instead of the SQLite store's hourly prune.
- **Open leases.** `CloseOrphanLeases` reads the `LEASEOPEN` markers, never the
  whole lease log.
- **Invites.** `UseInvite` is one conditional update, so two redemptions can't
  both take the last use.

## Instance lease

The `LOCK` item names the owner and holds a heartbeat (the owner's clock in
milliseconds) and a nonce. The owner renews it every 10 s, and each renewal is
a write conditioned on still being the owner. `Close` deletes the item, so a
clean handover is immediate.

A waiter takes the lease in either of two cases:
- the item hasn't changed for `StaleAfter` (45 s) by the waiter's own clock;
- its heartbeat is more than twice that behind the waiter's clock (an owner
  that died long ago).

The takeover write is conditioned on the exact owner and nonce the waiter saw,
so two waiters can't both win. If the owner loses the lease (it was taken over,
or renewals failed for `StaleAfter`), every store call returns `ErrLeaseLost`
and `Options.OnLost` fires; the process should exit. `Open` waits up to
`LockTimeout` and logs once while it waits, the same as the flock.

## Moving a relay.db

```sh
PF__MESH_RELAY__SECRET_KEY=... relaydynamo import -db relay.db -table <table>
```

Run it on a copy of `relay.db` taken while the relay was stopped. It copies
every gateway, client, invite, shared repeater (the sealed password byte for
byte), poll schedule and setting, plus:
- events and bridge logs, as capped;
- repeater attempts from the last week;
- lease audit rows from the last `-lease-days` (default 90), plus any lease
  still open, which the next relay closes at startup.

It refuses a table that already has relay data, and it writes under the
instance lease, so it can't run beside a relay. It then verifies:
- the counts;
- every token resolves to its gateway or client;
- every sealed password is identical and opens with
  `PF__MESH_RELAY__SECRET_KEY`.

The relay on the new table must use the same key, byte for byte, or shared
repeaters can't be polled. `relaydynamo verify` repeats the check.

## Tests

```sh
make test-dynamo   # starts amazon/dynamodb-local in Docker (make dynamo-down stops it)
```

The tests need `MESHRELAY_TEST_DYNAMO_ENDPOINT` (the Makefile sets it) and skip
without it, so `make test` stays offline.
