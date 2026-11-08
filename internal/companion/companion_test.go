package companion_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/sierragridteam/sierra-meshrelay/internal/companion"
	"github.com/sierragridteam/sierra-meshrelay/internal/fakenode"
)

func TestEncode(t *testing.T) {
	got := companion.Encode(companion.HeaderToNode, []byte{1, 2, 3})
	want := []byte{0x3c, 3, 0, 1, 2, 3}
	if !bytes.Equal(got, want) {
		t.Fatalf("Encode = % x, want % x", got, want)
	}
	big := companion.Encode(companion.HeaderFromNode, make([]byte, 300))
	if big[0] != 0x3e || big[1] != 0x2c || big[2] != 0x01 {
		t.Fatalf("length header = % x, want 3e 2c 01", big[:3])
	}
}

func TestReaderRoundTripAndResync(t *testing.T) {
	var stream bytes.Buffer
	stream.WriteString("debug: booting\r\n") // serial noise before the first frame
	stream.Write(companion.Encode(companion.HeaderFromNode, []byte("hello")))
	stream.Write(companion.Encode(companion.HeaderFromNode, nil))
	stream.Write(companion.Encode(companion.HeaderFromNode, bytes.Repeat([]byte{0xaa}, 176)))

	r := companion.NewReader(&stream, companion.HeaderFromNode, 512)
	for i, want := range [][]byte{[]byte("hello"), {}, bytes.Repeat([]byte{0xaa}, 176)} {
		got, err := r.ReadFrame()
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("frame %d: got %d bytes, want %d", i, len(got), len(want))
		}
	}
	if r.Skipped() != int64(len("debug: booting\r\n")) {
		t.Errorf("Skipped = %d", r.Skipped())
	}
	if _, err := r.ReadFrame(); err != io.EOF {
		t.Errorf("at end: %v, want EOF", err)
	}
}

func TestReaderRejectsOversize(t *testing.T) {
	r := companion.NewReader(bytes.NewReader([]byte{0x3e, 0x01, 0x02}), companion.HeaderFromNode, 512)
	if _, err := r.ReadFrame(); !errors.Is(err, companion.ErrFrameTooLarge) {
		t.Fatalf("err = %v, want ErrFrameTooLarge", err)
	}
}

func TestReaderTruncated(t *testing.T) {
	r := companion.NewReader(bytes.NewReader([]byte{0x3e, 0x05, 0x00, 1, 2}), companion.HeaderFromNode, 512)
	if _, err := r.ReadFrame(); err != io.ErrUnexpectedEOF {
		t.Fatalf("err = %v, want ErrUnexpectedEOF", err)
	}
}

func TestAppStartFrame(t *testing.T) {
	f := companion.AppStartFrame()
	if f[0] != 0x01 || len(f) != 8+len("sierra-meshbridge") || string(f[8:]) != "sierra-meshbridge" {
		t.Fatalf("APP_START = % x", f)
	}
}

func newNode(t *testing.T, opts fakenode.Options) *fakenode.Node {
	t.Helper()
	n, err := fakenode.Listen("127.0.0.1:0", opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.Close() })
	return n
}

func TestParseSelfInfo(t *testing.T) {
	n := newNode(t, fakenode.Options{Name: "JayNode 🛰"})
	pk, name, err := companion.ParseSelfInfo(n.SelfInfo())
	if err != nil {
		t.Fatal(err)
	}
	if pk != n.PubkeyHex() || name != "JayNode 🛰" {
		t.Fatalf("got %s %q", pk, name)
	}
	// An older firmware frame that stops after the pubkey still yields the key.
	pk, name, err = companion.ParseSelfInfo(n.SelfInfo()[:36])
	if err != nil || pk != n.PubkeyHex() || name != "" {
		t.Fatalf("short frame: %s %q %v", pk, name, err)
	}
	if _, _, err := companion.ParseSelfInfo([]byte{0x05, 1, 2}); err == nil {
		t.Fatal("expected an error for a truncated frame")
	}
}

func TestParseDeviceInfo(t *testing.T) {
	n := newNode(t, fakenode.Options{Model: "Heltec V4", FW: "v1.18.0"})
	fw, model, err := companion.ParseDeviceInfo(n.DeviceInfo())
	if err != nil || fw != "v1.18.0" || model != "Heltec V4" {
		t.Fatalf("got %q %q %v", fw, model, err)
	}
}

func TestIdentify(t *testing.T) {
	frames := make(chan []byte, 8)
	n := newNode(t, fakenode.Options{Name: "N1", Model: "RAK4631"})
	write := func(p []byte) error {
		switch p[0] {
		case companion.CmdAppStart:
			frames <- []byte{0x83} // a push arriving first is skipped
			frames <- n.SelfInfo()
		case companion.CmdDeviceQuery:
			frames <- n.DeviceInfo()
		}
		return nil
	}
	id, err := companion.Identify(context.Background(), frames, write, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if id.Pubkey != n.PubkeyHex() || id.Name != "N1" || id.Model != "RAK4631" || !strings.HasPrefix(id.FW, "v1.") {
		t.Fatalf("identity = %+v", id)
	}
}

func TestIdentifyWithoutDeviceInfo(t *testing.T) {
	frames := make(chan []byte, 8)
	n := newNode(t, fakenode.Options{Name: "Old"})
	write := func(p []byte) error {
		if p[0] == companion.CmdAppStart {
			frames <- n.SelfInfo()
		}
		return nil
	}
	id, err := companion.Identify(context.Background(), frames, write, 50*time.Millisecond)
	if err != nil || id.Name != "Old" || id.FW != "" {
		t.Fatalf("identity = %+v, %v", id, err)
	}
}

func TestIdentifyTimeoutAndClose(t *testing.T) {
	frames := make(chan []byte)
	_, err := companion.Identify(context.Background(), frames, func([]byte) error { return nil }, 20*time.Millisecond)
	if !errors.Is(err, companion.ErrTimeout) {
		t.Fatalf("err = %v, want timeout", err)
	}
	close(frames)
	_, err = companion.Identify(context.Background(), frames, func([]byte) error { return nil }, time.Second)
	if !errors.Is(err, companion.ErrLinkClosed) {
		t.Fatalf("err = %v, want link closed", err)
	}
}
