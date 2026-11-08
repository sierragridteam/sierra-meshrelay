package e2e

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sierragridteam/sierra-meshrelay/internal/admin"
	"github.com/sierragridteam/sierra-meshrelay/internal/config"
	"github.com/sierragridteam/sierra-meshrelay/internal/fakenode"
	"github.com/sierragridteam/sierra-meshrelay/meshops"
	"github.com/sierragridteam/sierra-meshrelay/meshops/meshsim"
	"github.com/sierragridteam/sierra-meshrelay/relay"
)

func withSecretKey(c *relay.Config) {
	c.SecretKey = make([]byte, 32)
	_, _ = rand.Read(c.SecretKey)
}

func startMeshNode(t *testing.T, name string, mesh *meshsim.Node) *fakenode.Node {
	t.Helper()
	n, err := fakenode.Listen("127.0.0.1:0", fakenode.Options{Name: name, Model: "e2e board", Mesh: mesh})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.Close() })
	return n
}

func hexKey(c string) string { return strings.Repeat(c, 64) }

func TestShareRepeaters(t *testing.T) {
	lr := startRelay(t, withSecretKey)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ridge := &meshsim.Repeater{Pubkey: hexKey("a"), Name: "Ridge RPT", AdminPassword: "rootpw", GuestPassword: "look", BatteryMV: 3950}
	valley := &meshsim.Repeater{Pubkey: hexKey("b"), Name: "Valley RPT", AdminPassword: "vpw", GuestPassword: "vguest", BatteryMV: 4100}
	cabinMesh := meshsim.New(ridge) // the cabin has heard Ridge only
	cabinMesh.AddInRange(valley)
	tok := lr.createGateway("cabin")
	startBridge(t, lr, "cabin", tok, startMeshNode(t, "Cabin Node", cabinMesh))
	lr.waitGateway("cabin", "cabin online", connected)

	otherMesh := meshsim.New(valley)
	otherTok := lr.createGateway("other")
	startBridge(t, lr, "other", otherTok, startMeshNode(t, "Other Node", otherMesh))
	lr.waitGateway("other", "other online", connected)

	host := &admin.Client{APIBase: config.HTTPBase(lr.wsBase()), Token: tok}
	other := &admin.Client{APIBase: host.APIBase, Token: otherTok}
	ac := adminClient(lr)

	near, err := host.NearbyRepeaters(ctx)
	if err != nil || len(near) != 1 || near[0].Name != "Ridge RPT" || near[0].Shared {
		t.Fatalf("nearby: %+v %v", near, err)
	}

	// A wrong password is refused and nothing is stored.
	if _, err := host.ShareRepeater(ctx, ridge.Pubkey, "", "wrong"); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("wrong password: %v", err)
	}
	if rs, _ := ac.Repeaters(ctx); len(rs) != 0 {
		t.Fatalf("stored after a failed check: %+v", rs)
	}

	res, err := host.ShareRepeater(ctx, ridge.Pubkey, "", "look")
	if err != nil || res.Check.Access != "guest" || res.Repeater.Name != "Ridge RPT" || res.Repeater.AddedByGateway != "cabin" ||
		res.Check.Status == nil || *res.Check.Status.BatteryMilliVolts != 3950 {
		t.Fatalf("share: %+v %v", res, err)
	}
	if !cabinMesh.HasContact(ridge.Pubkey) {
		t.Fatal("the host's own contact was removed")
	}

	// Another gateway can't take it over or remove it.
	if _, err := other.ShareRepeater(ctx, ridge.Pubkey, "", "look"); err == nil || !strings.Contains(err.Error(), "someone else") {
		t.Fatalf("takeover: %v", err)
	}
	if err := other.UnshareRepeater(ctx, ridge.Pubkey); err == nil {
		t.Fatal("another gateway removed the cabin's repeater")
	}

	// An admin shares Valley through the cabin, whose node hasn't heard it:
	// the contact is added for the check and removed after.
	got, err := ac.AddRepeater(ctx, valley.Pubkey, "cabin", "vpw")
	if err != nil || got.Check.Access != "admin" || got.Repeater.AddedByClient != "root" {
		t.Fatalf("admin add: %+v %v", got, err)
	}
	if cabinMesh.HasContact(valley.Pubkey) {
		t.Fatal("the check left a contact on the host's node")
	}

	// The poller's view has the passwords; the API never does.
	creds, err := lr.relay.RepeaterCredentials()
	if err != nil || len(creds) != 2 {
		t.Fatalf("credentials: %+v %v", len(creds), err)
	}
	for _, c := range creds {
		want := map[string]string{ridge.Pubkey: "look", valley.Pubkey: "vpw"}[c.Pubkey]
		if c.Password != want {
			t.Fatalf("credential for %s is wrong", c.Name)
		}
	}
	var raw map[string]any
	lr.api("GET", "/repeaters", nil, &raw)
	if s := strings.ToLower(jsonString(raw)); strings.Contains(s, "look") || strings.Contains(s, "vpw") || strings.Contains(s, "password") {
		t.Fatalf("the admin API exposes a password: %s", s)
	}

	mine, err := host.MyRepeaters(ctx)
	if err != nil || len(mine) != 1 || mine[0].Pubkey != ridge.Pubkey {
		t.Fatalf("mine: %+v %v", mine, err)
	}
	if err := host.UnshareRepeater(ctx, ridge.Pubkey); err != nil {
		t.Fatal(err)
	}
	if err := ac.RemoveRepeater(ctx, valley.Pubkey); err != nil {
		t.Fatal(err)
	}
	if creds, _ := lr.relay.RepeaterCredentials(); len(creds) != 0 {
		t.Fatalf("left after removal: %d", len(creds))
	}

	// A bad bridge token is refused.
	bad := &admin.Client{APIBase: host.APIBase, Token: "smb_nope"}
	if _, err := bad.MyRepeaters(ctx); err == nil {
		t.Fatal("bad bridge token accepted")
	}
}

