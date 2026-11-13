package relay

import (
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// refusal dials gw with token and returns the HTTP status and body of the
// refused upgrade.
func refusal(t *testing.T, h *harness, gw, token string) (int, string) {
	t.Helper()
	ws, resp, err := h.dial("/"+gw, token, ClientSubprotocol)
	if err == nil {
		ws.Close()
		t.Fatalf("dial %s: allowed", gw)
	}
	require.NotNil(t, resp, err)
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(b))
}

func gatewayEvents(t *testing.T, h *harness, gw, kind string) []string {
	t.Helper()
	evs, err := h.store.GatewayEvents(gw, 100)
	require.NoError(t, err)
	var out []string
	for _, e := range evs {
		if e.Kind == kind {
			out = append(out, e.Detail)
		}
	}
	return out
}

// TestHostTokens: a host token opens its own gateway and no other.
func TestHostTokens(t *testing.T) {
	h := newHarness(t, Config{})
	tokA := h.createGateway("gw-a")
	h.createGateway("gw-b")
	b := h.bridge("gw-a", tokA, pubkeyA)

	code, out := h.api("POST", "/clients", h.admin, map[string]any{"role": "host", "gateway": "gw-a", "label": "Jay-Laptop",
		"note": "Jay's laptop"})
	require.Equal(t, http.StatusOK, code, out)
	host := out["token"].(string)
	assert.True(t, strings.HasPrefix(host, HostTokenPrefix))
	assert.Equal(t, "gw-a.jay-laptop", out["client"])
	assert.Equal(t, "gw-a", out["gateway"])
	exp, err := time.Parse(time.RFC3339, out["expires_at"].(string))
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(HostTokenDefaultDays*24*time.Hour), exp, time.Minute)
	eventually(t, 2*time.Second, func() bool {
		ev := gatewayEvents(t, h, "gw-a", "access_issued")
		return len(ev) == 1 && ev[0] == "gw-a.jay-laptop by root"
	}, "no access_issued event")

	// Its own gateway: a lease, attached under its name.
	c := h.client("gw-a", host, "")
	expectControl(t, c, "lease")
	assert.Equal(t, "gw-a.jay-laptop", b.expect("attach")["client"])

	// Not another gateway, and no preempting.
	code, body := refusal(t, h, "gw-b", host)
	assert.Equal(t, http.StatusForbidden, code)
	assert.Equal(t, "this token is for gateway gw-a, not gw-b", body)
	code, _ = refusal(t, h, "gw-a?preempt=1", host)
	assert.Equal(t, http.StatusForbidden, code)

	// Revoking it ends its lease.
	code, out = h.api("DELETE", "/clients/gw-a.jay-laptop", h.admin, nil)
	require.Equal(t, http.StatusOK, code, out)
	assert.Equal(t, 1.0, out["leases_ended"])
	expectControl(t, c, "lease_ending")
	code, _ = refusal(t, h, "gw-a", host)
	assert.Equal(t, http.StatusUnauthorized, code)
}

func TestHostTokenIssuing(t *testing.T) {
	h := newHarness(t, Config{})
	h.createGateway("gw-a")
	for _, tc := range []struct {
		body map[string]any
		code int
		want string
	}{
		{map[string]any{"role": "host", "label": "x"}, http.StatusBadRequest, "needs gateway and label"},
		{map[string]any{"role": "host", "gateway": "gw-a"}, http.StatusBadRequest, "needs gateway and label"},
		{map[string]any{"role": "host", "gateway": "gw-a", "label": "x", "name": "root"}, http.StatusBadRequest, "not name"},
		{map[string]any{"role": "host", "gateway": "nowhere", "label": "x"}, http.StatusNotFound, "unknown gateway"},
		{map[string]any{"role": "host", "gateway": "gw-a", "label": "has space"}, http.StatusBadRequest, "lowercase"},
		{map[string]any{"role": "host", "gateway": "gw-a", "label": "x", "expires_in_days": 9999}, http.StatusBadRequest, "1 to 730 days"},
		{map[string]any{"role": "admin", "name": "x", "gateway": "gw-a"}, http.StatusBadRequest, "isn't scoped"},
	} {
		code, out := h.api("POST", "/clients", h.admin, tc.body)
		assert.Equal(t, tc.code, code, tc.body)
		assert.Contains(t, out["message"], tc.want, tc.body)
	}

	// At most MaxHostTokensPerGateway at once.
	for i := range MaxHostTokensPerGateway {
		h.hostToken("gw-a", "d"+string(rune('a'+i)))
	}
	code, out := h.api("POST", "/clients", h.admin, map[string]any{"role": "host", "gateway": "gw-a", "label": "one-more"})
	assert.Equal(t, http.StatusConflict, code)
	assert.Contains(t, out["message"], "the most it can have")
	code, _ = h.api("POST", "/clients", h.admin, map[string]any{"role": "host", "gateway": "gw-a", "label": "da"})
	assert.Equal(t, http.StatusConflict, code)
	h.api("DELETE", "/clients/gw-a.da", h.admin, nil)
	h.hostToken("gw-a", "one-more")

	// An admin token can expire too.
	code, out = h.api("POST", "/clients", h.admin, map[string]any{"name": "temp", "role": "admin", "expires_in_days": 30})
	require.Equal(t, http.StatusOK, code, out)
	assert.True(t, strings.HasPrefix(out["token"].(string), ClientTokenPrefix))
	assert.NotEmpty(t, out["expires_at"])
}

