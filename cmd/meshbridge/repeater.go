package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/sierragridteam/sierra-meshrelay/internal/admin"
	"github.com/sierragridteam/sierra-meshrelay/internal/config"
)

const repeaterUsage = `meshbridge repeater: share your MeshCore repeaters with SIERRA.

SIERRA logs in to a shared repeater over the mesh to read its status
(battery, airtime, packet counts) and report it on the Grid.

Usage:
  meshbridge repeater add [name or pubkey]   Pick a repeater your node knows (part of a name narrows the list) and share it
  meshbridge repeater list                   Repeaters this bridge has shared
  meshbridge repeater remove <name or pubkey>
                                             Stop sharing one (deletes its password from SIERRA)

Run them as root (sudo) or an administrator: they use this bridge's
credentials from its config. -config picks another config file.
`

const repeaterWarning = `Before you share it:

  • SIERRA stores the password (encrypted) and uses it to log in and read
    the repeater's status.
  • Any SIERRA gateway in range of the repeater may be used to log in to
    it, not just yours.
  • Use the repeater's guest password if it has one: a guest can only read
    status. An admin password works too, but gives full control.
  • You can stop sharing it at any time with: meshbridge repeater remove
`

func cmdRepeater(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(repeaterUsage)
		return nil
	}
	sub, args := args[0], args[1:]
	fs := flag.NewFlagSet("repeater "+sub, flag.ExitOnError)
	path := fs.String("config", config.DefaultPath(), "bridge config file")
	yes := fs.Bool("yes", false, "add: don't ask for confirmation")
	pos := parseInterleaved(fs, args)
	cfg, err := config.Load(*path)
	if err != nil {
		return fmt.Errorf("%w\nRun this as root (sudo) or an administrator on the bridge's computer", err)
	}
	c := &admin.Client{APIBase: config.HTTPBase(cfg.Relay), Token: cfg.Token}
	ctx := context.Background()
	in := bufio.NewReader(os.Stdin)

	switch sub {
	case "list", "ls":
		rs, err := c.MyRepeaters(ctx)
		if err != nil {
			return err
		}
		if len(rs) == 0 {
			fmt.Println("This bridge hasn't shared any repeaters. Share one with: meshbridge repeater add")
			return nil
		}
		printRepeaters(rs, false)
		return nil

	case "remove", "rm":
		if len(pos) == 0 {
			return fmt.Errorf("usage: meshbridge repeater remove <name or pubkey>")
		}
		rs, err := c.MyRepeaters(ctx)
		if err != nil {
			return err
		}
		r, err := pickShared(rs, pos[0])
		if err != nil {
			return err
		}
		if err := c.UnshareRepeater(ctx, r.Pubkey); err != nil {
			return err
		}
		fmt.Printf("Stopped sharing %s. SIERRA no longer has its password.\n", r.Name)
		return nil

	case "add":
		fmt.Println("Asking your node which repeaters it has heard…")
		near, err := c.NearbyRepeaters(ctx)
		if err != nil {
			return err
		}
		pick, err := pickNearby(near, strings.Join(pos, " "), in, isTerminal(os.Stdin))
		if err != nil {
			return err
		}
		fmt.Printf("\nSharing %s (%s…)\n\n", pick.Name, pick.Pubkey[:12])
		fmt.Print(repeaterWarning)
		if !*yes {
			fmt.Print("\nShare it? [y/N]: ")
			line, _ := in.ReadString('\n')
			if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
				fmt.Println("Not shared.")
				return nil
			}
		}
		pw, err := readPassword(in, "Repeater password (guest if it has one; Enter for none): ")
		if err != nil {
			return err
		}
		fmt.Println("Logging in to the repeater over the mesh (this can take up to a minute)…")
		res, err := c.ShareRepeater(ctx, pick.Pubkey, pick.Name, pw)
		if err != nil {
			return err
		}
		fmt.Printf("\nShared %s with SIERRA. Logged in as %s", res.Repeater.Name, res.Check.Access)
		if st := res.Check.Status; st != nil && st.BatteryMilliVolts != nil {
			fmt.Printf("; battery %.2f V", float64(*st.BatteryMilliVolts)/1000)
		}
		fmt.Println(".")
		if res.Check.Access == "admin" {
			fmt.Println("\nThat was the admin password. SIERRA only reads status, so a guest password is")
			fmt.Println("enough: set one on the repeater (set guest.password …), then run this again with it.")
		}
		return nil
	}
	return fmt.Errorf("unknown repeater command %q\n\n%s", sub, repeaterUsage)
}

