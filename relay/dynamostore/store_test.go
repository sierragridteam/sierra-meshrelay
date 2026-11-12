package dynamostore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sierragridteam/sierra-meshrelay/relay"
	"github.com/sierragridteam/sierra-meshrelay/relay/storetest"
)

// EndpointEnv opts in to the DynamoDB tests: the URL of a DynamoDB Local
// (make dynamo-up starts one on http://127.0.0.1:8000). Without it they
// skip, so plain `make test` stays offline.
const EndpointEnv = "MESHRELAY_TEST_DYNAMO_ENDPOINT"

// localClient is a client for DynamoDB Local; the fixed credentials are
// DynamoDB Local's convention (it ignores them), not real ones.
func localClient(t *testing.T) *dynamodb.Client {
	t.Helper()
	ep := os.Getenv(EndpointEnv)
	if ep == "" {
		t.Skipf("set %s (make test-dynamo) to run the DynamoDB tests", EndpointEnv)
	}
	return dynamodb.New(dynamodb.Options{
		Region:       "us-west-2",
		BaseEndpoint: aws.String(ep),
		Credentials:  credentials.NewStaticCredentialsProvider("local", "local", ""),
	})
}

// newTable creates a uniquely named table, deleted after the test.
func newTable(t *testing.T, db *dynamodb.Client) string {
	t.Helper()
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "relay-test-" + hex.EncodeToString(b)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	require.NoError(t, CreateTable(ctx, db, name))
	t.Cleanup(func() {
		_, _ = db.DeleteTable(context.Background(), &dynamodb.DeleteTableInput{TableName: aws.String(name)})
	})
	return name
}

func openTest(t *testing.T, db *dynamodb.Client, table string, o Options) *Store {
	t.Helper()
	o.Client, o.Table = db, table
	if o.LockTimeout == 0 {
		o.LockTimeout = time.Second
	}
	st, err := Open(context.Background(), o)
	require.NoError(t, err)
	return st
}

func TestConformance(t *testing.T) {
	db := localClient(t)
	storetest.Run(t, func(t *testing.T) relay.Storage {
		return openTest(t, db, newTable(t, db), Options{})
	})
}

func TestLeaseWaitsForRelease(t *testing.T) {
	db := localClient(t)
	table := newTable(t, db)
	a := openTest(t, db, table, Options{Owner: "a"})

	_, err := Open(context.Background(), Options{Client: db, Table: table, Owner: "b", LockTimeout: 300 * time.Millisecond})
	require.Error(t, err, "a live owner keeps the lease")
	assert.Contains(t, err.Error(), "still held by a")

	done := make(chan *Store)
	go func() {
		b, err := Open(context.Background(), Options{Client: db, Table: table, Owner: "b", LockTimeout: 10 * time.Second})
		assert.NoError(t, err)
		done <- b
	}()
	time.Sleep(300 * time.Millisecond)
	require.NoError(t, a.Close())
	select {
	case b := <-done:
		require.NotNil(t, b)
		h, err := b.Holder()
		require.NoError(t, err)
		assert.Equal(t, "b", h.Owner)
		_, err = a.Gateways()
		assert.ErrorIs(t, err, ErrClosed, "a released store must not reach the table")
		require.NoError(t, b.Close())
	case <-time.After(5 * time.Second):
		t.Fatal("a clean release must hand over at once")
	}
}

func TestStaleLeaseIsTakenOverAndTheOldOwnerStops(t *testing.T) {
	db := localClient(t)
	table := newTable(t, db)
	lost := make(chan error, 1)
	a := openTest(t, db, table, Options{Owner: "a", Heartbeat: 100 * time.Millisecond, StaleAfter: 600 * time.Millisecond,
		OnLost: func(err error) { lost <- err }})

	// A live owner is not stale however long the waiter looks.
	_, err := Open(context.Background(), Options{Client: db, Table: table, Owner: "b",
		Heartbeat: 100 * time.Millisecond, StaleAfter: 600 * time.Millisecond, LockTimeout: time.Second})
	require.Error(t, err)

	// a stops renewing (a crash, or a frozen task) and b takes over after
	// StaleAfter of no change.
	a.mu.Lock()
	l := a.lease
	a.mu.Unlock()
	l.once.Do(func() { close(l.stop) })
	<-l.done
	start := time.Now()
	b := openTest(t, db, table, Options{Owner: "b", Heartbeat: 100 * time.Millisecond, StaleAfter: 600 * time.Millisecond,
		LockTimeout: 5 * time.Second})
	defer b.Close()
	assert.GreaterOrEqual(t, time.Since(start), 500*time.Millisecond, "waited for the lease to go stale")

	// a's renewal now fails on its condition: it reports the loss and refuses calls.
	go a.heartbeat(&instanceLease{owner: "a", stop: make(chan struct{}), done: make(chan struct{})})
	select {
	case err := <-lost:
		assert.ErrorIs(t, err, ErrLeaseLost)
	case <-time.After(5 * time.Second):
		t.Fatal("the old owner never noticed it lost the lease")
	}
	_, err = a.Gateways()
	assert.ErrorIs(t, err, ErrLeaseLost)
	_, err = b.Gateways()
	assert.NoError(t, err)
	require.NoError(t, a.Close(), "closing a store that lost its lease leaves the new owner's lease alone")
	h, err := b.Holder()
	require.NoError(t, err)
	assert.Equal(t, "b", h.Owner)
}

func TestDeadOwnerWithAnOldHeartbeatIsTakenAtOnce(t *testing.T) {
	db := localClient(t)
	table := newTable(t, db)
	past := time.Now().Add(-time.Hour)
	a := openTest(t, db, table, Options{Owner: "a", Now: func() time.Time { return past }})
	a.abandon()
	start := time.Now()
	b := openTest(t, db, table, Options{Owner: "b", LockTimeout: 5 * time.Second})
	defer b.Close()
	assert.Less(t, time.Since(start), 2*time.Second, "an hour-old heartbeat is stale without waiting StaleAfter")
}
