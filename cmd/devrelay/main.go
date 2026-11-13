// Command devrelay runs the real relay on localhost for development: a fresh
// (or reused) SQLite file, a bootstrap admin, and optionally a gateway and an
// operator client, with their tokens printed. Point a bridge and `meshbridge
// connect` at it to test without the production relay or shared tokens.
//
//	devrelay                         # 127.0.0.1:8090, temp database
//	devrelay -gateway cabin          # also create gateway "cabin" + an operator
//	devrelay -db ./dev/relay.db      # keep state between runs
//
// Not for production: the deployed relay is sierra-data's cmd/mesh-relay.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/dpup/prefab"
	"github.com/dpup/prefab/logging"

	"github.com/sierragridteam/sierra-meshrelay/relay"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8090", "address to serve on")
	dbPath := flag.String("db", "", "SQLite file (default: a temp file, discarded on exit)")
	gateway := flag.String("gateway", "", "create this gateway and an operator client, and print their tokens")
	flag.Parse()

	host, portStr, err := net.SplitHostPort(*listen)
	if err != nil {
		fatal("-listen: %v", err)
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		fatal("-listen: bad port %q", portStr)
	}
	if *dbPath == "" {
		dir, err := os.MkdirTemp("", "devrelay-*")
		if err != nil {
			fatal("%v", err)
		}
		defer os.RemoveAll(dir)
		*dbPath = filepath.Join(dir, "relay.db")
	}

	ctx := logging.With(context.Background(), logging.NewDevLogger())
	st, err := relay.OpenStore(ctx, *dbPath, 5*time.Second)
	if err != nil {
		fatal("%v", err)
	}
	defer st.Close()

	admin, _ := relay.MintToken(relay.ClientTokenPrefix)
	base := fmt.Sprintf("http://%s", net.JoinHostPort(host, portStr))
	r := relay.New(ctx, relay.Config{
		BootstrapAdmins: map[string]string{"dev": relay.TokenHash(admin)},
		PublicBaseURL:   base,
	}, st)
	server := prefab.New(prefab.WithContext(ctx), prefab.WithHost(host), prefab.WithPort(port), prefab.WithPlugin(r))

	ws := "ws://" + net.JoinHostPort(host, portStr) + relay.PathPrefix
	api := base + relay.PathPrefix + "/api"
	fmt.Printf("\ndevrelay on %s (database %s)\n\n", base, *dbPath)
	fmt.Printf("  admin token     %s\n", admin)
	fmt.Printf("  relay URL       %s\n", ws)
	fmt.Printf("  admin API       %s\n\n", api)

	if *gateway != "" {
		bridgeTok, clientTok, err := seed(st, *gateway)
		if err != nil {
			fatal("seeding: %v", err)
		}
		fmt.Printf("  gateway         %s\n", *gateway)
		fmt.Printf("  bridge token    %s\n", bridgeTok)
		fmt.Printf("  operator token  %s\n\n", clientTok)
		fmt.Printf("Run a bridge:\n  MESHBRIDGE_TOKEN=%s meshbridge run -config <file with gateway = %q, relay = %q>\n", bridgeTok, *gateway, ws)
		fmt.Printf("Use it:\n  MESHBRIDGE_CLIENT_TOKEN=%s meshbridge connect %s -relay %s\n\n", clientTok, *gateway, ws)
	}
	fmt.Printf("Pairing: meshbridge setup -relay %s   (approve with POST %s/pairings/<code>/approve)\n\n", ws, api)

	// Start blocks until SIGINT or SIGTERM, which prefab handles itself
	// (calling Shutdown here too would race it and crash).
	if err := server.Start(); err != nil {
		fatal("%v", err)
	}
}

// seed creates a gateway and an operator client directly in the store.
func seed(st *relay.Store, gateway string) (bridgeTok, clientTok string, err error) {
	if !relay.ValidGatewayName(gateway) {
		return "", "", fmt.Errorf("gateway name %q must match [a-z0-9-]{3,40}", gateway)
	}
	now := time.Now()
	if bridgeTok, err = relay.MintToken(relay.BridgeTokenPrefix); err != nil {
		return "", "", err
	}
	if _, gerr := st.Gateway(gateway); gerr == nil {
		err = st.SetGatewayToken(gateway, relay.TokenHash(bridgeTok))
	} else {
		err = st.CreateGateway(gateway, relay.TokenHash(bridgeTok), "", "devrelay", now)
	}
	if err != nil {
		return "", "", err
	}
	if clientTok, err = relay.MintToken(relay.ClientTokenPrefix); err != nil {
		return "", "", err
	}
	name := "dev-operator-" + fmt.Sprint(now.Unix())
	return bridgeTok, clientTok, st.CreateClient(relay.ClientRecord{Name: name, Role: relay.RoleOperator, CreatedAt: now,
		CreatedBy: "devrelay"}, relay.TokenHash(clientTok))
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "devrelay: "+format+"\n", a...)
	os.Exit(1)
}
