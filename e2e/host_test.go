package e2e

// End-to-end tests for pause and pairing: the real relay, the real bridge and
// the bridge's pairing client, in process.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sierragridteam/sierra-meshrelay/internal/bridge"
	"github.com/sierragridteam/sierra-meshrelay/internal/config"
	"github.com/sierragridteam/sierra-meshrelay/internal/connect"
	"github.com/sierragridteam/sierra-meshrelay/internal/fakenode"
	"github.com/sierragridteam/sierra-meshrelay/internal/pairing"
	"github.com/sierragridteam/sierra-meshrelay/internal/relayclient"
	"github.com/sierragridteam/sierra-meshrelay/meshproto"
	"github.com/sierragridteam/sierra-meshrelay/relay"
)

// --- pause ---------------------------------------------------------------------

func TestLocalPause(t *testing.T) {
	lr := startRelay(t)
	tok := lr.createGateway("cabin")
	op := lr.createClient("tester", "operator")
	node := startNode(t, "Cabin Node")
	b, _ := startBridge(t, lr, "cabin", tok, node)
	lr.waitGateway("cabin", "ready", connected)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := relayclient.Dial(ctx, lr.wsBase(), "cabin", op, false)
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Hour).Truncate(time.Second)
	b.Pause(until)

	// The lease holder is told at once.
	var le meshproto.LeaseEnding
	nextControl(t, c, meshproto.TypeLeaseEnding, &le)
	if le.Reason != meshproto.ReasonHostPaused || le.InS != 0 {
		t.Fatalf("lease_ending %+v, want host_paused/0", le)
	}

	// New clients get node_paused, with the time, in plain words through connect.
	d := lr.waitGateway("cabin", "paused", func(d gatewayDetail) bool {
		return d.Node != nil && d.Node.State == meshproto.NodePaused
	})
	if d.Node.Until != until.UTC().Format(time.RFC3339) {
		t.Fatalf("relay sees until %q", d.Node.Until)
	}
	_, err = relayclient.Dial(ctx, lr.wsBase(), "cabin", op, false)
	var re *relayclient.RefusedError
	if !errors.As(err, &re) || re.Code != meshproto.NodeStateErr(meshproto.NodePaused) {
		t.Fatalf("client during pause: %v, want node_paused", err)
	}
	if got := connect.Explain(err); !strings.HasPrefix(got, "The host has paused sharing until ") {
		t.Errorf("Explain = %q", got)
	}

	// The public status shows the gateway as deliberately unavailable.
	var pub struct {
		Gateways []struct {
			Name      string `json:"name"`
			NodeState string `json:"node_state"`
			Ready     bool   `json:"ready"`
		} `json:"gateways"`
	}
	resp, err := http.Get(lr.base + relay.PathPrefix + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	decodeJSON(t, resp, &pub)
	if len(pub.Gateways) != 1 || pub.Gateways[0].NodeState != meshproto.NodePaused || pub.Gateways[0].Ready {
		t.Fatalf("public status %+v", pub)
	}

	b.Resume()
	lr.waitGateway("cabin", "resumed", connected)
	c2, err := relayclient.Dial(ctx, lr.wsBase(), "cabin", op, false)
	if err != nil {
		t.Fatalf("lease after resume: %v", err)
	}
	c2.Close()
}

// --- pairing -------------------------------------------------------------------

func pairReq(node *fakenode.Node, host string) meshproto.PairStartRequest {
	r := meshproto.PairStartRequest{
		Agent:     meshproto.Agent{Name: "meshbridge", Version: "e2e", OS: "test", Arch: "test"},
		HostLabel: host,
		Node:      meshproto.PairNode{Node: meshproto.Node{State: meshproto.NodeDisconnected}},
	}
	if node != nil {
		r.Node = meshproto.PairNode{Node: meshproto.Node{State: meshproto.NodeConnected,
			Pubkey: node.PubkeyHex(), Name: "Paired Node"}, Link: meshproto.LinkTCP}
	}
	return r
}

func pairClient(lr *localRelay) *pairing.Client {
	return &pairing.Client{APIBase: config.HTTPBase(lr.wsBase())}
}

