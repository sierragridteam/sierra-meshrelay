package relay

import (
	"encoding/json"
	stderrors "errors"
	"net/http"
	"strconv"
	"time"

	"github.com/dpup/prefab"
	"github.com/dpup/prefab/errors"
	"github.com/dpup/prefab/logging"
	"google.golang.org/grpc/codes"
)

// PathPrefix is where the reverse proxy routes the relay. The proxy does not
// strip it, so every route below carries it.
const PathPrefix = "/mesh/relay"

const maxAPIBody = 16 << 10

// ServerOptions implements prefab.OptionProvider, so prefab.WithPlugin(relay)
// alone mounts everything: the websocket endpoints, healthz and the root redirect
// as plain HTTP handlers, and the admin API as prefab JSON handlers (so errors
// share the {code, codeName, message} envelope of /api/v1). Being a plugin is
// what gives prefab the relay's lifecycle (Init/Shutdown in relay.go).
//
// Patterns are Go ServeMux patterns; prefab registers them on its mux as-is.
// The websocket handlers rely on prefab's gzip wrapper passing http.Hijacker
// through, which it does (relay_test.go dials with Accept-Encoding: gzip).
//
// Every route goes through the relay's admission gate, so a handoff that
// closes it stops admitting everything at once, healthz included (a load
// balancer should stop routing here). A websocket counts as in flight until
// its handler returns.
func (r *Relay) ServerOptions() []prefab.ServerOption {
	p := PathPrefix
	api := p + "/api"
	h := r.gate.HandlerFunc
	j := r.gatedJSON
	return []prefab.ServerOption{
		prefab.WithHTTPHandlerFunc("GET "+p+"/healthz", h(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("ok\n"))
		})),
		// The relay's own root has no page: status lives on the public site's
		// /relay (served by the Grid, same origin), so send people there.
		prefab.WithHTTPHandlerFunc("GET "+p+"/{$}", toSiteStatus),
		prefab.WithHTTPHandlerFunc("GET "+p, toSiteStatus),
		prefab.WithHTTPHandlerFunc("GET "+p+"/{gateway}/dial", h(r.handleBridge)),
		prefab.WithHTTPHandlerFunc("GET "+p+"/{gateway}", h(r.handleClient)),

		// The one unauthenticated API route: a deliberately minimal view for the
		// public site's /relay page. See apiPublicStatus for what it withholds.
		prefab.WithJSONHandler("GET "+api+"/status", j(r.apiPublicStatus)),
		prefab.WithJSONHandler("GET "+api+"/gateways", j(r.viewer(r.apiListGateways))),
		prefab.WithJSONHandler("POST "+api+"/gateways", j(r.admin(r.apiCreateGateway))),
		prefab.WithJSONHandler("GET "+api+"/gateways/{name}", j(r.viewer(r.apiGetGateway))),
		prefab.WithJSONHandler("DELETE "+api+"/gateways/{name}", j(r.admin(r.apiDeleteGateway))),
		prefab.WithJSONHandler("GET "+api+"/gateways/{name}/logs", j(r.admin(r.apiGatewayLogs))),
		prefab.WithJSONHandler("POST "+api+"/gateways/{name}/rotate-token", j(r.admin(r.apiRotateToken))),
		prefab.WithJSONHandler("POST "+api+"/gateways/{name}/accept-pubkey", j(r.admin(r.apiAcceptPubkey))),
		prefab.WithJSONHandler("POST "+api+"/gateways/{name}/relink", j(r.admin(r.apiRelink))),
		prefab.WithJSONHandler("GET "+api+"/gateways/{name}/repeaters/nearby", j(r.admin(r.apiNearbyRepeaters))),
		prefab.WithJSONHandler("GET "+api+"/clients", j(r.admin(r.apiListClients))),
		prefab.WithJSONHandler("POST "+api+"/clients", j(r.admin(r.apiCreateClient))),
		prefab.WithJSONHandler("DELETE "+api+"/clients/{name}", j(r.admin(r.apiDeleteClient))),
		prefab.WithJSONHandler("GET "+api+"/leases", j(r.viewer(r.apiLeases))),

		// Pairing. The two bridge endpoints have no auth (a bridge being
		// paired has no token yet) and are rate-limited instead; see pairing.go.
		prefab.WithHTTPHandlerFunc("POST "+api+"/pair/start", h(r.handlePairStart)),
		prefab.WithHTTPHandlerFunc("POST "+api+"/pair/poll", h(r.handlePairPoll)),
		prefab.WithJSONHandler("GET "+api+"/pairings", j(r.admin(r.apiListPairings))),
		prefab.WithJSONHandler("POST "+api+"/pairings/{code}/approve", j(r.admin(r.apiApprovePairing))),
		prefab.WithJSONHandler("POST "+api+"/pairings/{code}/deny", j(r.admin(r.apiDenyPairing))),
		prefab.WithJSONHandler("GET "+api+"/invites", j(r.admin(r.apiListInvites))),
		prefab.WithJSONHandler("POST "+api+"/invites", j(r.admin(r.apiCreateInvite))),
		prefab.WithJSONHandler("DELETE "+api+"/invites/{id}", j(r.admin(r.apiRevokeInvite))),
		prefab.WithJSONHandler("GET "+api+"/metrics", j(r.viewer(r.apiMetrics))),
		prefab.WithJSONHandler("GET "+api+"/bridge-update", j(r.admin(r.apiGetBridgeUpdate))),
		prefab.WithJSONHandler("PUT "+api+"/bridge-update", j(r.admin(r.apiSetBridgeUpdate))),

		// Shared repeaters. Bridges manage their own under gateway/ with their
		// bridge token; admins see and manage all of them.
		prefab.WithJSONHandler("GET "+api+"/gateway/repeaters", j(r.bridgeAuthed(r.apiGatewayListRepeaters))),
		prefab.WithJSONHandler("GET "+api+"/gateway/repeaters/nearby", j(r.bridgeAuthed(r.apiGatewayNearbyRepeaters))),
		// A host switching their gateway to another node (meshbridge change-node).
		prefab.WithJSONHandler("POST "+api+"/gateway/node", j(r.bridgeAuthed(r.apiGatewaySetNode))),
		prefab.WithJSONHandler("POST "+api+"/gateway/repeaters", j(r.bridgeAuthed(r.apiGatewayAddRepeater))),
		// The same, answered as a stream of steps (see streamJSON).
		prefab.WithHTTPHandlerFunc("POST "+api+"/gateway/repeaters/stream", h(r.streamJSON(r.bridgeAuthed(r.apiGatewayAddRepeater)))),
		prefab.WithJSONHandler("DELETE "+api+"/gateway/repeaters/{pubkey}", j(r.bridgeAuthed(r.apiGatewayRemoveRepeater))),
		prefab.WithJSONHandler("GET "+api+"/repeaters", j(r.admin(r.apiListRepeaters))),
		prefab.WithJSONHandler("POST "+api+"/repeaters", j(r.admin(r.apiAddRepeater))),
		prefab.WithHTTPHandlerFunc("POST "+api+"/repeaters/stream", h(r.streamJSON(r.admin(r.apiAddRepeater)))),
		prefab.WithJSONHandler("DELETE "+api+"/repeaters/{pubkey}", j(r.admin(r.apiRemoveRepeater))),
	}
}

