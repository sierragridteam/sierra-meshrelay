package nodelink

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/sierragridteam/sierra-meshrelay/internal/companion"
	"github.com/sierragridteam/sierra-meshrelay/internal/state"
	"github.com/sierragridteam/sierra-meshrelay/meshproto"
)

// A WiFi companion node gets its address by DHCP, so the address in the
// config can go stale. Discover finds the node again: the last address it
// answered on, then the configured address, then a scan of the local
// subnets for the companion port, keeping only the node whose pubkey matches.
// A known address can come to belong to another node (two nodes on one
// network swap addresses): the bridge then calls WrongNode, and the next
// Open scans.

// ErrNodeNotFound means no reachable node matched.
var ErrNodeNotFound = errors.New("nodelink: no node found on this network")

// Found is a node that answered the identity check during a scan.
type Found struct {
	Addr string // host:port
	companion.Identity
}

// Discover opens TCP links to a WiFi/Ethernet node, rediscovering it when
// its address changes.
type Discover struct {
	Host           string // configured host (may be empty)
	Port           int    // companion port (5000)
	ExpectedPubkey string // only a node with this pubkey is accepted from a scan
	State          *state.File

	// Hosts lists the addresses to scan. Default: LocalSubnetHosts.
	Hosts func() []netip.Addr
	// DialTimeout bounds connecting to the configured or cached address [3s].
	DialTimeout time.Duration
	// ProbeTimeout bounds each scan connect [500ms].
	ProbeTimeout time.Duration
	// Concurrency is the number of scan connects in flight [64].
	Concurrency int
	// ScanEvery is the minimum time between scans [1m].
	ScanEvery time.Duration
	Log       *slog.Logger

	mu       sync.Mutex
	lastScan time.Time
	misses   int  // scans in a row that didn't find the node
	rescan   bool // WrongNode: scan next, skipping the known addresses
}

// WrongNode says the link Open returned reached a node with the wrong
// pubkey. With two nodes on a network, DHCP can hand one the other's old
// address, and dialing that address would keep reaching the wrong node.
// The address is forgotten and the next Open scans for the expected node,
// whenever the last scan was.
func (d *Discover) WrongNode() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.defaults()
	d.rescan = true
	if d.State != nil && d.State.Get().NodeAddr != "" {
		d.remember("")
	}
}