// A token's prefix must match its role, so a host token can't pass as any
// other kind and a relay without host tokens rejects them.
func TestTokenPrefixMatchesRole(t *testing.T) {
	h := newHarness(t, Config{})
	tok := h.createGateway("gw-a")
	h.bridge("gw-a", tok, pubkeyA)

	smh, err := MintToken(HostTokenPrefix)
	require.NoError(t, err)
	require.NoError(t, h.store.CreateClient(ClientRecord{Name: "odd-admin", Role: RoleAdmin, CreatedAt: time.Now()}, TokenHash(smh)))
	code, _ := refusal(t, h, "gw-a", smh)
	assert.Equal(t, http.StatusUnauthorized, code, "an smh_ token that isn't a host token")
	code, _ = h.api("GET", "/gateways", smh, nil)
	assert.Equal(t, http.StatusUnauthorized, code)

	smc, err := MintToken(ClientTokenPrefix)
	require.NoError(t, err)
	require.NoError(t, h.store.CreateClient(ClientRecord{Name: "gw-a.odd", Role: RoleHost, Gateway: "gw-a", CreatedAt: time.Now()},
		TokenHash(smc)))
	code, _ = refusal(t, h, "gw-a", smc)
	assert.Equal(t, http.StatusUnauthorized, code, "a host token without the smh_ prefix")
}

// An expired token is refused with its own message, without counting
// toward the IP lockout (it's genuine).
func TestExpiredToken(t *testing.T) {
	h := newHarness(t, Config{})
	tok := h.createGateway("gw-a")
	h.bridge("gw-a", tok, pubkeyA)
	smh, err := MintToken(HostTokenPrefix)
	require.NoError(t, err)
	require.NoError(t, h.store.CreateClient(ClientRecord{Name: "gw-a.old", Role: RoleHost, Gateway: "gw-a",
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(-time.Minute)}, TokenHash(smh)))
	smc, err := MintToken(ClientTokenPrefix)
	require.NoError(t, err)
	require.NoError(t, h.store.CreateClient(ClientRecord{Name: "old-admin", Role: RoleAdmin,
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(-time.Minute)}, TokenHash(smc)))

	failures := h.relay.m.authFailures.Load()
	for range 10 { // more than the lockout allows
		code, body := refusal(t, h, "gw-a", smh)
		assert.Equal(t, http.StatusUnauthorized, code)
		assert.Contains(t, body, "expired")
		code, out := h.api("GET", "/gateways", smc, nil)
		assert.Equal(t, http.StatusUnauthorized, code)
		assert.Contains(t, out["message"], "expired")
	}
	assert.Equal(t, failures, h.relay.m.authFailures.Load(), "expired tokens aren't auth failures")
	c := h.client("gw-a", h.admin, "")
	expectControl(t, c, "lease")
}

// Retired roles can't lease; an admin turns such a token into an admin
// token (PATCH), and it works again.
func TestRetiredRoleAndPatch(t *testing.T) {
	h := newHarness(t, Config{})
	tok := h.createGateway("gw-a")
	h.bridge("gw-a", tok, pubkeyA)
	dev := h.legacyClient("bridge-dev-client", RoleOperator)

	code, body := refusal(t, h, "gw-a", dev)
	assert.Equal(t, http.StatusForbidden, code)
	assert.Contains(t, body, "operator role no longer work")

	code, out := h.api("PATCH", "/clients/bridge-dev-client", h.admin, map[string]any{"role": "host"})
	assert.Equal(t, http.StatusBadRequest, code, out)
	code, out = h.api("PATCH", "/clients/nobody", h.admin, map[string]any{"role": "admin"})
	assert.Equal(t, http.StatusNotFound, code, out)
	code, out = h.api("PATCH", "/clients/bridge-dev-client", h.admin, map[string]any{"role": "admin"})
	require.Equal(t, http.StatusOK, code, out)
	assert.Equal(t, "operator", out["was"])
	c := h.client("gw-a", dev, "")
	expectControl(t, c, "lease")

	h.hostToken("gw-a", "jay")
	code, out = h.api("PATCH", "/clients/gw-a.jay", h.admin, map[string]any{"role": "admin"})
	assert.Equal(t, http.StatusConflict, code, out)
}