// gatedJSON puts a JSON handler behind the admission gate.
func (r *Relay) gatedJSON(h prefab.JSONHandler) prefab.JSONHandler {
	return func(req *http.Request) (any, error) {
		ctx, done, ok := r.gate.Enter(req.Context())
		if !ok {
			return nil, errors.NewC("the relay is handing over to a new instance; retry", codes.Unavailable).
				WithHTTPStatusCode(http.StatusServiceUnavailable)
		}
		defer done()
		return h(req.WithContext(ctx))
	}
}

// SiteStatusPath is the public site's relay page, which renders /api/status.
const SiteStatusPath = "/relay"

func toSiteStatus(w http.ResponseWriter, req *http.Request) {
	http.Redirect(w, req, SiteStatusPath, http.StatusFound)
}

// apiHandler is an admin API handler with the authenticated caller.
type apiHandler func(req *http.Request, who *ClientRecord) (any, error)

// viewer admits any client role: the roles table gives admin, operator and
// agent alike "view status".
func (r *Relay) viewer(h apiHandler) prefab.JSONHandler { return r.authed(h, false) }

// admin admits only admins: everything that changes state.
func (r *Relay) admin(h apiHandler) prefab.JSONHandler { return r.authed(h, true) }

func (r *Relay) authed(h apiHandler, adminOnly bool) prefab.JSONHandler {
	return func(req *http.Request) (any, error) {
		ip := r.clientIP(req)
		if r.lockout.isLocked(ip, r.now()) {
			return nil, errors.NewC("too many authentication failures", codes.ResourceExhausted)
		}
		who, err := r.authClient(req)
		if stderrors.Is(err, ErrNotFound) {
			r.m.authFailures.Add(1)
			r.lockout.fail(ip, r.now())
			logging.Infow(r.ctx, "auth failed", "kind", "api", "ip", ip, "path", req.URL.Path)
			return nil, errors.NewC("a client token is required", codes.Unauthenticated)
		}
		if err != nil {
			return nil, errors.Wrap(err, 0)
		}
		if adminOnly && who.Role != RoleAdmin {
			return nil, errors.NewC("admin role required", codes.PermissionDenied)
		}
		if req.Method != http.MethodGet {
			logging.Infow(r.ctx, "admin action", "client", who.Name, "method", req.Method, "path", req.URL.Path)
		}
		return h(req, who)
	}
}

