// Package admin is a small client for the relay's HTTP API: the admin API
// (pairings, invites, gateways, repeaters) behind `meshbridge admin`, and the
// bridge self-service API (shared repeaters) behind `meshbridge repeater`,
// which authenticates with the bridge's own token. It defines its own response
// types instead of importing the relay package, so the bridge binary stays
// free of the relay's dependencies.
package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sierragridteam/sierra-meshrelay/meshproto"
)

// TokenEnv supplies the admin token.
const TokenEnv = "MESHBRIDGE_ADMIN_TOKEN"

// DefaultTokenFile is where the admin token is read from when TokenEnv is
// unset: ~/.config/meshbridge/admin.token.
func DefaultTokenFile() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "meshbridge", "admin.token")
}

// LoadToken returns the admin token from TokenEnv, else from file.
func LoadToken(file string) (string, error) {
	if t := strings.TrimSpace(os.Getenv(TokenEnv)); t != "" {
		return t, nil
	}
	if file == "" {
		return "", fmt.Errorf("no admin token: set %s", TokenEnv)
	}
	b, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("no admin token: set %s, or save it to %s (chmod 600)", TokenEnv, file)
	}
	if err != nil {
		return "", err
	}
	if fi, err := os.Stat(file); err == nil && fi.Mode().Perm()&0o077 != 0 {
		fmt.Fprintf(os.Stderr, "warning: %s is readable by others; chmod 600 it\n", file)
	}
	return strings.TrimSpace(string(b)), nil
}

// Client calls the relay API at APIBase (https://…/mesh/relay/api) with
// Token: an admin client token, or a bridge token for the gateway/ routes.
type Client struct {
	APIBase string
	Token   string
	HTTP    *http.Client
}

