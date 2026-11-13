package e2e

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sierragridteam/sierra-meshrelay/internal/bridge"
	"github.com/sierragridteam/sierra-meshrelay/internal/config"
	"github.com/sierragridteam/sierra-meshrelay/internal/diaglog"
	"github.com/sierragridteam/sierra-meshrelay/internal/nodelink"
)

// TestBridgeLogsReachAdmins: lines the bridge logs (including while it
// couldn't reach the relay) arrive at the relay for admins, secrets redacted.
func TestBridgeLogsReachAdmins(t *testing.T) {
	lr := startRelay(t)
	tok := lr.createGateway("cabin")
	node := startNode(t, "Cabin Node")

	ring := diaglog.NewRing(50)
	log := slog.New(ring.Handler(slog.NewTextHandler(testWriter{t}, nil)))
	// Logged before the bridge ever connects, as a setup problem would be.
	log.Warn("node not found at the configured address; scanning the LAN", "addr", "10.1.2.3:5000", "control_token", "s3cret")

	cfg := config.Default()
	cfg.Gateway, cfg.Relay, cfg.Token = "cabin", lr.wsBase(), tok
	b := bridge.New(bridge.Options{
		Config: cfg, Opener: nodelink.TCP{Host: "127.0.0.1", Port: node.Addr().Port}, Log: log, Version: "e2e", Diag: ring,
		NodeRetryMin: 20 * time.Millisecond, NodeRetryMax: 100 * time.Millisecond,
		Backoff: &bridge.Backoff{Min: 20 * time.Millisecond, Max: 200 * time.Millisecond, ResetAfter: time.Minute,
			AuthWait: time.Hour, UpgradeWait: time.Hour},
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); b.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	lr.waitGateway("cabin", "online", connected)

	ac := adminClient(lr)
	var text string
	deadline := time.Now().Add(10 * time.Second)
	for {
		logs, events, err := ac.Logs(context.Background(), "cabin", 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		for _, l := range logs {
			b.WriteString(l.Level + " " + l.Msg + " " + jsonString(l.Attrs) + "\n")
		}
		text = b.String()
		if strings.Contains(text, "scanning the LAN") && strings.Contains(text, "connected to relay") && len(events) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("logs never arrived:\n%s", text)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if strings.Contains(text, "s3cret") || !strings.Contains(text, `"control_token":"[redacted]"`) || !strings.Contains(text, "10.1.2.3:5000") {
		t.Fatalf("redaction:\n%s", text)
	}

	// Only admins can read them: not even the gateway's host token.
	var issued struct {
		Token string `json:"token"`
	}
	if code := lr.api("POST", "/clients", map[string]any{"role": "host", "gateway": "cabin", "label": "jay"}, &issued); code != 200 {
		t.Fatalf("host token: HTTP %d", code)
	}
	viewer := *ac
	viewer.Token = issued.Token
	if _, _, err := viewer.Logs(context.Background(), "cabin", 0, 10); err == nil {
		t.Fatal("a host token read bridge logs")
	}
}