func decodeBody(req *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, req.Body, maxAPIBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errors.NewC("bad request body: "+err.Error(), codes.InvalidArgument)
	}
	return nil
}

// storeErr maps a store error to an API error.
func storeErr(err error, notFound string) error {
	switch {
	case stderrors.Is(err, ErrNotFound):
		return errors.NewC(notFound, codes.NotFound)
	case stderrors.Is(err, ErrExists):
		return errors.NewC("already exists", codes.AlreadyExists)
	}
	return errors.Wrap(err, 0)
}

// conflict is a request that is valid but does not apply to the current state.
func conflict(msg string) error {
	return errors.NewC(msg, codes.FailedPrecondition).WithHTTPStatusCode(http.StatusConflict)
}

// oneTimeNote rides along with every response that carries a fresh token.
const oneTimeNote = "this token is shown once; the relay keeps only its hash"

// GatewayStatus is one gateway in GET /gateways.
type GatewayStatus struct {
	Name           string        `json:"name"`
	Notes          string        `json:"notes,omitempty"`
	Online         bool          `json:"online"`
	Session        string        `json:"session,omitempty"`
	ConnectedAt    *time.Time    `json:"connected_at,omitempty"`
	LastSeen       *time.Time    `json:"last_seen,omitempty"`
	Agent          *Agent        `json:"agent,omitempty"`
	Link           *Link         `json:"link,omitempty"`
	Node           *NodeInfo     `json:"node,omitempty"`
	PubkeyStatus   string        `json:"pubkey_status"` // unpinned | pinned | mismatch
	PinnedPubkey   string        `json:"pinned_pubkey,omitempty"`
	MismatchPubkey string        `json:"mismatch_pubkey,omitempty"`
	Lease          *LeaseSummary `json:"lease,omitempty"`
	Stats          *Stats        `json:"stats,omitempty"`
	StatsAt        *time.Time    `json:"stats_at,omitempty"`
	CreatedAt      time.Time     `json:"created_at"`
}

// LeaseSummary is a live lease.
type LeaseSummary struct {
	ID        string    `json:"id"`
	Gateway   string    `json:"gateway"`
	Client    string    `json:"client"`
	Role      Role      `json:"role"`
	StartedAt time.Time `json:"started_at"`
	ExpiresAt time.Time `json:"expires_at"`
	// FramesToNode / FramesFromNode so far.
	FramesToNode   int64 `json:"frames_to_node"`
	FramesFromNode int64 `json:"frames_from_node"`
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	t = t.UTC()
	return &t
}

