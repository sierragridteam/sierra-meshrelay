package nodelink_test

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sierragridteam/sierra-meshrelay/internal/fakenode"
	"github.com/sierragridteam/sierra-meshrelay/internal/nodelink"
	"github.com/sierragridteam/sierra-meshrelay/internal/state"
)

func TestSubnetHosts(t *testing.T) {
	h := nodelink.SubnetHosts(netip.MustParsePrefix("192.168.4.77/24"))
	if len(h) != 254 || h[0].String() != "192.168.4.1" || h[253].String() != "192.168.4.254" {
		t.Fatalf("/24: %d hosts, %v..%v", len(h), h[0], h[len(h)-1])
	}
	if h := nodelink.SubnetHosts(netip.MustParsePrefix("10.0.0.0/30")); len(h) != 2 {
		t.Fatalf("/30: %v", h)
	}
	if h := nodelink.SubnetHosts(netip.MustParsePrefix("10.0.0.0/8")); h != nil {
		t.Fatal("refused to enumerate a /8")
	}
}

// node starts a fake node at host on port (0: any).
func node(t *testing.T, host string, port int, name string) *fakenode.Node {
	t.Helper()
	n, err := fakenode.Listen(net.JoinHostPort(host, strconv.Itoa(port)), fakenode.Options{Name: name})
	if err != nil {
		t.Skipf("can't listen on %s: %v", host, err)
	}
	t.Cleanup(func() { n.Close() })
	return n
}

func hosts(addrs ...string) (func() []netip.Addr, *atomic.Int32) {
	var calls atomic.Int32
	return func() []netip.Addr {
		calls.Add(1)
		var out []netip.Addr
		for _, a := range addrs {
			out = append(out, netip.MustParseAddr(a))
		}
		return out
	}, &calls
}

func TestDiscoverConfiguredHost(t *testing.T) {
	n := node(t, "127.0.0.1", 0, "A")
	st := state.New(filepath.Join(t.TempDir(), "state.json"))
	d := &nodelink.Discover{Host: "127.0.0.1", Port: n.Addr().Port, State: st}
	link, err := d.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	link.Close()
	if got := st.Get().NodeAddr; got != n.Addr().String() {
		t.Fatalf("cached %q", got)
	}
}

func TestDiscoverScansWhenAddressChanges(t *testing.T) {
	n := node(t, "127.0.0.1", 0, "Moved")
	port := n.Addr().Port
	scan, calls := hosts("127.0.0.2", "127.0.0.1", "127.0.0.4")
	st := state.New(filepath.Join(t.TempDir(), "state.json"))
	d := &nodelink.Discover{Host: "127.0.0.2", Port: port, ExpectedPubkey: n.PubkeyHex(), State: st, Hosts: scan}
	link, err := d.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	link.Close()
	if link.Detail() != n.Addr().String() || calls.Load() != 1 {
		t.Fatalf("opened %s after %d scans", link.Detail(), calls.Load())
	}
	// Next time the cached address is used, no scan.
	d2 := &nodelink.Discover{Host: "127.0.0.2", Port: port, ExpectedPubkey: n.PubkeyHex(), State: st, Hosts: scan}
	link, err = d2.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	link.Close()
	if calls.Load() != 1 {
		t.Fatalf("scanned again despite the cache (%d scans)", calls.Load())
	}
}

func TestDiscoverPicksExpectedNode(t *testing.T) {
	a := node(t, "127.0.0.1", 0, "Neighbour")
	port := a.Addr().Port
	b := node(t, "127.0.0.3", port, "Ours")
	scan, _ := hosts("127.0.0.1", "127.0.0.3")

	d := &nodelink.Discover{Port: port, ExpectedPubkey: b.PubkeyHex(), Hosts: scan}
	link, err := d.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	link.Close()
	if link.Detail() != b.Addr().String() {
		t.Fatalf("picked %s, want %s", link.Detail(), b.Addr())
	}

	// With no expected pubkey, two nodes is ambiguous.
	d = &nodelink.Discover{Port: port, Hosts: scan}
	if _, err := d.Open(context.Background()); err == nil || !strings.Contains(err.Error(), "found 2 nodes") {
		t.Fatalf("ambiguous: %v", err)
	}
	found := (&nodelink.Discover{Port: port, Hosts: scan}).Scan(context.Background())
	if len(found) != 2 || found[0].Name != "Neighbour" || found[1].Name != "Ours" {
		t.Fatalf("scan found %+v", found)
	}
}

func TestDiscoverRateLimitsScans(t *testing.T) {
	scan, calls := hosts("127.0.0.2")
	d := &nodelink.Discover{Port: 1, Hosts: scan, ScanEvery: time.Hour}
	for range 3 {
		if _, err := d.Open(context.Background()); !errors.Is(err, nodelink.ErrNodeNotFound) {
			t.Fatalf("err = %v", err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("%d scans, want 1", calls.Load())
	}
}

func TestDiscoverRejectsWrongNode(t *testing.T) {
	n := node(t, "127.0.0.1", 0, "Someone else's")
	scan, _ := hosts("127.0.0.1")
	d := &nodelink.Discover{Port: n.Addr().Port, ExpectedPubkey: strings.Repeat("ab", 32), Hosts: scan}
	if _, err := d.Open(context.Background()); !errors.Is(err, nodelink.ErrNodeNotFound) {
		t.Fatalf("err = %v", err)
	}
}
