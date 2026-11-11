package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLoadBridgeConfigExplainsAMissingBridge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meshbridge.toml")
	_, err := loadBridgeConfig(path)
	var nb *errNoBridge
	if !errors.As(err, &nb) {
		t.Fatalf("err = %v, want errNoBridge", err)
	}
	if !strings.Contains(err.Error(), "no bridge is set up on this computer") {
		t.Errorf("message = %q", err)
	}
}

func TestLoadBridgeConfigExplainsAnUnreadableConfig(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs Unix file modes and a non-root user")
	}
	path := filepath.Join(t.TempDir(), "meshbridge.toml")
	if err := os.WriteFile(path, []byte("gateway = \"x\"\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	_, err := loadBridgeConfig(path)
	if err == nil || !strings.Contains(err.Error(), elevateHint) {
		t.Fatalf("err = %v, want the elevate hint", err)
	}
}

func TestIsConnRefused(t *testing.T) {
	for _, s := range []string{
		"dial tcp 127.0.0.1:8737: connect: connection refused",
		"dial tcp 127.0.0.1:8737: connectex: No connection could be made because the target machine actively refused it.",
	} {
		if !isConnRefused(errors.New(s)) {
			t.Errorf("isConnRefused(%q) = false", s)
		}
	}
	if isConnRefused(errors.New("i/o timeout")) {
		t.Error("a timeout isn't a refusal")
	}
}
