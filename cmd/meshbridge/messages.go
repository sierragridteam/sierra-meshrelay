package main

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/sierragridteam/sierra-meshrelay/internal/admin"
	"github.com/sierragridteam/sierra-meshrelay/internal/config"
)

// errNoBridge says there is no bridge config at the path a command was
// given: usually because it was run on a computer that isn't the bridge's.
type errNoBridge struct{ path string }

func (e *errNoBridge) Error() string {
	return fmt.Sprintf("no bridge is set up on this computer (no config at %s).\n"+
		"This command acts as the bridge, so run it on the bridge's computer, or pass -config.\n"+
		"To pair this computer as a bridge: %s", e.path, setupHint)
}

// loadBridgeConfig loads this computer's bridge config for a command that
// acts as the bridge, explaining the two common failures: no bridge here,
// and a config only root (an administrator) can read.
func loadBridgeConfig(path string) (config.Config, error) {
	cfg, err := config.Load(path)
	switch {
	case err == nil:
		return cfg, nil
	case errors.Is(err, fs.ErrNotExist):
		return cfg, &errNoBridge{path}
	case errors.Is(err, fs.ErrPermission):
		return cfg, fmt.Errorf("can't read the bridge's config %s: %s", path, elevateHint)
	}
	return cfg, err
}

// isConnRefused reports whether err is a refused TCP connection. Windows
// words it differently ("actively refused"), so match both.
func isConnRefused(err error) bool {
	s := err.Error()
	return strings.Contains(s, "connection refused") || strings.Contains(s, "actively refused")
}

// isUnauthorized reports whether the relay rejected the token (HTTP 401).
func isUnauthorized(err error) bool {
	var ae *admin.APIError
	return errors.As(err, &ae) && ae.Status == 401
}
