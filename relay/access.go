package relay

import (
	stderrors "errors"
	"fmt"
	"time"

	"github.com/dpup/prefab/errors"
	"github.com/dpup/prefab/logging"
	"google.golang.org/grpc/codes"
)

// Host tokens: a token that opens one gateway, for its host (or whoever the
// host or an admin gives it to). See docs/relay.md "Roles".
const (
	// HostTokenDefaultDays is a host token's lifetime when none is asked for.
	HostTokenDefaultDays = 180
	// HostTokenMaxDays bounds a host token's lifetime.
	HostTokenMaxDays = 730
	// MaxHostTokensPerGateway caps the host tokens a gateway can have at once.
	MaxHostTokensPerGateway = 5
)

// HostTokenRequest asks for a host token for Gateway.
type HostTokenRequest struct {
	Gateway string
	Label   string // the token is named <gateway>.<label>
	Note    string
	Days    int    // 0: HostTokenDefaultDays
	By      string // who issued it: an admin's name, or "gateway:<name>"
}

// IssuedToken is a newly minted client token. Token is shown once.
type IssuedToken struct {
	Client    string    `json:"client"`
	Role      Role      `json:"role"`
	Gateway   string    `json:"gateway,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
	Token     string    `json:"token"`
	Note      string    `json:"note"`
}

// IssueHostToken mints a host token for a gateway, within the cap, and
// records it in the gateway's events.
func (r *Relay) IssueHostToken(q HostTokenRequest) (*IssuedToken, error) {
	if _, err := r.store.Gateway(q.Gateway); err != nil {
		return nil, storeErr(err, "unknown gateway")
	}
	name, err := HostClientName(q.Gateway, q.Label)
	if err != nil {
		return nil, errors.NewC(err.Error(), codes.InvalidArgument)
	}
	days := q.Days
	if days == 0 {
		days = HostTokenDefaultDays
	}
	if days < 1 || days > HostTokenMaxDays {
		return nil, errors.NewC(fmt.Sprintf("a host token lasts 1 to %d days", HostTokenMaxDays), codes.InvalidArgument)
	}
	if len(q.Note) > 200 {
		return nil, errors.NewC("the note is longer than 200 characters", codes.InvalidArgument)
	}
	if _, isBootstrap := r.cfg.BootstrapAdmins[name]; isBootstrap {
		return nil, errors.NewC("that name is a bootstrap admin", codes.AlreadyExists)
	}
	existing, err := r.HostTokens(q.Gateway)
	if err != nil {
		return nil, errors.Wrap(err, 0)
	}
	// Expired tokens open nothing: clear them out, so they don't hold the
	// gateway's slots (or labels) forever.
	now := r.now()
	live := existing[:0]
	for _, c := range existing {
		if !c.ExpiresAt.IsZero() && !now.Before(c.ExpiresAt) {
			if err := r.store.DeleteClient(c.Name); err != nil && !stderrors.Is(err, ErrNotFound) {
				return nil, errors.Wrap(err, 0)
			}
			r.forgetTouch(c.Name)
			continue
		}
		live = append(live, c)
	}
	// Counting then creating isn't atomic: two admins issuing at once could
	// pass the cap by one. Only admins issue for now; host self-service
	// will need a conditional create.
	if len(live) >= MaxHostTokensPerGateway {
		return nil, conflict(fmt.Sprintf("gateway %s already has %d access tokens, the most it can have; remove one first",
			q.Gateway, MaxHostTokensPerGateway))
	}
	tok, err := MintToken(HostTokenPrefix)
	if err != nil {
		return nil, errors.Wrap(err, 0)
	}
	rec := ClientRecord{Name: name, Role: RoleHost, CreatedAt: now, Gateway: q.Gateway,
		ExpiresAt: now.Add(time.Duration(days) * 24 * time.Hour), CreatedBy: q.By, Note: q.Note}
	if err := r.store.CreateClient(rec, TokenHash(tok)); err != nil {
		return nil, storeErr(err, "")
	}
	r.event(q.Gateway, "access_issued", name+" by "+q.By)
	logging.Infow(r.ctx, "host token issued", "client", name, "gateway", q.Gateway, "by", q.By, "expires", rec.ExpiresAt)
	return &IssuedToken{Client: name, Role: RoleHost, Gateway: q.Gateway, ExpiresAt: rec.ExpiresAt, Token: tok,
		Note: oneTimeNote}, nil
}

// HostTokens lists the host tokens scoped to a gateway (never the tokens).
func (r *Relay) HostTokens(gateway string) ([]ClientRecord, error) {
	all, err := r.store.Clients()
	if err != nil {
		return nil, err
	}
	out := []ClientRecord{}
	for _, c := range all {
		if c.Gateway == gateway {
			out = append(out, c)
		}
	}
	return out, nil
}

// revokeHostTokens deletes a gateway's host tokens and ends their leases,
// recording why in the gateway's events. It returns how many it revoked.
func (r *Relay) revokeHostTokens(gateway, why string) (int, error) {
	cs, err := r.HostTokens(gateway)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, c := range cs {
		if err := r.store.DeleteClient(c.Name); err != nil && !stderrors.Is(err, ErrNotFound) {
			return n, err
		}
		r.EndLeasesFor(c.Name, ReasonRevoked)
		r.forgetTouch(c.Name)
		n++
	}
	if n > 0 {
		r.event(gateway, "access_revoked", fmt.Sprintf("%d token(s): %s", n, why))
		logging.Infow(r.ctx, "host tokens revoked", "gateway", gateway, "count", n, "why", why)
	}
	return n, nil
}

// publicRole is the lease holder's role as the public status shows it. A
// person's session (an admin's or a host's) reads as "operator", whoever it
// is, so the page tells a person from an automated session (agent) and
// never says "the host is on their node now". No token holds the operator
// role any more, so showing host alone as operator would give it away.
func publicRole(role Role) Role {
	switch role {
	case RoleHost, RoleAdmin, RoleOperator:
		return RoleOperator
	}
	return role
}

// forgetTouch drops a revoked client from the last-use throttle.
func (r *Relay) forgetTouch(name string) {
	r.touchMu.Lock()
	delete(r.touched, name)
	r.touchMu.Unlock()
}
