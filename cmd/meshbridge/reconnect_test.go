package main

import (
	"errors"
	"testing"
	"time"

	"github.com/sierragridteam/sierra-meshrelay/meshproto"
)

func states(ss ...string) func() (string, error) {
	i := 0
	return func() (string, error) {
		s := ss[min(i, len(ss)-1)]
		i++
		return s, nil
	}
}

// A node that was connected must be seen to drop before "connected" means
// the new link, so reconnect doesn't report the old one.
func TestWaitForNodeWaitsForTheOldLinkToDrop(t *testing.T) {
	calls := 0
	st := states(meshproto.NodeConnected, meshproto.NodeDisconnected, meshproto.NodeConnected)
	err := waitForNode(true, time.Millisecond, time.Second, func() (string, error) { calls++; return st() })
	if err != nil || calls != 3 {
		t.Fatalf("err %v after %d polls, want 3", err, calls)
	}
}

func TestWaitForNodeReturnsAtConnected(t *testing.T) {
	calls := 0
	st := states(meshproto.NodeDisconnected, meshproto.NodeConnected)
	if err := waitForNode(false, time.Millisecond, time.Second, func() (string, error) { calls++; return st() }); err != nil || calls != 2 {
		t.Fatalf("err %v after %d polls, want 2", err, calls)
	}
}

func TestWaitForNodeGivesUp(t *testing.T) {
	start := time.Now()
	if err := waitForNode(false, time.Millisecond, 20*time.Millisecond, states(meshproto.NodeDisconnected)); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("didn't give up at the timeout")
	}
}

func TestWaitForNodeReturnsErrors(t *testing.T) {
	boom := errors.New("boom")
	if err := waitForNode(false, time.Millisecond, time.Second, func() (string, error) { return "", boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}