// approveCode waits for code to show up in the admin listing, then approves it.
func (lr *localRelay) approveCode(code string, body map[string]any) int {
	lr.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var out struct {
			Pairings []relay.PendingPairing `json:"pairings"`
		}
		lr.api("GET", "/pairings", nil, &out)
		for _, p := range out.Pairings {
			if strings.EqualFold(strings.ReplaceAll(p.UserCode, "-", ""), strings.ReplaceAll(code, "-", "")) {
				return lr.api("POST", "/pairings/"+code+"/approve", body, nil)
			}
		}
		if time.Now().After(deadline) {
			lr.t.Fatalf("code %s never listed", code)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestLocalPairingDeviceCode(t *testing.T) {
	lr := startRelay(t)
	op := lr.createClient("tester", "operator")
	node := startNode(t, "Paired Node")
	pc := pairClient(lr)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start, err := pc.Start(ctx, pairReq(node, "Jay's PC"))
	if err != nil {
		t.Fatal(err)
	}
	if len(start.UserCode) != 7 || start.UserCode[3] != '-' || strings.ContainsAny(start.UserCode, "01OIL") {
		t.Fatalf("user code %q", start.UserCode)
	}
	if !strings.HasSuffix(start.VerificationURL, "/mesh/pair?code="+start.UserCode) {
		t.Fatalf("verification URL %q", start.VerificationURL)
	}

	waited := make(chan meshproto.PairResult, 1)
	go func() {
		res, err := pc.Wait(ctx, start)
		if err != nil {
			t.Errorf("wait: %v", err)
		}
		waited <- res
	}()
	// The admin types the code the host texted them, however they typed it.
	if code := lr.approveCode(strings.ToLower(strings.ReplaceAll(start.UserCode, "-", "")), map[string]any{"gateway": "jays-pc"}); code != 200 {
		t.Fatalf("approve: HTTP %d", code)
	}
	res := <-waited
	if res.Gateway != "jays-pc" || !strings.HasPrefix(res.Token, "smb_") || res.ExpectedPubkey != node.PubkeyHex() ||
		res.Relay != lr.wsBase() {
		t.Fatalf("approved result %+v", res)
	}

	// Delivered exactly once: the next poll reads expired.
	time.Sleep(time.Second)
	if _, err := pc.Wait(ctx, meshproto.PairStartResponse{PairID: start.PairID, PollSecret: start.PollSecret,
		Interval: 0, ExpiresIn: 1}); !errors.Is(err, pairing.ErrExpired) {
		t.Fatalf("second collection: %v, want expired", err)
	}

	// The token works, and the pin set at approval admits the node at once.
	startBridge(t, lr, "jays-pc", res.Token, node)
	d := lr.waitGateway("jays-pc", "paired bridge online", connected)
	if d.PinnedPubkey != node.PubkeyHex() {
		t.Fatalf("pin %q", d.PinnedPubkey)
	}
	c, err := relayclient.Dial(ctx, lr.wsBase(), "jays-pc", op, false)
	if err != nil {
		t.Fatalf("lease on the paired gateway: %v", err)
	}
	c.Close()
}

func TestLocalPairingWrongSecretAndDeny(t *testing.T) {
	lr := startRelay(t)
	pc := pairClient(lr)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start, err := pc.Start(ctx, pairReq(nil, "host"))
	if err != nil {
		t.Fatal(err)
	}
	lr.approveCode(start.UserCode, map[string]any{"gateway": "stolen"})
	// Knowing the pair id and code without the poll secret collects nothing.
	_, err = pc.Wait(ctx, meshproto.PairStartResponse{PairID: start.PairID, PollSecret: "guess", Interval: 1, ExpiresIn: 1})
	if !errors.Is(err, pairing.ErrExpired) {
		t.Fatalf("wrong secret: %v, want expired", err)
	}

	start2, err := pc.Start(ctx, pairReq(nil, "host"))
	if err != nil {
		t.Fatal(err)
	}
	if code := lr.api("POST", "/pairings/"+start2.UserCode+"/deny", nil, nil); code != 200 {
		t.Fatalf("deny: HTTP %d", code)
	}
	if _, err := pc.Wait(ctx, start2); !errors.Is(err, pairing.ErrDenied) {
		t.Fatalf("denied: %v", err)
	}
	if code := lr.api("POST", "/pairings/"+start2.UserCode+"/approve", map[string]any{"gateway": "late"}, nil); code != 404 {
		t.Fatalf("approving a consumed code: HTTP %d, want 404", code)
	}
}

func TestLocalPairingRepair(t *testing.T) {
	lr := startRelay(t)
	oldTok := lr.createGateway("cabin")
	node := startNode(t, "Cabin Node")
	oldBridge, _ := startBridge(t, lr, "cabin", oldTok, node)
	lr.waitGateway("cabin", "old bridge", connected)

	pc := pairClient(lr)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start, err := pc.Start(ctx, pairReq(node, "new PC"))
	if err != nil {
		t.Fatal(err)
	}
	if code := lr.approveCode(start.UserCode, map[string]any{"gateway": "cabin"}); code != http.StatusConflict {
		t.Fatalf("approve onto an existing gateway without repair: HTTP %d, want 409", code)
	}
	if code := lr.api("POST", "/pairings/"+start.UserCode+"/approve", map[string]any{"gateway": "cabin", "repair": true}, nil); code != 200 {
		t.Fatalf("repair approve: HTTP %d", code)
	}
	res, err := pc.Wait(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	// The old bridge is closed with 4001 and stops retrying quickly.
	waitStatus(t, oldBridge, "old bridge rejected", func(st bridge.Status) bool {
		return !st.RelayConnected && st.RelayClass == bridge.ClassAuth
	})
	if res.Token == oldTok {
		t.Fatal("re-pair kept the old token")
	}
	startBridge(t, lr, "cabin", res.Token, node)
	lr.waitGateway("cabin", "new bridge", connected)
}

// --- passphrase invites --------------------------------------------------------

func (lr *localRelay) invite(body map[string]any) (string, int) {
	lr.t.Helper()
	var out struct {
		Passphrase string `json:"passphrase"`
		Link       string `json:"link"`
	}
	code := lr.api("POST", "/invites", body, &out)
	if code == 200 && (len(strings.Split(out.Passphrase, "-")) != 4 || out.Link != "sierramesh://pair?passphrase="+out.Passphrase) {
		lr.t.Fatalf("invite response: %+v", out)
	}
	return out.Passphrase, code
}

func TestLocalPairingPassphrase(t *testing.T) {
	lr := startRelay(t)
	phrase, code := lr.invite(map[string]any{"gateway": "jay-murphys", "note": "Jay, Murphys, Heltec V4"})
	if code != 200 {
		t.Fatalf("create invite: HTTP %d", code)
	}
	node := startNode(t, "Jay Node")
	pc := pairClient(lr)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Typed from a text message on a phone: capitals, spaces, a comma.
	words := strings.Split(phrase, "-")
	sloppy := strings.ToUpper(words[0][:1]) + words[0][1:] + " " + words[1] + ",  " + words[2] + " " + strings.ToUpper(words[3])
	req := pairReq(node, "Jay's PC")
	req.Passphrase = sloppy
	res, err := pc.Redeem(ctx, req)
	if err != nil {
		t.Fatalf("redeem %q: %v", sloppy, err)
	}
	if res.Gateway != "jay-murphys" || res.ExpectedPubkey != node.PubkeyHex() || !strings.HasPrefix(res.Token, "smb_") {
		t.Fatalf("result %+v", res)
	}
	// Single use.
	if _, err := pc.Redeem(ctx, req); !errors.Is(err, pairing.ErrInvalidPassphrase) {
		t.Fatalf("second redemption: %v, want invalid passphrase", err)
	}
	req.Passphrase = "maple-river-quartz-otter"
	if _, err := pc.Redeem(ctx, req); !errors.Is(err, pairing.ErrInvalidPassphrase) {
		t.Fatalf("wrong passphrase: %v", err)
	}

	startBridge(t, lr, "jay-murphys", res.Token, node)
	lr.waitGateway("jay-murphys", "online", connected)

	var list struct {
		Invites []struct {
			Uses   int  `json:"uses"`
			Active bool `json:"active"`
		} `json:"invites"`
	}
	lr.api("GET", "/invites", nil, &list)
	if len(list.Invites) != 1 || list.Invites[0].Uses != 1 || list.Invites[0].Active {
		t.Fatalf("invites %+v", list)
	}
}

func TestLocalPairingMultiUseAndPubkey(t *testing.T) {
	lr := startRelay(t)
	phrase, _ := lr.invite(map[string]any{"name_prefix": "workshop", "max_uses": 2})
	pc := pairClient(lr)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for i, want := range []string{"workshop-1", "workshop-2"} {
		req := pairReq(nil, "pi")
		req.Passphrase = phrase
		res, err := pc.Redeem(ctx, req)
		if err != nil || res.Gateway != want {
			t.Fatalf("redemption %d: %+v %v, want %s", i, res, err, want)
		}
		if res.ExpectedPubkey != "" {
			t.Fatalf("no node reported, yet pinned %q", res.ExpectedPubkey)
		}
	}
	req := pairReq(nil, "pi")
	req.Passphrase = phrase
	if _, err := pc.Redeem(ctx, req); !errors.Is(err, pairing.ErrInvalidPassphrase) {
		t.Fatalf("third use: %v", err)
	}

	// An invite bound to a node refuses any other node, as an invalid passphrase.
	want := startNode(t, "Expected")
	other := startNode(t, "Other")
	phrase2, _ := lr.invite(map[string]any{"gateway": "bound", "expected_pubkey": want.PubkeyHex()})
	req = pairReq(other, "pc")
	req.Passphrase = phrase2
	if _, err := pc.Redeem(ctx, req); !errors.Is(err, pairing.ErrInvalidPassphrase) {
		t.Fatalf("wrong node: %v", err)
	}
	req = pairReq(want, "pc")
	req.Passphrase = phrase2
	if res, err := pc.Redeem(ctx, req); err != nil || res.Gateway != "bound" {
		t.Fatalf("right node: %+v %v", res, err)
	}

	// A revoked invite is just another invalid passphrase.
	phrase3, _ := lr.invite(map[string]any{"gateway": "revoked-one"})
	var list struct {
		Invites []struct {
			ID      string `json:"id"`
			Gateway string `json:"gateway"`
		} `json:"invites"`
	}
	lr.api("GET", "/invites", nil, &list)
	for _, i := range list.Invites {
		if i.Gateway == "revoked-one" {
			lr.api("DELETE", "/invites/"+i.ID, nil, nil)
		}
	}
	req.Passphrase = phrase3
	if _, err := pc.Redeem(ctx, req); !errors.Is(err, pairing.ErrInvalidPassphrase) {
		t.Fatalf("revoked: %v", err)
	}
}

func TestLocalPairingRateLimits(t *testing.T) {
	lr := startRelay(t, func(c *relay.Config) {
		c.Pairing.PassphrasesPerIP = 3
		c.Pairing.MaxPending = 2
	})
	pc := pairClient(lr)
	ctx := context.Background()
	req := pairReq(nil, "guesser")
	req.Passphrase = "wrong-wrong-wrong-wrong"
	for i := range 3 {
		if _, err := pc.Redeem(ctx, req); !errors.Is(err, pairing.ErrInvalidPassphrase) {
			t.Fatalf("guess %d: %v", i, err)
		}
	}
	if _, err := pc.Redeem(ctx, req); !errors.Is(err, pairing.ErrRateLimited) {
		t.Fatalf("fourth guess: %v, want rate limited", err)
	}
	for i := range 2 {
		if _, err := pc.Start(ctx, pairReq(nil, "h")); err != nil {
			t.Fatalf("code %d: %v", i, err)
		}
	}
	if _, err := pc.Start(ctx, pairReq(nil, "h")); !errors.Is(err, pairing.ErrRateLimited) {
		t.Fatalf("third pending code: %v, want 429", err)
	}
}

func decodeJSON(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if err := jsonDecode(resp, v); err != nil {
		t.Fatal(err)
	}
}