func TestShareRepeaterEdgeCases(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Without a secret key the registry is off.
	lr := startRelay(t)
	far := &meshsim.Repeater{Pubkey: hexKey("c"), Name: "Far", GuestPassword: "g", Unreachable: true}
	mesh := meshsim.New(far)
	tok := lr.createGateway("cabin")
	startBridge(t, lr, "cabin", tok, startMeshNode(t, "Cabin", mesh))
	lr.waitGateway("cabin", "online", connected)
	host := &admin.Client{APIBase: config.HTTPBase(lr.wsBase()), Token: tok}
	if _, err := host.ShareRepeater(ctx, far.Pubkey, "", "g"); err == nil || !strings.Contains(err.Error(), "secret key") {
		t.Fatalf("no key: %v", err)
	}

	// A repeater that doesn't answer.
	lr2 := startRelay(t, withSecretKey, func(c *relay.Config) {})
	tok2 := lr2.createGateway("cabin")
	startBridge(t, lr2, "cabin", tok2, startMeshNode(t, "Cabin", mesh))
	lr2.waitGateway("cabin", "online", connected)
	host2 := &admin.Client{APIBase: config.HTTPBase(lr2.wsBase()), Token: tok2}
	if _, err := host2.ShareRepeater(ctx, far.Pubkey, "", "g"); err == nil || !strings.Contains(err.Error(), "didn't answer") {
		t.Fatalf("unreachable: %v", err)
	}

	// A node whose clock went backwards is set before logging in.
	mesh.SetUnreachable(far.Pubkey, false)
	mesh.SetClockOffset(-200 * 24 * time.Hour)
	if _, err := host2.ShareRepeater(ctx, far.Pubkey, "", "g"); err != nil {
		t.Fatalf("after clock reset: %v", err)
	}
	if off := mesh.ClockOffset(); off < -time.Minute {
		t.Fatalf("clock not set: %s", off)
	}
}

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// TestCheckRepeaterOnlyIfKnown: the poller's mode never adds a contact to a
// host's node; a node that hasn't heard the repeater is skipped.
func TestCheckRepeaterOnlyIfKnown(t *testing.T) {
	lr := startRelay(t, withSecretKey)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rpt := &meshsim.Repeater{Pubkey: hexKey("d"), Name: "Peak", GuestPassword: "g", BatteryMV: 3700, TempC: -2.5,
		Advert: []byte{0x11, 0x00, 0xde, 0xad}}
	deaf := meshsim.New() // in range, but hasn't heard its advert
	deaf.AddInRange(rpt)
	heard := meshsim.New(rpt)
	for name, mesh := range map[string]*meshsim.Node{"deaf": deaf, "heard": heard} {
		tok := lr.createGateway(name)
		startBridge(t, lr, name, tok, startMeshNode(t, name, mesh))
		lr.waitGateway(name, name+" online", connected)
	}
	req := relay.CheckRequest{Client: "telemetry", Priority: relay.PriorityBackground, Pubkey: rpt.Pubkey, Password: "g", OnlyIfKnown: true}
	req.Gateway = "deaf"
	if _, err := lr.relay.CheckRepeater(ctx, req); !errors.Is(err, relay.ErrNotKnown) || deaf.HasContact(rpt.Pubkey) {
		t.Fatalf("deaf gateway: %v (contact added: %v)", err, deaf.HasContact(rpt.Pubkey))
	}
	req.Gateway = "heard"
	res, err := lr.relay.CheckRepeater(ctx, req)
	if err != nil || res.Name != "Peak" || *res.Status.BatteryMilliVolts != 3700 || res.Telemetry == nil || *res.Telemetry.TemperatureC != -2.5 ||
		string(res.Advert) != string(rpt.Advert) {
		t.Fatalf("heard gateway: %+v %v", res, err)
	}
}

