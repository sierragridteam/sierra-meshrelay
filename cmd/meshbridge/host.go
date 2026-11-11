package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	iofs "io/fs"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/sierragridteam/sierra-meshrelay/internal/companion"
	"github.com/sierragridteam/sierra-meshrelay/internal/config"
	"github.com/sierragridteam/sierra-meshrelay/internal/control"
	"github.com/sierragridteam/sierra-meshrelay/internal/nodelink"
	"github.com/sierragridteam/sierra-meshrelay/internal/pairing"
	"github.com/sierragridteam/sierra-meshrelay/internal/svc"
	"github.com/sierragridteam/sierra-meshrelay/meshproto"
)

// identifyTCP runs the identity check against a TCP node.
func identifyTCP(host string, port int) (companion.Identity, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	link, err := nodelink.TCP{Host: host, Port: port}.Open(ctx)
	if err != nil {
		return companion.Identity{}, "", err
	}
	defer link.Close()
	frames := make(chan []byte, 64)
	go func() {
		defer close(frames)
		for {
			f, err := link.ReadFrame()
			if err != nil {
				return
			}
			frames <- f
		}
	}()
	id, err := companion.Identify(ctx, frames, link.WriteFrame, 5*time.Second)
	if err != nil {
		return id, link.Detail(), fmt.Errorf("identity check against %s: %w", link.Detail(), err)
	}
	return id, link.Detail(), nil
}

// --- setup -------------------------------------------------------------------

