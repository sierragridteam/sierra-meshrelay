// Package storetest is the conformance suite every relay.Storage must pass:
// the SQLite store (relay) and the DynamoDB store (relay/dynamostore) run it,
// so the two backends cannot drift apart.
package storetest

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sierragridteam/sierra-meshrelay/meshproto"
	"github.com/sierragridteam/sierra-meshrelay/relay"
)

// Opener returns a fresh, empty store for one subtest. The suite closes it.
type Opener func(t *testing.T) relay.Storage

// Run runs the whole suite.
func Run(t *testing.T, open Opener) {
	tests := []struct {
		name string
		fn   func(*testing.T, relay.Storage)
	}{
		{"Gateways", testGateways},
		{"GatewayToken", testGatewayToken},
		{"GatewayLiveFields", testGatewayLiveFields},
		{"DeleteGateway", testDeleteGateway},
		{"GatewayEvents", testGatewayEvents},
		{"GatewayEventsCap", testGatewayEventsCap},
		{"Clients", testClients},
		{"ScopedClients", testScopedClients},
		{"Leases", testLeases},
		{"Invites", testInvites},
		{"Repeaters", testRepeaters},
		{"RepeaterPolling", testRepeaterPolling},
		{"BridgeLogs", testBridgeLogs},
		{"BridgeLogsCap", testBridgeLogsCap},
		{"Settings", testSettings},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := open(t)
			t.Cleanup(func() { _ = st.Close() })
			tc.fn(t, st)
		})
	}
}

// t0 is a fixed, millisecond-precise UTC time: stores keep milliseconds.
var t0 = time.Date(2026, 11, 12, 18, 30, 0, 0, time.UTC)

func at(d time.Duration) time.Time { return t0.Add(d) }

func testGateways(t *testing.T, st relay.Storage) {
	gws, err := st.Gateways()
	require.NoError(t, err)
	assert.Empty(t, gws)

	require.NoError(t, st.CreateGateway("gw-b", "hash-b", "pk-b", "notes b", at(0)))
	require.NoError(t, st.CreateGateway("gw-a", "hash-a", "", "", at(time.Second)))
	assert.ErrorIs(t, st.CreateGateway("gw-a", "hash-other", "", "", at(0)), relay.ErrExists, "name taken")
	assert.ErrorIs(t, st.CreateGateway("gw-c", "hash-a", "", "", at(0)), relay.ErrExists, "token hash taken")
	_, err = st.Gateway("gw-c")
	assert.ErrorIs(t, err, relay.ErrNotFound, "a refused create leaves nothing behind")

	g, err := st.Gateway("gw-b")
	require.NoError(t, err)
	assert.Equal(t, "gw-b", g.Name)
	assert.Equal(t, "hash-b", g.TokenHash)
	assert.Equal(t, "pk-b", g.PinnedPubkey)
	assert.Equal(t, "notes b", g.Notes)
	assert.True(t, g.CreatedAt.Equal(at(0)))
	assert.True(t, g.LastSeen.IsZero())
	assert.Nil(t, g.LastHello)
	assert.Nil(t, g.LastStats)

	_, err = st.Gateway("nope")
	assert.ErrorIs(t, err, relay.ErrNotFound)

	g, err = st.GatewayByToken("hash-a")
	require.NoError(t, err)
	assert.Equal(t, "gw-a", g.Name)
	_, err = st.GatewayByToken("hash-nope")
	assert.ErrorIs(t, err, relay.ErrNotFound)

	gws, err = st.Gateways()
	require.NoError(t, err)
	require.Len(t, gws, 2)
	assert.Equal(t, "gw-a", gws[0].Name, "by name")
	assert.Equal(t, "gw-b", gws[1].Name)

	require.NoError(t, st.SetPubkeys("gw-a", "pinned", "mismatch"))
	g, _ = st.Gateway("gw-a")
	assert.Equal(t, "pinned", g.PinnedPubkey)
	assert.Equal(t, "mismatch", g.MismatchPubkey)
	assert.ErrorIs(t, st.SetPubkeys("nope", "", ""), relay.ErrNotFound)
	_, err = st.Gateway("nope")
	assert.ErrorIs(t, err, relay.ErrNotFound, "an update never creates a gateway")
}

