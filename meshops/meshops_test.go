package meshops_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sierragridteam/sierra-meshrelay/meshops"
	"github.com/sierragridteam/sierra-meshrelay/meshops/meshsim"
)

func pk(c byte) string { return strings.Repeat(string("0123456789abcdef"[c%16]), 64) }

func session(n *meshsim.Node) *meshops.Session {
	var s *meshops.Session
	s = meshops.NewSession(func(f []byte) error {
		if !n.Handle(f, s.Deliver) {
			s.Deliver([]byte{1, 1}) // unsupported
		}
		return nil
	})
	s.MinMeshTimeout, s.MaxMeshTimeout = 200*time.Millisecond, 300*time.Millisecond
	return s
}

func TestRepeaterLoginStatusTelemetry(t *testing.T) {
	ctx := context.Background()
	r := &meshsim.Repeater{Pubkey: pk(1), Name: "Ridge RPT", AdminPassword: "adm", GuestPassword: "gst", BatteryMV: 4012, UptimeS: 3600, TempC: 21.5}
	n := meshsim.New(r)
	s := session(n)

	cs, err := s.Contacts(ctx)
	if err != nil || len(cs) != 1 || cs[0].Name != "Ridge RPT" || cs[0].Type != meshops.TypeRepeater || cs[0].Pubkey != r.Pubkey {
		t.Fatalf("contacts: %+v %v", cs, err)
	}
	if _, err := s.Login(ctx, r.Pubkey, "nope"); !errors.Is(err, meshops.ErrLoginFailed) {
		t.Fatalf("bad password: %v", err)
	}
	l, err := s.Login(ctx, r.Pubkey, "gst")
	if err != nil || l.Access() != "guest" {
		t.Fatalf("guest login: %+v %v", l, err)
	}
	if l, err = s.Login(ctx, r.Pubkey, "adm"); err != nil || l.Access() != "admin" {
		t.Fatalf("admin login: %+v %v", l, err)
	}
	st, err := s.Status(ctx, r.Pubkey)
	if err != nil || *st.BatteryMilliVolts != 4012 || *st.UptimeS != 3600 || *st.NoiseFloorDBm != -110 || *st.LastSNRdB != 5.5 || *st.RecvErrors != 0 {
		t.Fatalf("status: %+v %v", st, err)
	}
	tm, err := s.Telemetry(ctx, r.Pubkey)
	if err != nil || *tm.Voltage != 4.01 || *tm.TemperatureC != 21.5 {
		t.Fatalf("telemetry: %+v %v", tm, err)
	}
}

func TestContactsAndErrors(t *testing.T) {
	ctx := context.Background()
	far := &meshsim.Repeater{Pubkey: pk(2), Name: "Far", GuestPassword: ""}
	gone := &meshsim.Repeater{Pubkey: pk(3), Name: "Gone", Unreachable: true}
	n := meshsim.New()
	n.AddInRange(far)
	n.AddInRange(gone)
	s := session(n)

	if _, err := s.Contact(ctx, far.Pubkey); !errors.Is(err, meshops.ErrNotFound) {
		t.Fatalf("unknown contact: %v", err)
	}
	if _, err := s.Login(ctx, far.Pubkey, ""); !errors.Is(err, meshops.ErrNotFound) {
		t.Fatalf("login to a non-contact: %v", err)
	}
	for _, r := range []*meshsim.Repeater{far, gone} {
		if err := s.AddContact(ctx, meshops.Contact{Pubkey: r.Pubkey, Type: meshops.TypeRepeater, Name: r.Name}); err != nil {
			t.Fatal(err)
		}
	}
	c, err := s.Contact(ctx, far.Pubkey)
	if err != nil || c.Name != "Far" || c.PathKnown {
		t.Fatalf("added contact: %+v %v", c, err)
	}
	// A blank guest password logs in.
	if l, err := s.Login(ctx, far.Pubkey, ""); err != nil || l.Access() != "guest" {
		t.Fatalf("blank guest password: %+v %v", l, err)
	}
	if _, err := s.Login(ctx, gone.Pubkey, "x"); !errors.Is(err, meshops.ErrNoMeshReply) {
		t.Fatalf("unreachable: %v", err)
	}
	if err := s.RemoveContact(ctx, far.Pubkey); err != nil || n.HasContact(far.Pubkey) {
		t.Fatalf("remove: %v", err)
	}
	if err := s.RemoveContact(ctx, far.Pubkey); !errors.Is(err, meshops.ErrNotFound) {
		t.Fatalf("remove twice: %v", err)
	}

	n.MaxContacts = 1
	err = s.AddContact(ctx, meshops.Contact{Pubkey: pk(4), Type: meshops.TypeRepeater})
	var re *meshops.RefusedError
	if !errors.As(err, &re) || re.Code != meshops.ErrCodeTableFull {
		t.Fatalf("full table: %v", err)
	}
}