func leaseSummaryLocked(l *Lease) *LeaseSummary {
	return &LeaseSummary{ID: l.ID, Gateway: l.Gateway, Client: l.Client, Role: l.Role,
		StartedAt: l.started.UTC(), ExpiresAt: l.expires.UTC(),
		FramesToNode: l.framesTo.Load(), FramesFromNode: l.framesFrom.Load()}
}

// status merges a persisted row with live state: a live session's values win,
// otherwise the last ones the store recorded.
func (r *Relay) status(rec *GatewayRecord) GatewayStatus {
	st := GatewayStatus{Name: rec.Name, Notes: rec.Notes, CreatedAt: rec.CreatedAt,
		PinnedPubkey: rec.PinnedPubkey, MismatchPubkey: rec.MismatchPubkey,
		LastSeen: timePtr(rec.LastSeen), Stats: rec.LastStats, StatsAt: timePtr(rec.LastStatsAt)}
	if h := rec.LastHello; h != nil {
		a, l := h.Agent, h.Link
		st.Agent, st.Link = &a, &l
	}
	r.mu.Lock()
	if g := r.gws[rec.Name]; g != nil {
		if g.bridge != nil {
			st.Online, st.Session = true, g.bridge.id
			st.ConnectedAt = timePtr(g.connectedAt)
			n := g.node
			st.Node = &n
			if g.hello != nil {
				a, l := g.hello.Agent, g.hello.Link
				st.Agent, st.Link = &a, &l
			}
		}
		if !g.lastSeen.IsZero() {
			st.LastSeen = timePtr(g.lastSeen)
		}
		if g.stats != nil {
			s := *g.stats
			st.Stats, st.StatsAt = &s, timePtr(g.statsAt)
		}
		if g.lease != nil {
			st.Lease = leaseSummaryLocked(g.lease)
		}
	}
	r.mu.Unlock()
	if st.Node == nil && rec.LastHello != nil {
		// Offline: report the last node identity, but not as connected.
		n := rec.LastHello.Node
		n.State = NodeDisconnected
		st.Node = &n
	}
	switch {
	case rec.MismatchPubkey != "":
		st.PubkeyStatus = "mismatch"
	case rec.PinnedPubkey != "":
		st.PubkeyStatus = "pinned"
	default:
		st.PubkeyStatus = "unpinned"
	}
	return st
}

// PublicGateway is one gateway in the public status. It answers "can this
// gateway be used right now, and when was it last heard" and nothing else.
type PublicGateway struct {
	Name string `json:"name"`
	// Online: a bridge session is attached.
	Online bool `json:"online"`
	// NodeState is the node behind the bridge: connected | disconnected |
	// mismatch | paused (the host paused sharing on purpose). A pubkey the
	// relay has not verified reads as mismatch. Absent
	// while the bridge is offline (the relay cannot know).
	NodeState string `json:"node_state,omitempty"`
	// Ready: a lease could be granted on this gateway's node.
	Ready bool `json:"ready"`
	// InUse: someone holds the lease. Never who: InUseRole says only what
	// kind of client (admin | operator | agent), so someone waiting can tell an
	// automated session from a person's. A client's name would publish when a
	// person is on which gateway, and polling would make a history of it.
	InUse          bool       `json:"in_use"`
	InUseRole      Role       `json:"in_use_role,omitempty"`
	ConnectedSince *time.Time `json:"connected_since,omitempty"`
	LastSeen       *time.Time `json:"last_seen,omitempty"`
	AgentVersion   string     `json:"agent_version,omitempty"`
	LinkKind       string     `json:"link_kind,omitempty"`
}

