package bridge

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/sierragridteam/sierra-meshrelay/nodepolicy"
)

// A refusal on a session that is being torn down (queue full, writer gone,
// I/O cancelled) must not block the read loop: teardown waits for it.
func TestRefusalDoesNotBlockTeardown(t *testing.T) {
	b := &Bridge{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ioCtx, cancel := context.WithCancel(context.Background())
	s := &session{ioCtx: ioCtx, out: make(chan []byte, 1), done: make(chan struct{})}
	s.out <- []byte{0x80} // full: nothing drains it

	returned := make(chan struct{})
	go func() {
		b.toNode(s, []byte{nodepolicy.CmdExportPrivateKey})
		close(returned)
	}()
	select {
	case <-returned:
		t.Fatal("returned before the queue had room or I/O was cancelled")
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("toNode still blocked after the session's I/O was cancelled")
	}
	if b.refusedFrames.Load() != 1 {
		t.Fatalf("refused = %d", b.refusedFrames.Load())
	}
}
