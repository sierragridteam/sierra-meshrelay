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
  meshbridge admin update-bridges [vX.Y.Z|off] [-gateway a,b]
                                                 Show the release offered to bridges; with a version, offer it
                                                 (bridges install it when idle; never a downgrade)
  meshbridge admin repeaters                     Shared repeaters and their last reading
  meshbridge admin repeater add <pubkey> <gateway>
                                                 Share a repeater, checking its password through <gateway>
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
	note := fs.String("note", "", "invite: a note for admins")
	yes := fs.Bool("yes", false, "remove: don't ask for confirmation")
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
			if arg(1) == "" || arg(2) == "" {
				return fmt.Errorf("usage: meshbridge admin repeater add <pubkey> <gateway>")
			}
			pw, err := readPassword(bufio.NewReader(os.Stdin), "Repeater password (guest if it has one; Enter for none): ")
			if err != nil {
				return err
			}
			fmt.Printf("Logging in to the repeater through %s over the mesh (this can take up to a minute)…\n", arg(2))
			res, err := c.AddRepeater(ctx, arg(1), arg(2), pw)
			if err != nil {
				return err
			}
			fmt.Printf("Shared %s (logged in as %s through %s).\n", res.Repeater.Name, res.Check.Access, res.Check.Gateway)
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

// adminCommands are the admin subcommands, checked before the token is
// loaded so a typo isn't reported as a missing token.
var adminCommands = []string{"pairings", "approve", "deny", "invite", "invites", "revoke", "remove", "rm",
	"gateways", "update-bridges", "logs", "repeaters", "repeater"}

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
