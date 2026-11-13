package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sierragridteam/sierra-meshrelay/internal/admin"
)

// fakeAPI answers the relay's client routes and records the requests.
type fakeAPI struct {
	clients []map[string]any
	got     []string // "METHOD path body"
	post    func(body map[string]any) (int, any)
}

func (f *fakeAPI) server(t *testing.T) *admin.Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.got = append(f.got, r.Method+" "+r.URL.Path+" "+strings.TrimSpace(string(b)))
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/clients":
			_ = json.NewEncoder(w).Encode(map[string]any{"clients": f.clients})
		case r.Method == "GET" && r.URL.Path == "/gateways":
			_ = json.NewEncoder(w).Encode(map[string]any{"gateways": []map[string]any{{"name": "cabin"}}})
		case r.Method == "POST" && r.URL.Path == "/clients":
			var body map[string]any
			_ = json.Unmarshal(b, &body)
			code, out := f.post(body)
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == "DELETE":
			_ = json.NewEncoder(w).Encode(map[string]any{"leases_ended": 1})
		case r.Method == "PATCH":
			_ = json.NewEncoder(w).Encode(map[string]any{"role": "admin"})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return &admin.Client{APIBase: srv.URL, Token: "smc_test"}
}

func run(t *testing.T, c *admin.Client, sub string, pos []string, o adminTokenOpts, stdin string, tty bool) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := runAdminClients(context.Background(), c, sub, pos, o, bufio.NewReader(strings.NewReader(stdin)), tty, &out)
	return out.String(), err
}