// apiPublicStatus is GET /mesh/relay/api/status, the public status board.
//
// It withholds everything that is not needed to say whether a gateway works:
// notes, pubkeys, node names, the bridge's OS and link detail (a serial port
// or a LAN address), stats, the lease holder's identity (only its role is
// shown) and the event log.
// A gateway's node pubkey would tie it to the node's own adverts, which can
// carry a location — and a gateway sits at a volunteer's house.
func (r *Relay) apiPublicStatus(_ *http.Request) (any, error) {
	recs, err := r.store.Gateways()
	if err != nil {
		return nil, errors.Wrap(err, 0)
	}
	out := make([]PublicGateway, 0, len(recs))
	r.mu.Lock()
	for _, rec := range recs {
		pg := PublicGateway{Name: rec.Name, LastSeen: timePtr(rec.LastSeen)}
		if h := rec.LastHello; h != nil {
			pg.AgentVersion, pg.LinkKind = h.Agent.Version, h.Link.Kind
		}
		if g := r.gws[rec.Name]; g != nil {
			if !g.lastSeen.IsZero() {
				pg.LastSeen = timePtr(g.lastSeen)
			}
			if g.bridge != nil {
				pg.Online = true
				pg.ConnectedSince = timePtr(g.connectedAt)
				pg.NodeState = g.node.State
				if g.node.State == NodeConnected && !r.usableLocked(g) {
					pg.NodeState = NodeMismatch
				}
				pg.Ready = r.usableLocked(g)
				if g.hello != nil {
					pg.AgentVersion, pg.LinkKind = g.hello.Agent.Version, g.hello.Link.Kind
				}
			}
			if g.lease != nil {
				pg.InUse, pg.InUseRole = true, g.lease.Role
			}
		}
		out = append(out, pg)
	}
	r.mu.Unlock()
	return map[string]any{"generated_at": r.now().UTC(), "gateways": out}, nil
}

func (r *Relay) apiListGateways(_ *http.Request, _ *ClientRecord) (any, error) {
	recs, err := r.store.Gateways()
	if err != nil {
		return nil, errors.Wrap(err, 0)
	}
	out := make([]GatewayStatus, 0, len(recs))
	for _, rec := range recs {
		out = append(out, r.status(rec))
	}
	return map[string]any{"gateways": out}, nil
}

func (r *Relay) apiGetGateway(req *http.Request, _ *ClientRecord) (any, error) {
	rec, err := r.store.Gateway(req.PathValue("name"))
	if err != nil {
		return nil, storeErr(err, "unknown gateway")
	}
	events, err := r.store.GatewayEvents(rec.Name, 50)
	if err != nil {
		return nil, errors.Wrap(err, 0)
	}
	return struct {
		GatewayStatus
		Events []GatewayEvent `json:"events"`
	}{r.status(rec), events}, nil
}

func (r *Relay) apiCreateGateway(req *http.Request, who *ClientRecord) (any, error) {
	var body struct {
		Name           string `json:"name"`
		ExpectedPubkey string `json:"expected_pubkey"`
		Notes          string `json:"notes"`
	}
	if err := decodeBody(req, &body); err != nil {
		return nil, err
	}
	if !ValidGatewayName(body.Name) {
		return nil, errors.NewC("gateway name must match [a-z0-9-]{3,40} and not be reserved", codes.InvalidArgument)
	}
	pinned := ""
	if body.ExpectedPubkey != "" {
		pk, ok := NormalizePubkey(body.ExpectedPubkey)
		if !ok {
			return nil, errors.NewC("expected_pubkey must be 64 hex characters", codes.InvalidArgument)
		}
		pinned = pk
	}
	tok, err := MintToken(BridgeTokenPrefix)
	if err != nil {
		return nil, errors.Wrap(err, 0)
	}
	if err := r.store.CreateGateway(body.Name, TokenHash(tok), pinned, body.Notes, r.now()); err != nil {
		return nil, storeErr(err, "")
	}
	r.event(body.Name, "created", who.Name)
	rec, err := r.store.Gateway(body.Name)
	if err != nil {
		return nil, errors.Wrap(err, 0)
	}
	return map[string]any{"gateway": r.status(rec), "bridge_token": tok, "note": oneTimeNote}, nil
}

func (r *Relay) apiRotateToken(req *http.Request, who *ClientRecord) (any, error) {
	rec, err := r.store.Gateway(req.PathValue("name"))
	if err != nil {
		return nil, storeErr(err, "unknown gateway")
	}
	tok, err := MintToken(BridgeTokenPrefix)
	if err != nil {
		return nil, errors.Wrap(err, 0)
	}
	if err := r.store.SetGatewayToken(rec.Name, TokenHash(tok)); err != nil {
		return nil, errors.Wrap(err, 0)
	}
	closed := r.closeBridge(rec.Name, CloseBadToken, "token rotated")
	r.event(rec.Name, "token_rotated", who.Name)
	return map[string]any{"bridge_token": tok, "closed_session": closed, "note": oneTimeNote}, nil
}