func cmdSetup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	path := fs.String("config", config.DefaultPath(), "config file to write")
	passphrase := fs.String("passphrase", "", "the four words from your SIERRA contact (omit to be asked, or to get a pairing code)")
	relay := fs.String("relay", "", "relay URL (default: the config's, or SIERRA's)")
	host := fs.String("host", "", "your node's address on this network (WiFi/Ethernet nodes)")
	port := fs.Int("port", 5000, "your node's TCP port")
	repair := fs.Bool("repair", false, "pair again even though this bridge is already paired")
	byInstaller := fs.Bool("installer", false, "set by the installer script, which installs the service next")
	_ = fs.Parse(args)

	cfg, exists, err := config.LoadRaw(*path)
	if errors.Is(err, iofs.ErrPermission) {
		return fmt.Errorf("can't read %s: %s", *path, elevateHint)
	}
	if err != nil {
		return fmt.Errorf("reading %s: %w", *path, err)
	}
	if exists && cfg.Token != "" && !*repair {
		return fmt.Errorf("this bridge is already paired as gateway %q (%s). Use -repair to pair it again", cfg.Gateway, *path)
	}
	// Check the config can be saved before pairing: an approved pairing whose
	// token can't be written is wasted (and the code or passphrase used up).
	if err := checkWritable(filepath.Dir(*path)); err != nil {
		if errors.Is(err, iofs.ErrPermission) {
			return fmt.Errorf("can't write the config to %s: %s", filepath.Dir(*path), elevateHint)
		}
		return fmt.Errorf("can't write the config to %s: %w", filepath.Dir(*path), err)
	}
	if *relay != "" {
		cfg.Relay = *relay
	}
	if *host != "" {
		cfg.Node.TCPHost, cfg.Node.TCPPort, cfg.Node.Link = *host, *port, config.LinkTCP
	}

	req := meshproto.PairStartRequest{
		Agent: meshproto.Agent{Name: "meshbridge", Version: version, OS: runtime.GOOS, Arch: runtime.GOARCH},
		Node:  meshproto.PairNode{Node: meshproto.Node{State: meshproto.NodeDisconnected}},
	}
	if h, err := os.Hostname(); err == nil {
		req.HostLabel = h
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	interactive := isTerminal(os.Stdin)
	in := bufio.NewReader(os.Stdin)

	if id, ok := findNodeForSetup(ctx, &cfg, interactive, in); ok {
		req.Node = meshproto.PairNode{Node: meshproto.Node{State: meshproto.NodeConnected,
			Pubkey: id.Pubkey, Name: id.Name, FW: id.FW, Model: id.Model}, Link: meshproto.LinkTCP}
	}
	client := &pairing.Client{APIBase: config.HTTPBase(cfg.Relay)}

	phrase := *passphrase
	if phrase == "" && interactive {
		fmt.Print("Got a passphrase from SIERRA? Type it here, or press Enter for a pairing code: ")
		line, _ := in.ReadString('\n')
		phrase = strings.TrimSpace(line)
	}
	var res meshproto.PairResult
	if phrase != "" {
		req.Passphrase = phrase
		res, err = client.Redeem(ctx, req)
		switch {
		case errors.Is(err, pairing.ErrInvalidPassphrase) && interactive:
			fmt.Println("That passphrase didn't work. Check the words, or ask your SIERRA contact for a new one.")
			fmt.Println("Getting a pairing code instead…")
		case err != nil:
			return err
		}
	}
	for res.Token == "" {
		start, err := client.Start(ctx, req)
		if err != nil {
			return err
		}
		fmt.Printf("\n    Your pairing code:   %s\n\n", start.UserCode)
		fmt.Printf("Text this code to your SIERRA contact. Waiting for them to approve it (the code lasts %d minutes)…\n",
			start.ExpiresIn/60)
		res, err = client.Wait(ctx, start)
		if errors.Is(err, pairing.ErrExpired) && interactive {
			fmt.Print("The code expired. Press Enter for a new one, or Ctrl-C to stop: ")
			_, _ = in.ReadString('\n')
			continue
		}
		if err != nil {
			return err
		}
	}

	cfg.Gateway, cfg.Token = res.Gateway, res.Token
	if res.Relay != "" {
		cfg.Relay = res.Relay
	}
	if res.ExpectedPubkey != "" {
		cfg.Node.ExpectedPubkey = res.ExpectedPubkey
	}
	// Lock the folder down before the token is written into it (Windows;
	// elsewhere the file is written 0600).
	if err := os.MkdirAll(filepath.Dir(*path), 0o755); err == nil {
		if err := svc.SecureConfigDir(filepath.Dir(*path)); err != nil {
			return fmt.Errorf("paired as %q, but couldn't protect the config folder, so the token wasn't saved: %w", res.Gateway, err)
		}
	}
	if err := config.Save(*path, cfg); err != nil {
		return fmt.Errorf("paired as %q, but couldn't save the config to %s: %w", res.Gateway, *path, err)
	}
	fmt.Printf("\nConnected to SIERRA as gateway %q. Config saved to %s.\n", res.Gateway, *path)
	if cfg.Node.ExpectedPubkey == "" {
		fmt.Println("No node was found yet; the bridge will keep looking on this network.")
	}
	run := runHint
	if *path != config.DefaultPath() {
		run += " -config " + *path
	}
	switch {
	case *byInstaller:
		fmt.Println("Installing the service…")
	case serviceSupported:
		fmt.Printf("Install it as a service, so it starts at boot: %s\n(or run it in the foreground: %s)\n", installHint, run)
	default:
		fmt.Printf("Start the bridge with: %s\n", run)
	}
	return nil
}

// checkWritable reports whether files can be created in dir, creating it
// if needed.
func checkWritable(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".meshbridge-write-check-*")
	if err != nil {
		return err
	}
	f.Close()
	return os.Remove(f.Name())
}