func TestAccessGrant(t *testing.T) {
	f := &fakeAPI{post: func(body map[string]any) (int, any) {
		return 200, map[string]any{"client": "cabin." + body["label"].(string), "role": "host", "gateway": body["gateway"],
			"expires_at": "2027-05-12T00:00:00Z", "token": "smh_secret"}
	}}
	c := f.server(t)
	out, err := run(t, c, "access", []string{"grant", "cabin", "jay-laptop"}, adminTokenOpts{expires: "90d", note: "Jay"}, "", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Issued cabin.jay-laptop: it opens gateway cabin only", "smh_secret", "shown only this once",
		"meshbridge connect cabin", "$env:MESHBRIDGE_CLIENT_TOKEN", "admin access remove cabin.jay-laptop"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	post := f.got[len(f.got)-1]
	for _, want := range []string{`"role":"host"`, `"gateway":"cabin"`, `"label":"jay-laptop"`, `"expires_in_days":90`, `"note":"Jay"`} {
		if !strings.Contains(post, want) {
			t.Errorf("request lacks %s: %s", want, post)
		}
	}

	if _, err := run(t, c, "access", []string{"grant", "nowhere", "x"}, adminTokenOpts{}, "", false); err == nil ||
		!strings.Contains(err.Error(), `no gateway named "nowhere"`) {
		t.Errorf("unknown gateway: %v", err)
	}
	if _, err := run(t, c, "access", []string{"grant", "cabin"}, adminTokenOpts{}, "", false); err == nil ||
		!strings.Contains(err.Error(), "usage") {
		t.Errorf("no label: %v", err)
	}
	if _, err := run(t, c, "access", []string{"grant", "cabin", "x"}, adminTokenOpts{expires: "soon"}, "", false); err == nil {
		t.Error("accepted -expires soon")
	}
}

// A relay from before host tokens refuses the request's fields: say so.
func TestAccessGrantOldRelay(t *testing.T) {
	f := &fakeAPI{post: func(map[string]any) (int, any) {
		return 400, map[string]any{"message": `json: unknown field "gateway"`}
	}}
	_, err := run(t, f.server(t), "access", []string{"grant", "cabin", "jay"}, adminTokenOpts{}, "", false)
	if !errors.Is(err, admin.ErrNoHostTokens) {
		t.Fatalf("err = %v", err)
	}
}

func TestClientsList(t *testing.T) {
	f := &fakeAPI{clients: []map[string]any{
		{"name": "cabin.jay", "role": "host", "gateway": "cabin", "expires_at": "2027-05-12T00:00:00Z", "created_by": "dpup", "note": "Jay"},
		{"name": "ridge.amy", "role": "host", "gateway": "ridge", "expires_at": "2027-05-12T00:00:00Z"},
		{"name": "bridge-dev-client", "role": "operator"},
		{"name": "dpup", "role": "admin", "bootstrap": true},
	}}
	c := f.server(t)
	out, err := run(t, c, "clients", nil, adminTokenOpts{}, "", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"cabin.jay", "ridge.amy", "every gateway", "config", "retired role",
		"meshbridge admin client set bridge-dev-client -role admin"} {
		if !strings.Contains(out, want) {
			t.Errorf("clients lacks %q:\n%s", want, out)
		}
	}
	out, err = run(t, c, "access", []string{"list", "cabin"}, adminTokenOpts{}, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "cabin.jay") || strings.Contains(out, "ridge.amy") || strings.Contains(out, "every gateway") {
		t.Errorf("access list cabin:\n%s", out)
	}
	out, _ = run(t, c, "access", nil, adminTokenOpts{}, "", false)
	if !strings.Contains(out, "ridge.amy") || strings.Contains(out, "bridge-dev-client") {
		t.Errorf("access (all):\n%s", out)
	}
	out, _ = run(t, c, "access", []string{"list", "valley"}, adminTokenOpts{}, "", false)
	if !strings.Contains(out, "valley has no access tokens") {
		t.Errorf("empty:\n%s", out)
	}
}

func TestRevokeAndSet(t *testing.T) {
	f := &fakeAPI{clients: []map[string]any{
		{"name": "cabin.jay", "role": "host", "gateway": "cabin"},
		{"name": "dpup", "role": "admin", "bootstrap": true},
	}}
	c := f.server(t)
	if _, err := run(t, c, "access", []string{"remove", "cabin.jay"}, adminTokenOpts{}, "", false); err == nil ||
		!strings.Contains(err.Error(), "-yes") {
		t.Errorf("no prompt without a terminal: %v", err)
	}
	if _, err := run(t, c, "access", []string{"remove", "cabin.jay"}, adminTokenOpts{}, "n\n", true); err == nil {
		t.Error("revoked on n")
	}
	out, err := run(t, c, "access", []string{"remove", "cabin.jay"}, adminTokenOpts{}, "y\n", true)
	if err != nil || !strings.Contains(out, "opens gateway cabin") || !strings.Contains(out, "Ended 1 session") {
		t.Errorf("revoke: %v\n%s", err, out)
	}
	if f.got[len(f.got)-1] != "DELETE /clients/cabin.jay " {
		t.Errorf("request: %q", f.got[len(f.got)-1])
	}
	if _, err := run(t, c, "client", []string{"remove", "dpup"}, adminTokenOpts{yes: true}, "", false); err == nil ||
		!strings.Contains(err.Error(), "configured on the relay") {
		t.Errorf("bootstrap: %v", err)
	}
	if _, err := run(t, c, "client", []string{"remove", "ghost"}, adminTokenOpts{yes: true}, "", false); err == nil {
		t.Error("removed an unknown token")
	}

	if _, err := run(t, c, "client", []string{"set", "bridge-dev-client"}, adminTokenOpts{role: "host"}, "", false); err == nil {
		t.Error("set -role host")
	}
	out, err = run(t, c, "client", []string{"set", "bridge-dev-client"}, adminTokenOpts{role: "admin"}, "", false)
	if err != nil || !strings.Contains(out, "is an admin token now") ||
		f.got[len(f.got)-1] != `PATCH /clients/bridge-dev-client {"role":"admin"}` {
		t.Errorf("set: %v %q %q", err, out, f.got[len(f.got)-1])
	}
}

func TestParseDays(t *testing.T) {
	for in, want := range map[string]int{"": 0, "90d": 90, "30": 30, " 7d ": 7} {
		if got, err := parseDays(in); err != nil || got != want {
			t.Errorf("parseDays(%q) = %d, %v", in, got, err)
		}
	}
	for _, in := range []string{"0", "-3d", "soon", "1w"} {
		if _, err := parseDays(in); err == nil {
			t.Errorf("parseDays(%q) accepted", in)
		}
	}
}