func (r *Relay) apiAcceptPubkey(req *http.Request, who *ClientRecord) (any, error) {
	name := req.PathValue("name")
	r.pubkeyMu.Lock()
	defer r.pubkeyMu.Unlock()
	rec, err := r.store.Gateway(name)
	if err != nil {
		return nil, storeErr(err, "unknown gateway")
	}
	if rec.MismatchPubkey == "" {
		return nil, conflict("the gateway has no pubkey mismatch to accept")
	}
	accepted := rec.MismatchPubkey
	if err := r.store.SetPubkeys(name, accepted, ""); err != nil {
		return nil, errors.Wrap(err, 0)
	}
	r.mu.Lock()
	if g := r.gws[name]; g != nil {
		g.pinned, g.mismatch = accepted, ""
	}
	r.mu.Unlock()
	r.event(name, "pubkey_accepted", accepted+" by "+who.Name)
	logging.Infow(r.ctx, "pubkey change accepted", "gateway", name, "pubkey", accepted, "by", who.Name)
	rec.PinnedPubkey, rec.MismatchPubkey = accepted, ""
	return map[string]any{"gateway": r.status(rec)}, nil
}

type setNodeBody struct {
	Pubkey string `json:"pubkey"`
}

// apiGatewaySetNode is a host saying their gateway now uses a different
// node (meshbridge change-node), with the bridge's token. It re-pins the
// gateway to that node at once. The relay can't verify a node's identity (the
// bridge reports it), so the pin guards against a wrong node answering by
// accident, not against the host; an explicit change is theirs to make, and
// it's recorded for admins.
func (r *Relay) apiGatewaySetNode(req *http.Request, gw *GatewayRecord) (any, error) {
	var body setNodeBody
	if err := decodeBody(req, &body); err != nil {
		return nil, err
	}
	pk, ok := NormalizePubkey(body.Pubkey)
	if !ok {
		return nil, errors.NewC("pubkey must be 64 hex characters", codes.InvalidArgument)
	}
	r.pubkeyMu.Lock()
	defer r.pubkeyMu.Unlock()
	rec, err := r.store.Gateway(gw.Name)
	if err != nil {
		return nil, storeErr(err, "unknown gateway")
	}
	old := rec.PinnedPubkey
	if old == pk && rec.MismatchPubkey == "" {
		return map[string]any{"gateway": r.status(rec), "changed": false}, nil
	}
	if err := r.store.SetPubkeys(gw.Name, pk, ""); err != nil {
		return nil, errors.Wrap(err, 0)
	}
	r.mu.Lock()
	if g := r.gws[gw.Name]; g != nil {
		g.pinned, g.mismatch = pk, ""
	}
	r.mu.Unlock()
	r.event(gw.Name, "node_changed", shortKey(old)+" → "+shortKey(pk)+" by the host")
	logging.Infow(r.ctx, "gateway node changed by its host", "gateway", gw.Name, "from", old, "to", pk)
	rec.PinnedPubkey, rec.MismatchPubkey = pk, ""
	return map[string]any{"gateway": r.status(rec), "changed": true}, nil
}

// shortKey abbreviates a pubkey for an event ("none" if unset).
func shortKey(pk string) string {
	if len(pk) < 12 {
		return "none"
	}
	return pk[:12] + "…"
}

func (r *Relay) apiRelink(req *http.Request, who *ClientRecord) (any, error) {
	rec, err := r.store.Gateway(req.PathValue("name"))
	if err != nil {
		return nil, storeErr(err, "unknown gateway")
	}
	r.mu.Lock()
	g := r.gws[rec.Name]
	sent := g != nil && g.bridge != nil && g.bridge.conn.sendJSON(typeOnlyMsg{Type: "relink"})
	r.mu.Unlock()
	if !sent {
		return nil, conflict("the gateway's bridge is not connected")
	}
	r.event(rec.Name, "relink", who.Name)
	return map[string]any{"sent": true}, nil
}

