package admin

import (
	"context"
	"errors"
	"net/http"
	"net/url"
)

// ErrNotSupported means the relay is older than the request: it doesn't
// have the route.
var ErrNotSupported = errors.New("the relay doesn't support this yet")

// SetNode tells the relay this bridge's gateway now uses the node with
// pubkey (bridge token). It reports whether the pin changed.
func (c *Client) SetNode(ctx context.Context, pubkey string) (bool, error) {
	var out struct {
		Changed bool `json:"changed"`
	}
	err := c.do(ctx, http.MethodPost, "/gateway/node", map[string]string{"pubkey": pubkey}, &out)
	var ae *APIError
	if errors.As(err, &ae) && (ae.Status == http.StatusNotFound || ae.Status == http.StatusMethodNotAllowed) {
		return false, ErrNotSupported
	}
	return out.Changed, err
}

// AcceptPubkey pins a gateway to the node its bridge reports now, after a
// mismatch (admin).
func (c *Client) AcceptPubkey(ctx context.Context, gateway string) error {
	return c.do(ctx, http.MethodPost, "/gateways/"+url.PathEscape(gateway)+"/accept-pubkey", nil, nil)
}