// TestCheckRepeaterResetsStalePath: a node whose stored direct route to the
// repeater no longer works gets no answer; the check resets the route,
// floods, and the node learns a working one from the reply.
func TestCheckRepeaterResetsStalePath(t *testing.T) {
	lr := startRelay(t, withSecretKey)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rpt := &meshsim.Repeater{Pubkey: hexKey("e"), Name: "Outpost", GuestPassword: "g", BatteryMV: 3900}
	mesh := meshsim.New(rpt)
	mesh.SetStalePath(rpt.Pubkey)
	tok := lr.createGateway("mission")
	startBridge(t, lr, "mission", tok, startMeshNode(t, "mission", mesh))
	lr.waitGateway("mission", "online", connected)

	req := relay.CheckRequest{Gateway: "mission", Client: "telemetry", Priority: relay.PriorityBackground,
		Pubkey: rpt.Pubkey, Password: "g", OnlyIfKnown: true}
	res, err := lr.relay.CheckRepeater(ctx, req)
	if err != nil || !res.PathReset || res.Access != "guest" || *res.Status.BatteryMilliVolts != 3900 {
		t.Fatalf("stale path: %+v %v", res, err)
	}
	if !mesh.PathKnown(rpt.Pubkey) {
		t.Fatal("the node didn't learn a route from the flood reply")
	}
	// The learned route works: no reset next time.
	if res, err = lr.relay.CheckRepeater(ctx, req); err != nil || res.PathReset {
		t.Fatalf("second check: %+v %v", res, err)
	}
	// A repeater that's really gone isn't retried forever: one reset, then the error.
	mesh.SetUnreachable(rpt.Pubkey, true)
	if _, err = lr.relay.CheckRepeater(ctx, req); !errors.Is(err, meshops.ErrNoMeshReply) {
		t.Fatalf("unreachable: %v", err)
	}
}

