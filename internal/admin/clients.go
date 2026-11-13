package admin

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ClientToken is an issued client token's record (never the token).
type ClientToken struct {
	Name       string    `json:"name"`
	Role       string    `json:"role"`
	CreatedAt  time.Time `json:"created_at"`
	Gateway    string    `json:"gateway"`
	ExpiresAt  time.Time `json:"expires_at"`
	LastUsedAt time.Time `json:"last_used_at"`
	CreatedBy  string    `json:"created_by"`
	Note       string    `json:"note"`
	Bootstrap  bool      `json:"bootstrap"`
}

// IssuedToken is a newly issued token. Token is shown once.
type IssuedToken struct {
	Client    string    `json:"client"`
	Role      string    `json:"role"`
	Gateway   string    `json:"gateway"`
	ExpiresAt time.Time `json:"expires_at"`
	Token     string    `json:"token"`
}

// ErrNoHostTokens means the relay is older than host tokens (v0.15.0).
var ErrNoHostTokens = errors.New("the relay doesn't issue host tokens yet (it needs meshrelay v0.15.0 or later)")

// ClientTokens lists the issued tokens.
func (c *Client) ClientTokens(ctx context.Context) ([]ClientToken, error) {
	var out struct {
		Clients []ClientToken `json:"clients"`
	}
	return out.Clients, c.do(ctx, http.MethodGet, "/clients", nil, &out)
}

// IssueHostToken issues a token that opens one gateway, named
// <gateway>.<label>. days 0 takes the relay's default.
func (c *Client) IssueHostToken(ctx context.Context, gateway, label string, days int, note string) (IssuedToken, error) {
	body := map[string]any{"role": "host", "gateway": gateway, "label": label, "note": note}
	if days > 0 {
		body["expires_in_days"] = days
	}
	var out IssuedToken
	err := c.do(ctx, http.MethodPost, "/clients", body, &out)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusBadRequest &&
		(strings.Contains(ae.Message, "unknown role") || strings.Contains(ae.Message, "unknown field")) {
		return out, ErrNoHostTokens
	}
	return out, err
}

// IssueAdminToken issues an admin token. days 0 never expires.
func (c *Client) IssueAdminToken(ctx context.Context, name string, days int, note string) (IssuedToken, error) {
	body := map[string]any{"role": "admin", "name": name}
	if days > 0 {
		body["expires_in_days"] = days
	}
	if note != "" {
		body["note"] = note
	}
	var out IssuedToken
	return out, c.do(ctx, http.MethodPost, "/clients", body, &out)
}

// MakeAdmin turns an operator or agent token into an admin token.
func (c *Client) MakeAdmin(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodPatch, "/clients/"+url.PathEscape(name), map[string]string{"role": "admin"}, nil)
}

// RevokeClient revokes a token, ending its sessions.
func (c *Client) RevokeClient(ctx context.Context, name string) (ended int, err error) {
	var out struct {
		LeasesEnded int `json:"leases_ended"`
	}
	return out.LeasesEnded, c.do(ctx, http.MethodDelete, "/clients/"+url.PathEscape(name), nil, &out)
}