func TestSyncClock(t *testing.T) {
	ctx := context.Background()
	n := meshsim.New()
	s := session(n)
	if set, err := s.SyncClock(ctx, 5*time.Minute); err != nil || set {
		t.Fatalf("in sync: %v %v", set, err)
	}
	n.SetClockOffset(-400 * 24 * time.Hour)
	if set, err := s.SyncClock(ctx, 5*time.Minute); err != nil || !set {
		t.Fatalf("behind: %v %v", set, err)
	}
	if off := n.ClockOffset(); off > 2*time.Second || off < -2*time.Second {
		t.Fatalf("clock still off by %s", off)
	}
}

func TestSessionEndAndUnsolicitedPushes(t *testing.T) {
	s := meshops.NewSession(func([]byte) error { return nil })
	s.Timeout = time.Second
	// Pushes the session didn't ask for are skipped.
	go func() {
		time.Sleep(20 * time.Millisecond)
		s.Deliver([]byte{0x83})       // MSG_WAITING
		s.Deliver([]byte{0x80, 1, 2}) // ADVERT
		s.End()
	}()
	if _, err := s.DeviceTime(context.Background()); !errors.Is(err, meshops.ErrSessionEnded) {
		t.Fatalf("after End: %v", err)
	}
}

func TestParseShortStatus(t *testing.T) {
	// Older firmware sends a shorter struct: missing fields are nil.
	st := meshops.ParseStatus([]byte{0xA0, 0x0F, 1, 0})
	if *st.BatteryMilliVolts != 4000 || *st.TxQueueLen != 1 || st.NoiseFloorDBm != nil || st.RecvErrors != nil {
		t.Fatalf("%+v", st)
	}
}

func TestResetPath(t *testing.T) {
	ctx := context.Background()
	r := &meshsim.Repeater{Pubkey: pk(5), Name: "Outpost", GuestPassword: "g"}
	n := meshsim.New(r)
	s := session(n)
	n.SetStalePath(r.Pubkey)
	if c, _ := s.Contact(ctx, r.Pubkey); !c.PathKnown {
		t.Fatal("stale path not reported as known")
	}
	if _, err := s.Login(ctx, r.Pubkey, "g"); !errors.Is(err, meshops.ErrNoMeshReply) {
		t.Fatalf("over the stale path: %v", err)
	}
	if err := s.ResetPath(ctx, r.Pubkey); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.Contact(ctx, r.Pubkey); c.PathKnown {
		t.Fatal("path still known after reset")
	}
	if _, err := s.Login(ctx, r.Pubkey, "g"); err != nil || !n.PathKnown(r.Pubkey) {
		t.Fatalf("flooded login: %v (learned route: %v)", err, n.PathKnown(r.Pubkey))
	}
	if err := s.ResetPath(ctx, pk(6)); !errors.Is(err, meshops.ErrNotFound) {
		t.Fatalf("unknown contact: %v", err)
	}
}
