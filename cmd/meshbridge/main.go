// Command meshbridge connects a MeshCore companion node to the SIERRA Mesh
// Relay. See docs/mesh-bridge-handoff.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/sierragridteam/sierra-meshrelay/internal/bridge"
	"github.com/sierragridteam/sierra-meshrelay/internal/config"
	"github.com/sierragridteam/sierra-meshrelay/internal/connect"
	"github.com/sierragridteam/sierra-meshrelay/internal/control"
	"github.com/sierragridteam/sierra-meshrelay/internal/diaglog"
	"github.com/sierragridteam/sierra-meshrelay/internal/nodelink"
	"github.com/sierragridteam/sierra-meshrelay/internal/relayclient"
	"github.com/sierragridteam/sierra-meshrelay/internal/state"
	"github.com/sierragridteam/sierra-meshrelay/meshproto"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

var usage = `meshbridge connects a MeshCore companion node to the SIERRA Mesh Relay.

On the bridge's computer (with sudo, or as an administrator on Windows):
  meshbridge setup [-passphrase words] [-host addr]
                                  Pair this computer's bridge with SIERRA and write its config
  meshbridge install              Install and start the bridge as a service (Linux, Windows)
  meshbridge uninstall            Stop and remove the service (keeps the config)
  meshbridge run                  Run the bridge in the foreground
  meshbridge status               Show what the running bridge is doing
  meshbridge pause [1h|tomorrow|until-resumed]
                                  Stop sharing the node (default 1h)
  meshbridge resume               Start sharing again
  meshbridge reconnect            Look for the node now instead of waiting for the next retry
  meshbridge change-node [-host addr]
                                  Switch this bridge to a different node (a replacement or a reflashed one)
  meshbridge repeater add|list|remove
                                  Share your repeaters with SIERRA

From any computer:
  meshbridge find-node [-host addr]
                                  Find MeshCore nodes on this network and print their identity
  meshbridge connect <gateway>    Use a remote gateway as a local companion port (needs a client token)
  meshbridge admin <command>      Manage gateways, pairing, bridge updates and repeaters (SIERRA admins)
  meshbridge version              Print version and build info

The bridge's commands read its config, ` + config.DefaultPath() + `
(-config picks another). meshbridge <command> -h lists a command's flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = cmdRun(os.Args[2:])
	case "setup":
		err = cmdSetup(os.Args[2:])
	case "status":
		err = cmdStatus(os.Args[2:])
	case "pause":
		err = cmdPause(os.Args[2:])
	case "resume":
		err = cmdResume(os.Args[2:])
	case "reconnect":
		err = cmdReconnect(os.Args[2:])
	case "change-node":
		err = cmdChangeNode(os.Args[2:])
	case "connect":
		err = cmdConnect(os.Args[2:])
	case "repeater", "repeaters":
		err = cmdRepeater(os.Args[2:])
	case "admin":
		err = cmdAdmin(os.Args[2:])
	case "install":
		err = cmdInstall(os.Args[2:])
	case "uninstall":
		err = cmdUninstall(os.Args[2:])
	case "find-node":
		err = cmdFindNode(os.Args[2:])
	case "apply-update": // run by the service (systemd ExecStartPre, Windows scheduled task)
		err = cmdApplyUpdate(os.Args[2:])
	case "version", "-version", "--version":
		cmdVersion()
	case "help", "-h", "-help", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "meshbridge: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "meshbridge:", err)
		os.Exit(1)
	}
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	path := fs.String("config", config.DefaultPath(), "config file")
	logJSON := fs.Bool("log-json", false, "log as JSON")
	controlAddr := fs.String("control", control.DefaultAddr, `local control API address ("off" to disable)`)
	service := fs.Bool("service", false, "running as the installed systemd service (set by its unit): updates apply on restart")
	_ = fs.Parse(args)

	if underServiceManager() {
		// Windows: the service has no console, so it logs to a file beside
		// its config, and stops when the service manager says so.
		return runAsService(filepath.Dir(*path), func(ctx context.Context, logOut io.Writer) error {
			return runBridge(ctx, *path, *logJSON, *controlAddr, logOut, true)
		})
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runBridge(ctx, *path, *logJSON, *controlAddr, os.Stderr, *service)
}

// runBridge runs the bridge until ctx ends. managed says it's the installed
// service, which may update itself.
func runBridge(ctx context.Context, path string, logJSON bool, controlAddr string, logOut io.Writer, managed bool) error {
	cfg, err := loadBridgeConfig(path)
	if err != nil {
		return err
	}
	var diag *diaglog.Ring
	if cfg.Host.SendLogs {
		diag = diaglog.NewRing(diaglog.DefaultSize)
	}
	log := newLogger(cfg.Log.Level, logJSON, logOut, diag)
	if cfg.Host.KeepAwake {
		keepAwake(ctx, log)
	}

	dir := filepath.Dir(path)
	st := state.New(filepath.Join(dir, "state.json"))
	// A WiFi node's DHCP address can change: Discover tries the configured
	// address, then the last one that worked, then scans the LAN for the
	// node with the expected pubkey. (Serial nodes arrive with M2.)
	opener := &nodelink.Discover{Host: cfg.Node.TCPHost, Port: cfg.Node.TCPPort,
		ExpectedPubkey: cfg.Node.ExpectedPubkey, State: st, Log: log}
	detail := "auto (LAN scan)"
	if cfg.Node.TCPHost != "" {
		detail = fmt.Sprintf("%s:%d", cfg.Node.TCPHost, cfg.Node.TCPPort)
	}
	log.Info("meshbridge starting", "version", version, "gateway", cfg.Gateway, "relay", cfg.Relay, "node", detail)
	reportApplied(dir, log)
	ctx, stopForUpdate := context.WithCancel(ctx)
	defer stopForUpdate()
	upd := &autoUpdater{dir: dir, log: log, managed: managed && cfg.Host.AutoUpdate,
		allowHTTP: strings.HasPrefix(cfg.Relay, "ws://"), restart: stopForUpdate}
	b := bridge.New(bridge.Options{
		Config: cfg, Opener: opener, Log: log, Version: version,
		LinkKind: meshproto.LinkTCP, LinkDetail: detail, State: st, Diag: diag, OnUpdate: upd.offer,
	})
	upd.bridge = b
	if controlAddr != "off" {
		tok, err := control.LoadOrCreateToken(filepath.Join(dir, control.TokenFile))
		if err != nil {
			log.Warn("control API disabled: can't create its secret", "error", err)
		} else {
			srv := &control.Server{Bridge: b, Token: tok, Gateway: cfg.Gateway, Version: version}
			go func() {
				if err := srv.ListenAndServe(ctx, controlAddr); err != nil {
					log.Warn("control API stopped", "addr", controlAddr, "error", err)
				}
			}()
			log.Info("control API listening", "addr", controlAddr)
		}
	}
	if err := b.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	log.Info("meshbridge stopped")
	return nil
}

// newLogger logs to out at level and, with diag, keeps Info and above for
// the relay.
func newLogger(level string, asJSON bool, out io.Writer, diag *diaglog.Ring) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	if os.Getenv("JOURNAL_STREAM") != "" {
		// Under systemd, journald timestamps every line itself.
		opts.ReplaceAttr = func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		}
	}
	var h slog.Handler = slog.NewTextHandler(out, opts)
	if asJSON {
		h = slog.NewJSONHandler(out, opts)
	}
	if diag != nil {
		h = diag.Handler(h)
	}
	return slog.New(h)
}

func cmdVersion() {
	fmt.Printf("meshbridge %s (%s/%s, %s)\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" || s.Key == "vcs.modified" {
				fmt.Printf("  %s=%s\n", s.Key, s.Value)
			}
		}
	}
}

// cmdFindNode runs the identity check against a TCP node. Serial detection
// and LAN scanning come later.
func cmdFindNode(args []string) error {
	fs := flag.NewFlagSet("find-node", flag.ExitOnError)
	host := fs.String("host", "", "node address")
	port := fs.Int("port", 5000, "node TCP port")
	_ = fs.Parse(args)
	if *host == "" {
		fmt.Printf("Looking for nodes on this network (port %d)…\n", *port)
		found := (&nodelink.Discover{Port: *port}).Scan(context.Background())
		if len(found) == 0 {
			return errors.New("no node found on this network (use -host if it's on another subnet)")
		}
		for _, f := range found {
			fmt.Printf("Found: %s\n  link    tcp %s\n  pubkey  %s\n  model   %s\n  fw      %s\n", f.Name, f.Addr, f.Pubkey, f.Model, f.FW)
		}
		return nil
	}
	id, detail, err := identifyTCP(*host, *port)
	if err != nil {
		return err
	}
	fmt.Printf("Found: %s\n  link    tcp %s\n  pubkey  %s\n  model   %s\n  fw      %s\n",
		id.Name, detail, id.Pubkey, id.Model, id.FW)
	return nil
}

// clientTokenEnv supplies the client token without putting it in shell history.
const clientTokenEnv = "MESHBRIDGE_CLIENT_TOKEN"

func cmdConnect(args []string) error {
	fs := flag.NewFlagSet("connect", flag.ExitOnError)
	token := fs.String("token", "", "access token (smh_… for one gateway, or an admin's smc_…); default $"+clientTokenEnv)
	listen := fs.String("listen", "127.0.0.1:5000", "local address for companion apps")
	relay := fs.String("relay", config.DefaultRelay, "relay base URL")
	preempt := fs.Bool("preempt", false, "take the gateway from its current user (admins only)")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: meshbridge connect <gateway> [flags]\n\n"+
			"Opens a local TCP port that MeshCore apps and meshcore-cli can use as if the\n"+
			"gateway's node were on this network. Needs an access token from a SIERRA admin,\n"+
			"in $%s (safer than -token, which lands in shell history).\n\n", clientTokenEnv)
		fs.PrintDefaults()
	}
	// Accept the gateway before or after the flags.
	var gateway string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		gateway, args = args[0], args[1:]
	}
	_ = fs.Parse(args)
	if gateway == "" && fs.NArg() > 0 {
		gateway = fs.Arg(0)
	}
	if gateway == "" {
		fs.Usage()
		os.Exit(2)
	}
	if *token == "" {
		*token = os.Getenv(clientTokenEnv)
	}
	if *token == "" {
		return fmt.Errorf("connect needs an access token for the gateway (smh_…) from a SIERRA admin: set $%s (or pass -token)", clientTokenEnv)
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		return fmt.Errorf("-listen: %w", err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		fmt.Fprintf(os.Stderr, "WARNING: listening on %s. Anyone who can reach this address can use %s with YOUR token: companion apps have no authentication.\n", *listen, gateway)
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	say := func(format string, a ...any) {
		fmt.Printf("%s  %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, a...))
	}
	say("Listening on %s for %s. Point your MeshCore app or meshcore-cli at it (e.g. meshcore-cli -t %s -p %s).",
		ln.Addr(), gateway, host, portOf(ln.Addr()))
	shim := connect.New(connect.Options{
		Gateway: gateway,
		Say:     say,
		Dial: func(ctx context.Context) (*relayclient.Conn, error) {
			return relayclient.Dial(ctx, *relay, gateway, *token, *preempt)
		},
	})
	return shim.Serve(ctx, ln)
}

func portOf(a net.Addr) string {
	_, p, _ := net.SplitHostPort(a.String())
	return p
}
