package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/sierragridteam/sierra-meshrelay/internal/admin"
	"github.com/sierragridteam/sierra-meshrelay/internal/config"
	"github.com/sierragridteam/sierra-meshrelay/meshproto"
)

const adminUsage = `meshbridge admin: manage SIERRA gateways, pairing, bridge updates and shared repeaters
(needs an admin token).

Usage:
  meshbridge admin pairings                      Pending pairing codes
  meshbridge admin approve <code> [gateway]      Approve a code (default name: the node's)
        -repair                                  re-pair an existing gateway (rotates its token)
  meshbridge admin deny <code>                   Refuse a code
  meshbridge admin invite [gateway]              Create a passphrase invite and print a message to send
        -uses N -prefix NAME -days N -node PUBKEY -note TEXT
  meshbridge admin invites                       List invites
  meshbridge admin revoke <invite-id>            Revoke an invite
  meshbridge admin gateways                      Gateways and their status
  meshbridge admin remove <gateway> [-yes]       Delete a gateway (revokes its token, disconnects its bridge)
  meshbridge admin logs <gateway> [-n 50] [-f]   A bridge's recent log lines and connects/disconnects
  meshbridge admin reconnect <gateway> [-yes]    Have a bridge look for its node now (ends any session using it)
  meshbridge admin accept-node <gateway> [-yes]  Accept the node a gateway's bridge reports when it isn't the one
                                                 on record (a host can switch nodes themselves: meshbridge change-node)
  meshbridge admin access grant <gateway> <label> [-expires 180d] [-note text]
                                                 Issue a token that opens one gateway (meshbridge connect),
                                                 named <gateway>.<label>; prints it once, with instructions
  meshbridge admin access list [gateway]         Access tokens, for every gateway or one
  meshbridge admin access remove <gateway>.<label> [-yes]
                                                 Revoke an access token (ends any session using it)
  meshbridge admin clients                       Every token: admin and access, expiry, last use
  meshbridge admin client add <name> [-expires 90d] [-note text]
                                                 Issue an admin token (for another admin or a tool)
  meshbridge admin client set <name> -role admin Turn a token with a retired role (operator, agent) into an admin token
  meshbridge admin client remove <name> [-yes]   Revoke a token
  meshbridge admin update-bridges [vX.Y.Z|off] [-gateway a,b]
                                                 Show the release offered to bridges; with a version, offer it
                                                 (bridges install it when idle; never a downgrade)
  meshbridge admin repeaters                     Shared repeaters and their last reading
  meshbridge admin repeater add <gateway> [name or pubkey]
                                                 Share a repeater that <gateway>'s node has heard (any
                                                 host's gateway; recorded in its logs). Pick from its list,
                                                 or name one. -yes skips the confirmation
  meshbridge admin repeater add <pubkey> <gateway>
                                                 Share a repeater by pubkey, checking its password through <gateway>
  meshbridge admin repeater remove <name or pubkey>
                                                 Stop sharing a repeater (deletes its password)

These work from any computer. The admin token comes from
$MESHBRIDGE_ADMIN_TOKEN, or from ` + "~/.config/meshbridge/admin.token" + ` (chmod 600;
-token-file picks another). Every command takes -relay.
`