// A host token is revoked when its gateway's bridge token is rotated, when
// the gateway is re-paired or deleted; a token from before the gateway
// (its name reused) never opens it.
func TestHostTokenRevocation(t *testing.T) {
	h := newHarness(t, Config{})
	tok := h.createGateway("gw-a")
	b := h.bridge("gw-a", tok, pubkeyA)

	host := h.hostToken("gw-a", "jay")
	c := h.client("gw-a", host, "")
	expectControl(t, c, "lease")
	b.expect("attach")
	code, out := h.api("POST", "/gateways/gw-a/rotate-token", h.admin, nil)
	require.Equal(t, http.StatusOK, code, out)
	assert.Equal(t, 1.0, out["access_tokens_revoked"])
	code, _ = refusal(t, h, "gw-a", host)
	assert.Equal(t, http.StatusUnauthorized, code, "revoked by the rotation")
	eventually(t, 2*time.Second, func() bool {
		ev := gatewayEvents(t, h, "gw-a", "access_revoked")
		return len(ev) == 1 && ev[0] == "1 token(s): bridge token rotated by root"
	}, "no access_revoked event")
	expectClosed(t, c, CloseNormal)

	host = h.hostToken("gw-a", "jay")
	_, repaired, err := h.relay.issueGateway("gw-a", "", "", true)
	require.NoError(t, err)
	require.True(t, repaired)
	code, _ = refusal(t, h, "gw-a", host)
	assert.Equal(t, http.StatusUnauthorized, code, "revoked by the re-pair")

	host = h.hostToken("gw-a", "jay")
	code, _ = h.api("DELETE", "/gateways/gw-a", h.admin, nil)
	require.Equal(t, http.StatusOK, code)
	h.createGateway("gw-a")
	code, _ = refusal(t, h, "gw-a", host)
	assert.Equal(t, http.StatusUnauthorized, code, "deleted with its gateway")

	// Belt and braces: a scoped token older than the gateway (as a store
	// that missed the cascade would leave it) is refused.
	smh, err := MintToken(HostTokenPrefix)
	require.NoError(t, err)
	require.NoError(t, h.store.CreateClient(ClientRecord{Name: "gw-a.stale", Role: RoleHost, Gateway: "gw-a",
		CreatedAt: time.Now().Add(-time.Hour)}, TokenHash(smh)))
	code, body := refusal(t, h, "gw-a", smh)
	assert.Equal(t, http.StatusForbidden, code)
	assert.Contains(t, body, "this token is for gateway gw-a")
}

func TestLastUse(t *testing.T) {
	h := newHarness(t, Config{})
	tok := h.createGateway("gw-a")
	h.bridge("gw-a", tok, pubkeyA)
	host := h.hostToken("gw-a", "jay")
	c := h.client("gw-a", host, "")
	expectControl(t, c, "lease")
	eventually(t, 2*time.Second, func() bool {
		cs, err := h.store.Clients()
		return err == nil && len(cs) == 1 && !cs[0].LastUsedAt.IsZero()
	}, "last use never recorded")
}

// flakyStore fails the operations a test switches on.
type flakyStore struct {
	Storage
	failDelete     atomic.Bool
	goneAfterFirst atomic.Bool
	tokenLookups   atomic.Int32
}

func (f *flakyStore) DeleteClient(name string) error {
	if f.failDelete.Load() {
		return errors.New("store unavailable")
	}
	return f.Storage.DeleteClient(name)
}

func (f *flakyStore) ClientByToken(hash string) (*ClientRecord, error) {
	if f.tokenLookups.Add(1) > 1 && f.goneAfterFirst.Load() {
		return nil, ErrNotFound
	}
	return f.Storage.ClientByToken(hash)
}

func flakyHarness(t *testing.T, cfg Config) (*harness, *flakyStore) {
	t.Helper()
	var f *flakyStore
	h := newHarnessWith(t, cfg, filepath.Join(t.TempDir(), "relay.db"), func(s Storage) Storage {
		f = &flakyStore{Storage: s}
		return f
	})
	return h, f
}

