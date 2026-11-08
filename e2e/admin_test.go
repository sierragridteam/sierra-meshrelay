package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sierragridteam/sierra-meshrelay/internal/admin"
	"github.com/sierragridteam/sierra-meshrelay/internal/config"
	"github.com/sierragridteam/sierra-meshrelay/internal/pairing"
)

func adminClient(lr *localRelay) *admin.Client {
	return &admin.Client{APIBase: config.HTTPBase(lr.wsBase()), Token: lr.admin}
}

func TestAdminApproveWithDefaultNames(t *testing.T) {
	lr := startRelay(t)
	ac := adminClient(lr)
	pc := pairClient(lr)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Two bridges whose nodes share a name: the second gets -2.
	node := startNode(t, "Paired Node")
	for i, want := range []string{"paired-node", "paired-node-2"} {
		start, err := pc.Start(ctx, pairReq(node, "city-bastion"))
		if err != nil {
			t.Fatal(err)
		}
		ps, err := ac.Pairings(ctx)
		if err != nil || len(ps) != 1 {
			t.Fatalf("pairings: %+v %v", ps, err)
		}
		p := ps[0]
		if p.UserCode != start.UserCode || p.HostLabel != "city-bastion" || p.Node.Name != "Paired Node" || p.SuggestedGateway != want {
			t.Fatalf("pending %d: %+v", i, p)
		}
		g, err := ac.Approve(ctx, strings.ToLower(start.UserCode), "", false)
		if err != nil || g.Name != want {
			t.Fatalf("approve %d: %+v %v", i, g, err)
		}
		if res, err := pc.Wait(ctx, start); err != nil || res.Gateway != want {
			t.Fatalf("collect %d: %+v %v", i, res, err)
		}
	}

	// No node found yet: named after the computer.
	start, _ := pc.Start(ctx, pairReq(nil, "Jay's PC"))
	if g, err := ac.Approve(ctx, start.UserCode, "", false); err != nil || g.Name != "jay-s-pc" {
		t.Fatalf("named after host: %+v %v", g, err)
	}

	// An explicit name still wins; an existing one needs repair.
	start, _ = pc.Start(ctx, pairReq(node, "pc"))
	if _, err := ac.Approve(ctx, start.UserCode, "paired-node", false); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("existing name without repair: %v", err)
	}
	if g, err := ac.Approve(ctx, start.UserCode, "paired-node", true); err != nil || g.Name != "paired-node" {
		t.Fatalf("repair: %+v %v", g, err)
	}

	start, _ = pc.Start(ctx, pairReq(nil, "h"))
	if err := ac.Deny(ctx, start.UserCode); err != nil {
		t.Fatal(err)
	}
	if _, err := pc.Wait(ctx, start); err != pairing.ErrDenied {
		t.Fatalf("after deny: %v", err)
	}

	gs, err := ac.Gateways(ctx)
	if err != nil || len(gs) != 3 {
		t.Fatalf("gateways: %+v %v", gs, err)
	}
}

func TestAdminInviteNamedAtRedemption(t *testing.T) {
	lr := startRelay(t)
	ac := adminClient(lr)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	inv, err := ac.CreateInvite(ctx, admin.InviteRequest{Note: "for Jay", ExpiresInHours: 48})
	if err != nil {
		t.Fatal(err)
	}
	msg := admin.InviteMessage(inv)
	words := strings.ReplaceAll(inv.Passphrase, "-", " ")
	if !strings.Contains(msg, words) || !strings.Contains(msg, `install.sh | sh -s -- --passphrase "`+words+`"`) ||
		!strings.Contains(msg, `install.ps1))) -Passphrase "`+words+`"`) {
		t.Fatalf("message:\n%s", msg)
	}

	node := startNode(t, "Jay Node")
	req := pairReq(node, "Jay's PC")
	req.Node.Name = "Jay Node"
	req.Passphrase = words
	res, err := pairClient(lr).Redeem(ctx, req)
	if err != nil || res.Gateway != "jay-node" {
		t.Fatalf("redeem: %+v %v", res, err)
	}

	invs, err := ac.Invites(ctx)
	if err != nil || len(invs) != 1 || invs[0].Uses != 1 || invs[0].Active || invs[0].Note != "for Jay" {
		t.Fatalf("invites: %+v %v", invs, err)
	}
	if err := ac.RevokeInvite(ctx, invs[0].ID); err != nil {
		t.Fatal(err)
	}

	bad := &admin.Client{APIBase: ac.APIBase, Token: "smc_not-an-admin"}
	if _, err := bad.Pairings(ctx); err == nil {
		t.Fatal("a bad admin token was accepted")
	}
}