func cmdAdmin(args []string) (err error) {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(adminUsage)
		return nil
	}
	sub, args := args[0], args[1:]
	if !slices.Contains(adminCommands, sub) {
		return fmt.Errorf("unknown admin command %q\n\n%s", sub, adminUsage)
	}
	fs := flag.NewFlagSet("admin "+sub, flag.ExitOnError)
	relayURL := fs.String("relay", config.DefaultRelay, "relay URL")
	tokenFile := fs.String("token-file", admin.DefaultTokenFile(), "admin token file (if $"+admin.TokenEnv+" is unset)")
	repair := fs.Bool("repair", false, "approve: re-pair an existing gateway (rotates its token)")
	uses := fs.Int("uses", 1, "invite: how many bridges it can set up")
	prefix := fs.String("prefix", "", "invite: name gateways <prefix>-1, -2… (multi-use)")
	days := fs.Float64("days", 7, "invite: days until it expires (max 30)")
	node := fs.String("node", "", "invite: only redeemable by the node with this pubkey")
	note := fs.String("note", "", "invite, access grant, client add: a note for admins")
	yes := fs.Bool("yes", false, "remove, reconnect, accept-node, repeater add, access/client remove: don't ask for confirmation")
	expires := fs.String("expires", "", "access grant, client add: days until the token expires, like 90d (default: 180d for access, never for admin)")
	role := fs.String("role", "", "client set: the new role (admin)")
	lines := fs.Int("n", 50, "logs: how many lines")
	follow := fs.Bool("f", false, "logs: keep printing new lines")
	only := fs.String("gateway", "", "update-bridges: only these gateways (comma-separated)")
	pos := parseInterleaved(fs, args)

	tok, err := admin.LoadToken(*tokenFile)
	if err != nil {
		return err
	}
	c := &admin.Client{APIBase: config.HTTPBase(*relayURL), Token: tok}
	defer func() {
		if isUnauthorized(err) {
			err = fmt.Errorf("the relay at %s didn't accept your admin token (from $%s or %s)", *relayURL, admin.TokenEnv, *tokenFile)
		}
	}()
	ctx := context.Background()
	arg := func(i int) string {
		if i < len(pos) {
			return pos[i]
		}
		return ""
	}

	switch sub {
	case "access", "clients", "client":
		return runAdminClients(ctx, c, sub, pos, adminTokenOpts{expires: *expires, note: *note, yes: *yes, role: *role},
			bufio.NewReader(os.Stdin), isTerminal(os.Stdin), os.Stdout)

	case "pairings":
		ps, err := c.Pairings(ctx)
		if err != nil {
			return err
		}
		if len(ps) == 0 {
			fmt.Println("No pairing codes waiting.")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "CODE\tCOMPUTER\tNODE\tFROM\tAGE\tWOULD BE NAMED")
		for _, p := range ps {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", p.UserCode, dash(p.HostLabel), nodeLabel(p.Node.Name, p.Node.Model),
				p.IP, time.Since(p.CreatedAt).Round(time.Second), p.SuggestedGateway)
		}
		w.Flush()
		fmt.Println("\nApprove with: meshbridge admin approve <code> [gateway]")
		return nil

	case "approve":
		if arg(0) == "" {
			return fmt.Errorf("usage: meshbridge admin approve <code> [gateway]")
		}
		g, err := c.Approve(ctx, arg(0), arg(1), *repair)
		if err != nil {
			return err
		}
		verb := "Approved"
		if *repair {
			verb = "Re-paired"
		}
		fmt.Printf("%s %s as gateway %q. The bridge picks up its token within a few seconds.\n",
			verb, strings.ToUpper(arg(0)), g.Name)
		return nil

	case "deny":
		if arg(0) == "" {
			return fmt.Errorf("usage: meshbridge admin deny <code>")
		}
		if err := c.Deny(ctx, arg(0)); err != nil {
			return err
		}
		fmt.Printf("Denied %s.\n", strings.ToUpper(arg(0)))
		return nil

	case "invite":
		req := admin.InviteRequest{Gateway: arg(0), NamePrefix: *prefix, MaxUses: *uses,
			ExpiresInHours: *days * 24, ExpectedPubkey: *node, Note: *note}
		inv, err := c.CreateInvite(ctx, req)
		if err != nil {
			return err
		}
		name := inv.Invite.Gateway
		switch {
		case name == "" && inv.Invite.NamePrefix != "":
			name = inv.Invite.NamePrefix + "-1, -2…"
		case name == "":
			name = "named after the node"
		}
		fmt.Printf("Invite %s · gateway %s · %d use(s) · expires %s\n", inv.Invite.ID, name, inv.Invite.MaxUses,
			inv.Invite.ExpiresAt.Local().Format("Mon 2 Jan 15:04"))
		fmt.Printf("Passphrase: %s   (shown once)\n", strings.ReplaceAll(inv.Passphrase, "-", " "))
		fmt.Printf("Link:       %s\n\n", inv.Link)
		fmt.Println("Message to send:")
		fmt.Println("────────────────────────────────────────")
		fmt.Print(admin.InviteMessage(inv))
		fmt.Println("────────────────────────────────────────")
		return nil

	case "invites":
		invs, err := c.Invites(ctx)
		if err != nil {
			return err
		}
		if len(invs) == 0 {
			fmt.Println("No invites.")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tGATEWAY\tUSES\tEXPIRES\tSTATE\tNOTE")
		for _, i := range invs {
			g := i.Gateway
			switch {
			case g == "" && i.NamePrefix != "":
				g = i.NamePrefix + "-N"
			case g == "":
				g = "(from node)"
			}
			state := "active"
			switch {
			case i.Revoked:
				state = "revoked"
			case !i.Active && i.Uses >= i.MaxUses:
				state = "used"
			case !i.Active:
				state = "expired"
			}
			fmt.Fprintf(w, "%s\t%s\t%d/%d\t%s\t%s\t%s\n", i.ID, dash(g), i.Uses, i.MaxUses,
				i.ExpiresAt.Local().Format("Mon 2 Jan"), state, i.Note)
		}
		w.Flush()
		return nil

	case "revoke":
		if arg(0) == "" {
			return fmt.Errorf("usage: meshbridge admin revoke <invite-id>")
		}
		if err := c.RevokeInvite(ctx, arg(0)); err != nil {
			return err
		}
		fmt.Printf("Revoked %s.\n", arg(0))
		return nil

	case "remove", "rm":
		name := arg(0)
		if name == "" {
			return fmt.Errorf("usage: meshbridge admin remove <gateway>")
		}
		gs, err := c.Gateways(ctx)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(gs, func(g admin.Gateway) bool { return g.Name == name }) {
			return fmt.Errorf("no gateway named %q (see meshbridge admin gateways)", name)
		}
		// Repeaters it shared stay shared, but only an admin can remove them now.
		var shared []string
		if rs, err := c.Repeaters(ctx); err == nil {
			for _, r := range rs {
				if r.AddedByGateway == name {
					shared = append(shared, r.Name)
				}
			}
		}
		fmt.Printf("Removing %s revokes its token and disconnects its bridge now. Its connect history and logs are deleted.\n", name)
		fmt.Println("To pair the host again, send it a new invite; it stops the service and runs: meshbridge setup -repair")
		if len(shared) > 0 {
			fmt.Printf("It shared %s. They stay shared; only an admin can remove them after this.\n", strings.Join(shared, ", "))
		}
		if !*yes {
			if !isTerminal(os.Stdin) {
				return fmt.Errorf("pass -yes to remove %s without a prompt", name)
			}
			fmt.Printf("Type the gateway name to confirm: ")
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			if strings.TrimSpace(line) != name {
				return fmt.Errorf("not removed")
			}
		}
		if err := c.RemoveGateway(ctx, name); err != nil {
			return err
		}
		fmt.Printf("Removed %s.\n", name)
		return nil

	case "gateways":
		gs, err := c.Gateways(ctx)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "GATEWAY\tBRIDGE\tNODE\tVERSION\tLAST SEEN\tIN USE")
		for _, g := range gs {
			bridge := "offline"
			if g.Online {
				bridge = "online"
			}
			node := "-"
			if g.Node != nil {
				node = g.Node.State
				if g.Node.Name != "" {
					node += " (" + g.Node.Name + ")"
				}
			}
			if g.PubkeyStatus == "mismatch" {
				node += " ⚠ pubkey mismatch"
			}
			version := "-"
			if g.Agent != nil && g.Agent.Version != "" {
				version = g.Agent.Version + " " + g.Agent.OS
			}
			seen := "-"
			if g.LastSeen != nil {
				seen = ago(*g.LastSeen)
			}
			inUse := ""
			if g.Lease != nil {
				inUse = g.Lease.Client
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", g.Name, bridge, node, version, seen, inUse)
		}
		w.Flush()
		for _, g := range gs {
			if g.PubkeyStatus == "mismatch" {
				fmt.Printf("\n%s's bridge reports a node that isn't the one on record, so SIERRA can't use it. If its host\n"+
					"replaced the node on purpose, they can run meshbridge change-node, or accept it from here:\n"+
					"  meshbridge admin accept-node %s\n", g.Name, g.Name)
			}
		}
		return nil

	case "update-bridges":
		if arg(0) == "" {
			u, err := c.GetBridgeUpdate(ctx)
			if err != nil {
				return err
			}
			// A query: say so, so it doesn't read as the start of a rollout.
			switch {
			case u.Version == "":
				fmt.Println("Currently not offering bridges an update.")
			case len(u.Gateways) > 0:
				fmt.Printf("Currently offering %s to %s.\n", u.Version, strings.Join(u.Gateways, ", "))
			default:
				fmt.Printf("Currently offering %s to every bridge.\n", u.Version)
			}
			fmt.Println("Nothing changed. To offer a release: meshbridge admin update-bridges vX.Y.Z [-gateway a,b]")
			fmt.Println("(a bridge installs only a release newer than its own; see versions with: meshbridge admin gateways)")
			return nil
		}
		u := admin.BridgeUpdate{Version: arg(0)}
		if u.Version == "off" {
			u.Version = ""
		}
		if *only != "" {
			u.Gateways = strings.Split(*only, ",")
		}
		offered, err := c.SetBridgeUpdate(ctx, u)
		if err != nil {
			return err
		}
		if u.Version == "" {
			fmt.Println("Stopped offering bridges an update.")
			return nil
		}
		fmt.Printf("Offering %s. Sent now to: %s\n", u.Version, dashList(offered))
		fmt.Println("Each bridge verifies the release's signature, waits until nobody is using its node, then")
		fmt.Println("installs it (bridges installed before v0.7.0 need the installer run once). Watch with: meshbridge admin gateways")
		return nil

	case "accept-node":
		name := arg(0)
		if name == "" {
			return fmt.Errorf("usage: meshbridge admin accept-node <gateway> [-yes]")
		}
		g, err := findGateway(ctx, c, name)
		if err != nil {
			return err
		}
		if g.PubkeyStatus != "mismatch" || g.MismatchPubkey == "" {
			return fmt.Errorf("%s's bridge reports the node on record (%s); there's nothing to accept", name, shortPubkey(g.PinnedPubkey))
		}
		reported := shortPubkey(g.MismatchPubkey)
		if g.Node != nil && g.Node.Pubkey == g.MismatchPubkey && g.Node.Name != "" {
			reported = nodeLabel(g.Node.Name, g.Node.Model) + " (" + reported + ")"
		}
		fmt.Printf("Gateway %s\n  on record:      %s\n  bridge reports: %s\n", name, shortPubkey(g.PinnedPubkey), reported)
		fmt.Println("Accepting makes the reported node the gateway's node; SIERRA can use it again straight away.")
		if !*yes {
			if !isTerminal(os.Stdin) {
				return fmt.Errorf("pass -yes to accept %s's node without a prompt", name)
			}
			fmt.Print("Accept it? [y/N]: ")
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
				return fmt.Errorf("not accepted")
			}
		}
		if err := c.AcceptPubkey(ctx, name); err != nil {
			return err
		}
		fmt.Printf("Accepted: %s now uses %s.\n", name, reported)
		return nil

	case "reconnect":
		name := arg(0)
		if name == "" {
			return fmt.Errorf("usage: meshbridge admin reconnect <gateway> [-yes]")
		}
		g, err := findGateway(ctx, c, name)
		if err != nil {
			return err
		}
		if !g.Online {
			return fmt.Errorf("%s's bridge is offline, so it can't be asked to reconnect. Its host needs to restart it", name)
		}
		if g.Lease != nil && !*yes {
			return fmt.Errorf("%s is in use by %s; reconnecting ends that session. Use -yes to do it anyway", name, g.Lease.Client)
		}
		wasConnected := g.Node != nil && g.Node.State == meshproto.NodeConnected
		if err := c.Relink(ctx, name); err != nil {
			return err
		}
		fmt.Printf("Asked %s to look for its node now…\n", name)
		err = waitForNode(wasConnected, 2*time.Second, 30*time.Second, func() (string, error) {
			var err error
			g, err = findGateway(ctx, c, name)
			if g.Node == nil {
				return "", err
			}
			return g.Node.State, err
		})
		if err != nil {
			return err
		}
		state := "unknown"
		if g.Node != nil {
			state = g.Node.State
			if g.Node.Name != "" {
				state += " (" + g.Node.Name + ")"
			}
		}
		fmt.Printf("Node: %s\n", state)
		if g.Node == nil || g.Node.State != meshproto.NodeConnected {
			fmt.Printf("The bridge keeps trying on its own. See why with: meshbridge admin logs %s\n", name)
		}
		return nil

	case "logs":
		if arg(0) == "" {
			return fmt.Errorf("usage: meshbridge admin logs <gateway> [-n 50] [-f]")
		}
		return printLogs(ctx, c, arg(0), *lines, *follow)

	case "repeaters":
		rs, err := c.Repeaters(ctx)
		if err != nil {
			return err
		}
		if len(rs) == 0 {
			fmt.Println("No shared repeaters.")
			return nil
		}
		printRepeaters(rs, true)
		return nil

	case "repeater":
		switch arg(0) {
		case "add":
			if arg(1) == "" {
				return fmt.Errorf("usage: meshbridge admin repeater add <gateway> [name or pubkey]\n" +
					"   or: meshbridge admin repeater add <pubkey> <gateway>")
			}
			in := bufio.NewReader(os.Stdin)
			pubkey, gateway := arg(1), arg(2)
			if !isPubkey(pubkey) || gateway == "" {
				// add <gateway> [name or pubkey]: pick from what its node has heard.
				gateway = arg(1)
				fmt.Printf("Asking %s's node which repeaters it has heard…\n", gateway)
				near, err := c.GatewayNearbyRepeaters(ctx, gateway)
				if err != nil {
					return err
				}
				pick, err := pickNearby(near, strings.Join(pos[2:], " "), in, isTerminal(os.Stdin),
					gateway+"'s node", "meshbridge admin repeater add "+gateway)
				if err != nil {
					return err
				}
				if pick.Shared {
					// Sharing again would replace the password and make it an
					// admin's, taking it out of its host's list.
					return fmt.Errorf("%s is already shared (see meshbridge admin repeaters)", pick.Name)
				}
				fmt.Printf("\nSharing %s (%s…) through %s. The check logs in over the mesh from %s's node,\n", pick.Name, pick.Pubkey[:12], gateway, gateway)
				fmt.Println("and it's recorded in that gateway's logs. Its host won't see it in `meshbridge repeater list`.")
				if !*yes {
					if !isTerminal(os.Stdin) {
						return fmt.Errorf("pass -yes to share %s without a prompt", pick.Name)
					}
					fmt.Print("Share it? [y/N]: ")
					line, _ := in.ReadString('\n')
					if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
						fmt.Println("Not shared.")
						return nil
					}
				}
				pubkey = pick.Pubkey
			}
			pw, err := readPassword(in, "Repeater password (guest if it has one; Enter for none): ")
			if err != nil {
				return err
			}
			fmt.Printf("Checking the password through %s's node, over the mesh. This usually takes under a minute,\nand gives up after 5.\n", gateway)
			res, err := c.AddRepeater(ctx, pubkey, gateway, pw, printStep)
			if err != nil {
				return err
			}
			fmt.Printf("Shared %s (logged in as %s through %s).\n", res.Repeater.Name, res.Check.Access, res.Check.Gateway)
			if res.Check.Access == "admin" {
				fmt.Println("That was the admin password. SIERRA only reads status, so a guest password is enough.")
			}
			return nil
		case "remove", "rm":
			if arg(1) == "" {
				return fmt.Errorf("usage: meshbridge admin repeater remove <name or pubkey>")
			}
			rs, err := c.Repeaters(ctx)
			if err != nil {
				return err
			}
			r, err := pickShared(rs, arg(1), "meshbridge admin repeaters")
			if err != nil {
				return err
			}
			if err := c.RemoveRepeater(ctx, r.Pubkey); err != nil {
				return err
			}
			fmt.Printf("Stopped sharing %s and deleted its password.\n", r.Name)
			return nil
		}
		return fmt.Errorf("usage: meshbridge admin repeater add|remove …")
	}
	return fmt.Errorf("unknown admin command %q\n\n%s", sub, adminUsage)
}