// pickNearby chooses a repeater. want (from the command line) picks one by
// exact name or pubkey prefix, or narrows the list to names containing it.
// Otherwise it lists them by name and asks: a number picks one, anything else
// narrows the list again.
func pickNearby(near []admin.NearbyRepeater, want string, in *bufio.Reader, interactive bool) (admin.NearbyRepeater, error) {
	if len(near) == 0 {
		return admin.NearbyRepeater{}, fmt.Errorf("your node hasn't heard any repeaters yet. A repeater shows up once your node hears its advert; " +
			"send a flood advert from the repeater (or wait for its next one) and try again")
	}
	all := slices.Clone(near)
	slices.SortStableFunc(all, func(a, b admin.NearbyRepeater) int {
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	shown := all
	if want != "" {
		var exact []admin.NearbyRepeater
		for _, r := range all {
			if strings.EqualFold(r.Name, want) || strings.HasPrefix(r.Pubkey, strings.ToLower(want)) {
				exact = append(exact, r)
			}
		}
		if len(exact) == 1 {
			return exact[0], nil
		}
		if shown = exact; len(shown) == 0 {
			shown = matchNearby(all, want)
		}
		switch len(shown) {
		case 0:
			return admin.NearbyRepeater{}, fmt.Errorf("your node doesn't know a repeater whose name contains %q", want)
		case 1:
			return shown[0], nil
		}
	}
	for {
		if len(shown) == len(all) {
			fmt.Printf("\nRepeaters your node has heard (%d):\n", len(all))
		} else {
			fmt.Printf("\nRepeaters matching (%d of %d):\n", len(shown), len(all))
		}
		printNearby(shown)
		if !interactive {
			return admin.NearbyRepeater{}, fmt.Errorf("name the repeater: meshbridge repeater add <name, part of a name, or pubkey>")
		}
		for {
			fmt.Printf("Number [1-%d], or part of a name to search: ", len(shown))
			line, err := in.ReadString('\n')
			line = strings.TrimSpace(line)
			if n, perr := strconv.Atoi(line); perr == nil && n >= 1 && n <= len(shown) {
				return shown[n-1], nil
			}
			if line != "" {
				if hits := matchNearby(all, line); len(hits) > 0 {
					shown = hits
					break
				}
				fmt.Printf("No repeater's name contains %q.\n", line)
			}
			if err != nil {
				return admin.NearbyRepeater{}, fmt.Errorf("no repeater chosen")
			}
		}
	}
}

// matchNearby keeps the repeaters whose name contains q, ignoring case.
func matchNearby(rs []admin.NearbyRepeater, q string) []admin.NearbyRepeater {
	q = strings.ToLower(q)
	var out []admin.NearbyRepeater
	for _, r := range rs {
		if strings.Contains(strings.ToLower(r.Name), q) {
			out = append(out, r)
		}
	}
	return out
}

func printNearby(rs []admin.NearbyRepeater) {
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	for i, r := range rs {
		heard, shared := "-", ""
		if r.LastAdvert != nil {
			heard = ago(*r.LastAdvert)
		}
		if r.Shared {
			shared = "(already shared)"
		}
		fmt.Fprintf(w, "  %d.\t%s\t%s…\tadvert %s\t%s\n", i+1, r.Name, r.Pubkey[:12], heard, shared)
	}
	w.Flush()
}

func pickShared(rs []admin.Repeater, want string) (admin.Repeater, error) {
	var hits []admin.Repeater
	for _, r := range rs {
		if strings.EqualFold(r.Name, want) || strings.HasPrefix(r.Pubkey, strings.ToLower(want)) {
			hits = append(hits, r)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return admin.Repeater{}, fmt.Errorf("no shared repeater matches %q", want)
	}
	return admin.Repeater{}, fmt.Errorf("%q matches %d repeaters; use more of the pubkey", want, len(hits))
}

// readPassword reads a password without echoing it when stdin is a terminal,
// or from $MESHBRIDGE_REPEATER_PASSWORD (for scripts: never a flag, which
// other users could see in the process list).
func readPassword(in *bufio.Reader, prompt string) (string, error) {
	if pw, ok := os.LookupEnv("MESHBRIDGE_REPEATER_PASSWORD"); ok {
		return pw, nil
	}
	fmt.Print(prompt)
	if isTerminal(os.Stdin) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		return strings.TrimRight(string(b), "\r\n"), err
	}
	line, err := in.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("no password given")
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func printRepeaters(rs []admin.Repeater, withOwner bool) {
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	hdr := "REPEATER\tPUBKEY\tACCESS\tLAST READ\tVIA\tPOLLING\tPROBLEM"
	if withOwner {
		hdr += "\tSHARED BY"
	}
	fmt.Fprintln(w, hdr)
	for _, r := range rs {
		read := "never"
		if r.LastOKAt != nil {
			read = ago(*r.LastOKAt)
		}
		problem := ""
		if r.LastError != "" && (r.LastOKAt == nil || (r.LastAttemptAt != nil && r.LastAttemptAt.After(*r.LastOKAt))) {
			problem = r.LastError
		}
		if r.Poll != nil && r.Poll.WaitingSince != nil && r.Poll.Waiting != "" {
			problem = r.Poll.Waiting
		}
		line := fmt.Sprintf("%s\t%s…\t%s\t%s\t%s\t%s\t%s", r.Name, r.Pubkey[:12], r.Access, read, dash(r.LastGateway),
			pollingState(r), problem)
		if withOwner {
			by := r.AddedByGateway
			if by == "" {
				by = r.AddedByClient + " (admin)"
			}
			line += "\t" + by
		}
		fmt.Fprintln(w, line)
	}
	w.Flush()
	// How each gateway's checks went, for repeaters that have any.
	for _, r := range rs {
		if len(r.Links) == 0 {
			continue
		}
		fmt.Printf("\n%s, last 24h:\n", r.Name)
		lw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		for _, l := range r.Links {
			median := "-"
			if l.MedianMS > 0 {
				median = fmt.Sprintf("%.1fs", float64(l.MedianMS)/1000)
			}
			last := ""
			if l.LastOutcome != "" {
				last = "last try: " + strings.ReplaceAll(l.LastOutcome, "_", " ")
			}
			fmt.Fprintf(lw, "  via %s\t%d/%d ok\t%d flooded\tmedian %s\t%s\n", l.Gateway, l.OK, l.Tries, l.Flooded, median, last)
		}
		lw.Flush()
	}
}

// pollingState says how the poller is treating a repeater.
func pollingState(r admin.Repeater) string {
	p := r.Poll
	switch {
	case p == nil:
		return "-"
	case p.RefusedAt != nil:
		return "stopped (password refused)"
	case p.WaitingSince != nil:
		return "waiting (no gateway since " + since(*p.WaitingSince) + ")"
	case p.HibernatingSince != nil:
		return "daily (unreachable since " + p.HibernatingSince.Local().Format("Jan 2") + ")"
	case p.FailedCycles > 0:
		return fmt.Sprintf("retrying (%d missed)", p.FailedCycles)
	}
	return "normal"
}

// since formats a time for "since …": the clock time today, else the date.
func since(t time.Time) string {
	t = t.Local()
	if y, m, d := t.Date(); time.Now().Year() == y && time.Now().Month() == m && time.Now().Day() == d {
		return t.Format("15:04")
	}
	return t.Format("Jan 2 15:04")
}