func testGatewayToken(t *testing.T, st relay.Storage) {
	require.NoError(t, st.CreateGateway("gw", "old", "", "", at(0)))
	require.NoError(t, st.CreateGateway("other", "theirs", "", "", at(0)))
	require.NoError(t, st.SetGatewayToken("gw", "new"))
	_, err := st.GatewayByToken("old")
	assert.ErrorIs(t, err, relay.ErrNotFound, "the old token stops working at once")
	g, err := st.GatewayByToken("new")
	require.NoError(t, err)
	assert.Equal(t, "gw", g.Name)
	assert.Equal(t, "new", g.TokenHash)

	require.NoError(t, st.SetGatewayToken("gw", "new"), "setting the same hash again is fine")
	assert.Error(t, st.SetGatewayToken("gw", "theirs"), "another gateway's hash is refused")
	g, _ = st.GatewayByToken("theirs")
	assert.Equal(t, "other", g.Name)
	assert.ErrorIs(t, st.SetGatewayToken("nope", "x"), relay.ErrNotFound)
	_, err = st.GatewayByToken("x")
	assert.ErrorIs(t, err, relay.ErrNotFound)
}

func testGatewayLiveFields(t *testing.T, st relay.Storage) {
	require.NoError(t, st.CreateGateway("gw", "h", "", "", at(0)))
	hello := &meshproto.Hello{Type: "hello", Protocol: 1, Gateway: "gw",
		Agent: meshproto.Agent{Name: "meshbridge", Version: "v0.10.1", OS: "linux", Arch: "arm64"}}
	require.NoError(t, st.RecordHello("gw", hello, at(time.Minute)))
	g, err := st.Gateway("gw")
	require.NoError(t, err)
	require.NotNil(t, g.LastHello)
	assert.Equal(t, "v0.10.1", g.LastHello.Agent.Version)
	assert.True(t, g.LastSeen.Equal(at(time.Minute)))

	require.NoError(t, st.RecordSeen("gw", at(2*time.Minute)))
	stats := meshproto.Stats{UptimeS: 3600, FramesToNode: 12, FramesFromNode: 34}
	require.NoError(t, st.RecordStats("gw", stats, at(3*time.Minute)))
	g, _ = st.Gateway("gw")
	require.NotNil(t, g.LastStats)
	assert.Equal(t, stats, *g.LastStats)
	assert.True(t, g.LastStatsAt.Equal(at(3*time.Minute)))
	assert.True(t, g.LastSeen.Equal(at(3*time.Minute)), "stats count as seen")
	assert.Equal(t, "v0.10.1", g.LastHello.Agent.Version, "kept")

	// Sessions record against gateways an admin may just have deleted.
	assert.NoError(t, st.RecordHello("gone", hello, at(0)))
	assert.NoError(t, st.RecordSeen("gone", at(0)))
	assert.NoError(t, st.RecordStats("gone", stats, at(0)))
	_, err = st.Gateway("gone")
	assert.ErrorIs(t, err, relay.ErrNotFound, "recording never creates a gateway")
}

