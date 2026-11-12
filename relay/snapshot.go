package relay

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"database/sql"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"os"
	"time"
)

// Snapshot is a relay.db's contents, for copying to another store (the
// one-off SQLite -> DynamoDB migration). It holds token hashes and sealed
// repeater passwords: never log or serialize it.
type Snapshot struct {
	Gateways []*GatewayRecord
	Clients  []SnapshotClient
	Invites  []*InviteRecord
	// Repeaters carry their sealed passwords (RepeaterRecord.Sealed).
	Repeaters []*RepeaterRecord
	// PollStates are the poller's saved schedules, as stored (JSON).
	PollStates map[string]json.RawMessage
	// Attempts are the repeater attempts since SnapshotOptions.AttemptsSince.
	Attempts []RepeaterAttempt
	// Events are each gateway's events, oldest first (at most
	// MaxEventsPerGateway, as the SQLite store keeps).
	Events map[string][]GatewayEvent
	// BridgeLogs are each gateway's stored lines, oldest first, with their
	// IDs; MaxBridgeLogID is the highest ID ever used in the file (so a copy
	// can keep new IDs past any cursor an admin holds).
	BridgeLogs     map[string][]BridgeLog
	MaxBridgeLogID int64
	// Leases are lease audit rows started since SnapshotOptions.LeasesSince,
	// plus every open one (the next relay closes those at startup).
	Leases []LeaseRecord
	// Settings are the settings rows, as stored (JSON).
	Settings map[string]json.RawMessage
	// Dropped counts rows left out by the options, by table.
	Dropped map[string]int64
}

// SnapshotClient is a client row with its token hash.
type SnapshotClient struct {
	ClientRecord
	TokenHash string
}

// SnapshotOptions trims history. Zero times keep everything.
type SnapshotOptions struct {
	LeasesSince   time.Time
	AttemptsSince time.Time
}

