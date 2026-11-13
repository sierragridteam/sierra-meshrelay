package relay

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dpup/prefab/logging"
	_ "modernc.org/sqlite" // pure-Go driver: keeps CGO_ENABLED=0 builds working
)

// Store is the relay's own SQLite database: gateways, client tokens, the lease
// audit log and gateway connect/disconnect events. It is deliberately NOT the
// Grid's grid.db — a separate file keeps the two processes' failure domains
// (and their single-writer locks) independent.
//
// Volume is tiny (a handful of gateways and clients, a row per lease), so
// unlike internal/store there is no pool tuning here.
type Store struct {
	db   *sql.DB
	lock *os.File

	attemptMu     sync.Mutex
	attemptPruned time.Time // last prune of repeater_attempts
}

// ErrNotFound is returned when a gateway or client does not exist.
var ErrNotFound = errors.New("not found")

// ErrExists is returned when creating a gateway or client whose name is taken.
var ErrExists = errors.New("already exists")

const schema = `
CREATE TABLE IF NOT EXISTS gateways (
	name                TEXT PRIMARY KEY,
	bridge_token_sha256 TEXT NOT NULL UNIQUE,
	pinned_pubkey       TEXT NOT NULL DEFAULT '',
	mismatch_pubkey     TEXT NOT NULL DEFAULT '',
	notes               TEXT NOT NULL DEFAULT '',
	created_at          INTEGER NOT NULL,
	last_seen           INTEGER NOT NULL DEFAULT 0,
	last_hello          TEXT NOT NULL DEFAULT '',
	last_stats          TEXT NOT NULL DEFAULT '',
	last_stats_at       INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS gateway_events (
	id      INTEGER PRIMARY KEY AUTOINCREMENT,
	gateway TEXT NOT NULL,
	at      INTEGER NOT NULL,
	kind    TEXT NOT NULL,
	detail  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS gateway_events_by_gw ON gateway_events (gateway, at);
CREATE TABLE IF NOT EXISTS clients (
	name         TEXT PRIMARY KEY,
	role         TEXT NOT NULL,
	token_sha256 TEXT NOT NULL UNIQUE,
	created_at   INTEGER NOT NULL,
	gateway      TEXT NOT NULL DEFAULT '',
	expires_at   INTEGER NOT NULL DEFAULT 0,
	last_used_at INTEGER NOT NULL DEFAULT 0,
	created_by   TEXT NOT NULL DEFAULT '',
	note         TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS leases (
	id               TEXT PRIMARY KEY,
	gateway          TEXT NOT NULL,
	client           TEXT NOT NULL,
	role             TEXT NOT NULL,
	started_at       INTEGER NOT NULL,
	ended_at         INTEGER,
	end_reason       TEXT NOT NULL DEFAULT '',
	frames_to_node   INTEGER NOT NULL DEFAULT 0,
	frames_from_node INTEGER NOT NULL DEFAULT 0,
	bytes_to_node    INTEGER NOT NULL DEFAULT 0,
	bytes_from_node  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS leases_by_start ON leases (started_at);
CREATE TABLE IF NOT EXISTS invites (
	id              TEXT PRIMARY KEY,
	passphrase_hash TEXT NOT NULL,
	gateway         TEXT NOT NULL DEFAULT '',
	name_prefix     TEXT NOT NULL DEFAULT '',
	max_uses        INTEGER NOT NULL,
	uses            INTEGER NOT NULL DEFAULT 0,
	expires_at      INTEGER NOT NULL,
	expected_pubkey TEXT NOT NULL DEFAULT '',
	note            TEXT NOT NULL DEFAULT '',
	revoked         INTEGER NOT NULL DEFAULT 0,
	created_by      TEXT NOT NULL DEFAULT '',
	created_at      INTEGER NOT NULL
);
`

