package relay

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dpup/prefab/errors"
	"github.com/dpup/prefab/logging"
	"golang.org/x/crypto/bcrypt"
	"google.golang.org/grpc/codes"

	"github.com/sierragridteam/sierra-meshrelay/meshproto"
)

// Pairing gives a fresh bridge its token without anyone copying a secret.
// See PROTOCOL.md "Pairing".
//
//   - Device code: pair/start returns a short code; an admin approves it
//     (POST /api/pairings/{code}/approve); the bridge's pair/poll then gets
//     the token, exactly once. Pending pairings live in memory only: a relay
//     restart drops them and the bridge's next poll reads "expired".
//   - Passphrase: an admin creates an invite ahead of time (POST /api/invites,
//     passphrase shown once, stored as a bcrypt hash); pair/start with the
//     passphrase is approved on the spot.
//
// Approval mints the gateway's bridge token at once (creating the gateway, or
// re-pairing an existing one, which rotates its token and closes the old
// session with 4001). A device-code token waits in memory for the poll that
// collects it; it is never written to the store, which keeps only its hash.

// PairingConfig tunes pairing. Zero values take the defaults in brackets.
type PairingConfig struct {
	CodeTTL      time.Duration // how long a device code stays valid [10m]
	PollInterval time.Duration // the poll interval bridges are told [5s]
	MaxPending   int           // pending device codes across all IPs [50]

	StartsPerIP int           // pair/start calls per IP per StartWindow [20]
	StartWindow time.Duration // [10m]

	PassphrasesPerIP int           // passphrase attempts per IP per PassphraseWindow [10]
	PassphraseGlobal int           // passphrase attempts in total per PassphraseWindow [200]
	PassphraseWindow time.Duration // [1h]

	InviteTTL    time.Duration // default invite lifetime [7d]
	InviteMaxTTL time.Duration // longest allowed [30d]
	BcryptCost   int           // passphrase hash cost [bcrypt.DefaultCost]
}

func (c PairingConfig) withDefaults() PairingConfig {
	def := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	defInt := func(n *int, v int) {
		if *n == 0 {
			*n = v
		}
	}
	def(&c.CodeTTL, 10*time.Minute)
	def(&c.PollInterval, 5*time.Second)
	def(&c.StartWindow, 10*time.Minute)
	def(&c.PassphraseWindow, time.Hour)
	def(&c.InviteTTL, 7*24*time.Hour)
	def(&c.InviteMaxTTL, 30*24*time.Hour)
	defInt(&c.MaxPending, 50)
	defInt(&c.StartsPerIP, 20)
	defInt(&c.PassphrasesPerIP, 10)
	defInt(&c.PassphraseGlobal, 200)
	defInt(&c.BcryptCost, bcrypt.DefaultCost)
	return c
}

// pairing is one pending device-code request.
type pairing struct {
	id        string
	userCode  string // canonical XXX-XXX
	pollHash  [32]byte
	created   time.Time
	expires   time.Time
	lastPoll  time.Time
	ip        string
	hostLabel string
	agent     Agent
	node      meshproto.PairNode

	status string // meshproto.PairPending / PairApproved / PairDenied
	result meshproto.PairResult
}

// pairingState is the relay's pairing bookkeeping. Guarded by mu, which also
// serializes approvals and redemptions so two can't claim one gateway name.
type pairingState struct {
	mu      sync.Mutex
	byID    map[string]*pairing
	byCode  map[string]*pairing
	starts  *windowLimiter
	phrases *windowLimiter
	global  *windowLimiter
}

func newPairingState(c PairingConfig) *pairingState {
	return &pairingState{
		byID: map[string]*pairing{}, byCode: map[string]*pairing{},
		starts:  newWindowLimiter(c.StartsPerIP, c.StartWindow),
		phrases: newWindowLimiter(c.PassphrasesPerIP, c.PassphraseWindow),
		global:  newWindowLimiter(c.PassphraseGlobal, c.PassphraseWindow),
	}
}

// pruneLocked drops expired device codes.
func (p *pairingState) pruneLocked(now time.Time) {
	for id, pr := range p.byID {
		if !now.Before(pr.expires) {
			delete(p.byID, id)
			delete(p.byCode, pr.userCode)
		}
	}
}

func (p *pairingState) removeLocked(pr *pairing) {
	delete(p.byID, pr.id)
	delete(p.byCode, pr.userCode)
}