func testDeleteGateway(t *testing.T, st relay.Storage) {
	require.NoError(t, st.CreateGateway("gw", "h", "", "", at(0)))
	require.NoError(t, st.AddGatewayEvent("gw", relay.GatewayEvent{At: at(0), Kind: "connect"}))
	require.NoError(t, st.AddBridgeLogs("gw", at(0), []relay.BridgeLog{{T: "t", Level: "INFO", Msg: "hi"}}))
	require.NoError(t, st.InsertLease(relay.LeaseRecord{ID: "l_1", Gateway: "gw", Client: "c", Role: relay.RoleAgent, StartedAt: at(0)}))
	end := at(time.Minute)
	require.NoError(t, st.EndLease(relay.LeaseRecord{ID: "l_1", Gateway: "gw", Client: "c", Role: relay.RoleAgent,
		StartedAt: at(0), EndedAt: &end, EndReason: "client_closed"}))

	// Tokens scoped to the gateway go with it; others stay.
	require.NoError(t, st.CreateClient(relay.ClientRecord{Name: "gw.laptop", Role: "host", CreatedAt: at(0), Gateway: "gw"}, "hs"))
	require.NoError(t, st.CreateClient(relay.ClientRecord{Name: "other.laptop", Role: "host", CreatedAt: at(0), Gateway: "other"}, "ho"))
	require.NoError(t, st.CreateClient(client("admin", relay.RoleAdmin, at(0)), "ha"))

	require.NoError(t, st.DeleteGateway("gw"))
	_, err := st.ClientByToken("hs")
	assert.ErrorIs(t, err, relay.ErrNotFound, "a scoped token outlived its gateway")
	_, err = st.ClientByToken("ho")
	assert.NoError(t, err, "another gateway's token")
	_, err = st.ClientByToken("ha")
	assert.NoError(t, err, "an unscoped token")
	require.NoError(t, st.CreateClient(relay.ClientRecord{Name: "gw.laptop", Role: "host", CreatedAt: at(0), Gateway: "gw"}, "hs"),
		"the scoped token's name and hash are free again")
	require.NoError(t, st.DeleteClient("gw.laptop"))

	_, err = st.Gateway("gw")
	assert.ErrorIs(t, err, relay.ErrNotFound)
	_, err = st.GatewayByToken("h")
	assert.ErrorIs(t, err, relay.ErrNotFound)
	evs, err := st.GatewayEvents("gw", 10)
	require.NoError(t, err)
	assert.Empty(t, evs)
	logs, err := st.BridgeLogs("gw", 0, 10)
	require.NoError(t, err)
	assert.Empty(t, logs)
	leases, err := st.RecentLeases(10)
	require.NoError(t, err)
	assert.Len(t, leases, 1, "lease audit rows outlive the gateway")
	assert.ErrorIs(t, st.DeleteGateway("gw"), relay.ErrNotFound)

	require.NoError(t, st.CreateGateway("gw", "h", "", "", at(time.Hour)), "the name and token are free again")
}

func testGatewayEvents(t *testing.T, st relay.Storage) {
	evs, err := st.GatewayEvents("gw", 10)
	require.NoError(t, err)
	assert.NotNil(t, evs, "an empty list, not nil (it is serialized)")
	assert.Empty(t, evs)

	// Same millisecond: insertion order still decides.
	require.NoError(t, st.AddGatewayEvent("gw", relay.GatewayEvent{At: at(0), Kind: "connect", Detail: "v1"}))
	require.NoError(t, st.AddGatewayEvent("gw", relay.GatewayEvent{At: at(0), Kind: "disconnect"}))
	require.NoError(t, st.AddGatewayEvent("gw", relay.GatewayEvent{At: at(time.Second), Kind: "connect", Detail: "v2"}))
	require.NoError(t, st.AddGatewayEvent("other", relay.GatewayEvent{At: at(0), Kind: "connect"}))

	evs, err = st.GatewayEvents("gw", 10)
	require.NoError(t, err)
	require.Len(t, evs, 3)
	assert.Equal(t, "v2", evs[0].Detail, "newest first")
	assert.Equal(t, "disconnect", evs[1].Kind)
	assert.Equal(t, "connect", evs[2].Kind)
	assert.Equal(t, "v1", evs[2].Detail)
	assert.True(t, evs[0].At.Equal(at(time.Second)))

	evs, _ = st.GatewayEvents("gw", 2)
	assert.Len(t, evs, 2)
}

func testGatewayEventsCap(t *testing.T, st relay.Storage) {
	n := relay.MaxEventsPerGateway + 5
	for i := range n {
		require.NoError(t, st.AddGatewayEvent("gw", relay.GatewayEvent{At: at(time.Duration(i) * time.Millisecond),
			Kind: "connect", Detail: fmt.Sprint(i)}))
	}
	evs, err := st.GatewayEvents("gw", 2*n)
	require.NoError(t, err)
	require.Len(t, evs, relay.MaxEventsPerGateway)
	assert.Equal(t, fmt.Sprint(n-1), evs[0].Detail)
	assert.Equal(t, fmt.Sprint(n-relay.MaxEventsPerGateway), evs[len(evs)-1].Detail, "the oldest went")
}

func client(name string, role relay.Role, created time.Time) relay.ClientRecord {
	return relay.ClientRecord{Name: name, Role: role, CreatedAt: created}
}

