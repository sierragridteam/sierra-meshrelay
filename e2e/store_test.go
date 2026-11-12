package e2e

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/sierragridteam/sierra-meshrelay/relay"
	"github.com/sierragridteam/sierra-meshrelay/relay/dynamostore"
)

// openStore is the relay's store for an e2e test: SQLite in a temp dir, or,
// with MESHRELAY_TEST_DYNAMO_ENDPOINT set (make test-dynamo), a fresh table
// on DynamoDB Local, so the whole suite also runs on the DynamoDB store.
func openStore(t *testing.T, ctx context.Context) relay.Storage {
	t.Helper()
	ep := os.Getenv("MESHRELAY_TEST_DYNAMO_ENDPOINT")
	if ep == "" {
		st, err := relay.OpenStore(ctx, filepath.Join(t.TempDir(), "relay.db"), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	db := dynamodb.New(dynamodb.Options{Region: "us-west-2", BaseEndpoint: aws.String(ep),
		// DynamoDB Local ignores credentials; these are placeholders.
		Credentials: credentials.NewStaticCredentialsProvider("local", "local", "")})
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	table := "relay-e2e-" + hex.EncodeToString(b)
	if err := dynamostore.CreateTable(ctx, db, table); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.DeleteTable(context.Background(), &dynamodb.DeleteTableInput{TableName: aws.String(table)})
	})
	st, err := dynamostore.Open(ctx, dynamostore.Options{Client: db, Table: table, LockTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return st
}
