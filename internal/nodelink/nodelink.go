// Package nodelink connects to a MeshCore companion node. Every link speaks the
// companion serial/TCP framing and exposes bare payloads.
package nodelink

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/sierragridteam/sierra-meshrelay/internal/companion"
	"github.com/sierragridteam/sierra-meshrelay/meshproto"
)

// Link is an open connection to a node. ReadFrame and WriteFrame may be called
// concurrently with each other, but each from one goroutine at a time.
type Link interface {
	// ReadFrame returns the next payload from the node.
	ReadFrame() ([]byte, error)
	// WriteFrame sends one payload to the node.
	WriteFrame(payload []byte) error
	// Close closes the link and unblocks any pending ReadFrame.
	Close() error
	// Kind is meshproto.LinkTCP or meshproto.LinkSerial.
	Kind() string
	// Detail is informational: a port name or host:port.
	Detail() string
}

// Opener opens a link to the node.
type Opener interface {
	Open(ctx context.Context) (Link, error)
}

// ErrNotConfigured means the opener has nothing to try (e.g. no TCP host).
var ErrNotConfigured = errors.New("nodelink: no node address configured")

// TCP opens companion links over TCP (WiFi/Ethernet nodes, port 5000).
type TCP struct {
	Host        string
	Port        int
	DialTimeout time.Duration
}

func (t TCP) addr() string { return net.JoinHostPort(t.Host, strconv.Itoa(t.Port)) }

// Open dials the node.
func (t TCP) Open(ctx context.Context) (Link, error) {
	if t.Host == "" {
		return nil, ErrNotConfigured
	}
	d := net.Dialer{Timeout: t.DialTimeout, KeepAlive: 15 * time.Second}
	if d.Timeout == 0 {
		d.Timeout = 3 * time.Second
	}
	c, err := d.DialContext(ctx, "tcp", t.addr())
	if err != nil {
		return nil, err
	}
	return NewStream(c, meshproto.LinkTCP, t.addr()), nil
}

// Stream adapts any byte stream (TCP socket, serial port, pipe) to a Link.
type Stream struct {
	rwc    io.ReadWriteCloser
	r      *companion.Reader
	kind   string
	detail string
	wmu    sync.Mutex
}

// NewStream wraps rwc.
func NewStream(rwc io.ReadWriteCloser, kind, detail string) *Stream {
	return &Stream{
		rwc:    rwc,
		r:      companion.NewReader(rwc, companion.HeaderFromNode, meshproto.MaxFrameBytes),
		kind:   kind,
		detail: detail,
	}
}

func (s *Stream) ReadFrame() ([]byte, error) { return s.r.ReadFrame() }

func (s *Stream) WriteFrame(p []byte) error {
	if len(p) > meshproto.MaxFrameBytes {
		return fmt.Errorf("nodelink: frame of %d bytes exceeds %d", len(p), meshproto.MaxFrameBytes)
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.rwc.Write(companion.Encode(companion.HeaderToNode, p))
	return err
}

func (s *Stream) Close() error   { return s.rwc.Close() }
func (s *Stream) Kind() string   { return s.kind }
func (s *Stream) Detail() string { return s.detail }

// SkippedBytes reports bytes discarded while resyncing on a frame header.
func (s *Stream) SkippedBytes() int64 { return s.r.Skipped() }
