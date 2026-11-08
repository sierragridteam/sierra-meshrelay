// Package fakenode is a fake MeshCore companion node for tests. It speaks the
// companion TCP framing, answers APP_START with SELF_INFO and DEVICE_QUERY with
// DEVICE_INFO, and echoes every other frame back with a 0x80 prefix (the same
// convention as sierra-data's test-mesh-relay fake node).
//
// Like real firmware, it serves one connection at a time.
package fakenode

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net"
	"sync"
	"sync/atomic"

	"github.com/sierragridteam/sierra-meshrelay/internal/companion"
	"github.com/sierragridteam/sierra-meshrelay/meshops/meshsim"
)

// EchoPrefix is prepended to echoed frames.
const EchoPrefix byte = 0x80

// Options configure a Node.
type Options struct {
	Pubkey []byte // 32 bytes; default derived from Name
	Name   string // default "FakeNode"
	Model  string // default "Fake Board"
	FW     string // default "v1.18.0"
	// Silent nodes never answer APP_START (to test identity timeouts).
	Silent bool
	// Mesh, when set, answers the contact, clock and repeater commands it
	// simulates (meshsim) instead of echoing them.
	Mesh *meshsim.Node
}

// Node is a running fake node.
type Node struct {
	opts Options
	ln   net.Listener

	mu       sync.Mutex
	conn     net.Conn
	accepted atomic.Int64
	received atomic.Int64
	closed   chan struct{}
}

// KeyFor derives a stable 32-byte pubkey from a seed string.
func KeyFor(seed string) []byte {
	h := sha256.Sum256([]byte("fakenode:" + seed))
	return h[:]
}

// Listen starts a fake node on addr (e.g. "127.0.0.1:0").
func Listen(addr string, opts Options) (*Node, error) {
	if opts.Name == "" {
		opts.Name = "FakeNode"
	}
	if len(opts.Pubkey) == 0 {
		opts.Pubkey = KeyFor(opts.Name)
	}
	if len(opts.Pubkey) != companion.PubkeySize {
		return nil, errors.New("fakenode: pubkey must be 32 bytes")
	}
	if opts.Model == "" {
		opts.Model = "Fake Board"
	}
	if opts.FW == "" {
		opts.FW = "v1.18.0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	n := &Node{opts: opts, ln: ln, closed: make(chan struct{})}
	go n.acceptLoop()
	return n, nil
}

// Addr is the listening address.
func (n *Node) Addr() *net.TCPAddr { return n.ln.Addr().(*net.TCPAddr) }

// PubkeyHex is the node's pubkey in lowercase hex.
func (n *Node) PubkeyHex() string { return hex.EncodeToString(n.opts.Pubkey) }

// Accepted counts connections accepted so far.
func (n *Node) Accepted() int64 { return n.accepted.Load() }

// Received counts frames received (all kinds).
func (n *Node) Received() int64 { return n.received.Load() }

// Close stops the node and drops its connection.
func (n *Node) Close() error {
	select {
	case <-n.closed:
		return nil
	default:
	}
	close(n.closed)
	err := n.ln.Close()
	n.DropConn()
	return err
}

// DropConn closes the current connection, as if the node rebooted.
func (n *Node) DropConn() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.conn != nil {
		n.conn.Close()
		n.conn = nil
	}
}

// Push sends an unsolicited frame (e.g. a push code) to the connected client.
func (n *Node) Push(payload []byte) error {
	n.mu.Lock()
	c := n.conn
	n.mu.Unlock()
	if c == nil {
		return errors.New("fakenode: no connection")
	}
	_, err := c.Write(companion.Encode(companion.HeaderFromNode, payload))
	return err
}

func (n *Node) acceptLoop() {
	for {
		c, err := n.ln.Accept()
		if err != nil {
			return
		}
		// One connection at a time: a new one replaces the old.
		n.mu.Lock()
		if n.conn != nil {
			n.conn.Close()
		}
		n.conn = c
		n.mu.Unlock()
		n.accepted.Add(1)
		go n.serve(c)
	}
}

func (n *Node) serve(c net.Conn) {
	defer c.Close()
	r := companion.NewReader(c, companion.HeaderToNode, companion.FirmwareMaxFrame)
	var wmu sync.Mutex
	write := func(p []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		_, err := c.Write(companion.Encode(companion.HeaderFromNode, p))
		return err
	}
	for {
		f, err := r.ReadFrame()
		if err != nil {
			return
		}
		n.received.Add(1)
		var reply []byte
		switch {
		case len(f) >= 8 && f[0] == companion.CmdAppStart:
			if n.opts.Silent {
				continue
			}
			reply = n.SelfInfo()
		case len(f) >= 2 && f[0] == companion.CmdDeviceQuery:
			if n.opts.Silent {
				continue
			}
			reply = n.DeviceInfo()
		default:
			if n.opts.Mesh != nil && n.opts.Mesh.Handle(f, func(p []byte) { _ = write(p) }) {
				continue
			}
			reply = append([]byte{EchoPrefix}, f...)
		}
		if err := write(reply); err != nil {
			return
		}
	}
}

// SelfInfo builds a SELF_INFO frame with the firmware's v1.18 layout.
func (n *Node) SelfInfo() []byte {
	f := []byte{companion.RespSelfInfo, 1 /* ADV_TYPE_CHAT */, 22, 22}
	f = append(f, n.opts.Pubkey...)
	lat, lon := int32(38_500_000), int32(-120_500_000)
	f = binary.LittleEndian.AppendUint32(f, uint32(lat))
	f = binary.LittleEndian.AppendUint32(f, uint32(lon))
	f = append(f, 0, 0, 0, 0)                        // multi_acks, loc policy, telemetry, manual add
	f = binary.LittleEndian.AppendUint32(f, 910_525) // freq kHz
	f = binary.LittleEndian.AppendUint32(f, 62_500)  // bw Hz
	f = append(f, 7, 5)                              // sf, cr
	return append(f, n.opts.Name...)
}

// DeviceInfo builds a DEVICE_INFO frame with the firmware's v1.18 layout.
func (n *Node) DeviceInfo() []byte {
	f := []byte{companion.RespDeviceInfo, 14, 175, 40}
	f = binary.LittleEndian.AppendUint32(f, 123456) // BLE PIN: must never be logged
	f = append(f, fixed("15 Oct 2026", 12)...)
	f = append(f, fixed(n.opts.Model, 40)...)
	f = append(f, fixed(n.opts.FW, 20)...)
	return append(f, 0, 0)
}

func fixed(s string, size int) []byte {
	b := make([]byte, size)
	copy(b, s)
	return b
}