// ReadSnapshot reads a relay.db read-only, without taking its lock or
// migrating its schema: point it at a copy (a snapshot taken while the
// relay was stopped), not at a live relay's file.
func ReadSnapshot(ctx context.Context, path string, o SnapshotOptions) (*Snapshot, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?mode=ro&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("relay snapshot: open %s: %w", path, err)
	}
	ro := &Store{db: db} // read helpers only; never written through
	snap := &Snapshot{PollStates: map[string]json.RawMessage{}, Events: map[string][]GatewayEvent{},
		BridgeLogs: map[string][]BridgeLog{}, Settings: map[string]json.RawMessage{}, Dropped: map[string]int64{}}

	if snap.Gateways, err = ro.Gateways(); err != nil {
		return nil, fmt.Errorf("gateways: %w", err)
	}
	if err := eachRow(ctx, db, `SELECT name, role, token_sha256, created_at FROM clients ORDER BY name`, func(rows *sql.Rows) error {
		var c SnapshotClient
		var role string
		var created int64
		if err := rows.Scan(&c.Name, &role, &c.TokenHash, &created); err != nil {
			return err
		}
		c.Role, c.CreatedAt = Role(role), fromMS(created)
		snap.Clients = append(snap.Clients, c)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("clients: %w", err)
	}
	if snap.Invites, err = ro.Invites(); err != nil {
		return nil, fmt.Errorf("invites: %w", err)
	}
	if snap.Repeaters, err = ro.Repeaters(); err != nil {
		return nil, fmt.Errorf("repeaters: %w", err)
	}
	if err := eachRow(ctx, db, `SELECT pubkey, state FROM repeater_poll_state`, func(rows *sql.Rows) error {
		var pk, state string
		if err := rows.Scan(&pk, &state); err != nil {
			return err
		}
		snap.PollStates[pk] = json.RawMessage(state)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("repeater poll state: %w", err)
	}
	if err := eachRow(ctx, db, `SELECT pubkey, at, gateway, client, outcome, step, flooded, path_reset, duration_ms
		FROM repeater_attempts ORDER BY pubkey, at`, func(rows *sql.Rows) error {
		var a RepeaterAttempt
		var at, dur int64
		if err := rows.Scan(&a.Pubkey, &at, &a.Gateway, &a.Client, &a.Outcome, &a.Step, &a.Flooded, &a.PathReset, &dur); err != nil {
			return err
		}
		a.At, a.Duration = fromMS(at), time.Duration(dur)*time.Millisecond
		if a.At.Before(o.AttemptsSince) {
			snap.Dropped["repeater_attempts"]++
			return nil
		}
		snap.Attempts = append(snap.Attempts, a)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("repeater attempts: %w", err)
	}
	if err := eachRow(ctx, db, `SELECT gateway, at, kind, detail FROM gateway_events ORDER BY gateway, id`, func(rows *sql.Rows) error {
		var gw string
		var e GatewayEvent
		var at int64
		if err := rows.Scan(&gw, &at, &e.Kind, &e.Detail); err != nil {
			return err
		}
		e.At = fromMS(at)
		snap.Events[gw] = append(snap.Events[gw], e)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("gateway events: %w", err)
	}
	for gw, es := range snap.Events {
		if n := len(es) - maxEventsPerGateway; n > 0 {
			snap.Events[gw] = es[n:]
			snap.Dropped["gateway_events"] += int64(n)
		}
	}
	if err := eachRow(ctx, db, `SELECT id, gateway, received_at, t, level, msg, attrs FROM bridge_logs ORDER BY gateway, id`,
		func(rows *sql.Rows) error {
			var gw, attrs string
			var e BridgeLog
			var at int64
			if err := rows.Scan(&e.ID, &gw, &at, &e.T, &e.Level, &e.Msg, &attrs); err != nil {
				return err
			}
			e.ReceivedAt = fromMS(at)
			if attrs != "" {
				_ = json.Unmarshal([]byte(attrs), &e.Attrs)
			}
			snap.BridgeLogs[gw] = append(snap.BridgeLogs[gw], e)
			return nil
		}); err != nil {
		return nil, fmt.Errorf("bridge logs: %w", err)
	}
	// AUTOINCREMENT keeps the high-water mark in sqlite_sequence, past rows
	// the cap has deleted.
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM sqlite_sequence WHERE name = 'bridge_logs'`).
		Scan(&snap.MaxBridgeLogID); err != nil {
		return nil, fmt.Errorf("bridge log sequence: %w", err)
	}
	if err := eachRow(ctx, db, `SELECT id, gateway, client, role, started_at, ended_at, end_reason,
		frames_to_node, frames_from_node, bytes_to_node, bytes_from_node FROM leases ORDER BY started_at`,
		func(rows *sql.Rows) error {
			var l LeaseRecord
			var role string
			var started int64
			var ended sql.NullInt64
			if err := rows.Scan(&l.ID, &l.Gateway, &l.Client, &role, &started, &ended, &l.EndReason,
				&l.FramesToNode, &l.FramesFromNode, &l.BytesToNode, &l.BytesFromNode); err != nil {
				return err
			}
			l.Role, l.StartedAt = Role(role), fromMS(started)
			if ended.Valid {
				t := fromMS(ended.Int64)
				l.EndedAt = &t
				if l.StartedAt.Before(o.LeasesSince) {
					snap.Dropped["leases"]++
					return nil
				}
			}
			snap.Leases = append(snap.Leases, l)
			return nil
		}); err != nil {
		return nil, fmt.Errorf("leases: %w", err)
	}
	if err := eachRow(ctx, db, `SELECT key, value FROM settings`, func(rows *sql.Rows) error {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return err
		}
		snap.Settings[k] = json.RawMessage(v)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("settings: %w", err)
	}
	return snap, nil
}

func eachRow(ctx context.Context, db *sql.DB, q string, fn func(*sql.Rows) error) error {
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// CheckSealed reports whether a repeater's sealed password opens with key
// (the relay's Config.SecretKey), without returning the password. A
// migration uses it to prove the new relay's key is byte-identical.
func CheckSealed(key []byte, pubkey string, sealed []byte) error {
	if len(key) != 32 {
		return fmt.Errorf("the secret key must be 32 bytes, not %d", len(key))
	}
	b, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	a, err := cipher.NewGCM(b)
	if err != nil {
		return err
	}
	if len(sealed) < a.NonceSize() {
		return stderrors.New("sealed password is too short")
	}
	if _, err := a.Open(nil, sealed[:a.NonceSize()], sealed[a.NonceSize():], []byte("repeater:"+pubkey)); err != nil {
		return stderrors.New("can't open the sealed password (wrong secret key?)")
	}
	return nil
}