func testClients(t *testing.T, st relay.Storage) {
	cs, err := st.Clients()
	require.NoError(t, err)
	assert.NotNil(t, cs)
	assert.Empty(t, cs)

	require.NoError(t, st.CreateClient(client("zed", relay.RoleOperator, at(0)), "hz"))
	require.NoError(t, st.CreateClient(client("amy", relay.RoleAdmin, at(time.Second)), "ha"))
	assert.ErrorIs(t, st.CreateClient(client("amy", relay.RoleAgent, at(0)), "hx"), relay.ErrExists)
	assert.ErrorIs(t, st.CreateClient(client("bob", relay.RoleAgent, at(0)), "ha"), relay.ErrExists)

	c, err := st.ClientByToken("ha")
	require.NoError(t, err)
	assert.Equal(t, "amy", c.Name)
	assert.Equal(t, relay.RoleAdmin, c.Role)
	assert.True(t, c.CreatedAt.Equal(at(time.Second)))
	assert.Empty(t, c.Gateway, "unscoped")
	assert.True(t, c.ExpiresAt.IsZero(), "no expiry")
	assert.True(t, c.LastUsedAt.IsZero(), "never used")
	_, err = st.ClientByToken("hx")
	assert.ErrorIs(t, err, relay.ErrNotFound)

	cs, err = st.Clients()
	require.NoError(t, err)
	require.Len(t, cs, 2)
	assert.Equal(t, "amy", cs[0].Name)
	assert.Equal(t, "zed", cs[1].Name)

	require.NoError(t, st.DeleteClient("amy"))
	_, err = st.ClientByToken("ha")
	assert.ErrorIs(t, err, relay.ErrNotFound)
	assert.ErrorIs(t, st.DeleteClient("amy"), relay.ErrNotFound)
	require.NoError(t, st.CreateClient(client("amy2", relay.RoleAdmin, at(0)), "ha"), "the hash is free again")
}

// testScopedClients: the fields a host token carries, and changing a
// client's role and scope.
func testScopedClients(t *testing.T, st relay.Storage) {
	host := relay.ClientRecord{Name: "cabin.laptop", Role: "host", CreatedAt: at(0), Gateway: "cabin",
		ExpiresAt: at(180 * 24 * time.Hour), CreatedBy: "gateway:cabin", Note: "Jay's laptop"}
	require.NoError(t, st.CreateClient(host, "hh"))

	got, err := st.ClientByToken("hh")
	require.NoError(t, err)
	assert.Equal(t, "cabin", got.Gateway)
	assert.True(t, got.ExpiresAt.Equal(at(180*24*time.Hour)))
	assert.Equal(t, "gateway:cabin", got.CreatedBy)
	assert.Equal(t, "Jay's laptop", got.Note)
	assert.True(t, got.LastUsedAt.IsZero())

	require.NoError(t, st.TouchClient("cabin.laptop", at(time.Hour)))
	got, err = st.ClientByToken("hh")
	require.NoError(t, err)
	assert.True(t, got.LastUsedAt.Equal(at(time.Hour)))
	assert.ErrorIs(t, st.TouchClient("nobody", at(0)), relay.ErrNotFound)

	// An operator becomes an admin (unscoped), and back to a scoped host.
	require.NoError(t, st.CreateClient(client("dev", relay.RoleOperator, at(0)), "hd"))
	require.NoError(t, st.UpdateClient("dev", relay.RoleAdmin, ""))
	got, err = st.ClientByToken("hd")
	require.NoError(t, err)
	assert.Equal(t, relay.RoleAdmin, got.Role)
	assert.Empty(t, got.Gateway)
	require.NoError(t, st.UpdateClient("dev", "host", "ridge"))
	got, err = st.ClientByToken("hd")
	require.NoError(t, err)
	assert.Equal(t, relay.Role("host"), got.Role)
	assert.Equal(t, "ridge", got.Gateway)
	require.NoError(t, st.UpdateClient("dev", relay.RoleAdmin, ""))
	got, err = st.ClientByToken("hd")
	require.NoError(t, err)
	assert.Empty(t, got.Gateway, "unscoped again")
	assert.ErrorIs(t, st.UpdateClient("nobody", relay.RoleAdmin, ""), relay.ErrNotFound)

	cs, err := st.Clients()
	require.NoError(t, err)
	require.Len(t, cs, 2)
	assert.Equal(t, "cabin", cs[0].Gateway, "Clients carries the new fields")
	assert.Equal(t, "Jay's laptop", cs[0].Note)
}