func (d *Discover) defaults() {
	if d.Port == 0 {
		d.Port = 5000
	}
	if d.DialTimeout == 0 {
		d.DialTimeout = 3 * time.Second
	}
	if d.ProbeTimeout == 0 {
		d.ProbeTimeout = 500 * time.Millisecond
	}
	if d.Concurrency == 0 {
		d.Concurrency = 64
	}
	if d.ScanEvery == 0 {
		d.ScanEvery = time.Minute
	}
	if d.Hosts == nil {
		d.Hosts = LocalSubnetHosts
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
}

func (d *Discover) configured() string {
	if d.Host == "" {
		return ""
	}
	return net.JoinHostPort(d.Host, strconv.Itoa(d.Port))
}

// Open connects to the node.
func (d *Discover) Open(ctx context.Context) (Link, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.defaults()

	var lastErr error
	tried := map[string]bool{}
	// The address the node last answered on first: after a rescan found it
	// somewhere new, the configured one may now be another node's.
	known := []string{d.State.Get().NodeAddr, d.configured()}
	rescan := d.rescan
	if rescan {
		// The known addresses just reached the wrong node: scan instead.
		known, d.rescan = nil, false
	}
	for _, addr := range known {
		if addr == "" || tried[addr] {
			continue
		}
		tried[addr] = true
		link, err := dialStream(ctx, addr, d.DialTimeout)
		if err == nil {
			d.remember(addr)
			d.misses = 0
			return link, nil
		}
		lastErr = err
	}

	if since := time.Since(d.lastScan); !rescan && !d.lastScan.IsZero() && since < d.ScanEvery {
		if lastErr == nil {
			lastErr = ErrNodeNotFound
		}
		return nil, lastErr
	}
	d.lastScan = time.Now()
	// The first scan of an outage is logged; the rest (one a minute while
	// the node is off) are debug, and the bridge summarizes the outage.
	lvl, warn := slog.LevelDebug, slog.LevelDebug
	if d.misses == 0 {
		lvl, warn = slog.LevelInfo, slog.LevelWarn
	}
	d.Log.Log(ctx, lvl, "looking for the node on the local network", "port", d.Port)
	found := d.scan(ctx)
	pick, err := choose(found, d.ExpectedPubkey)
	if err != nil {
		d.misses++
		d.Log.Log(ctx, warn, "node not found on the local network", "candidates", len(found), "error", err)
		return nil, err
	}
	d.Log.Info("found the node", "addr", pick.Addr, "name", pick.Name, "pubkey", pick.Pubkey, "scans", d.misses+1)
	d.misses = 0
	link, err := dialStream(ctx, pick.Addr, d.DialTimeout)
	if err != nil {
		return nil, err
	}
	d.remember(pick.Addr)
	return link, nil
}

func (d *Discover) remember(addr string) {
	if err := d.State.Update(func(s *state.State) { s.NodeAddr = addr }); err != nil {
		d.Log.Warn("couldn't save the node's address", "error", err)
	}
}

// choose picks the node to use: the one with the expected pubkey, or the
// only one found when none is expected.
func choose(found []Found, expected string) (Found, error) {
	if expected != "" {
		for _, f := range found {
			if f.Pubkey == expected {
				return f, nil
			}
		}
		return Found{}, fmt.Errorf("%w (none of %d node(s) found has the expected pubkey)", ErrNodeNotFound, len(found))
	}
	switch len(found) {
	case 0:
		return Found{}, ErrNodeNotFound
	case 1:
		return found[0], nil
	}
	return Found{}, fmt.Errorf("found %d nodes on this network; set node.expected_pubkey (or tcp_host) to pick one", len(found))
}

// Scan probes every candidate host for the companion port and runs the
// identity check on each that answers. Results are sorted by address.
func (d *Discover) Scan(ctx context.Context) []Found {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.defaults()
	return d.scan(ctx)
}

func (d *Discover) scan(ctx context.Context) []Found {
	hosts := d.Hosts()
	jobs := make(chan netip.Addr)
	var mu sync.Mutex
	var found []Found
	var wg sync.WaitGroup
	for range min(d.Concurrency, max(len(hosts), 1)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for h := range jobs {
				addr := net.JoinHostPort(h.String(), strconv.Itoa(d.Port))
				if id, err := Probe(ctx, addr, d.ProbeTimeout); err == nil {
					mu.Lock()
					found = append(found, Found{Addr: addr, Identity: id})
					mu.Unlock()
				}
			}
		}()
	}
	for _, h := range hosts {
		select {
		case jobs <- h:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(jobs)
	wg.Wait()
	sort.Slice(found, func(i, j int) bool { return found[i].Addr < found[j].Addr })
	return found
}

// Probe connects to addr and runs the identity check, then disconnects.
func Probe(ctx context.Context, addr string, connectTimeout time.Duration) (companion.Identity, error) {
	link, err := dialStream(ctx, addr, connectTimeout)
	if err != nil {
		return companion.Identity{}, err
	}
	defer link.Close()
	ictx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	frames := make(chan []byte, 16)
	go func() {
		defer close(frames)
		for {
			f, err := link.ReadFrame()
			if err != nil {
				return
			}
			select {
			case frames <- f:
			case <-ictx.Done():
				return
			}
		}
	}()
	return companion.Identify(ictx, frames, link.WriteFrame, 2*time.Second)
}

func dialStream(ctx context.Context, addr string, timeout time.Duration) (*Stream, error) {
	dl := net.Dialer{Timeout: timeout, KeepAlive: 15 * time.Second}
	c, err := dl.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	return NewStream(c, meshproto.LinkTCP, addr), nil
}

// LocalSubnetHosts lists the hosts on this machine's private IPv4 subnets
// (RFC 1918), excluding its own addresses. A subnet wider than /22 is
// narrowed to the /24 around this machine's address, which keeps a scan to a
// few seconds.
func LocalSubnetHosts() []netip.Addr {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	seen := map[netip.Addr]bool{}
	self := map[netip.Addr]bool{}
	var out []netip.Addr
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(ipn.IP.To4())
			if !ok || !ip.IsPrivate() {
				continue
			}
			self[ip] = true
			bits, _ := ipn.Mask.Size()
			if bits < 22 {
				bits = 24
			}
			for _, h := range SubnetHosts(netip.PrefixFrom(ip, bits).Masked()) {
				if !seen[h] {
					seen[h] = true
					out = append(out, h)
				}
			}
		}
	}
	filtered := out[:0]
	for _, h := range out {
		if !self[h] {
			filtered = append(filtered, h)
		}
	}
	return filtered
}

// SubnetHosts lists the usable host addresses in an IPv4 prefix (no network
// or broadcast address for prefixes up to /30).
func SubnetHosts(p netip.Prefix) []netip.Addr {
	p = p.Masked()
	if !p.Addr().Is4() || p.Bits() < 16 {
		return nil
	}
	var out []netip.Addr
	for a := p.Addr(); p.Contains(a); a = a.Next() {
		out = append(out, a)
	}
	if p.Bits() <= 30 && len(out) >= 2 {
		out = out[1 : len(out)-1]
	}
	return out
}