// OpenStore opens (creating if needed) the relay database at path. The journal
// mode is TRUNCATE: production keeps this file on EFS, where WAL's memory-mapped
// -shm is unsafe (see internal/store/CLAUDE.md for the long version).
//
// Like the grid store it takes an exclusive flock on <path>.lock, waiting up to
// lockTimeout, so a rolling deploy's new task waits for the old one to exit
// instead of two writers corrupting the file.
func OpenStore(ctx context.Context, path string, lockTimeout time.Duration) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("relay store: creating db dir: %w", err)
		}
	}
	lock, err := acquireLock(ctx, path+".lock", lockTimeout)
	if err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(TRUNCATE)&_pragma=synchronous(FULL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		lock.Close()
		return nil, fmt.Errorf("relay store: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	if _, err := db.Exec(schema + repeaterSchema + repeaterPollSchema + bridgeLogSchema + settingsSchema); err != nil {
		db.Close()
		lock.Close()
		return nil, fmt.Errorf("relay store: migrate: %w", err)
	}
	for _, m := range addedColumns {
		if err := addColumns(ctx, db, m.table, m.cols); err != nil {
			db.Close()
			lock.Close()
			return nil, fmt.Errorf("relay store: migrate %s: %w", m.table, err)
		}
	}
	return &Store{db: db, lock: lock}, nil
}

// column is a column added to a table after it was first created.
type column struct{ name, def string }

// addedColumns are the columns added to existing tables since the first
// release, oldest first. CREATE TABLE IF NOT EXISTS leaves an existing
// table as it was, so OpenStore adds whichever are missing. Each needs a
// DEFAULT, and the CREATE TABLE above lists it too, for new files. An older
// relay ignores them: every INSERT names its columns.
var addedColumns = []struct {
	table string
	cols  []column
}{
	{"clients", []column{
		{"gateway", "TEXT NOT NULL DEFAULT ''"},
		{"expires_at", "INTEGER NOT NULL DEFAULT 0"},
		{"last_used_at", "INTEGER NOT NULL DEFAULT 0"},
		{"created_by", "TEXT NOT NULL DEFAULT ''"},
		{"note", "TEXT NOT NULL DEFAULT ''"},
	}},
}

// addColumns adds the columns table lacks.
func addColumns(ctx context.Context, db *sql.DB, table string, cols []column) error {
	have, err := tableColumns(ctx, db, table)
	if err != nil {
		return err
	}
	for _, c := range cols {
		if have[c.name] {
			continue
		}
		if _, err := db.ExecContext(ctx, `ALTER TABLE `+table+` ADD COLUMN `+c.name+` `+c.def); err != nil {
			return fmt.Errorf("adding %s: %w", c.name, err)
		}
	}
	return nil
}

// tableColumns returns the names of table's columns.
func tableColumns(ctx context.Context, db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		have[n] = true
	}
	return have, rows.Err()
}

// Close closes the database and releases the lock.
func (s *Store) Close() error {
	err := s.db.Close()
	if s.lock != nil {
		s.lock.Close()
	}
	return err
}