// findNodeForSetup finds the node to pair: at the configured address, or by
// scanning the local network. It fills in cfg.Node.TCPHost/TCPPort.
func findNodeForSetup(ctx context.Context, cfg *config.Config, interactive bool, in *bufio.Reader) (companion.Identity, bool) {
	if cfg.Node.TCPHost != "" {
		fmt.Printf("Looking for your node at %s:%d…\n", cfg.Node.TCPHost, cfg.Node.TCPPort)
		id, detail, err := identifyTCP(cfg.Node.TCPHost, cfg.Node.TCPPort)
		if err != nil {
			fmt.Printf("Couldn't reach it (%v). Pairing anyway; the bridge will keep looking.\n\n", err)
			return id, false
		}
		fmt.Printf("Found: %s · %s (%s)\n\n", id.Name, id.Model, detail)
		return id, true
	}
	fmt.Println("Looking for your node on this network…")
	found := (&nodelink.Discover{Port: cfg.Node.TCPPort}).Scan(ctx)
	var pick *nodelink.Found
	switch {
	case len(found) == 0:
		fmt.Println("No node found on this network. Pairing anyway: make sure the node's WiFi is on this network,")
		fmt.Println("or rerun setup with -host <address>. The bridge will keep looking.")
		fmt.Println()
		return companion.Identity{}, false
	case len(found) == 1:
		pick = &found[0]
	case interactive:
		fmt.Println("Found more than one node:")
		for i, f := range found {
			fmt.Printf("  %d. %s · %s (%s)\n", i+1, f.Name, f.Model, f.Addr)
		}
		for pick == nil {
			fmt.Printf("Which one is yours? [1-%d]: ", len(found))
			line, err := in.ReadString('\n')
			var n int
			if _, serr := fmt.Sscanf(strings.TrimSpace(line), "%d", &n); serr == nil && n >= 1 && n <= len(found) {
				pick = &found[n-1]
			} else if err != nil {
				return companion.Identity{}, false
			}
		}
	default:
		fmt.Println("Found more than one node; rerun setup with -host <address> to pick yours:")
		for _, f := range found {
			fmt.Printf("  %s · %s (%s)\n", f.Name, f.Model, f.Addr)
		}
		return companion.Identity{}, false
	}
	host, port, _ := net.SplitHostPort(pick.Addr)
	cfg.Node.TCPHost, cfg.Node.Link = host, config.LinkTCP
	fmt.Sscanf(port, "%d", &cfg.Node.TCPPort)
	fmt.Printf("Found: %s · %s (%s)\n\n", pick.Name, pick.Model, pick.Addr)
	return pick.Identity, true
}

func isTerminal(f *os.File) bool { return term.IsTerminal(int(f.Fd())) }

// --- status, pause, resume -----------------------------------------------------

// controlClient finds the running bridge's control API from its config path.
func controlClient(fs *flag.FlagSet, args []string) (*control.Client, *flag.FlagSet, error) {
	path := fs.String("config", config.DefaultPath(), "config file of the running bridge")
	addr := fs.String("control", control.DefaultAddr, "the bridge's control API address")
	_ = fs.Parse(args)
	secret := filepath.Join(filepath.Dir(*path), control.TokenFile)
	tok, err := control.ReadToken(secret)
	switch {
	case err == nil:
	case errors.Is(err, iofs.ErrPermission):
		return nil, fs, fmt.Errorf("can't read the bridge's control secret %s: %s", secret, elevateHint)
	case errors.Is(err, iofs.ErrNotExist):
		if _, serr := os.Stat(*path); errors.Is(serr, iofs.ErrNotExist) {
			return nil, fs, &errNoBridge{*path}
		}
		return nil, fs, fmt.Errorf("the bridge hasn't run yet: it creates its control secret (%s) when it first starts. Start it with: %s",
			secret, startHint())
	default:
		return nil, fs, fmt.Errorf("can't read the bridge's control secret: %w", err)
	}
	return &control.Client{Addr: *addr, Token: tok}, fs, nil
}

func cmdStatus(args []string) error {
	c, _, err := controlClient(flag.NewFlagSet("status", flag.ExitOnError), args)
	if err != nil {
		return err
	}
	d, err := c.Do(context.Background(), "GET", "/status", nil)
	if err != nil {
		return notRunning(err)
	}
	printStatus(d)
	return nil
}