func testLeases(t *testing.T, st relay.Storage) {
	ls, err := st.RecentLeases(10)
	require.NoError(t, err)
	assert.NotNil(t, ls)
	assert.Empty(t, ls)

	start := func(id string, d time.Duration) relay.LeaseRecord {
		l := relay.LeaseRecord{ID: id, Gateway: "gw", Client: "c-" + id, Role: relay.RoleOperator, StartedAt: at(d)}
		require.NoError(t, st.InsertLease(l))
		return l
	}
	end := func(l relay.LeaseRecord, d time.Duration, reason string) {
		e := at(d)
		l.EndedAt, l.EndReason = &e, reason
		l.FramesToNode, l.FramesFromNode, l.BytesToNode, l.BytesFromNode = 1, 2, 30, 40
		require.NoError(t, st.EndLease(l))
	}
	a := start("l_a", 0)
	b := start("l_b", time.Minute)
	start("l_c", 2*time.Minute)
	start("l_d", 3*time.Minute)
	end(a, 5*time.Minute, "client_closed")
	end(b, 4*time.Minute, "expired")

	ls, err = st.RecentLeases(10)
	require.NoError(t, err)
	require.Len(t, ls, 2, "only finished leases")
	assert.Equal(t, "l_b", ls[0].ID, "newest start first")
	assert.Equal(t, "l_a", ls[1].ID)
	assert.Equal(t, "c-l_b", ls[0].Client)
	assert.Equal(t, relay.RoleOperator, ls[0].Role)
	assert.Equal(t, "expired", ls[0].EndReason)
	require.NotNil(t, ls[0].EndedAt)
	assert.True(t, ls[0].EndedAt.Equal(at(4*time.Minute)))
	assert.EqualValues(t, 2, ls[0].FramesFromNode)
	assert.EqualValues(t, 40, ls[0].BytesFromNode)

	ls, _ = st.RecentLeases(1)
	assert.Len(t, ls, 1)

	n, err := st.CloseOrphanLeases(at(time.Hour))
	require.NoError(t, err)
	assert.EqualValues(t, 2, n)
	ls, _ = st.RecentLeases(10)
	require.Len(t, ls, 4)
	assert.Equal(t, "l_d", ls[0].ID)
	assert.Equal(t, "relay_restart", ls[0].EndReason)
	assert.True(t, ls[0].EndedAt.Equal(at(time.Hour)))
	n, _ = st.CloseOrphanLeases(at(time.Hour))
	assert.EqualValues(t, 0, n)
}

func testInvites(t *testing.T, st relay.Storage) {
	invs, err := st.Invites()
	require.NoError(t, err)
	assert.Empty(t, invs)

	mk := func(id string, created time.Duration, maxUses int, expires time.Duration) relay.InviteRecord {
		return relay.InviteRecord{ID: id, PassphraseHash: "ph-" + id, Gateway: "gw", NamePrefix: "cabin",
			MaxUses: maxUses, ExpiresAt: at(expires), ExpectedPubkey: "pk", Note: "n", CreatedBy: "amy",
			CreatedAt: at(created)}
	}
	require.NoError(t, st.CreateInvite(mk("i_old", 0, 2, time.Hour)))
	require.NoError(t, st.CreateInvite(mk("i_new", time.Minute, 1, time.Hour)))
	require.NoError(t, st.CreateInvite(mk("i_expired", 2*time.Minute, 1, 30*time.Second)))
	assert.ErrorIs(t, st.CreateInvite(mk("i_old", 0, 1, time.Hour)), relay.ErrExists)

	invs, err = st.Invites()
	require.NoError(t, err)
	require.Len(t, invs, 3)
	assert.Equal(t, []string{"i_expired", "i_new", "i_old"}, []string{invs[0].ID, invs[1].ID, invs[2].ID}, "newest first")
	got := invs[2]
	assert.Equal(t, "ph-i_old", got.PassphraseHash)
	assert.Equal(t, "cabin", got.NamePrefix)
	assert.Equal(t, 2, got.MaxUses)
	assert.Equal(t, 0, got.Uses)
	assert.False(t, got.Revoked)
	assert.Equal(t, "amy", got.CreatedBy)
	assert.True(t, got.ExpiresAt.Equal(at(time.Hour)))

	now := at(5 * time.Minute)
	act, err := st.ActiveInvites(now)
	require.NoError(t, err)
	require.Len(t, act, 2)
	assert.Equal(t, "i_old", act[0].ID, "oldest first")

	require.NoError(t, st.UseInvite("i_new", now))
	assert.ErrorIs(t, st.UseInvite("i_new", now), relay.ErrNotFound, "no uses left")
	assert.ErrorIs(t, st.UseInvite("i_expired", now), relay.ErrNotFound, "expired")
	assert.ErrorIs(t, st.UseInvite("i_nope", now), relay.ErrNotFound)
	require.NoError(t, st.RevokeInvite("i_old"))
	assert.ErrorIs(t, st.UseInvite("i_old", now), relay.ErrNotFound, "revoked")
	assert.ErrorIs(t, st.RevokeInvite("i_nope"), relay.ErrNotFound)

	act, _ = st.ActiveInvites(now)
	assert.Empty(t, act)
	invs, _ = st.Invites()
	assert.Equal(t, 1, invs[1].Uses)
	assert.True(t, invs[2].Revoked)
}

