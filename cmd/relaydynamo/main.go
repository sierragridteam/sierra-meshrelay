// Command relaydynamo moves a relay's store from SQLite (relay.db) to
// DynamoDB (relay/dynamostore), once.
//
//	relaydynamo import -db relay.db -table sierra-relay   # copy, then verify
//	relaydynamo verify -db relay.db -table sierra-relay   # verify again
//	relaydynamo create-table -table relay-dev             # dev/DynamoDB Local only
//
// Point -db at a snapshot of relay.db taken while the relay was stopped, never
// at a live relay's file. Import refuses a table that already has relay data,
// and holds the table's instance lease while it writes, so it cannot run
// beside a relay on the same table.
//
// The secret key that sealed the repeater passwords comes from
// PF__MESH_RELAY__SECRET_KEY (base64, 32 bytes), the same variable the relay
// reads. Verify proves every sealed password opens with it; it is never
// printed. AWS credentials and region come from the usual AWS environment
// (a task role on ECS). -endpoint points at DynamoDB Local.
package main

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/dpup/prefab/logging"

	"github.com/sierragridteam/sierra-meshrelay/relay"
	"github.com/sierragridteam/sierra-meshrelay/relay/dynamostore"
)

// SecretKeyEnv is the relay's repeater-password key.
const SecretKeyEnv = "PF__MESH_RELAY__SECRET_KEY"

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd, args := os.Args[1], os.Args[2:]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	table := fs.String("table", "", "DynamoDB table name")
	endpoint := fs.String("endpoint", "", "DynamoDB endpoint URL (DynamoDB Local); default: AWS")
	dbPath := fs.String("db", "", "relay.db snapshot (import, verify)")
	leaseDays := fs.Int("lease-days", 90, "copy lease audit rows started in the last N days (0: all)")
	noKey := fs.Bool("no-key-check", false, "verify without "+SecretKeyEnv+" (the passwords are copied but not proven to open)")
	_ = fs.Parse(args)
	if *table == "" {
		fatal("-table is required")
	}
	ctx := logging.With(context.Background(), logging.NewDevLogger())
	db, err := client(ctx, *endpoint)
	if err != nil {
		fatal("%v", err)
	}
	switch cmd {
	case "create-table":
		if err := dynamostore.CreateTable(ctx, db, *table); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("table %s ready (production tables come from Terraform)\n", *table)
	case "import", "verify":
		if *dbPath == "" {
			fatal("-db is required")
		}
		key, err := secretKey(*noKey)
		if err != nil {
			fatal("%v", err)
		}
		os.Exit(run(ctx, cmd, db, *table, *dbPath, *leaseDays, key))
	default:
		usage()
	}
}

func run(ctx context.Context, cmd string, db *dynamodb.Client, table, dbPath string, leaseDays int, key []byte) int {
	now := time.Now()
	opts := relay.SnapshotOptions{AttemptsSince: now.Add(-relay.AttemptRetention)}
	if leaseDays > 0 {
		opts.LeasesSince = now.AddDate(0, 0, -leaseDays)
	}
	snap, err := relay.ReadSnapshot(ctx, dbPath, opts)
	if err != nil {
		fatal("reading %s: %v", dbPath, err)
	}
	fmt.Printf("snapshot %s: %d gateways, %d clients, %d invites, %d repeaters, %d leases, %d settings\n",
		dbPath, len(snap.Gateways), len(snap.Clients), len(snap.Invites), len(snap.Repeaters), len(snap.Leases), len(snap.Settings))
	for t, n := range snap.Dropped {
		fmt.Printf("  left out by the history window: %d %s\n", n, t)
	}
	st, err := dynamostore.Open(ctx, dynamostore.Options{Client: db, Table: table, LockTimeout: 30 * time.Second,
		Owner: "relaydynamo-" + cmd})
	if err != nil {
		fatal("%v", err)
	}
	defer st.Close()
	if cmd == "import" {
		start := time.Now()
		if err := st.Import(snap); err != nil {
			if errors.Is(err, dynamostore.ErrNotEmpty) {
				fatal("%v: import only into a new, empty table", err)
			}
			fatal("import: %v", err)
		}
		fmt.Printf("imported into %s in %s\n", table, time.Since(start).Round(time.Millisecond))
	}
	rep, err := st.Verify(snap, key)
	if err != nil {
		fatal("verify: %v", err)
	}
	fmt.Print(rep.String())
	if !rep.OK() {
		fmt.Println("VERIFY FAILED")
		return 1
	}
	fmt.Println("verify ok")
	return 0
}

func client(ctx context.Context, endpoint string) (*dynamodb.Client, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	var opts []func(*dynamodb.Options)
	if endpoint != "" {
		opts = append(opts, func(o *dynamodb.Options) {
			o.BaseEndpoint = aws.String(endpoint)
			if o.Region == "" {
				o.Region = "us-west-2"
			}
			if os.Getenv("AWS_ACCESS_KEY_ID") == "" && os.Getenv("AWS_PROFILE") == "" {
				// DynamoDB Local accepts any credentials; these are placeholders.
				o.Credentials = credentials.NewStaticCredentialsProvider("local", "local", "")
			}
		})
	}
	return dynamodb.NewFromConfig(cfg, opts...), nil
}

func secretKey(optional bool) ([]byte, error) {
	k := strings.TrimSpace(os.Getenv(SecretKeyEnv))
	if k == "" {
		if optional {
			return nil, nil
		}
		return nil, fmt.Errorf("%s is not set: it must be the relay's key, byte-identical, or the sealed repeater "+
			"passwords are unreadable (use -no-key-check only for a dry run)", SecretKeyEnv)
	}
	b, err := base64.StdEncoding.DecodeString(k)
	if err != nil || len(b) != 32 {
		return nil, fmt.Errorf("%s must be 32 bytes, base64", SecretKeyEnv)
	}
	return b, nil
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: relaydynamo import|verify -db relay.db -table NAME [-endpoint URL] [-lease-days 90]")
	fmt.Fprintln(os.Stderr, "       relaydynamo create-table -table NAME [-endpoint URL]")
	os.Exit(2)
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "relaydynamo: "+format+"\n", a...)
	os.Exit(1)
}
