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
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sierragridteam/sierra-meshrelay/internal/admin"
	"github.com/sierragridteam/sierra-meshrelay/internal/config"
	"github.com/sierragridteam/sierra-meshrelay/internal/control"
	"github.com/sierragridteam/sierra-meshrelay/internal/nodelink"
	"github.com/sierragridteam/sierra-meshrelay/internal/state"
	"github.com/sierragridteam/sierra-meshrelay/internal/svc"
	"github.com/sierragridteam/sierra-meshrelay/meshproto"
)

// changeNodeWait bounds waiting for the restarted bridge to reach the new
// node.
const changeNodeWait = 45 * time.Second

// cmdChangeNode switches this bridge to a different node: a replacement,
// or a reflashed node with a new identity. It finds the node, tells SIERRA
// (which re-pins the gateway to it: the host's choice, recorded for
// admins), saves it in the config and restarts the bridge. The gateway keeps
// its name, token and shared repeaters.
func cmdChangeNode(args []string) error {
	fs := flag.NewFlagSet("change-node", flag.ExitOnError)
	path := fs.String("config", config.DefaultPath(), "bridge config file")
	host := fs.String("host", "", "the new node's address (default: look for nodes on this network)")
	port := fs.Int("port", 0, "the new node's TCP port (default: the config's, usually 5000)")
	yes := fs.Bool("yes", false, "don't ask for confirmation")
	controlAddr := fs.String("control", control.DefaultAddr, "the bridge's control API address")
	_ = fs.Parse(args)

	cfg, err := loadBridgeConfig(*path)
	if err != nil {
		return err
	}
	if cfg.Token == "" {
		return fmt.Errorf("this bridge isn't paired with SIERRA yet; pair it with the new node: %s", setupHint)
	}
	dir := filepath.Dir(*path)
	if err := checkWritable(dir); err != nil {
		if errors.Is(err, iofs.ErrPermission) {
			return fmt.Errorf("can't change the bridge's config in %s: %s", dir, elevateHint)
		}
		return fmt.Errorf("can't change the bridge's config in %s: %w", dir, err)
	}
	p := cfg.Node.TCPPort
	if *port > 0 {
		p = *port
	}
	if p == 0 {
		p = 5000
	}
	ctx := context.Background()
	interactive := isTerminal(os.Stdin)
	in := bufio.NewReader(os.Stdin)

	// What the running bridge has now, to show the change (best effort).
	var now control.NodeDoc
	if tok, err := control.ReadToken(filepath.Join(dir, control.TokenFile)); err == nil {
		if d, err := (&control.Client{Addr: *controlAddr, Token: tok}).Do(ctx, "GET", "/status", nil); err == nil {
			now = d.Node
		}
	}
	current := cfg.Node.ExpectedPubkey

	var found []nodelink.Found
	if *host != "" {
		fmt.Printf("Checking the node at %s…\n", net.JoinHostPort(*host, strconv.Itoa(p)))
		id, detail, err := identifyTCP(*host, p)
		if err != nil {
			return fmt.Errorf("couldn't identify a node at %s: %w", net.JoinHostPort(*host, strconv.Itoa(p)), err)
		}
		found = []nodelink.Found{{Addr: detail, Identity: id}}
	} else {
		fmt.Println("Looking for nodes on this network…")
		found = (&nodelink.Discover{Port: p}).Scan(ctx)
	}
	pick, err := pickNode(found, current, now, interactive, in)
	if err != nil {
		return err
	}
	if pick.Pubkey == current {
		if len(found) == 1 && *host == "" {
			return fmt.Errorf("the only node found is the one this bridge already uses, %s. Check the new node is on, "+
				"with its WiFi on this network, or give its address with -host", nodeWithKey(pick.Name, pick.Pubkey))
		}
		fmt.Printf("This bridge already uses %s. Nothing to change.\n", nodeWithKey(pick.Name, pick.Pubkey))
		return nil
	}

	from := "no node yet"
	if current != "" {
		name := ""
		if now.Pubkey == current {
			name = now.Name
		}
		from = nodeWithKey(name, current)
	}
	fmt.Printf("\nSwitch gateway %q\n  from %s\n  to   %s · %s (%s)\n", cfg.Gateway, from,
		nodeWithKey(pick.Name, pick.Pubkey), pick.Model, pick.Addr)
	if !*yes {
		if !interactive {
			return errors.New("not switching without confirmation: run it again with -yes")
		}
		fmt.Print("\nSIERRA will use the new node for this gateway from now on. Switch? [y/N]: ")
		line, _ := in.ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			fmt.Println("Not changed.")
			return nil
		}
	}

	// SIERRA first: if it can't be told, nothing changes.
	c := &admin.Client{APIBase: config.HTTPBase(cfg.Relay), Token: cfg.Token}
	repinned, err := c.SetNode(ctx, pick.Pubkey)
	needsAdmin := errors.Is(err, admin.ErrNotSupported)
	switch {
	case needsAdmin:
	case isUnauthorized(err):
		return fmt.Errorf("the relay didn't accept this bridge's token: SIERRA may have removed or re-paired gateway %q. "+
			"Ask your SIERRA contact for an invite, then run: %s -repair", cfg.Gateway, setupHint)
	case err != nil:
		return fmt.Errorf("couldn't tell SIERRA about the new node, so nothing was changed: %w", err)
	}

	cfg.Node.ExpectedPubkey = pick.Pubkey
	if *host != "" || cfg.Node.TCPHost != "" {
		// A configured address was the old node's: use the new one's.
		if h, ps, err := net.SplitHostPort(pick.Addr); err == nil {
			cfg.Node.TCPHost, cfg.Node.Link = h, config.LinkTCP
			cfg.Node.TCPPort, _ = strconv.Atoi(ps)
		}
	}
	if err := config.Save(*path, cfg); err != nil {
		if repinned && current != "" {
			_, _ = c.SetNode(ctx, current)
		}
		return fmt.Errorf("couldn't save the config to %s, so nothing was changed: %w", *path, err)
	}
	// Start from the new node's address, not the old one's.
	if err := state.New(filepath.Join(dir, "state.json")).Update(func(s *state.State) { s.NodeAddr = pick.Addr }); err != nil {
		fmt.Printf("(couldn't update the bridge's remembered node address: %v; it will look for the node instead)\n", err)
	}
	fmt.Printf("\nSaved: this bridge now uses %s.\n", nodeWithKey(pick.Name, pick.Pubkey))

	err = svc.Restart()
	switch {
	case errors.Is(err, svc.ErrNotInstalled):
		fmt.Printf("Restart the bridge so it uses the new node: stop it, then start it again (%s).\n", runHint)
	case err != nil:
		return fmt.Errorf("saved, but couldn't restart the bridge's service (%w); restart it yourself so it uses the new node", err)
	default:
		fmt.Println("Restarted the bridge; waiting for it to connect to the new node…")
		if d, ok := waitForNewNode(dir, *controlAddr, pick.Pubkey); ok {
			printStatus(d)
		} else {
			fmt.Println("It hasn't connected to the new node yet; it keeps trying. Check with: meshbridge status")
		}
	}
	if needsAdmin {
		fmt.Printf("\nSIERRA's relay doesn't take node changes from bridges yet, so it will hold this gateway until a\n"+
			"SIERRA admin accepts the new node. Ask your SIERRA contact to run:\n  meshbridge admin accept-node %s\n", cfg.Gateway)
	}
	return nil
}