func testRepeaters(t *testing.T, st relay.Storage) {
	rs, err := st.Repeaters()
	require.NoError(t, err)
	assert.Empty(t, rs)

	sealed := []byte{0, 1, 2, 0xfe, 0xff}
	require.NoError(t, st.PutRepeater(relay.RepeaterRecord{Pubkey: "pk2", Name: "Ridge", Access: "guest",
		AddedByGateway: "gw", LastGateway: "gw", Sealed: sealed}, at(0)))
	require.NoError(t, st.PutRepeater(relay.RepeaterRecord{Pubkey: "pk1", Name: "Ridge", Access: "admin",
		AddedByClient: "amy", LastGateway: "gw2", Sealed: []byte("x")}, at(time.Second)))
	require.NoError(t, st.PutRepeater(relay.RepeaterRecord{Pubkey: "pk0", Name: "Peak", Sealed: []byte("y")}, at(0)))

	r, err := st.Repeater("pk2")
	require.NoError(t, err)
	assert.Equal(t, sealed, r.Sealed)
	assert.Equal(t, "Ridge", r.Name)
	assert.Equal(t, "guest", r.Access)
	assert.Equal(t, "gw", r.AddedByGateway)
	assert.True(t, r.CreatedAt.Equal(at(0)))
	assert.True(t, r.UpdatedAt.Equal(at(0)))
	require.NotNil(t, r.LastOKAt, "a share is checked before it's stored")
	assert.True(t, r.LastOKAt.Equal(at(0)))
	assert.Nil(t, r.LastAttemptAt)
	_, err = st.Repeater("nope")
	assert.ErrorIs(t, err, relay.ErrNotFound)

	rs, err = st.Repeaters()
	require.NoError(t, err)
	require.Len(t, rs, 3)
	assert.Equal(t, []string{"pk0", "pk1", "pk2"}, []string{rs[0].Pubkey, rs[1].Pubkey, rs[2].Pubkey}, "by name, then pubkey")

	require.NoError(t, st.RecordRepeaterPoll("pk2", "gw3", at(time.Minute), "no answer"))
	r, _ = st.Repeater("pk2")
	assert.Equal(t, "no answer", r.LastError)
	assert.Equal(t, "gw", r.LastGateway, "a failure keeps the last good gateway")
	require.NotNil(t, r.LastAttemptAt)
	assert.True(t, r.LastAttemptAt.Equal(at(time.Minute)))
	assert.True(t, r.LastOKAt.Equal(at(0)))

	require.NoError(t, st.RecordRepeaterPoll("pk2", "gw3", at(2*time.Minute), ""))
	r, _ = st.Repeater("pk2")
	assert.Empty(t, r.LastError)
	assert.Equal(t, "gw3", r.LastGateway)
	assert.True(t, r.LastOKAt.Equal(at(2*time.Minute)))
	require.NoError(t, st.RecordRepeaterPoll("pk2", "gw3", at(3*time.Minute), "refused"))

	// Re-sharing replaces the password and who added it, keeps created_at and
	// the last attempt, and clears the error.
	require.NoError(t, st.PutRepeater(relay.RepeaterRecord{Pubkey: "pk2", Name: "Ridge 2", Access: "admin",
		AddedByClient: "bob", LastGateway: "gw4", Sealed: []byte("new")}, at(time.Hour)))
	r, _ = st.Repeater("pk2")
	assert.Equal(t, []byte("new"), r.Sealed)
	assert.Equal(t, "Ridge 2", r.Name)
	assert.Equal(t, "", r.AddedByGateway)
	assert.Equal(t, "bob", r.AddedByClient)
	assert.Equal(t, "gw4", r.LastGateway)
	assert.Empty(t, r.LastError)
	assert.True(t, r.CreatedAt.Equal(at(0)))
	assert.True(t, r.UpdatedAt.Equal(at(time.Hour)))
	assert.True(t, r.LastOKAt.Equal(at(time.Hour)))
	require.NotNil(t, r.LastAttemptAt)
	assert.True(t, r.LastAttemptAt.Equal(at(3*time.Minute)))

	assert.NoError(t, st.RecordRepeaterPoll("gone", "gw", at(0), ""))
	_, err = st.Repeater("gone")
	assert.ErrorIs(t, err, relay.ErrNotFound, "recording a poll never creates a repeater")

	require.NoError(t, st.DeleteRepeater("pk2"))
	_, err = st.Repeater("pk2")
	assert.ErrorIs(t, err, relay.ErrNotFound)
	assert.ErrorIs(t, st.DeleteRepeater("pk2"), relay.ErrNotFound)
}