// windowLimiter allows limit events per key per window (a sliding log; the
// volumes here are tiny).
type windowLimiter struct {
	limit  int
	window time.Duration
	hits   map[string][]time.Time
}

func newWindowLimiter(limit int, window time.Duration) *windowLimiter {
	return &windowLimiter{limit: limit, window: window, hits: map[string][]time.Time{}}
}

// allow records an event for key and reports whether it is within the limit.
func (w *windowLimiter) allow(key string, now time.Time) bool {
	h := w.hits[key]
	cut := 0
	for cut < len(h) && now.Sub(h[cut]) >= w.window {
		cut++
	}
	h = h[cut:]
	if len(h) >= w.limit {
		w.hits[key] = h
		return false
	}
	w.hits[key] = append(h, now)
	return true
}

// --- codes, secrets and passphrases ---------------------------------------------

// userCodeAlphabet has no 0 O 1 I L, so a code read aloud or retyped from a
// text message survives.
const userCodeAlphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"

func randomUserCode() string {
	var b [6]byte
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(userCodeAlphabet))))
		b[i] = userCodeAlphabet[n.Int64()]
	}
	return string(b[:3]) + "-" + string(b[3:])
}

// normalizeUserCode accepts "k7m4qx", "K7M-4QX" or "k7m 4qx".
func normalizeUserCode(s string) string {
	s = strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(s)))
	if len(s) != 6 {
		return ""
	}
	return s[:3] + "-" + s[3:]
}

func randomSecret() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

//go:embed wordlist_bip39.txt
var wordlistRaw string

// passphraseWords is the BIP-39 English wordlist (2,048 short, common,
// unambiguous words; MIT licensed, github.com/bitcoin/bips bip-0039). Four
// words carry 44 bits.
var passphraseWords = func() []string {
	w := strings.Fields(wordlistRaw)
	if len(w) != 2048 {
		panic(fmt.Sprintf("relay: passphrase wordlist has %d words, want 2048", len(w)))
	}
	return w
}()

func randomPassphrase() string {
	words := make([]string, 4)
	for i := range words {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(passphraseWords))))
		words[i] = passphraseWords[n.Int64()]
	}
	return strings.Join(words, "-")
}

// relayURL is the bridge's relay base URL, derived from PublicBaseURL.
func (r *Relay) relayURL() string {
	u := strings.TrimRight(r.cfg.PublicBaseURL, "/")
	switch {
	case strings.HasPrefix(u, "https://"):
		u = "wss://" + strings.TrimPrefix(u, "https://")
	case strings.HasPrefix(u, "http://"):
		u = "ws://" + strings.TrimPrefix(u, "http://")
	}
	return u + PathPrefix
}

// --- bridge endpoints (no auth) --------------------------------------------------

const maxPairBody = 8 << 10

func writePairJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func pairError(w http.ResponseWriter, status int, code string) {
	writePairJSON(w, status, meshproto.PairError{Error: code})
}

// handlePairStart serves POST /mesh/relay/api/pair/start.
func (r *Relay) handlePairStart(w http.ResponseWriter, req *http.Request) {
	var body meshproto.PairStartRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, maxPairBody)).Decode(&body); err != nil {
		pairError(w, http.StatusBadRequest, "bad_request")
		return
	}
	now, ip := r.now(), r.clientIP(req)
	body.HostLabel = truncate(body.HostLabel, 80)
	p := r.pairs
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.starts.allow(ip, now) {
		pairError(w, http.StatusTooManyRequests, "rate_limited")
		return
	}
	if body.Passphrase != "" {
		if !p.phrases.allow(ip, now) || !p.global.allow("", now) {
			pairError(w, http.StatusTooManyRequests, "rate_limited")
			return
		}
		res, err := r.redeemInviteLocked(body, ip, now)
		if stderrors.Is(err, errInvalidPassphrase) {
			logging.Infow(r.ctx, "pairing: passphrase refused", "ip", ip, "host_label", body.HostLabel)
			pairError(w, http.StatusForbidden, meshproto.ErrInvalidPassphrase)
			return
		}
		if err != nil {
			logging.Errorw(r.ctx, "pairing: redeeming invite", "error", err)
			pairError(w, http.StatusInternalServerError, "internal_error")
			return
		}
		writePairJSON(w, http.StatusOK, res)
		return
	}

	p.pruneLocked(now)
	if len(p.byID) >= r.cfg.Pairing.MaxPending {
		pairError(w, http.StatusTooManyRequests, "too_many_pending")
		return
	}
	pr := &pairing{
		id: randomID("p_"), created: now, expires: now.Add(r.cfg.Pairing.CodeTTL),
		ip: ip, hostLabel: body.HostLabel, agent: body.Agent, node: body.Node, status: meshproto.PairPending,
	}
	for {
		pr.userCode = randomUserCode()
		if p.byCode[pr.userCode] == nil {
			break
		}
	}
	secret := randomSecret()
	pr.pollHash = sha256.Sum256([]byte(secret))
	p.byID[pr.id], p.byCode[pr.userCode] = pr, pr
	logging.Infow(r.ctx, "pairing: device code issued", "code", pr.userCode, "ip", ip,
		"host_label", pr.hostLabel, "node_state", pr.node.State, "node_pubkey", pr.node.Pubkey)
	writePairJSON(w, http.StatusOK, meshproto.PairStartResponse{
		PairID: pr.id, PollSecret: secret, UserCode: pr.userCode,
		VerificationURL: strings.TrimRight(r.cfg.PublicBaseURL, "/") + "/mesh/pair?code=" + pr.userCode,
		ExpiresIn:       int(r.cfg.Pairing.CodeTTL / time.Second),
		Interval:        int(r.cfg.Pairing.PollInterval / time.Second),
	})
}

