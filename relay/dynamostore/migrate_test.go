package dynamostore

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sierragridteam/sierra-meshrelay/meshproto"
	"github.com/sierragridteam/sierra-meshrelay/relay"
)

// sealForTest seals like the relay does (AES-256-GCM, nonce first, the
// pubkey as additional data).
func sealForTest(t *testing.T, key []byte, pubkey, password string) []byte {
	b, err := aes.NewCipher(key)
	require.NoError(t, err)
	a, err := cipher.NewGCM(b)
	require.NoError(t, err)
	nonce := make([]byte, a.NonceSize())
	_, _ = rand.Read(nonce)
	return a.Seal(nonce, nonce, []byte(password), []byte("repeater:"+pubkey))
}

func TestImportFromSQLite(t *testing.T) {
	db := localClient(t)
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	now := time.Now().UTC().Truncate(time.Millisecond)

	// A relay.db with a bit of everything, written through the SQLite store.
	path := filepath.Join(t.TempDir(), "relay.db")
	sq, err := relay.OpenStore(context.Background(), path, time.Second)
	require.NoError(t, err)
	require.NoError(t, sq.CreateGateway("cabin", "hash-cabin", "pk-cabin", "a note", now.Add(-48*time.Hour)))
	require.NoError(t, sq.CreateGateway("ridge", "hash-ridge", "", "", now.Add(-24*time.Hour)))
	require.NoError(t, sq.RecordHello("cabin", &meshproto.Hello{Type: "hello", Agent: meshproto.Agent{Version: "v0.10.1"}}, now))
	require.NoError(t, sq.CreateClient("amy", relay.RoleAdmin, "hash-amy", now))
	require.NoError(t, sq.CreateClient("telemetry-op", relay.RoleOperator, "hash-op", now))
	require.NoError(t, sq.CreateInvite(relay.InviteRecord{ID: "i_1", PassphraseHash: "ph", MaxUses: 3,
		ExpiresAt: now.Add(time.Hour), CreatedAt: now}))
	require.NoError(t, sq.UseInvite("i_1", now))
	require.NoError(t, sq.PutRepeater(relay.RepeaterRecord{Pubkey: "rpk", Name: "Peak", Access: "guest",
		AddedByGateway: "cabin", LastGateway: "cabin", Sealed: sealForTest(t, key, "rpk", "guestpw")}, now))
	require.NoError(t, sq.SetRepeaterPollState("rpk", relay.RepeaterPollState{Primary: "cabin", NextCycle: now.Add(time.Hour)}, now))
	require.NoError(t, sq.RecordRepeaterAttempt(relay.RepeaterAttempt{Pubkey: "rpk", At: now.Add(-time.Hour), Gateway: "cabin", Outcome: relay.AttemptOK}))
	require.NoError(t, sq.RecordRepeaterAttempt(relay.RepeaterAttempt{Pubkey: "rpk", At: now.Add(-30 * 24 * time.Hour), Gateway: "cabin", Outcome: relay.AttemptOK}))
	require.NoError(t, sq.AddGatewayEvent("cabin", relay.GatewayEvent{At: now, Kind: "connect"}))
	require.NoError(t, sq.AddBridgeLogs("cabin", now, []relay.BridgeLog{{T: "t", Level: "INFO", Msg: "one"}, {T: "t", Level: "INFO", Msg: "two"}}))
	require.NoError(t, sq.InsertLease(relay.LeaseRecord{ID: "l_old", Gateway: "cabin", Client: "amy", Role: relay.RoleAdmin, StartedAt: now.Add(-200 * 24 * time.Hour)}))
	end := now.Add(-199 * 24 * time.Hour)
	require.NoError(t, sq.EndLease(relay.LeaseRecord{ID: "l_old", EndedAt: &end, EndReason: "client_closed"}))
	require.NoError(t, sq.InsertLease(relay.LeaseRecord{ID: "l_done", Gateway: "cabin", Client: "amy", Role: relay.RoleAdmin, StartedAt: now.Add(-time.Hour)}))
	end2 := now.Add(-30 * time.Minute)
	require.NoError(t, sq.EndLease(relay.LeaseRecord{ID: "l_done", EndedAt: &end2, EndReason: "expired", FramesToNode: 7}))
	require.NoError(t, sq.InsertLease(relay.LeaseRecord{ID: "l_open", Gateway: "ridge", Client: "telemetry", Role: relay.RoleAgent, StartedAt: now}))
	raw, _ := json.Marshal(map[string]any{"version": "v0.10.1"})
	require.NoError(t, sq.SetSetting("bridge_update", raw))
	require.NoError(t, sq.Close())

	snap, err := relay.ReadSnapshot(context.Background(), path, relay.SnapshotOptions{
		LeasesSince: now.Add(-90 * 24 * time.Hour), AttemptsSince: now.Add(-relay.AttemptRetention)})
	require.NoError(t, err)
	assert.EqualValues(t, 1, snap.Dropped["leases"], "the 200-day-old lease")
	assert.EqualValues(t, 1, snap.Dropped["repeater_attempts"])
	assert.EqualValues(t, 2, snap.MaxBridgeLogID)

	table := newTable(t, db)
	st := openTest(t, db, table, Options{})
	require.NoError(t, st.Import(snap))
	rep, err := st.Verify(snap, key)
	require.NoError(t, err)
	assert.True(t, rep.OK(), rep.String())
	assert.Equal(t, 1, rep.SealedOK)

	wrong := make([]byte, 32)
	rep, err = st.Verify(snap, wrong)
	require.NoError(t, err)
	assert.False(t, rep.OK(), "a different key can't open the passwords")
	rep, _ = st.Verify(snap, nil)
	assert.False(t, rep.KeyChecked, "no key: the report says nothing was opened")

	assert.ErrorIs(t, st.Import(snap), ErrNotEmpty, "never import over existing data")

	// What the relay will read.
	g, err := st.GatewayByToken("hash-cabin")
	require.NoError(t, err)
	assert.Equal(t, "pk-cabin", g.PinnedPubkey)
	assert.Equal(t, "v0.10.1", g.LastHello.Agent.Version)
	inv, _ := st.Invites()
	assert.Equal(t, 1, inv[0].Uses)
	require.NoError(t, st.AddBridgeLogs("cabin", now, []relay.BridgeLog{{T: "t", Level: "INFO", Msg: "three"}}))
	logs, err := st.BridgeLogs("cabin", 2, 10)
	require.NoError(t, err)
	require.Len(t, logs, 1)
	assert.EqualValues(t, 3, logs[0].ID, "new IDs continue past the file's")
	n, err := st.CloseOrphanLeases(now)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n, "the lease open in the snapshot is closed at the next start")
	recent, _ := st.RecentLeases(10)
	require.Len(t, recent, 2)
	assert.Equal(t, "l_open", recent[0].ID)
	assert.EqualValues(t, 7, recent[1].FramesToNode)
	require.NoError(t, st.Close())
}