// findGateway returns the named gateway's current status.
func findGateway(ctx context.Context, c *admin.Client, name string) (admin.Gateway, error) {
	gs, err := c.Gateways(ctx)
	if err != nil {
		return admin.Gateway{}, err
	}
	for _, g := range gs {
		if g.Name == name {
			return g, nil
		}
	}
	return admin.Gateway{}, fmt.Errorf("no gateway named %q (see meshbridge admin gateways)", name)
}

// adminCommands are the admin subcommands, checked before the token is
// loaded so a typo isn't reported as a missing token.
var adminCommands = []string{"pairings", "approve", "deny", "invite", "invites", "revoke", "remove", "rm",
	"gateways", "update-bridges", "logs", "reconnect", "accept-node", "repeaters", "repeater", "access", "clients", "client"}

// isPubkey reports whether s is a full 64-hex-character pubkey.
func isPubkey(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range strings.ToLower(s) {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

// parseInterleaved parses flags that may come before or after positional
// arguments (`approve K7M-4QX cabin -repair`), returning the positionals.
func parseInterleaved(fs *flag.FlagSet, args []string) []string {
	var pos []string
	for {
		_ = fs.Parse(args)
		args = fs.Args()
		if len(args) == 0 {
			return pos
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// shortPubkey abbreviates a pubkey for display.
func shortPubkey(pk string) string {
	if pk == "" {
		return "none"
	}
	if len(pk) > 12 {
		return pk[:12] + "…"
	}
	return pk
}

func nodeLabel(name, model string) string {
	switch {
	case name == "":
		return "(not found yet)"
	case model != "":
		return name + " · " + model
	}
	return name
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

// printLogs prints a gateway's log lines merged with the relay's events for
// it, oldest first; with follow, it then polls for new lines.
func printLogs(ctx context.Context, c *admin.Client, gw string, n int, follow bool) error {
	logs, events, err := c.Logs(ctx, gw, 0, n)
	if err != nil {
		return err
	}
	type line struct {
		at   time.Time
		text string
	}
	var all []line
	for _, l := range logs {
		all = append(all, line{logTime(l), formatLog(l)})
	}
	oldest := time.Time{}
	if len(logs) > 0 && len(logs) >= n {
		oldest = logTime(logs[0])
	}
	for _, e := range events {
		if e.At.Before(oldest) {
			continue
		}
		text := fmt.Sprintf("%s RELAY %s", e.At.Local().Format("Jan _2 15:04:05"), e.Kind)
		if e.Detail != "" {
			text += " " + e.Detail
		}
		all = append(all, line{e.At, text})
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
	if len(all) == 0 {
		fmt.Printf("Nothing from %s yet. Bridges send their logs from v0.7.0 on, once connected.\n", gw)
	}
	for _, l := range all {
		fmt.Println(l.text)
	}
	if !follow {
		return nil
	}
	var last int64
	if len(logs) > 0 {
		last = logs[len(logs)-1].ID
	}
	for {
		time.Sleep(3 * time.Second)
		more, _, err := c.Logs(ctx, gw, last, 500)
		if err != nil {
			return err
		}
		for _, l := range more {
			fmt.Println(formatLog(l))
			last = l.ID
		}
	}
}

func logTime(l admin.BridgeLog) time.Time {
	if t, err := time.Parse(time.RFC3339Nano, l.T); err == nil {
		return t
	}
	return l.ReceivedAt
}

func formatLog(l admin.BridgeLog) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %-5s %s", logTime(l).Local().Format("Jan _2 15:04:05"), l.Level, l.Msg)
	keys := make([]string, 0, len(l.Attrs))
	for k := range l.Attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := l.Attrs[k]
		if strings.ContainsAny(v, " \t\"") {
			v = strconv.Quote(v)
		}
		fmt.Fprintf(&b, " %s=%s", k, v)
	}
	return b.String()
}

func dashList(s []string) string {
	if len(s) == 0 {
		return "(none online that need it)"
	}
	return strings.Join(s, ", ")
}