// handlePairPoll serves POST /mesh/relay/api/pair/poll.
func (r *Relay) handlePairPoll(w http.ResponseWriter, req *http.Request) {
	var body meshproto.PairPollRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, maxPairBody)).Decode(&body); err != nil {
		pairError(w, http.StatusBadRequest, "bad_request")
		return
	}
	now := r.now()
	p := r.pairs
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pruneLocked(now)
	pr := p.byID[body.PairID]
	got := sha256.Sum256([]byte(body.PollSecret))
	// Unknown, expired, consumed and wrong-secret all read "expired": the
	// code alone can never collect the token.
	if pr == nil || subtle.ConstantTimeCompare(got[:], pr.pollHash[:]) != 1 {
		writePairJSON(w, http.StatusOK, meshproto.PairResult{Status: meshproto.PairExpired})
		return
	}
	if !pr.lastPoll.IsZero() && now.Sub(pr.lastPoll) < r.cfg.Pairing.PollInterval/2 {
		pairError(w, http.StatusTooManyRequests, "slow_down")
		return
	}
	pr.lastPoll = now
	switch pr.status {
	case meshproto.PairApproved:
		p.removeLocked(pr) // delivered exactly once
		logging.Infow(r.ctx, "pairing: token delivered", "code", pr.userCode, "gateway", pr.result.Gateway)
		writePairJSON(w, http.StatusOK, pr.result)
	case meshproto.PairDenied:
		p.removeLocked(pr)
		writePairJSON(w, http.StatusOK, meshproto.PairResult{Status: meshproto.PairDenied})
	default:
		writePairJSON(w, http.StatusOK, meshproto.PairResult{Status: meshproto.PairPending})
	}
}

var errInvalidPassphrase = stderrors.New("invalid passphrase")

// redeemInviteLocked approves a pair/start that carries a passphrase. Every
// failure is errInvalidPassphrase, whatever the reason.
func (r *Relay) redeemInviteLocked(body meshproto.PairStartRequest, ip string, now time.Time) (meshproto.PairResult, error) {
	phrase := []byte(meshproto.NormalizePassphrase(body.Passphrase))
	invites, err := r.store.ActiveInvites(now)
	if err != nil {
		return meshproto.PairResult{}, err
	}
	var inv *InviteRecord
	for _, i := range invites {
		if bcrypt.CompareHashAndPassword([]byte(i.PassphraseHash), phrase) == nil {
			inv = i
			break
		}
	}
	if inv == nil {
		return meshproto.PairResult{}, errInvalidPassphrase
	}
	reported, reportedOK := "", false
	if body.Node.State == NodeConnected {
		reported, reportedOK = NormalizePubkey(body.Node.Pubkey)
	}
	pinned := ""
	switch {
	case inv.ExpectedPubkey != "":
		if !reportedOK || reported != inv.ExpectedPubkey {
			return meshproto.PairResult{}, errInvalidPassphrase
		}
		pinned = inv.ExpectedPubkey
	case reportedOK:
		pinned = reported
	}
	name := inv.Gateway
	switch {
	case name != "":
	case inv.NamePrefix != "":
		if name, err = r.nextGatewayName(inv.NamePrefix, inv.Uses+1); err != nil {
			return meshproto.PairResult{}, err
		}
	default:
		if name, err = r.suggestGatewayName(body.Node.Name, body.HostLabel); err != nil {
			return meshproto.PairResult{}, err
		}
	}
	if err := r.store.UseInvite(inv.ID, now); stderrors.Is(err, ErrNotFound) {
		return meshproto.PairResult{}, errInvalidPassphrase // the last use went to someone else
	} else if err != nil {
		return meshproto.PairResult{}, err
	}
	tok, repaired, err := r.issueGateway(name, pinned, inv.Note, true)
	if err != nil {
		return meshproto.PairResult{}, err
	}
	detail := fmt.Sprintf("invite %s from %s (%s)", inv.ID, ip, body.HostLabel)
	r.event(name, map[bool]string{false: "paired", true: "repaired"}[repaired], detail)
	logging.Warnw(r.ctx, "pairing: invite redeemed (admins: check this was expected)", "gateway", name,
		"invite", inv.ID, "ip", ip, "host_label", body.HostLabel, "node_pubkey", reported, "repaired", repaired)
	return meshproto.PairResult{Status: meshproto.PairApproved, Gateway: name, Token: tok,
		Relay: r.relayURL(), ExpectedPubkey: pinned}, nil
}