func testRepeaterPolling(t *testing.T, st relay.Storage) {
	require.NoError(t, st.PutRepeater(relay.RepeaterRecord{Pubkey: "pk", Name: "R", Sealed: []byte("s")}, at(0)))

	ps, err := st.RepeaterPollState("pk")
	require.NoError(t, err)
	assert.Equal(t, relay.RepeaterPollState{}, ps, "the zero state when none is saved")

	hib := at(-time.Hour)
	want := relay.RepeaterPollState{Primary: "gw", NextCycle: at(30 * time.Minute), FailedCycles: 2,
		HibernatingSince: &hib, Waiting: "gw: offline", Reading: json.RawMessage(`{"battery_mv":4100}`)}
	require.NoError(t, st.SetRepeaterPollState("pk", want, at(0)))
	want.FailedCycles = 3
	require.NoError(t, st.SetRepeaterPollState("pk", want, at(time.Second)), "overwrites")
	ps, err = st.RepeaterPollState("pk")
	require.NoError(t, err)
	assert.Equal(t, "gw", ps.Primary)
	assert.Equal(t, 3, ps.FailedCycles)
	assert.True(t, ps.NextCycle.Equal(want.NextCycle))
	require.NotNil(t, ps.HibernatingSince)
	assert.True(t, ps.HibernatingSince.Equal(hib))
	assert.JSONEq(t, `{"battery_mv":4100}`, string(ps.Reading))

	rec := func(d time.Duration, gw, outcome string) {
		require.NoError(t, st.RecordRepeaterAttempt(relay.RepeaterAttempt{Pubkey: "pk", At: at(d), Gateway: gw,
			Client: "telemetry", Outcome: outcome, Step: "login", Flooded: true, PathReset: outcome == relay.AttemptOK,
			Duration: 1500 * time.Millisecond}))
	}
	rec(-2*time.Hour, "gw", relay.AttemptOK)
	rec(0, "gw", relay.AttemptNoReply)
	rec(0, "gw2", relay.AttemptOK)
	rec(time.Minute, "gw", relay.AttemptOK)
	require.NoError(t, st.RecordRepeaterAttempt(relay.RepeaterAttempt{Pubkey: "other", At: at(0), Gateway: "gw", Outcome: relay.AttemptOK}))

	as, err := st.RepeaterAttempts("pk", at(-time.Hour))
	require.NoError(t, err)
	require.Len(t, as, 3)
	assert.True(t, as[0].At.Equal(at(0)), "oldest first")
	assert.True(t, as[2].At.Equal(at(time.Minute)))
	assert.Equal(t, "pk", as[0].Pubkey)
	assert.Equal(t, "telemetry", as[0].Client)
	assert.Equal(t, "login", as[0].Step)
	assert.True(t, as[0].Flooded)
	assert.Equal(t, 1500*time.Millisecond, as[0].Duration)
	as, _ = st.RepeaterAttempts("pk", at(-3*time.Hour))
	assert.Len(t, as, 4)

	require.NoError(t, st.DeleteRepeater("pk"))
	as, err = st.RepeaterAttempts("pk", time.Time{})
	require.NoError(t, err)
	assert.Empty(t, as, "deleting a repeater forgets its attempts")
	ps, _ = st.RepeaterPollState("pk")
	assert.Equal(t, relay.RepeaterPollState{}, ps, "and its schedule")
	as, _ = st.RepeaterAttempts("other", time.Time{})
	assert.Len(t, as, 1, "other repeaters keep theirs")
}

