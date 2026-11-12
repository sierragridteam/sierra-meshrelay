// Package control is the bridge's local control API: how the CLI (and later
// the native tray/menu bar apps) see status and pause or resume sharing.
//
// It listens on loopback only (127.0.0.1:8737 by default, so no firewall
// prompt) and requires a bearer secret read from a file that only the service
// account and admins can read. See docs/mesh-bridge-handoff.md, "Local control
// API".
package control

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sierragridteam/sierra-meshrelay/internal/bridge"
	"github.com/sierragridteam/sierra-meshrelay/meshproto"
)

// DefaultAddr is the control API's address.
const DefaultAddr = "127.0.0.1:8737"

// TokenFile is the secret's file name, next to the config file.
const TokenFile = "control.token"

// LoadOrCreateToken reads the control secret at path, creating it (0600) if
// it doesn't exist.
func LoadOrCreateToken(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil {
		if tok := strings.TrimSpace(string(b)); tok != "" {
			return tok, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	tok := base64.RawURLEncoding.EncodeToString(raw[:])
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", err
	}
	return tok, nil
}

// ReadToken reads an existing control secret.
func ReadToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// Server serves the control API for one bridge.
type Server struct {
	Bridge  *bridge.Bridge
	Token   string
	Gateway string
	Version string
	// Now is the clock (tests). Default time.Now.
	Now func() time.Time
}

// ListenAndServe serves on addr, which must be a loopback address, until ctx
// ends.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("control API must listen on loopback, not %s", addr)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return s.Serve(ctx, ln)
}

// Serve serves on ln until ctx ends.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Handler is the API's HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", s.auth(s.status))
	mux.HandleFunc("POST /pause", s.auth(s.pause))
	mux.HandleFunc("POST /resume", s.auth(s.resume))
	mux.HandleFunc("POST /find-node", s.auth(s.findNode))
	return mux
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if s.Token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Status levels.
const (
	Green = "green"
	Amber = "amber"
	Red   = "red"
)

// StatusDoc is GET /status: everything the tray menu or `meshbridge status`
// shows. Reason is one plain sentence for the host.
type StatusDoc struct {
	Level           string     `json:"level"` // green | amber | red
	Reason          string     `json:"reason"`
	Gateway         string     `json:"gateway"`
	Version         string     `json:"version"`
	RelayConnected  bool       `json:"relay_connected"`
	Node            NodeDoc    `json:"node"`
	Paused          bool       `json:"paused"`
	PauseUntil      *time.Time `json:"pause_until,omitempty"` // absent while paused: until resumed
	ClientAttached  bool       `json:"client_attached"`
	LastMeshTraffic *time.Time `json:"last_mesh_traffic,omitempty"`
	// NodeDownSince is when the node became unavailable, while it is.
	NodeDownSince *time.Time `json:"node_down_since,omitempty"`
}