func cmdPause(args []string) error {
	c, fs, err := controlClient(flag.NewFlagSet("pause", flag.ExitOnError), args)
	if err != nil {
		return err
	}
	until := "1h"
	if fs.NArg() > 0 {
		until = fs.Arg(0)
	}
	d, err := c.Do(context.Background(), "POST", "/pause", control.PauseRequest{Until: until})
	if err != nil {
		return notRunning(err)
	}
	printStatus(d)
	return nil
}

func cmdResume(args []string) error {
	c, _, err := controlClient(flag.NewFlagSet("resume", flag.ExitOnError), args)
	if err != nil {
		return err
	}
	d, err := c.Do(context.Background(), "POST", "/resume", nil)
	if err != nil {
		return notRunning(err)
	}
	printStatus(d)
	return nil
}

func notRunning(err error) error {
	if isConnRefused(err) {
		return fmt.Errorf("the bridge isn't running. Start it with: %s", startHint())
	}
	return err
}

// startHint says how to start the bridge: its service where there is one.
func startHint() string {
	switch {
	case runtime.GOOS == "windows":
		return "Start-Service meshbridge (elevated PowerShell), or " + runHint
	case serviceSupported:
		return "sudo systemctl start meshbridge (if installed), or " + runHint
	}
	return runHint
}

func printStatus(d control.StatusDoc) {
	dot := map[string]string{control.Green: "\033[32m●\033[0m", control.Amber: "\033[33m●\033[0m", control.Red: "\033[31m●\033[0m"}[d.Level]
	if !isTerminal(os.Stdout) {
		dot = "[" + d.Level + "]"
	}
	fmt.Printf("%s %s\n", dot, d.Reason)
	fmt.Printf("  Gateway:   %s\n", d.Gateway)
	node := d.Node.State
	if d.Node.Name != "" {
		node = d.Node.Name
		if d.Node.Model != "" {
			node += " (" + d.Node.Model + ")"
		}
		if d.Node.Link != "" {
			node += " · " + d.Node.Link
		}
	}
	fmt.Printf("  Node:      %s\n", node)
	if d.LastMeshTraffic != nil {
		fmt.Printf("  Last mesh traffic: %s ago\n", time.Since(*d.LastMeshTraffic).Round(time.Second))
	}
	switch {
	case d.Paused && d.PauseUntil == nil:
		fmt.Println("  Sharing:   paused until you resume")
	case d.Paused:
		fmt.Printf("  Sharing:   paused until %s\n", d.PauseUntil.Local().Format("Mon 15:04"))
	case d.ClientAttached:
		fmt.Println("  Sharing:   on (a SIERRA client is using the node now)")
	default:
		fmt.Println("  Sharing:   on")
	}
}

// --- install, uninstall ---------------------------------------------------------

func cmdInstall(args []string) error {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	path := fs.String("config", config.DefaultPath(), "config file the service runs with")
	_ = fs.Parse(args)
	if err := svc.Supported(); err != nil {
		return err
	}
	abs, err := filepath.Abs(*path)
	if err != nil {
		return err
	}
	if _, err := loadBridgeConfig(abs); err != nil {
		var nb *errNoBridge
		if errors.As(err, &nb) {
			return fmt.Errorf("this computer's bridge isn't paired yet (no config at %s). Pair it first with: %s", abs, setupHint)
		}
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	inst, err := svc.Install(exe, abs)
	if err != nil {
		return err
	}
	fmt.Printf("Installed and started the meshbridge service (%s, config %s).\n", inst.Binary, abs)
	fmt.Println("It starts at boot and restarts if it stops. Check on it with:")
	for _, c := range inst.CheckOn {
		fmt.Println("  " + c)
	}
	return nil
}

func cmdUninstall(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
	_ = fs.Parse(args)
	if err := svc.Supported(); err != nil {
		return err
	}
	if err := svc.Uninstall(); err != nil {
		return err
	}
	fmt.Printf("Stopped and removed the meshbridge service. The config (and this bridge's token) is still in %s;\n",
		filepath.Dir(config.DefaultPath()))
	fmt.Println("delete it too if this computer is leaving SIERRA.")
	return nil
}