// pickNode chooses the node to switch to from the ones found. current is
// the configured node's pubkey; now is what the running bridge reports
// (its node is usually busy with the bridge, so a scan can't see it).
func pickNode(found []nodelink.Found, current string, now control.NodeDoc, interactive bool, in *bufio.Reader) (nodelink.Found, error) {
	if len(found) == 0 {
		return nodelink.Found{}, errors.New("no node found on this network. Check the new node is on, with its WiFi on this " +
			"network, or give its address with -host")
	}
	var others []nodelink.Found
	for _, f := range found {
		if f.Pubkey != current {
			others = append(others, f)
		}
	}
	if len(found) == 1 {
		return found[0], nil
	}
	if !interactive {
		if len(others) == 1 {
			return others[0], nil
		}
		var b strings.Builder
		for _, f := range found {
			fmt.Fprintf(&b, "\n  %s · %s (%s)", nodeWithKey(f.Name, f.Pubkey), f.Model, f.Addr)
		}
		return nodelink.Found{}, fmt.Errorf("found %d nodes; give the new one's address with -host:%s", len(found), b.String())
	}
	if now.State == meshproto.NodeConnected && now.Pubkey == current {
		fmt.Printf("This bridge uses %s now.\n", nodeWithKey(now.Name, now.Pubkey))
	}
	fmt.Println("Found:")
	for i, f := range found {
		mark := ""
		if f.Pubkey == current {
			mark = "  (this bridge's node now)"
		}
		fmt.Printf("  %d. %s · %s (%s)%s\n", i+1, nodeWithKey(f.Name, f.Pubkey), f.Model, f.Addr, mark)
	}
	for {
		fmt.Printf("Which node should this bridge use? [1-%d]: ", len(found))
		line, err := in.ReadString('\n')
		var n int
		if _, serr := fmt.Sscanf(strings.TrimSpace(line), "%d", &n); serr == nil && n >= 1 && n <= len(found) {
			return found[n-1], nil
		}
		if err != nil {
			return nodelink.Found{}, errors.New("no node chosen")
		}
	}
}

// nodeWithKey names a node: "Name (abcdef123456…)".
func nodeWithKey(name, pubkey string) string {
	short := pubkey
	if len(short) > 12 {
		short = short[:12] + "…"
	}
	if name == "" {
		return short
	}
	return name + " (" + short + ")"
}

// waitForNewNode polls the restarted bridge until it reports the node with
// pubkey connected, or changeNodeWait passes. Errors (the bridge still
// starting) are retried.
func waitForNewNode(dir, addr, pubkey string) (control.StatusDoc, bool) {
	deadline := time.Now().Add(changeNodeWait)
	var last control.StatusDoc
	for time.Now().Before(deadline) {
		time.Sleep(time.Second)
		tok, err := control.ReadToken(filepath.Join(dir, control.TokenFile))
		if err != nil {
			continue
		}
		d, err := (&control.Client{Addr: addr, Token: tok}).Do(context.Background(), "GET", "/status", nil)
		if err != nil {
			continue
		}
		last = d
		if d.Node.State == meshproto.NodeConnected && d.Node.Pubkey == pubkey {
			return d, true
		}
	}
	return last, false
}
