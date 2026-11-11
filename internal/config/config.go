// Package config loads the bridge's TOML config file.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/BurntSushi/toml"
)

// DefaultRelay is the production relay's base URL.
const DefaultRelay = "wss://data.sierragridteam.org/mesh/relay"

// Link modes.
const (
	LinkAuto   = "auto"
	LinkSerial = "serial"
	LinkTCP    = "tcp"
)

// Config is the bridge config. See docs/mesh-bridge-handoff.md, "Config".
type Config struct {
	Gateway string `toml:"gateway"`
	Relay   string `toml:"relay"`
	Token   string `toml:"token"`
	Node    Node   `toml:"node"`
	Host    Host   `toml:"host"`
	Log     Log    `toml:"log"`
}

// Node configures the node link.
type Node struct {
	Link           string `toml:"link"`
	ExpectedPubkey string `toml:"expected_pubkey"`
	SerialPort     string `toml:"serial_port"`
	Baud           int    `toml:"baud"`
	TCPHost        string `toml:"tcp_host"`
	TCPPort        int    `toml:"tcp_port"`
}

// Host configures host-machine behaviour.
type Host struct {
	KeepAwake bool `toml:"keep_awake"`
	// SendLogs sends the bridge's recent log lines (never message content)
	// to the relay, where SIERRA admins can read them to help.
	SendLogs bool `toml:"send_logs"`
	// AutoUpdate installs signed bridge updates SIERRA offers, when nobody
	// is using the node (installed service only).
	AutoUpdate bool `toml:"auto_update"`
}

// Log configures logging.
type Log struct {
	Level string `toml:"level"`
}

// TokenEnv overrides the config file's token, so developers and containers
// can keep the secret out of the file.
const TokenEnv = "MESHBRIDGE_TOKEN"

// Default returns a config with every default filled in.
func Default() Config {
	return Config{
		Relay: DefaultRelay,
		Node:  Node{Link: LinkAuto, Baud: 115200, TCPPort: 5000},
		Host:  Host{KeepAwake: true, SendLogs: true, AutoUpdate: true},
		Log:   Log{Level: "info"},
	}
}

// Load reads path over the defaults, applies env overrides and validates.
func Load(path string) (Config, error) {
	c := Default()
	md, err := toml.DecodeFile(path, &c)
	if err != nil {
		return c, fmt.Errorf("reading config %s: %w", path, err)
	}
	if un := md.Undecoded(); len(un) > 0 {
		keys := make([]string, len(un))
		for i, k := range un {
			keys[i] = k.String()
		}
		return c, fmt.Errorf("config %s: unknown keys: %s", path, strings.Join(keys, ", "))
	}
	if t := os.Getenv(TokenEnv); t != "" {
		c.Token = t
	}
	return c, c.Validate()
}

var gatewayRe = regexp.MustCompile(`^[a-z0-9-]{3,40}$`)
var pubkeyRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Validate checks the config is usable.
func (c *Config) Validate() error {
	var errs []error
	if !gatewayRe.MatchString(c.Gateway) {
		errs = append(errs, fmt.Errorf("gateway %q must match [a-z0-9-]{3,40}", c.Gateway))
	}
	if !strings.HasPrefix(c.Relay, "wss://") && !strings.HasPrefix(c.Relay, "ws://") {
		errs = append(errs, fmt.Errorf("relay %q must be a ws:// or wss:// URL", c.Relay))
	}
	c.Relay = strings.TrimRight(c.Relay, "/")
	if c.Token == "" {
		errs = append(errs, fmt.Errorf("no token: this bridge isn't paired yet (run meshbridge setup), or set %s", TokenEnv))
	} else if !strings.HasPrefix(c.Token, "smb_") {
		errs = append(errs, errors.New("token must be a bridge token (smb_…)"))
	}
	c.Node.ExpectedPubkey = strings.ToLower(strings.TrimSpace(c.Node.ExpectedPubkey))
	if c.Node.ExpectedPubkey != "" && !pubkeyRe.MatchString(c.Node.ExpectedPubkey) {
		errs = append(errs, errors.New("node.expected_pubkey must be 64 hex characters"))
	}
	switch c.Node.Link {
	case LinkAuto, LinkTCP:
	case LinkSerial:
		errs = append(errs, errors.New("node.link = \"serial\" isn't supported yet (M2)"))
	default:
		errs = append(errs, fmt.Errorf("node.link %q must be auto, serial or tcp", c.Node.Link))
	}
	if c.Node.TCPPort <= 0 || c.Node.TCPPort > 65535 {
		errs = append(errs, fmt.Errorf("node.tcp_port %d is out of range", c.Node.TCPPort))
	}
	return errors.Join(errs...)
}

// DefaultPath is the config file location for this OS.
func DefaultPath() string {
	switch runtime.GOOS {
	case "windows":
		pd := os.Getenv("ProgramData")
		if pd == "" {
			pd = `C:\ProgramData`
		}
		return pd + `\SIERRA\meshbridge\meshbridge.toml`
	case "darwin":
		return "/Library/Application Support/SIERRA/meshbridge/meshbridge.toml"
	default:
		return "/etc/meshbridge/meshbridge.toml"
	}
}

// Save writes c to path, readable only by its owner (it holds the bridge
// token). The directory is created if needed. The write is atomic, so a
// crash never leaves a half-written config.
func Save(path string, c Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var buf bytes.Buffer
	buf.WriteString("# meshbridge config, written by `meshbridge setup`.\n# It holds this bridge's token: keep it private.\n\n")
	if err := toml.NewEncoder(&buf).Encode(c); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadRaw reads path over the defaults without validating, for setup, which
// edits a config that may not be complete yet. A missing file is not an error.
func LoadRaw(path string) (Config, bool, error) {
	c := Default()
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return c, false, nil
	}
	_, err := toml.DecodeFile(path, &c)
	return c, err == nil, err
}

// HTTPBase turns the relay's WebSocket base URL (wss://host/mesh/relay) into
// its HTTP API base (https://host/mesh/relay/api).
func HTTPBase(relay string) string {
	relay = strings.TrimRight(relay, "/")
	switch {
	case strings.HasPrefix(relay, "wss://"):
		relay = "https://" + strings.TrimPrefix(relay, "wss://")
	case strings.HasPrefix(relay, "ws://"):
		relay = "http://" + strings.TrimPrefix(relay, "ws://")
	}
	return relay + "/api"
}