// NodeDoc is the node part of StatusDoc.
type NodeDoc struct {
	State  string `json:"state"`
	Name   string `json:"name,omitempty"`
	Model  string `json:"model,omitempty"`
	FW     string `json:"fw,omitempty"`
	Pubkey string `json:"pubkey,omitempty"`
	Link   string `json:"link,omitempty"`
	Detail string `json:"detail,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Describe builds the status document from a bridge snapshot.
func Describe(st bridge.Status, gateway, version string) StatusDoc {
	d := StatusDoc{
		Gateway: gateway, Version: version, RelayConnected: st.RelayConnected,
		Paused: st.Paused, ClientAttached: st.Attached,
		Node: NodeDoc{State: st.Node.State, Name: st.Node.Name, Model: st.Node.Model, FW: st.Node.FW,
			Pubkey: st.Node.Pubkey, Link: st.LinkKind, Detail: st.LinkDetail, Error: st.Node.Error},
	}
	if st.Paused && !st.PauseUntil.IsZero() {
		u := st.PauseUntil
		d.PauseUntil = &u
	}
	if !st.LastTraffic.IsZero() {
		t := st.LastTraffic
		d.LastMeshTraffic = &t
	}
	if !st.NodeDownSince.IsZero() && st.Node.State != meshproto.NodeConnected {
		t := st.NodeDownSince
		d.NodeDownSince = &t
	}
	switch {
	case st.Node.State == meshproto.NodeMismatch:
		d.Level, d.Reason = Red, "This isn't the node SIERRA expects. Contact your SIERRA contact."
	case st.Paused && st.PauseUntil.IsZero():
		d.Level, d.Reason = Amber, "Sharing is paused until you resume it."
	case st.Paused:
		d.Level, d.Reason = Amber, "Sharing is paused until "+st.PauseUntil.Local().Format("Mon 15:04")+"."
	case !st.RelayConnected && st.RelayClass == bridge.ClassAuth:
		d.Level, d.Reason = Red, "SIERRA didn't accept this bridge's credentials. Contact your SIERRA contact."
	case !st.RelayConnected && st.RelayClass == bridge.ClassUpgrade:
		d.Level, d.Reason = Red, "This bridge needs updating to talk to SIERRA."
	case !st.RelayConnected:
		d.Level, d.Reason = Red, "Can't reach SIERRA. Check this computer's internet connection."
	case st.Node.State != meshproto.NodeConnected && st.Node.Error == bridge.NodeErrResuming:
		d.Level, d.Reason = Amber, "Resuming sharing…"
	case st.Node.State != meshproto.NodeConnected:
		d.Level, d.Reason = Amber, "Connected to SIERRA, but your node isn't connected."
	default:
		d.Level, d.Reason = Green, "Connected to SIERRA."
	}
	return d
}

func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Describe(s.Bridge.Status(), s.Gateway, s.Version))
}

// PauseRequest is the body of POST /pause.
type PauseRequest struct {
	// Until is "until-resumed", "tomorrow" (08:00 tomorrow, local time), a
	// duration such as "1h", or an RFC 3339 time.
	Until string `json:"until"`
}

// ParseUntil turns a pause request into a time (zero: until resumed).
func ParseUntil(s string, now time.Time) (time.Time, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "until-resumed", "resumed", "indefinitely":
		return time.Time{}, nil
	case "tomorrow", "until-tomorrow":
		y, m, d := now.AddDate(0, 0, 1).Date()
		return time.Date(y, m, d, 8, 0, 0, 0, now.Location()), nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		if d <= 0 {
			return time.Time{}, fmt.Errorf("pause duration must be positive")
		}
		return now.Add(d), nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("pause until %q: want until-resumed, tomorrow, a duration like 1h, or an RFC 3339 time", s)
	}
	if !t.After(now) {
		return time.Time{}, fmt.Errorf("pause until %s is in the past", s)
	}
	return t, nil
}

func (s *Server) pause(w http.ResponseWriter, r *http.Request) {
	var req PauseRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request body"})
		return
	}
	until, err := ParseUntil(req.Until, s.now())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.Bridge.Pause(until)
	s.status(w, r)
}

func (s *Server) resume(w http.ResponseWriter, r *http.Request) {
	s.Bridge.Resume()
	s.status(w, r)
}

func (s *Server) findNode(w http.ResponseWriter, r *http.Request) {
	s.Bridge.Relink()
	s.status(w, r)
}

// Client calls a running bridge's control API.
type Client struct {
	Addr  string
	Token string
	HTTP  *http.Client
}

// Do sends a request and decodes the status document it returns.
func (c *Client) Do(ctx context.Context, method, path string, body any) (StatusDoc, error) {
	var doc StatusDoc
	var rd *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	} else {
		rd = strings.NewReader("")
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+c.Addr+path, rd)
	if err != nil {
		return doc, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return doc, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return doc, fmt.Errorf("bridge answered %d: %s", resp.StatusCode, e.Error)
	}
	return doc, json.NewDecoder(resp.Body).Decode(&doc)
}