// APIError is an error answer from the admin API.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("relay answered HTTP %d", e.Status)
	}
	return fmt.Sprintf("%s (HTTP %d)", e.Message, e.Status)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.APIBase+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return unreachable(c.APIBase, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if resp.StatusCode == http.StatusUnauthorized && e.Message == "" {
			e.Message = "the relay didn't accept the token"
		}
		return &APIError{Status: resp.StatusCode, Message: e.Message}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Pairing is a device code waiting for approval.
type Pairing struct {
	UserCode         string             `json:"user_code"`
	HostLabel        string             `json:"host_label"`
	IP               string             `json:"ip"`
	Agent            meshproto.Agent    `json:"agent"`
	Node             meshproto.PairNode `json:"node"`
	SuggestedGateway string             `json:"suggested_gateway"`
	CreatedAt        time.Time          `json:"created_at"`
	ExpiresAt        time.Time          `json:"expires_at"`
}

// Pairings lists pending device codes.
func (c *Client) Pairings(ctx context.Context) ([]Pairing, error) {
	var out struct {
		Pairings []Pairing `json:"pairings"`
	}
	return out.Pairings, c.do(ctx, http.MethodGet, "/pairings", nil, &out)
}

// Gateway is one gateway's status.
type Gateway struct {
	Name         string           `json:"name"`
	Notes        string           `json:"notes"`
	Online       bool             `json:"online"`
	LastSeen     *time.Time       `json:"last_seen"`
	Agent        *meshproto.Agent `json:"agent"`
	Link         *meshproto.Link  `json:"link"`
	Node         *meshproto.Node  `json:"node"`
	PubkeyStatus string           `json:"pubkey_status"`
	PinnedPubkey string           `json:"pinned_pubkey"`
	// MismatchPubkey is the node the bridge reports when it isn't the
	// pinned one (PubkeyStatus "mismatch").
	MismatchPubkey string                   `json:"mismatch_pubkey"`
	Lease          *struct{ Client string } `json:"lease"`
}

// Approve approves a code onto gateway ("" lets the relay name it after the
// node). repair re-pairs an existing gateway, rotating its token.
func (c *Client) Approve(ctx context.Context, code, gateway string, repair bool) (Gateway, error) {
	var out struct {
		Gateway Gateway `json:"gateway"`
	}
	body := map[string]any{"repair": repair}
	if gateway != "" {
		body["gateway"] = gateway
	}
	return out.Gateway, c.do(ctx, http.MethodPost, "/pairings/"+url.PathEscape(code)+"/approve", body, &out)
}

// Deny refuses a code.
func (c *Client) Deny(ctx context.Context, code string) error {
	return c.do(ctx, http.MethodPost, "/pairings/"+url.PathEscape(code)+"/deny", nil, nil)
}

// InviteRequest creates a passphrase invite. Leave Gateway and NamePrefix
// empty to name the gateway after the node when it's redeemed.
type InviteRequest struct {
	Gateway        string  `json:"gateway,omitempty"`
	NamePrefix     string  `json:"name_prefix,omitempty"`
	MaxUses        int     `json:"max_uses,omitempty"`
	ExpiresInHours float64 `json:"expires_in_hours,omitempty"`
	ExpectedPubkey string  `json:"expected_pubkey,omitempty"`
	Note           string  `json:"note,omitempty"`
}

// Invite is a passphrase invite (never the passphrase itself).
type Invite struct {
	ID             string    `json:"id"`
	Gateway        string    `json:"gateway"`
	NamePrefix     string    `json:"name_prefix"`
	MaxUses        int       `json:"max_uses"`
	Uses           int       `json:"uses"`
	ExpiresAt      time.Time `json:"expires_at"`
	ExpectedPubkey string    `json:"expected_pubkey"`
	Note           string    `json:"note"`
	Revoked        bool      `json:"revoked"`
	Active         bool      `json:"active"`
	CreatedBy      string    `json:"created_by"`
}

// NewInvite is a created invite with its passphrase, shown once.
type NewInvite struct {
	Invite     Invite `json:"invite"`
	Passphrase string `json:"passphrase"`
	Link       string `json:"link"`
}

// CreateInvite creates an invite.
func (c *Client) CreateInvite(ctx context.Context, req InviteRequest) (NewInvite, error) {
	var out NewInvite
	return out, c.do(ctx, http.MethodPost, "/invites", req, &out)
}

// Invites lists invites.
func (c *Client) Invites(ctx context.Context) ([]Invite, error) {
	var out struct {
		Invites []Invite `json:"invites"`
	}
	return out.Invites, c.do(ctx, http.MethodGet, "/invites", nil, &out)
}

// RevokeInvite revokes an invite.
func (c *Client) RevokeInvite(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/invites/"+url.PathEscape(id), nil, nil)
}

// RemoveGateway deletes a gateway: its token stops working, its bridge is
// disconnected, and its connect history and logs are deleted.
func (c *Client) RemoveGateway(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/gateways/"+url.PathEscape(name), nil, nil)
}

// Gateways lists gateways with their status.
func (c *Client) Gateways(ctx context.Context) ([]Gateway, error) {
	var out struct {
		Gateways []Gateway `json:"gateways"`
	}
	return out.Gateways, c.do(ctx, http.MethodGet, "/gateways", nil, &out)
}

// Relink asks a gateway's bridge to reopen its node link now, skipping its
// retry wait. It ends any session using the node.
func (c *Client) Relink(ctx context.Context, gateway string) error {
	return c.do(ctx, http.MethodPost, "/gateways/"+url.PathEscape(gateway)+"/relink", nil, nil)
}

// InstallOneLiner is the Linux install command for a passphrase.
func InstallOneLiner(passphrase string) string {
	return `curl -fsSL https://raw.githubusercontent.com/sierragridteam/sierra-meshrelay/main/scripts/install.sh | sh -s -- --passphrase "` +
		strings.ReplaceAll(passphrase, "-", " ") + `"`
}

// InstallOneLinerWindows is the Windows (PowerShell) install command for a
// passphrase.
func InstallOneLinerWindows(passphrase string) string {
	return `& ([scriptblock]::Create((irm https://raw.githubusercontent.com/sierragridteam/sierra-meshrelay/main/scripts/install.ps1))) -Passphrase "` +
		strings.ReplaceAll(passphrase, "-", " ") + `"`
}

// InviteMessage is a message to send the host with everything they need.
func InviteMessage(inv NewInvite) string {
	words := strings.ReplaceAll(inv.Passphrase, "-", " ")
	var b strings.Builder
	b.WriteString("Here's how to connect your MeshCore node to SIERRA.\n\n")
	b.WriteString("Your SIERRA passphrase is:  " + words + "\n\n")
	b.WriteString("Use a computer that stays on, on the same network as your node.\n\n")
	b.WriteString("On Windows, open PowerShell as administrator (right-click PowerShell, Run as administrator) and run:\n\n")
	b.WriteString("  " + InstallOneLinerWindows(inv.Passphrase) + "\n\n")
	b.WriteString("On Linux or a Raspberry Pi, run:\n\n")
	b.WriteString("  " + InstallOneLiner(inv.Passphrase) + "\n\n")
	fmt.Fprintf(&b, "The passphrase works until %s.", inv.Invite.ExpiresAt.Local().Format("Mon 2 Jan"))
	if inv.Invite.MaxUses > 1 {
		fmt.Fprintf(&b, " It can set up %d bridges.", inv.Invite.MaxUses)
	}
	b.WriteString("\n")
	return b.String()
}

// BridgeLog is one log line a bridge sent the relay.
type BridgeLog struct {
	ID         int64             `json:"id"`
	ReceivedAt time.Time         `json:"received_at"`
	T          string            `json:"t"`
	Level      string            `json:"level"`
	Msg        string            `json:"msg"`
	Attrs      map[string]string `json:"attrs"`
}

// GatewayEvent is a relay-side event for a gateway (connected, disconnected…).
type GatewayEvent struct {
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"`
	Detail string    `json:"detail"`
}

// Logs returns a gateway's log lines after id `after` (oldest first, at most
// limit of the newest), and its recent relay-side events (newest first).
func (c *Client) Logs(ctx context.Context, gateway string, after int64, limit int) ([]BridgeLog, []GatewayEvent, error) {
	var out struct {
		Logs   []BridgeLog    `json:"logs"`
		Events []GatewayEvent `json:"events"`
	}
	q := url.Values{"after": {fmt.Sprint(after)}, "limit": {fmt.Sprint(limit)}}
	err := c.do(ctx, http.MethodGet, "/gateways/"+url.PathEscape(gateway)+"/logs?"+q.Encode(), nil, &out)
	return out.Logs, out.Events, err
}

// BridgeUpdate is the release the relay offers bridges.
type BridgeUpdate struct {
	Version  string   `json:"version"`
	Gateways []string `json:"gateways,omitempty"`
}

// GetBridgeUpdate returns the release the relay offers bridges.
func (c *Client) GetBridgeUpdate(ctx context.Context) (BridgeUpdate, error) {
	var out BridgeUpdate
	return out, c.do(ctx, http.MethodGet, "/bridge-update", nil, &out)
}

// SetBridgeUpdate sets the release to offer ("" stops offering), and
// returns the gateways it was offered to right away.
func (c *Client) SetBridgeUpdate(ctx context.Context, u BridgeUpdate) ([]string, error) {
	var out struct {
		Offered []string `json:"offered"`
	}
	return out.Offered, c.do(ctx, http.MethodPut, "/bridge-update", u, &out)
}

// unreachable explains a request that got no answer from the relay at base,
// without the request URL and method that *url.Error adds.
func unreachable(base string, err error) error {
	var ue *url.Error
	if errors.As(err, &ue) && !errors.Is(err, context.Canceled) {
		host := base
		if u, perr := url.Parse(base); perr == nil && u.Host != "" {
			host = u.Host
		}
		return fmt.Errorf("couldn't reach the relay at %s: %w", host, ue.Err)
	}
	return err
}
