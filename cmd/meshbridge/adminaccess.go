package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/sierragridteam/sierra-meshrelay/internal/admin"
)

// adminTokenOpts are the flags the token commands take.
type adminTokenOpts struct {
	expires string // "180d", "30" (days); "" is the relay's default
	note    string
	yes     bool
	role    string
}

// runAdminClients serves `admin clients`, `admin client …` and
// `admin access …`: the relay's client tokens. Host tokens open one
// gateway; admin tokens are for SIERRA admins and their tools.
func runAdminClients(ctx context.Context, c *admin.Client, sub string, pos []string, o adminTokenOpts,
	in *bufio.Reader, interactive bool, out io.Writer) error {
	arg := func(i int) string {
		if i < len(pos) {
			return pos[i]
		}
		return ""
	}
	switch sub {
	case "clients":
		return listClientTokens(ctx, c, "", out)

	case "client":
		switch arg(0) {
		case "add":
			if arg(1) == "" {
				return errors.New("usage: meshbridge admin client add <name> [-expires 90d] [-note text]")
			}
			days, err := parseDays(o.expires)
			if err != nil {
				return err
			}
			t, err := c.IssueAdminToken(ctx, arg(1), days, o.note)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "Issued admin token %s%s.\n\n", t.Client, expiresText(t.ExpiresAt))
			fmt.Fprintf(out, "    %s\n\n", t.Token)
			fmt.Fprintln(out, "It's shown only this once. It can do everything an admin can: keep it in a password manager,")
			fmt.Fprintf(out, "or in $%s on the computer that uses it.\n", admin.TokenEnv)
			return nil
		case "set":
			if arg(1) == "" || o.role != "admin" {
				return errors.New("usage: meshbridge admin client set <name> -role admin\n" +
					"(turns a token with a retired role, operator or agent, into an admin token; " +
					"for a token that opens one gateway, use: meshbridge admin access grant)")
			}
			if err := c.MakeAdmin(ctx, arg(1)); err != nil {
				return err
			}
			fmt.Fprintf(out, "%s is an admin token now.\n", arg(1))
			return nil
		case "remove", "rm":
			return revokeToken(ctx, c, arg(1), "meshbridge admin client remove <name>", o.yes, in, interactive, out)
		}
		return errors.New("usage: meshbridge admin client add|set|remove <name>")

	case "access":
		switch arg(0) {
		case "grant":
			if arg(1) == "" || arg(2) == "" {
				return errors.New("usage: meshbridge admin access grant <gateway> <label> [-expires 180d] [-note text]\n" +
					"(the token is named <gateway>.<label>; a label says whose it is, like jay-laptop)")
			}
			days, err := parseDays(o.expires)
			if err != nil {
				return err
			}
			if _, err := findGateway(ctx, c, arg(1)); err != nil {
				return err
			}
			t, err := c.IssueHostToken(ctx, arg(1), arg(2), days, o.note)
			if err != nil {
				return err
			}
			printGrant(out, t)
			return nil
		case "list", "ls", "":
			return listClientTokens(ctx, c, orAll(arg(1)), out)
		case "remove", "rm", "revoke":
			return revokeToken(ctx, c, arg(1), "meshbridge admin access remove <gateway>.<label>", o.yes, in, interactive, out)
		}
		return errors.New("usage: meshbridge admin access grant|list|remove")
	}
	return fmt.Errorf("unknown admin command %q", sub)
}

func orAll(gateway string) string {
	if gateway == "" {
		return "*"
	}
	return gateway
}

// printGrant shows a new host token and what to send its holder.
func printGrant(out io.Writer, t admin.IssuedToken) {
	fmt.Fprintf(out, "Issued %s: it opens gateway %s only%s.\n\n", t.Client, t.Gateway, expiresText(t.ExpiresAt))
	fmt.Fprintf(out, "    %s\n\n", t.Token)
	fmt.Fprintln(out, "It's shown only this once. Send it privately (not in a group chat), with these instructions:")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "    Windows (PowerShell):")
	fmt.Fprintf(out, "      $env:%s = \"<token>\"\n", clientTokenEnv)
	fmt.Fprintf(out, "      meshbridge connect %s\n", t.Gateway)
	fmt.Fprintln(out, "    macOS / Linux:")
	fmt.Fprintf(out, "      %s=<token> meshbridge connect %s\n", clientTokenEnv, t.Gateway)
	fmt.Fprintln(out, "    Then point a MeshCore app or meshcore-cli at 127.0.0.1, port 5000.")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Revoke it with: meshbridge admin access remove %s\n", t.Client)
}