func testBridgeLogs(t *testing.T, st relay.Storage) {
	logs, err := st.BridgeLogs("gw", 0, 10)
	require.NoError(t, err)
	assert.NotNil(t, logs)
	assert.Empty(t, logs)

	require.NoError(t, st.AddBridgeLogs("gw", at(0), []relay.BridgeLog{
		{T: "t1", Level: "INFO", Msg: "one", Attrs: map[string]string{"k": "v"}},
		{T: "t2", Level: "WARN", Msg: "two"},
	}))
	require.NoError(t, st.AddBridgeLogs("other", at(0), []relay.BridgeLog{{T: "t", Level: "INFO", Msg: "x"}}))
	require.NoError(t, st.AddBridgeLogs("gw", at(time.Second), []relay.BridgeLog{{T: "t3", Level: "ERROR", Msg: "three"}}))

	logs, err = st.BridgeLogs("gw", 0, 10)
	require.NoError(t, err)
	require.Len(t, logs, 3)
	assert.Equal(t, []string{"one", "two", "three"}, []string{logs[0].Msg, logs[1].Msg, logs[2].Msg}, "oldest first")
	assert.Less(t, logs[0].ID, logs[1].ID)
	assert.Less(t, logs[1].ID, logs[2].ID)
	assert.Equal(t, map[string]string{"k": "v"}, logs[0].Attrs)
	assert.Nil(t, logs[1].Attrs)
	assert.Equal(t, "WARN", logs[1].Level)
	assert.Equal(t, "t2", logs[1].T)
	assert.True(t, logs[2].ReceivedAt.Equal(at(time.Second)))

	after, err := st.BridgeLogs("gw", logs[0].ID, 10)
	require.NoError(t, err)
	require.Len(t, after, 2, "after is exclusive")
	assert.Equal(t, "two", after[0].Msg)

	newest, _ := st.BridgeLogs("gw", 0, 2)
	require.Len(t, newest, 2, "the newest limit lines")
	assert.Equal(t, "two", newest[0].Msg)
	assert.Equal(t, "three", newest[1].Msg)

	none, _ := st.BridgeLogs("gw", logs[2].ID, 10)
	assert.Empty(t, none)
}

func testBridgeLogsCap(t *testing.T, st relay.Storage) {
	total := 0
	for batch := 0; total < relay.MaxLogsPerGateway+30; batch++ {
		es := make([]relay.BridgeLog, 0, 100)
		for range 100 {
			es = append(es, relay.BridgeLog{T: "t", Level: "INFO", Msg: fmt.Sprint(total)})
			total++
		}
		require.NoError(t, st.AddBridgeLogs("gw", at(time.Duration(batch)*time.Second), es))
	}
	logs, err := st.BridgeLogs("gw", 0, 10*relay.MaxLogsPerGateway)
	require.NoError(t, err)
	require.Len(t, logs, relay.MaxLogsPerGateway)
	assert.Equal(t, fmt.Sprint(total-1), logs[len(logs)-1].Msg)
	assert.Equal(t, fmt.Sprint(total-relay.MaxLogsPerGateway), logs[0].Msg, "the oldest went")
}

func testSettings(t *testing.T, st relay.Storage) {
	_, err := st.Setting("bridge_update")
	assert.ErrorIs(t, err, relay.ErrNotFound)
	require.NoError(t, st.SetSetting("bridge_update", []byte(`{"version":"v0.10.0"}`)))
	require.NoError(t, st.SetSetting("bridge_update", []byte(`{"version":"v0.10.1","gateways":["gw"]}`)))
	v, err := st.Setting("bridge_update")
	require.NoError(t, err)
	assert.JSONEq(t, `{"version":"v0.10.1","gateways":["gw"]}`, string(v))
}