// Revocation fails closed: if the host tokens can't be revoked, the bridge
// token isn't rotated and the gateway isn't re-paired, so nothing is left
// half done with the old tokens still live.
func TestRevocationFailsClosed(t *testing.T) {
	h, flaky := flakyHarness(t, Config{})
	tok := h.createGateway("gw-a")
	h.bridge("gw-a", tok, pubkeyA)
	host := h.hostToken("gw-a", "jay")
	before, err := h.store.Gateway("gw-a")
	require.NoError(t, err)

	flaky.failDelete.Store(true)
	code, out := h.api("POST", "/gateways/gw-a/rotate-token", h.admin, nil)
	assert.Equal(t, http.StatusInternalServerError, code, out)
	_, _, err = h.relay.issueGateway("gw-a", "", "", true)
	assert.Error(t, err, "re-pair")
	flaky.failDelete.Store(false)

	after, err := h.store.Gateway("gw-a")
	require.NoError(t, err)
	assert.Equal(t, before.TokenHash, after.TokenHash, "the bridge token wasn't changed")
	assert.Equal(t, true, gatewayStatus(h, "gw-a")["online"], "the bridge wasn't disconnected")
	c := h.client("gw-a", host, "")
	expectControl(t, c, "lease")
}

// A token revoked between its check and its lease (when revoking found no
// lease to end) loses the lease as soon as it's granted.
func TestRevokedWhileGranted(t *testing.T) {
	h, flaky := flakyHarness(t, Config{})
	tok := h.createGateway("gw-a")
	b := h.bridge("gw-a", tok, pubkeyA)
	host := h.hostToken("gw-a", "jay")
	flaky.tokenLookups.Store(0)
	flaky.goneAfterFirst.Store(true)
	c := h.client("gw-a", host, "")
	expectControl(t, c, "lease")
	b.expect("attach")
	m := expectControl(t, c, "lease_ending")
	assert.Equal(t, ReasonRevoked, m["reason"])
}

// A lease never outlasts its token: it ends when the token expires, and
// renewing past that is refused.
func TestLeaseEndsWithItsToken(t *testing.T) {
	h := newHarness(t, Config{LeaseDuration: time.Hour, LeaseWarning: 500 * time.Millisecond})
	tok := h.createGateway("gw-a")
	h.bridge("gw-a", tok, pubkeyA)
	smh, err := MintToken(HostTokenPrefix)
	require.NoError(t, err)
	expires := time.Now().Add(1500 * time.Millisecond)
	require.NoError(t, h.store.CreateClient(ClientRecord{Name: "gw-a.short", Role: RoleHost, Gateway: "gw-a",
		CreatedAt: time.Now(), ExpiresAt: expires}, TokenHash(smh)))

	c := h.client("gw-a", smh, "")
	lease := expectControl(t, c, "lease")
	at, err := time.Parse(time.RFC3339, lease["expires_at"].(string))
	require.NoError(t, err)
	assert.False(t, at.After(expires), "the lease runs to %s, past the token's %s", at, expires)

	require.NoError(t, c.WriteJSON(map[string]any{"type": "renew", "lease": lease["lease"]}))
	m := expectControl(t, c, "error")
	assert.Equal(t, "renew_denied", m["code"])
	assert.Contains(t, m["message"], "token")
	m = expectControl(t, c, "lease_ending")
	assert.Equal(t, ReasonExpiry, m["reason"])
	expectClosed(t, c, CloseNormal)
}

func TestPublicRole(t *testing.T) {
	assert.Equal(t, RoleOperator, publicRole(RoleHost))
	assert.Equal(t, RoleOperator, publicRole(RoleAdmin), "a person's session, whoever")
	assert.Equal(t, RoleAgent, publicRole(RoleAgent), "an automated session")
}

// Expired host tokens don't hold the gateway's slots or labels.
func TestExpiredTokensFreeTheirSlots(t *testing.T) {
	h := newHarness(t, Config{})
	h.createGateway("gw-a")
	for i := range MaxHostTokensPerGateway {
		smh, err := MintToken(HostTokenPrefix)
		require.NoError(t, err)
		require.NoError(t, h.store.CreateClient(ClientRecord{Name: "gw-a.old" + string(rune('a'+i)), Role: RoleHost,
			Gateway: "gw-a", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(-time.Minute)}, TokenHash(smh)))
	}
	h.hostToken("gw-a", "olda") // a slot and an expired token's label
	cs, err := h.relay.HostTokens("gw-a")
	require.NoError(t, err)
	assert.Len(t, cs, 1, "the expired ones were cleared")

	code, out := h.api("POST", "/clients", h.admin, map[string]any{"name": "gw-a.x", "role": "admin"})
	assert.Equal(t, http.StatusBadRequest, code, "an admin name with a dot reads as a host token")
	assert.Contains(t, out["message"], "without dots")
}
