package relay

// Bridge updates: an admin sets the release bridges should run
// (PUT /bridge-update), optionally only for some gateways (a canary first).
// The relay offers it with `update_available` to each matching bridge that
// runs something else, when it connects and when the target changes. The
// bridge verifies the release's signature itself (internal/update in this
// repo), so the relay only chooses the version.

import (
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/dpup/prefab/errors"
	"github.com/dpup/prefab/logging"
	"google.golang.org/grpc/codes"

	"github.com/dpup/sierra-meshrelay/meshproto"
)

const settingsSchema = `
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

// DefaultReleaseBaseURL is where bridge releases are downloaded from:
// <base>/<version>/SHA256SUMS etc.
const DefaultReleaseBaseURL = "https://github.com/sierragridteam/sierra-meshrelay/releases/download"

// BridgeUpdate is the release bridges should run.
type BridgeUpdate struct {
	// Version is a release tag (v0.7.0); "" offers nothing.
	Version string `json:"version"`
	// Gateways limits the offer to these gateways; empty means all.
	Gateways []string `json:"gateways,omitempty"`
}

func (u BridgeUpdate) appliesTo(gateway string) bool {
	return u.Version != "" && (len(u.Gateways) == 0 || slices.Contains(u.Gateways, gateway))
}

func (s *Store) setting(key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, string(b))
	return err
}

func (s *Store) getSetting(key string, v any) error {
	var raw string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&raw)
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(raw), v)
}

// BridgeUpdate returns the configured bridge release ("" if none).
func (s *Store) BridgeUpdate() BridgeUpdate {
	var u BridgeUpdate
	_ = s.getSetting("bridge_update", &u)
	return u
}

var releaseTag = regexp.MustCompile(`^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)

// updateOffer is the update_available message for a bridge running
// running, or nil if there's nothing to offer it.
func (r *Relay) updateOffer(u BridgeUpdate, gateway, running string) *meshproto.UpdateAvailable {
	if !u.appliesTo(gateway) || running == u.Version {
		return nil
	}
	base := strings.TrimRight(r.cfg.ReleaseBaseURL, "/")
	if base == "" {
		base = DefaultReleaseBaseURL
	}
	return &meshproto.UpdateAvailable{Type: meshproto.TypeUpdateAvailable, Version: u.Version, URL: base + "/" + u.Version}
}

// offerUpdate sends the configured update to a bridge that just connected.
func (r *Relay) offerUpdate(s *bridgeSession, running string) {
	if m := r.updateOffer(r.store.BridgeUpdate(), s.gateway, running); m != nil {
		s.conn.sendJSON(m)
		logging.Infow(r.ctx, "offered bridge update", "gateway", s.gateway, "running", running, "version", m.Version)
	}
}

func (r *Relay) apiGetBridgeUpdate(_ *http.Request, _ *ClientRecord) (any, error) {
	return r.store.BridgeUpdate(), nil
}

func (r *Relay) apiSetBridgeUpdate(req *http.Request, who *ClientRecord) (any, error) {
	var body BridgeUpdate
	if err := decodeBody(req, &body); err != nil {
		return nil, err
	}
	if body.Version != "" && !releaseTag.MatchString(body.Version) {
		return nil, errors.NewC("version must be a release tag like v0.7.0 (or empty to stop offering updates)", codes.InvalidArgument)
	}
	for _, g := range body.Gateways {
		if !ValidGatewayName(g) {
			return nil, errors.NewC("bad gateway name "+g, codes.InvalidArgument)
		}
	}
	if err := r.store.setting("bridge_update", body); err != nil {
		return nil, errors.Wrap(err, 0)
	}
	logging.Infow(r.ctx, "bridge update target set", "version", body.Version, "gateways", body.Gateways, "by", who.Name)

	// Offer it to the bridges online now.
	offered := []string{}
	r.mu.Lock()
	for name, g := range r.gws {
		if g.bridge == nil || g.hello == nil {
			continue
		}
		if m := r.updateOffer(body, name, g.hello.Agent.Version); m != nil {
			g.bridge.conn.sendJSON(m)
			offered = append(offered, name)
		}
	}
	r.mu.Unlock()
	slices.Sort(offered)
	return map[string]any{"update": body, "offered": offered}, nil
}