// listClientTokens prints the tokens: all of them (gateway ""), host
// tokens for any gateway ("*"), or one gateway's host tokens.
func listClientTokens(ctx context.Context, c *admin.Client, gateway string, out io.Writer) error {
	ts, err := c.ClientTokens(ctx)
	if err != nil {
		return err
	}
	if gateway != "" {
		ts = slices.DeleteFunc(ts, func(t admin.ClientToken) bool {
			return t.Role != "host" || (gateway != "*" && t.Gateway != gateway)
		})
	}
	if len(ts) == 0 {
		switch gateway {
		case "":
			fmt.Fprintln(out, "No tokens.")
		case "*":
			fmt.Fprintln(out, "No gateway has access tokens. Issue one with: meshbridge admin access grant <gateway> <label>")
		default:
			fmt.Fprintf(out, "%s has no access tokens. Issue one with: meshbridge admin access grant %s <label>\n", gateway, gateway)
		}
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tROLE\tOPENS\tEXPIRES\tLAST USED\tISSUED BY\tNOTE")
	var retired []string
	for _, t := range ts {
		opens := "every gateway"
		switch t.Role {
		case "host":
			opens = t.Gateway
		case "operator", "agent":
			opens = "retired role"
			retired = append(retired, t.Name)
		}
		role, by := t.Role, dash(t.CreatedBy)
		if t.Bootstrap {
			by = "config"
		}
		used := "-"
		if !t.LastUsedAt.IsZero() {
			used = ago(t.LastUsedAt)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", t.Name, role, opens, expiryCell(t.ExpiresAt), used, by, dash(t.Note))
	}
	w.Flush()
	for _, n := range retired {
		fmt.Fprintf(out, "\n%s has a retired role, which relays from meshrelay v0.15.0 refuse. To keep it working,\n"+
			"make it an admin token: meshbridge admin client set %s -role admin\n", n, n)
	}
	return nil
}

// revokeToken revokes a token after confirming, ending its sessions.
func revokeToken(ctx context.Context, c *admin.Client, name, usage string, yes bool, in *bufio.Reader,
	interactive bool, out io.Writer) error {
	if name == "" {
		return errors.New("usage: " + usage)
	}
	ts, err := c.ClientTokens(ctx)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(ts, func(t admin.ClientToken) bool { return t.Name == name })
	if i < 0 {
		return fmt.Errorf("no token named %q (see meshbridge admin clients)", name)
	}
	t := ts[i]
	if t.Bootstrap {
		return fmt.Errorf("%s is configured on the relay (meshRelay.admins), not issued; remove it there", name)
	}
	if !yes {
		if !interactive {
			return fmt.Errorf("pass -yes to revoke %s without a prompt", name)
		}
		what := "every gateway"
		if t.Role == "host" {
			what = "gateway " + t.Gateway
		}
		fmt.Fprintf(out, "Revoke %s (%s, opens %s)? Anyone using it is disconnected. [y/N]: ", name, t.Role, what)
		line, _ := in.ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			return errors.New("not revoked")
		}
	}
	ended, err := c.RevokeClient(ctx, name)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Revoked %s.", name)
	if ended > 0 {
		fmt.Fprintf(out, " Ended %d session(s) using it.", ended)
	}
	fmt.Fprintln(out)
	return nil
}

// parseDays reads "180d" or "180" as days; "" is 0 (the relay's default).
func parseDays(s string) (int, error) {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "d"))
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("-expires %q: give days, like 90d", s)
	}
	return n, nil
}

func expiresText(t time.Time) string {
	if t.IsZero() {
		return ", and doesn't expire"
	}
	return ", until " + t.Local().Format("2 Jan 2006")
}

func expiryCell(t time.Time) string {
	switch {
	case t.IsZero():
		return "never"
	case time.Until(t) <= 0:
		return "expired " + t.Local().Format("2 Jan 2006")
	}
	return t.Local().Format("2 Jan 2006")
}
