package relay

import (
	"time"

	"github.com/sierragridteam/sierra-meshrelay/meshproto"
)

// Storage is everything the relay persists. The relay is its only writer
// (one process, see OpenStore's lock), and the volume is tiny: a handful of
// gateways and clients, a row per lease.
//
// Two implementations exist: *Store (SQLite, OpenStore), the default for
// local and self-hosted relays and the tests; and relay/dynamostore
// (DynamoDB), for the hosted relay. They behave the same; the store
// conformance tests (relay/storetest) hold both to it. Lookups of a missing
// row return ErrNotFound; creating a row whose key is taken returns
// ErrExists. The Record* methods are fire-and-forget updates from live
// sessions: a missing gateway or repeater is not an error.
type Storage interface {
	// Close releases the store (and its single-writer lock).
	Close() error

	// Gateways.
	CreateGateway(name, tokenHash, pinnedPubkey, notes string, now time.Time) error
	Gateway(name string) (*GatewayRecord, error)
	GatewayByToken(hash string) (*GatewayRecord, error)
	Gateways() ([]*GatewayRecord, error) // by name
	SetGatewayToken(name, hash string) error
	SetPubkeys(name, pinned, mismatch string) error
	// DeleteGateway removes a gateway, its events and its bridge logs; its
	// lease audit rows stay.
	DeleteGateway(name string) error
	RecordHello(name string, h *meshproto.Hello, at time.Time) error
	RecordSeen(name string, at time.Time) error
	RecordStats(name string, st Stats, at time.Time) error

	// Gateway connect/disconnect history (bounded per gateway).
	AddGatewayEvent(name string, e GatewayEvent) error
	GatewayEvents(name string, limit int) ([]GatewayEvent, error) // newest first

	// Client tokens.
	CreateClient(name string, role Role, hash string, now time.Time) error
	ClientByToken(hash string) (*ClientRecord, error)
	Clients() ([]ClientRecord, error) // by name, never the hashes
	DeleteClient(name string) error

	// The lease audit log. EndLease gets the whole record (ID, StartedAt and
	// the end fields); it updates the row InsertLease wrote.
	InsertLease(l LeaseRecord) error
	EndLease(l LeaseRecord) error
	CloseOrphanLeases(now time.Time) (int64, error)
	RecentLeases(limit int) ([]LeaseRecord, error) // finished, newest first

	// Passphrase invites.
	CreateInvite(i InviteRecord) error
	Invites() ([]*InviteRecord, error)                    // newest first
	ActiveInvites(now time.Time) ([]*InviteRecord, error) // oldest first
	RevokeInvite(id string) error
	// UseInvite consumes one use atomically; ErrNotFound if none remain.
	UseInvite(id string, now time.Time) error

	// Shared repeaters (RepeaterRecord.Sealed holds the sealed password).
	Repeater(pubkey string) (*RepeaterRecord, error)
	Repeaters() ([]*RepeaterRecord, error) // by name, then pubkey
	// PutRepeater inserts or replaces a repeater's password and who added
	// it, keeping its created time and poll history.
	PutRepeater(r RepeaterRecord, now time.Time) error
	// DeleteRepeater removes a repeater, its attempts and its poll state.
	DeleteRepeater(pubkey string) error
	RecordRepeaterPoll(pubkey, gateway string, at time.Time, errText string) error

	// Repeater poll bookkeeping (attempts are kept a week).
	RecordRepeaterAttempt(a RepeaterAttempt) error
	RepeaterAttempts(pubkey string, since time.Time) ([]RepeaterAttempt, error) // oldest first
	RepeaterPollState(pubkey string) (RepeaterPollState, error)                 // zero state if none
	SetRepeaterPollState(pubkey string, st RepeaterPollState, now time.Time) error

	// Bridge logs (bounded per gateway). IDs increase per gateway and are the
	// API's `after` cursor.
	AddBridgeLogs(gateway string, at time.Time, es []BridgeLog) error
	BridgeLogs(gateway string, afterID int64, limit int) ([]BridgeLog, error) // oldest first, the newest limit

	// Settings: small JSON values by key; Setting returns ErrNotFound if unset.
	Setting(key string) ([]byte, error)
	SetSetting(key string, value []byte) error
}

var _ Storage = (*Store)(nil)

// Retention and caps that every Storage applies.
const (
	// MaxEventsPerGateway bounds a gateway's connect/disconnect history.
	MaxEventsPerGateway = maxEventsPerGateway
	// MaxLogsPerGateway bounds a gateway's stored bridge log lines.
	MaxLogsPerGateway = maxLogsPerGateway
	// AttemptRetention is how long repeater attempts are kept.
	AttemptRetention = attemptRetention
)