// slugify turns a node or computer name into a gateway name candidate:
// lowercase, runs of anything but [a-z0-9] become one hyphen, trimmed to
// fit the name rules (leaving room for a -N suffix).
func slugify(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 36 {
		out = strings.Trim(out[:36], "-")
	}
	return out
}

// suggestGatewayName picks a free gateway name from the first usable
// candidate (node name, then computer name), adding -2, -3… if it's taken.
func (r *Relay) suggestGatewayName(candidates ...string) (string, error) {
	base := "gateway"
	for _, c := range candidates {
		if s := slugify(c); ValidGatewayName(s) {
			base = s
			break
		}
	}
	for n := 1; n < 1000; n++ {
		name := base
		if n > 1 {
			name = base + "-" + strconv.Itoa(n)
		}
		if !ValidGatewayName(name) {
			continue
		}
		if _, err := r.store.Gateway(name); stderrors.Is(err, ErrNotFound) {
			return name, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("no free gateway name for %q", base)
}

// nextGatewayName finds the first free <prefix>-<n>, starting at n.
func (r *Relay) nextGatewayName(prefix string, n int) (string, error) {
	for ; n < 10000; n++ {
		name := prefix + "-" + strconv.Itoa(n)
		if _, err := r.store.Gateway(name); stderrors.Is(err, ErrNotFound) {
			return name, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("no free gateway name for prefix %q", prefix)
}

// issueGateway mints a bridge token for name, creating the gateway or (when
// allowRepair) re-pairing an existing one: its token rotates, the old bridge
// session closes with 4001, and a non-empty pinned replaces the pin.
func (r *Relay) issueGateway(name, pinned, notes string, allowRepair bool) (token string, repaired bool, err error) {
	if token, err = MintToken(BridgeTokenPrefix); err != nil {
		return "", false, err
	}
	err = r.store.CreateGateway(name, TokenHash(token), pinned, notes, r.now())
	if !stderrors.Is(err, ErrExists) {
		return token, false, err
	}
	if !allowRepair {
		return "", false, err
	}
	if err := r.store.SetGatewayToken(name, TokenHash(token)); err != nil {
		return "", false, err
	}
	if pinned != "" {
		r.pubkeyMu.Lock()
		err = r.store.SetPubkeys(name, pinned, "")
		r.pubkeyMu.Unlock()
		if err != nil {
			return "", false, err
		}
	}
	r.closeBridge(name, CloseBadToken, "gateway re-paired")
	return token, true, nil
}

// --- admin endpoints -----------------------------------------------------------

// PendingPairing is one device code waiting for an admin.
type PendingPairing struct {
	UserCode  string             `json:"user_code"`
	HostLabel string             `json:"host_label,omitempty"`
	IP        string             `json:"ip"`
	Agent     Agent              `json:"agent"`
	Node      meshproto.PairNode `json:"node"`
	Status    string             `json:"status"`
	// SuggestedGateway is the name approve uses when none is given.
	SuggestedGateway string    `json:"suggested_gateway"`
	CreatedAt        time.Time `json:"created_at"`
	ExpiresAt        time.Time `json:"expires_at"`
}

func (r *Relay) apiListPairings(_ *http.Request, _ *ClientRecord) (any, error) {
	now := r.now()
	p := r.pairs
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pruneLocked(now)
	out := make([]PendingPairing, 0, len(p.byID))
	for _, pr := range p.byID {
		suggested, _ := r.suggestGatewayName(pr.node.Name, pr.hostLabel)
		out = append(out, PendingPairing{UserCode: pr.userCode, HostLabel: pr.hostLabel, IP: pr.ip,
			Agent: pr.agent, Node: pr.node, Status: pr.status, SuggestedGateway: suggested,
			CreatedAt: pr.created.UTC(), ExpiresAt: pr.expires.UTC()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return map[string]any{"pairings": out}, nil
}

func (r *Relay) pendingByCode(code string) (*pairing, error) {
	r.pairs.pruneLocked(r.now())
	pr := r.pairs.byCode[normalizeUserCode(code)]
	if pr == nil {
		return nil, errors.NewC("no pending pairing with that code (it may have expired)", codes.NotFound)
	}
	if pr.status != meshproto.PairPending {
		return nil, conflict("that pairing was already " + pr.status)
	}
	return pr, nil
}

func (r *Relay) apiApprovePairing(req *http.Request, who *ClientRecord) (any, error) {
	var body struct {
		Gateway        string `json:"gateway"`
		ExpectedPubkey string `json:"expected_pubkey"`
		Notes          string `json:"notes"`
		// Repair must be set to approve onto an existing gateway, which
		// rotates its token and replaces the bridge using it.
		Repair bool `json:"repair"`
	}
	if err := decodeBody(req, &body); err != nil {
		return nil, err
	}
	p := r.pairs
	p.mu.Lock()
	defer p.mu.Unlock()
	pr, err := r.pendingByCode(req.PathValue("code"))
	if err != nil {
		return nil, err
	}
	if body.Gateway == "" {
		// No name given: name it after the node (or the computer).
		if body.Gateway, err = r.suggestGatewayName(pr.node.Name, pr.hostLabel); err != nil {
			return nil, errors.Wrap(err, 0)
		}
	}
	if !ValidGatewayName(body.Gateway) {
		return nil, errors.NewC("gateway name must match [a-z0-9-]{3,40} and not be reserved", codes.InvalidArgument)
	}
	pinned := ""
	switch {
	case body.ExpectedPubkey != "":
		pk, ok := NormalizePubkey(body.ExpectedPubkey)
		if !ok {
			return nil, errors.NewC("expected_pubkey must be 64 hex characters", codes.InvalidArgument)
		}
		pinned = pk
	case pr.node.State == NodeConnected:
		if pk, ok := NormalizePubkey(pr.node.Pubkey); ok {
			pinned = pk
		}
	}
	tok, repaired, err := r.issueGateway(body.Gateway, pinned, body.Notes, body.Repair)
	if stderrors.Is(err, ErrExists) {
		return nil, conflict("gateway " + body.Gateway + " already exists; set repair:true to re-pair it (rotates its token)")
	}
	if err != nil {
		return nil, errors.Wrap(err, 0)
	}
	pr.status = meshproto.PairApproved
	pr.result = meshproto.PairResult{Status: meshproto.PairApproved, Gateway: body.Gateway, Token: tok,
		Relay: r.relayURL(), ExpectedPubkey: pinned}
	r.event(body.Gateway, map[bool]string{false: "paired", true: "repaired"}[repaired],
		fmt.Sprintf("code %s approved by %s (%s, %s)", pr.userCode, who.Name, pr.ip, pr.hostLabel))
	rec, err := r.store.Gateway(body.Gateway)
	if err != nil {
		return nil, errors.Wrap(err, 0)
	}
	return map[string]any{"gateway": r.status(rec), "repaired": repaired,
		"note": "the bridge collects its token on its next poll"}, nil
}

func (r *Relay) apiDenyPairing(req *http.Request, who *ClientRecord) (any, error) {
	p := r.pairs
	p.mu.Lock()
	defer p.mu.Unlock()
	pr, err := r.pendingByCode(req.PathValue("code"))
	if err != nil {
		return nil, err
	}
	pr.status = meshproto.PairDenied
	logging.Infow(r.ctx, "pairing: denied", "code", pr.userCode, "by", who.Name)
	return map[string]any{"denied": pr.userCode}, nil
}

// inviteView is an invite in API responses (never the hash).
type inviteView struct {
	*InviteRecord
	Active bool `json:"active"`
}

func (r *Relay) apiCreateInvite(req *http.Request, who *ClientRecord) (any, error) {
	var body struct {
		Gateway        string     `json:"gateway"`
		NamePrefix     string     `json:"name_prefix"`
		MaxUses        int        `json:"max_uses"`
		ExpiresInHours float64    `json:"expires_in_hours"`
		ExpiresAt      *time.Time `json:"expires_at"`
		ExpectedPubkey string     `json:"expected_pubkey"`
		Note           string     `json:"note"`
	}
	if err := decodeBody(req, &body); err != nil {
		return nil, err
	}
	now := r.now()
	if body.MaxUses == 0 {
		body.MaxUses = 1
	}
	switch {
	case body.MaxUses < 1 || body.MaxUses > 100:
		return nil, errors.NewC("max_uses must be 1..100", codes.InvalidArgument)
	case body.Gateway != "" && body.NamePrefix != "":
		return nil, errors.NewC("set gateway or name_prefix, not both", codes.InvalidArgument)
	case body.Gateway != "" && body.MaxUses > 1:
		return nil, errors.NewC("a multi-use invite names gateways with name_prefix, not gateway", codes.InvalidArgument)
	case body.Gateway != "" && !ValidGatewayName(body.Gateway):
		return nil, errors.NewC("gateway name must match [a-z0-9-]{3,40} and not be reserved", codes.InvalidArgument)
	case body.NamePrefix != "" && !ValidGatewayName(body.NamePrefix+"-1") || len(body.NamePrefix) > 34:
		return nil, errors.NewC("name_prefix must be [a-z0-9-], at most 34 characters", codes.InvalidArgument)
	}
	expires := now.Add(r.cfg.Pairing.InviteTTL)
	switch {
	case body.ExpiresAt != nil:
		expires = *body.ExpiresAt
	case body.ExpiresInHours > 0:
		expires = now.Add(time.Duration(body.ExpiresInHours * float64(time.Hour)))
	}
	if !expires.After(now) || expires.Sub(now) > r.cfg.Pairing.InviteMaxTTL {
		return nil, errors.NewC(fmt.Sprintf("an invite must expire within %s", r.cfg.Pairing.InviteMaxTTL), codes.InvalidArgument)
	}
	expected := ""
	if body.ExpectedPubkey != "" {
		pk, ok := NormalizePubkey(body.ExpectedPubkey)
		if !ok {
			return nil, errors.NewC("expected_pubkey must be 64 hex characters", codes.InvalidArgument)
		}
		expected = pk
	}
	phrase := randomPassphrase()
	hash, err := bcrypt.GenerateFromPassword([]byte(phrase), r.cfg.Pairing.BcryptCost)
	if err != nil {
		return nil, errors.Wrap(err, 0)
	}
	inv := InviteRecord{ID: randomID("i_"), PassphraseHash: string(hash), Gateway: body.Gateway,
		NamePrefix: body.NamePrefix, MaxUses: body.MaxUses, ExpiresAt: expires, ExpectedPubkey: expected,
		Note: truncate(body.Note, 500), CreatedBy: who.Name, CreatedAt: now}
	if err := r.store.CreateInvite(inv); err != nil {
		return nil, errors.Wrap(err, 0)
	}
	logging.Infow(r.ctx, "invite created", "invite", inv.ID, "by", who.Name, "gateway", inv.Gateway,
		"name_prefix", inv.NamePrefix, "max_uses", inv.MaxUses, "expires_at", expires)
	return map[string]any{
		"invite":     inviteView{&inv, true},
		"passphrase": phrase,
		"link":       "sierramesh://pair?passphrase=" + phrase,
		"note":       "this passphrase is shown once; the relay keeps only a slow hash",
	}, nil
}

func (r *Relay) apiListInvites(_ *http.Request, _ *ClientRecord) (any, error) {
	invs, err := r.store.Invites()
	if err != nil {
		return nil, errors.Wrap(err, 0)
	}
	now := r.now()
	out := make([]inviteView, 0, len(invs))
	for _, i := range invs {
		out = append(out, inviteView{i, i.Active(now)})
	}
	return map[string]any{"invites": out}, nil
}

func (r *Relay) apiRevokeInvite(req *http.Request, who *ClientRecord) (any, error) {
	id := req.PathValue("id")
	if err := r.store.RevokeInvite(id); err != nil {
		return nil, storeErr(err, "unknown invite")
	}
	logging.Infow(r.ctx, "invite revoked", "invite", id, "by", who.Name)
	return map[string]any{"revoked": id}, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