func (r *Relay) apiDeleteGateway(req *http.Request, who *ClientRecord) (any, error) {
	name := req.PathValue("name")
	if err := r.store.DeleteGateway(name); err != nil {
		return nil, storeErr(err, "unknown gateway")
	}
	// The token is gone from the store, so the bridge cannot reconnect; end the
	// live session and any lease now.
	r.mu.Lock()
	if g := r.gws[name]; g != nil {
		if g.lease != nil {
			r.endLeaseLocked(g.lease, ReasonRevoked, true)
		}
		if g.bridge != nil {
			g.bridge.conn.close(CloseBadToken, "gateway removed")
		}
		delete(r.gws, name)
	}
	r.mu.Unlock()
	logging.Infow(r.ctx, "gateway removed", "gateway", name, "by", who.Name)
	return map[string]any{"deleted": name}, nil
}

// --- clients ----------------------------------------------------------------

func (r *Relay) apiListClients(_ *http.Request, _ *ClientRecord) (any, error) {
	out, err := r.store.Clients()
	if err != nil {
		return nil, errors.Wrap(err, 0)
	}
	for _, name := range sortedNames(r.cfg.BootstrapAdmins) {
		out = append(out, ClientRecord{Name: name, Role: RoleAdmin, Bootstrap: true})
	}
	return map[string]any{"clients": out}, nil
}

func (r *Relay) apiCreateClient(req *http.Request, who *ClientRecord) (any, error) {
	var body struct {
		Name string `json:"name"`
		Role string `json:"role"`
	}
	if err := decodeBody(req, &body); err != nil {
		return nil, err
	}
	if !ValidClientName(body.Name) {
		return nil, errors.NewC("client name must be a lowercase slug", codes.InvalidArgument)
	}
	if _, isBootstrap := r.cfg.BootstrapAdmins[body.Name]; isBootstrap {
		return nil, errors.NewC("that name is a bootstrap admin", codes.AlreadyExists)
	}
	role, err := ParseRole(body.Role)
	if err != nil {
		return nil, errors.NewC(err.Error(), codes.InvalidArgument)
	}
	tok, err := MintToken(ClientTokenPrefix)
	if err != nil {
		return nil, errors.Wrap(err, 0)
	}
	if err := r.store.CreateClient(body.Name, role, TokenHash(tok), r.now()); err != nil {
		return nil, storeErr(err, "")
	}
	logging.Infow(r.ctx, "client token issued", "client", body.Name, "role", role, "by", who.Name)
	return map[string]any{"client": body.Name, "role": role, "token": tok, "note": oneTimeNote}, nil
}

func (r *Relay) apiDeleteClient(req *http.Request, who *ClientRecord) (any, error) {
	name := req.PathValue("name")
	if _, isBootstrap := r.cfg.BootstrapAdmins[name]; isBootstrap {
		return nil, conflict("bootstrap admins are configured, not issued; remove it from meshRelay.admins")
	}
	if err := r.store.DeleteClient(name); err != nil {
		return nil, storeErr(err, "unknown client")
	}
	ended := r.EndLeasesFor(name, ReasonRevoked)
	logging.Infow(r.ctx, "client token revoked", "client", name, "by", who.Name, "leases_ended", ended)
	return map[string]any{"deleted": name, "leases_ended": ended}, nil
}

// --- leases & metrics -------------------------------------------------------

func (r *Relay) apiLeases(req *http.Request, _ *ClientRecord) (any, error) {
	limit := 100
	if v, err := strconv.Atoi(req.URL.Query().Get("limit")); err == nil && v > 0 && v <= 1000 {
		limit = v
	}
	recent, err := r.store.RecentLeases(limit)
	if err != nil {
		return nil, errors.Wrap(err, 0)
	}
	current := []*LeaseSummary{}
	r.mu.Lock()
	for _, name := range sortedNames(r.gws) {
		if l := r.gws[name].lease; l != nil {
			current = append(current, leaseSummaryLocked(l))
		}
	}
	r.mu.Unlock()
	return map[string]any{"current": current, "recent": recent}, nil
}

func (r *Relay) apiMetrics(_ *http.Request, _ *ClientRecord) (any, error) {
	return r.Metrics(), nil
}