// TestRepeaterAttemptsAndPollState: every check is recorded as an attempt;
// admins see each gateway's link and the poller's saved schedule, which the
// credentials carry back; FloodFirst floods at once; removing the repeater
// forgets both.
func TestRepeaterAttemptsAndPollState(t *testing.T) {
	lr := startRelay(t, withSecretKey)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rpt := &meshsim.Repeater{Pubkey: hexKey("f"), Name: "Knob", GuestPassword: "g", BatteryMV: 3800}
	mesh := meshsim.New(rpt)
	tok := lr.createGateway("knob-gw")
	startBridge(t, lr, "knob-gw", tok, startMeshNode(t, "knob", mesh))
	lr.waitGateway("knob-gw", "online", connected)
	host := &admin.Client{APIBase: config.HTTPBase(lr.wsBase()), Token: tok}
	ac := adminClient(lr)

	if _, err := host.ShareRepeater(ctx, rpt.Pubkey, "", "g"); err != nil {
		t.Fatal(err)
	}
	req := relay.CheckRequest{Gateway: "knob-gw", Client: "telemetry", Priority: relay.PriorityBackground,
		Pubkey: rpt.Pubkey, Password: "g", OnlyIfKnown: true}
	// The share's flooded login taught the node a route; FloodFirst resets it.
	if !mesh.PathKnown(rpt.Pubkey) {
		t.Fatal("no route learned from the share")
	}
	req.FloodFirst = true
	if res, err := lr.relay.CheckRepeater(ctx, req); err != nil || !res.PathReset {
		t.Fatalf("flood first: %+v %v", res, err)
	}
	req.FloodFirst = false
	mesh.SetUnreachable(rpt.Pubkey, true)
	if _, err := lr.relay.CheckRepeater(ctx, req); !errors.Is(err, meshops.ErrNoMeshReply) {
		t.Fatalf("unreachable: %v", err)
	}
	next := time.Now().Add(30 * time.Minute).UTC().Truncate(time.Second)
	lr.relay.SetRepeaterPollState(rpt.Pubkey, relay.RepeaterPollState{Primary: "knob-gw", NextCycle: next, FailedCycles: 1,
		Reading: json.RawMessage(`{"uptime_s":7}`)})

	// Writes are queued: wait for them.
	var rep admin.Repeater
	deadline := time.Now().Add(10 * time.Second)
	for {
		rs, err := ac.Repeaters(ctx)
		if err == nil && len(rs) == 1 && rs[0].Poll != nil && len(rs[0].Links) == 1 && rs[0].Links[0].Tries == 3 {
			rep = rs[0]
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("attempts and state never showed: %+v %v", rs, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	l := rep.Links[0]
	// The share (flooded: no route yet), the flood-first check, and the
	// failure (its direct try went unanswered, so it reset and flooded too).
	if l.Gateway != "knob-gw" || l.OK != 2 || l.Flooded != 3 || l.LastOutcome != "no_reply" || l.MedianMS <= 0 {
		t.Fatalf("link: %+v", l)
	}
	if rep.Poll.Primary != "knob-gw" || rep.Poll.FailedCycles != 1 || !rep.Poll.NextCycle.Equal(next) {
		t.Fatalf("poll state: %+v", rep.Poll)
	}
	creds, err := lr.relay.RepeaterCredentials()
	if err != nil || len(creds) != 1 || creds[0].Poll.Primary != "knob-gw" || string(creds[0].Poll.Reading) != `{"uptime_s":7}` {
		t.Fatalf("credentials: %+v %v", creds, err)
	}

	// The host sees the same in their own list.
	mine, err := host.MyRepeaters(ctx)
	if err != nil || len(mine) != 1 || len(mine[0].Links) != 1 || mine[0].Poll == nil {
		t.Fatalf("host list: %+v %v", mine, err)
	}

	if err := ac.RemoveRepeater(ctx, rpt.Pubkey); err != nil {
		t.Fatal(err)
	}
	if _, err := host.ShareRepeater(ctx, rpt.Pubkey, "", "g"); err == nil {
		t.Fatal("unreachable repeater shared")
	}
	mesh.SetUnreachable(rpt.Pubkey, false)
	if _, err := host.ShareRepeater(ctx, rpt.Pubkey, "", "g"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // let queued writes land
	rs, _ := ac.Repeaters(ctx)
	// Only the two shares since removal: the failed one and this one.
	if len(rs) != 1 || rs[0].Poll != nil || len(rs[0].Links) != 1 || rs[0].Links[0].Tries != 2 || rs[0].Links[0].OK != 1 {
		t.Fatalf("after re-sharing, the old attempts and state should be gone: %+v", rs)
	}
}