func acquireLock(ctx context.Context, path string, timeout time.Duration) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("relay store: opening lock file %s: %w", path, err)
	}
	deadline := time.Now().Add(timeout)
	announced := false
	for {
		held, lockErr := tryLock(f)
		if lockErr == nil {
			return f, nil
		}
		if !held || !time.Now().Before(deadline) {
			f.Close()
			return nil, fmt.Errorf("relay store: %s still locked by another process after %s "+
				"(on a deploy, the previous relay task has not exited): %w", path, timeout, lockErr)
		}
		if !announced {
			logging.Warnw(ctx, "relay database is locked by another process; waiting for it to exit",
				"lock", path, "timeout", timeout.String())
			announced = true
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromMS(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.UnixMilli(v).UTC()
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// GatewayRecord is a gateway's persisted row.
type GatewayRecord struct {
	Name           string
	TokenHash      string
	PinnedPubkey   string
	MismatchPubkey string
	Notes          string
	CreatedAt      time.Time
	LastSeen       time.Time
	LastHello      *helloMsg
	LastStats      *Stats
	LastStatsAt    time.Time
}

const gatewayCols = `name, bridge_token_sha256, pinned_pubkey, mismatch_pubkey, notes, created_at,
	last_seen, last_hello, last_stats, last_stats_at`

func scanGateway(row interface{ Scan(...any) error }) (*GatewayRecord, error) {
	var g GatewayRecord
	var created, seen, statsAt int64
	var hello, stats string
	if err := row.Scan(&g.Name, &g.TokenHash, &g.PinnedPubkey, &g.MismatchPubkey, &g.Notes,
		&created, &seen, &hello, &stats, &statsAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	g.CreatedAt, g.LastSeen, g.LastStatsAt = fromMS(created), fromMS(seen), fromMS(statsAt)
	if hello != "" {
		var h helloMsg
		if json.Unmarshal([]byte(hello), &h) == nil {
			g.LastHello = &h
		}
	}
	if stats != "" {
		var st Stats
		if json.Unmarshal([]byte(stats), &st) == nil {
			g.LastStats = &st
		}
	}
	return &g, nil
}

// CreateGateway inserts a gateway with its bridge token hash.
func (s *Store) CreateGateway(name, tokenHash, pinnedPubkey, notes string, now time.Time) error {
	_, err := s.db.Exec(`INSERT INTO gateways (name, bridge_token_sha256, pinned_pubkey, notes, created_at)
		VALUES (?, ?, ?, ?, ?)`, name, tokenHash, pinnedPubkey, notes, ms(now))
	if isUniqueViolation(err) {
		return ErrExists
	}
	return err
}

// Gateway loads one gateway.
func (s *Store) Gateway(name string) (*GatewayRecord, error) {
	return scanGateway(s.db.QueryRow(`SELECT `+gatewayCols+` FROM gateways WHERE name = ?`, name))
}

// GatewayByToken resolves a bridge token hash to its gateway.
func (s *Store) GatewayByToken(hash string) (*GatewayRecord, error) {
	return scanGateway(s.db.QueryRow(`SELECT `+gatewayCols+` FROM gateways WHERE bridge_token_sha256 = ?`, hash))
}

// Gateways lists every gateway, by name.
func (s *Store) Gateways() ([]*GatewayRecord, error) {
	rows, err := s.db.Query(`SELECT ` + gatewayCols + ` FROM gateways ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*GatewayRecord
	for rows.Next() {
		g, err := scanGateway(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Store) execOne(query string, args ...any) error {
	res, err := s.db.Exec(query, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetGatewayToken replaces a gateway's bridge token hash.
func (s *Store) SetGatewayToken(name, hash string) error {
	return s.execOne(`UPDATE gateways SET bridge_token_sha256 = ? WHERE name = ?`, hash, name)
}

// SetPubkeys sets a gateway's pinned and mismatching pubkeys.
func (s *Store) SetPubkeys(name, pinned, mismatch string) error {
	return s.execOne(`UPDATE gateways SET pinned_pubkey = ?, mismatch_pubkey = ? WHERE name = ?`, pinned, mismatch, name)
}

// DeleteGateway removes a gateway, its event history and the client tokens
// scoped to it: a gateway's name can be used again, and its old tokens must
// not open the new one. Lease audit rows are kept: they record who used the
// node, which outlives the gateway.
func (s *Store) DeleteGateway(name string) error {
	if _, err := s.Gateway(name); err != nil {
		return err
	}
	// Tokens first: if a later step fails, no token outlives its gateway.
	if _, err := s.db.Exec(`DELETE FROM clients WHERE gateway = ? AND gateway != ''`, name); err != nil {
		return err
	}
	if err := s.execOne(`DELETE FROM gateways WHERE name = ?`, name); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM gateway_events WHERE gateway = ?`, name); err != nil {
		return err
	}
	return s.deleteBridgeLogs(name)
}

func (s *Store) RecordHello(name string, h *helloMsg, at time.Time) error {
	b, _ := json.Marshal(h)
	_, err := s.db.Exec(`UPDATE gateways SET last_hello = ?, last_seen = ? WHERE name = ?`, string(b), ms(at), name)
	return err
}

func (s *Store) RecordSeen(name string, at time.Time) error {
	_, err := s.db.Exec(`UPDATE gateways SET last_seen = ? WHERE name = ?`, ms(at), name)
	return err
}

func (s *Store) RecordStats(name string, st Stats, at time.Time) error {
	b, _ := json.Marshal(st)
	_, err := s.db.Exec(`UPDATE gateways SET last_stats = ?, last_stats_at = ?, last_seen = ? WHERE name = ?`,
		string(b), ms(at), ms(at), name)
	return err
}

// GatewayEvent is one row of a gateway's connect/disconnect history.
type GatewayEvent struct {
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"`
	Detail string    `json:"detail,omitempty"`
}

// maxEventsPerGateway bounds gateway_events. A flapping bridge (two bridges on
// one token) writes two rows per flap; this keeps that from growing forever.
const maxEventsPerGateway = 500

func (s *Store) AddGatewayEvent(name string, e GatewayEvent) error {
	if _, err := s.db.Exec(`INSERT INTO gateway_events (gateway, at, kind, detail) VALUES (?, ?, ?, ?)`,
		name, ms(e.At), e.Kind, e.Detail); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM gateway_events WHERE gateway = ? AND id <= (
		SELECT id FROM gateway_events WHERE gateway = ? ORDER BY id DESC LIMIT 1 OFFSET ?)`,
		name, name, maxEventsPerGateway)
	return err
}

// GatewayEvents returns a gateway's most recent events, newest first.
func (s *Store) GatewayEvents(name string, limit int) ([]GatewayEvent, error) {
	rows, err := s.db.Query(`SELECT at, kind, detail FROM gateway_events WHERE gateway = ?
		ORDER BY id DESC LIMIT ?`, name, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GatewayEvent{}
	for rows.Next() {
		var e GatewayEvent
		var at int64
		if err := rows.Scan(&at, &e.Kind, &e.Detail); err != nil {
			return nil, err
		}
		e.At = fromMS(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ClientRecord is a client token's row.
type ClientRecord struct {
	Name      string    `json:"name"`
	Role      Role      `json:"role"`
	CreatedAt time.Time `json:"created_at"`
	// Gateway scopes the token to one gateway; "" is unscoped.
	Gateway string `json:"gateway,omitempty"`
	// ExpiresAt is when the token stops working; zero never.
	ExpiresAt time.Time `json:"expires_at,omitzero"`
	// LastUsedAt is roughly when the token was last used (TouchClient).
	LastUsedAt time.Time `json:"last_used_at,omitzero"`
	// CreatedBy is the admin who issued it, or "gateway:<name>" for a
	// token a host issued from their bridge.
	CreatedBy string `json:"created_by,omitempty"`
	Note      string `json:"note,omitempty"`
	// Bootstrap marks a config-defined admin, which lives outside the table
	// and cannot be revoked through the API.
	Bootstrap bool `json:"bootstrap,omitempty"`
}

const clientColumns = `name, role, created_at, gateway, expires_at, last_used_at, created_by, note`

func scanClient(sc interface{ Scan(...any) error }, extra ...any) (ClientRecord, error) {
	var c ClientRecord
	var role string
	var created, expires, used int64
	dst := append([]any{&c.Name, &role, &created, &c.Gateway, &expires, &used, &c.CreatedBy, &c.Note}, extra...)
	if err := sc.Scan(dst...); err != nil {
		return c, err
	}
	c.Role, c.CreatedAt, c.ExpiresAt, c.LastUsedAt = Role(role), fromMS(created), fromMS(expires), fromMS(used)
	return c, nil
}

// CreateClient inserts a client token hash. c.Name, Role and CreatedAt are
// required; the rest are optional.
func (s *Store) CreateClient(c ClientRecord, hash string) error {
	_, err := s.db.Exec(`INSERT INTO clients (name, role, token_sha256, created_at, gateway, expires_at, created_by, note)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		c.Name, string(c.Role), hash, ms(c.CreatedAt), c.Gateway, ms(c.ExpiresAt), c.CreatedBy, c.Note)
	if isUniqueViolation(err) {
		return ErrExists
	}
	return err
}

// ClientByToken resolves a client token hash.
func (s *Store) ClientByToken(hash string) (*ClientRecord, error) {
	c, err := scanClient(s.db.QueryRow(`SELECT `+clientColumns+` FROM clients WHERE token_sha256 = ?`, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// Clients lists client tokens (never their hashes).
func (s *Store) Clients() ([]ClientRecord, error) {
	rows, err := s.db.Query(`SELECT ` + clientColumns + ` FROM clients ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ClientRecord{}
	for rows.Next() {
		c, err := scanClient(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// UpdateClient changes a client's role and the gateway it's scoped to.
func (s *Store) UpdateClient(name string, role Role, gateway string) error {
	return s.execOne(`UPDATE clients SET role = ?, gateway = ? WHERE name = ?`, string(role), gateway, name)
}

// TouchClient records that a client's token was used at at.
func (s *Store) TouchClient(name string, at time.Time) error {
	return s.execOne(`UPDATE clients SET last_used_at = ? WHERE name = ?`, ms(at), name)
}

// DeleteClient revokes a client token.
func (s *Store) DeleteClient(name string) error {
	return s.execOne(`DELETE FROM clients WHERE name = ?`, name)
}

// LeaseRecord is one row of the lease audit log.
type LeaseRecord struct {
	ID             string     `json:"id"`
	Gateway        string     `json:"gateway"`
	Client         string     `json:"client"`
	Role           Role       `json:"role"`
	StartedAt      time.Time  `json:"started_at"`
	EndedAt        *time.Time `json:"ended_at,omitempty"`
	EndReason      string     `json:"end_reason,omitempty"`
	FramesToNode   int64      `json:"frames_to_node"`
	FramesFromNode int64      `json:"frames_from_node"`
	BytesToNode    int64      `json:"bytes_to_node"`
	BytesFromNode  int64      `json:"bytes_from_node"`
}

func (s *Store) InsertLease(l LeaseRecord) error {
	_, err := s.db.Exec(`INSERT INTO leases (id, gateway, client, role, started_at) VALUES (?, ?, ?, ?, ?)`,
		l.ID, l.Gateway, l.Client, string(l.Role), ms(l.StartedAt))
	return err
}

func (s *Store) EndLease(l LeaseRecord) error {
	_, err := s.db.Exec(`UPDATE leases SET ended_at = ?, end_reason = ?, frames_to_node = ?, frames_from_node = ?,
		bytes_to_node = ?, bytes_from_node = ? WHERE id = ?`,
		ms(*l.EndedAt), l.EndReason, l.FramesToNode, l.FramesFromNode, l.BytesToNode, l.BytesFromNode, l.ID)
	return err
}

// CloseOrphanLeases ends any lease the previous process left open. Leases do
// not survive a restart, so an open row at startup means the relay died
// without recording the end; the frame counts for it are lost.
func (s *Store) CloseOrphanLeases(now time.Time) (int64, error) {
	res, err := s.db.Exec(`UPDATE leases SET ended_at = ?, end_reason = 'relay_restart' WHERE ended_at IS NULL`, ms(now))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RecentLeases returns finished leases, newest first.
func (s *Store) RecentLeases(limit int) ([]LeaseRecord, error) {
	rows, err := s.db.Query(`SELECT id, gateway, client, role, started_at, ended_at, end_reason,
		frames_to_node, frames_from_node, bytes_to_node, bytes_from_node
		FROM leases WHERE ended_at IS NOT NULL ORDER BY started_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LeaseRecord{}
	for rows.Next() {
		var l LeaseRecord
		var role string
		var started int64
		var ended sql.NullInt64
		if err := rows.Scan(&l.ID, &l.Gateway, &l.Client, &role, &started, &ended, &l.EndReason,
			&l.FramesToNode, &l.FramesFromNode, &l.BytesToNode, &l.BytesFromNode); err != nil {
			return nil, err
		}
		l.Role, l.StartedAt = Role(role), fromMS(started)
		if ended.Valid {
			t := fromMS(ended.Int64)
			l.EndedAt = &t
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
