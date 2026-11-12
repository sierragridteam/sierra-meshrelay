// Package dynamostore is a relay.Storage on Amazon DynamoDB, for the hosted
// relay: one on-demand table, no filesystem, point-in-time recovery from AWS.
// The SQLite store (relay.OpenStore) stays the default for local and
// self-hosted relays; this package is separate so nothing else links the
// AWS SDK.
//
// The table is single-table design (docs/dynamodb.md has the layout): string
// keys pk and sk, a TTL attribute "expires", no secondary indexes. Every
// read the relay depends on is a strongly consistent GetItem or Query.
//
// Like the SQLite store's flock, Open takes the relay's single-writer lock,
// here an "instance lease" item with a heartbeat (lock.go).
package dynamostore

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/dpup/prefab/logging"

	"github.com/sierragridteam/sierra-meshrelay/relay"
)

// API is the part of the DynamoDB client the store uses (*dynamodb.Client).
type API interface {
	GetItem(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	PutItem(context.Context, *dynamodb.PutItemInput, ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	UpdateItem(context.Context, *dynamodb.UpdateItemInput, ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
	DeleteItem(context.Context, *dynamodb.DeleteItemInput, ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error)
	Query(context.Context, *dynamodb.QueryInput, ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
	BatchWriteItem(context.Context, *dynamodb.BatchWriteItemInput, ...func(*dynamodb.Options)) (*dynamodb.BatchWriteItemOutput, error)
	TransactWriteItems(context.Context, *dynamodb.TransactWriteItemsInput, ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error)
}

// Options configures Open.
type Options struct {
	Client API
	Table  string
	// LockTimeout is how long Open waits for another relay's instance lease
	// to be released or go stale (like OpenStore's lockTimeout).
	LockTimeout time.Duration
	// Owner names this process in the lease item (default: a random ID).
	Owner string
	// Heartbeat is how often the lease is renewed (10s). StaleAfter is how
	// long a lease without a heartbeat is held before another process may
	// take it (45s); it must exceed Heartbeat by a few renewals.
	Heartbeat  time.Duration
	StaleAfter time.Duration
	// OnLost is called once if the lease is lost while open (another
	// process took it, or renewals failed for StaleAfter). The store then
	// refuses every call with ErrLeaseLost; the owner should exit.
	OnLost func(error)
	// OpTimeout bounds each call to DynamoDB (10s).
	OpTimeout time.Duration
	// Now is the clock (time.Now); tests set it.
	Now func() time.Time
}

// ErrClosed is returned by a store after Close.
var ErrClosed = errors.New("dynamostore: store is closed")

// ErrLeaseLost is returned once the instance lease has been lost.
var ErrLeaseLost = errors.New("dynamostore: instance lease lost to another relay")

// Store is the DynamoDB relay.Storage.
type Store struct {
	db    API
	table string
	opts  Options
	base  context.Context // the background context calls derive from

	seq atomic.Uint64 // sort-key tiebreak, increasing across restarts

	mu     sync.Mutex
	closed bool
	lost   bool
	lease  *instanceLease
}

var _ relay.Storage = (*Store)(nil)

func (o Options) withDefaults() Options {
	if o.Heartbeat <= 0 {
		o.Heartbeat = 10 * time.Second
	}
	if o.StaleAfter <= 0 {
		o.StaleAfter = 45 * time.Second
	}
	if o.OpTimeout <= 0 {
		o.OpTimeout = 10 * time.Second
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Owner == "" {
		o.Owner = randomID()
	}
	return o
}

// Open takes the relay's instance lease on the table, waiting up to
// LockTimeout, and returns the store. ctx carries the logger for the wait.
func Open(ctx context.Context, o Options) (*Store, error) {
	if o.Client == nil || o.Table == "" {
		return nil, errors.New("dynamostore: Client and Table are required")
	}
	ctx = logging.EnsureLogger(ctx)
	o = o.withDefaults()
	if o.StaleAfter < 2*o.Heartbeat {
		return nil, fmt.Errorf("dynamostore: StaleAfter (%s) must be at least twice Heartbeat (%s)", o.StaleAfter, o.Heartbeat)
	}
	s := &Store{db: o.Client, table: o.Table, opts: o, base: context.WithoutCancel(ctx)}
	s.seq.Store(uint64(o.Now().UnixNano()))
	l, err := s.acquire(ctx)
	if err != nil {
		return nil, err
	}
	s.lease = l
	go s.heartbeat(l)
	return s, nil
}

// Close stops the heartbeat and releases the instance lease, so the next
// relay can open the table at once. Later calls return ErrClosed.
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	l := s.lease
	s.mu.Unlock()
	if l == nil {
		return nil
	}
	return s.release(l)
}

// check refuses calls after Close or a lost lease.
func (s *Store) check() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.closed:
		return ErrClosed
	case s.lost:
		return ErrLeaseLost
	}
	return nil
}

func (s *Store) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(s.base, s.opts.OpTimeout)
}

func (s *Store) now() time.Time { return s.opts.Now() }

// nextSeq is a sort-key tiebreak that increases within a process and, being
// seeded from the clock in nanoseconds, across restarts.
func (s *Store) nextSeq() uint64 { return s.seq.Add(1) }

// --- low-level helpers -------------------------------------------------------

type item = map[string]types.AttributeValue

func str(v string) types.AttributeValue   { return &types.AttributeValueMemberS{Value: v} }
func num(v int64) types.AttributeValue    { return &types.AttributeValueMemberN{Value: fmt.Sprint(v)} }
func boolean(v bool) types.AttributeValue { return &types.AttributeValueMemberBOOL{Value: v} }

func key(pk, sk string) item { return item{"pk": str(pk), "sk": str(sk)} }

// pad makes a non-negative number sort as a string.
func pad(n int64) string { return fmt.Sprintf("%020d", n) }

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromMS(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.UnixMilli(v).UTC()
}

func timePtr(v int64) *time.Time {
	if v == 0 {
		return nil
	}
	t := fromMS(v)
	return &t
}

func condFailed(err error) bool {
	var c *types.ConditionalCheckFailedException
	return errors.As(err, &c)
}

// txFailed reports which items of a cancelled transaction failed their
// condition (by index), or ok=false if err isn't a cancellation.
func txFailed(err error) (failed []int, ok bool) {
	var t *types.TransactionCanceledException
	if !errors.As(err, &t) {
		return nil, false
	}
	for i, r := range t.CancellationReasons {
		if aws.ToString(r.Code) == "ConditionalCheckFailed" {
			failed = append(failed, i)
		}
	}
	return failed, true
}

func (s *Store) get(pk, sk string) (item, error) {
	ctx, cancel := s.ctx()
	defer cancel()
	out, err := s.db.GetItem(ctx, &dynamodb.GetItemInput{TableName: &s.table, Key: key(pk, sk), ConsistentRead: aws.Bool(true)})
	if err != nil {
		return nil, err
	}
	if out.Item == nil {
		return nil, relay.ErrNotFound
	}
	return out.Item, nil
}

// query runs a strongly consistent query, page by page, until fn returns
// false or the results end.
func (s *Store) query(in *dynamodb.QueryInput, fn func(item) bool) error {
	in.TableName = &s.table
	in.ConsistentRead = aws.Bool(true)
	for {
		ctx, cancel := s.ctx()
		out, err := s.db.Query(ctx, in)
		cancel()
		if err != nil {
			return err
		}
		for _, it := range out.Items {
			if !fn(it) {
				return nil
			}
		}
		if len(out.LastEvaluatedKey) == 0 {
			return nil
		}
		in.ExclusiveStartKey = out.LastEvaluatedKey
	}
}

// queryPK lists a partition in sort-key order.
func (s *Store) queryPK(pk string, fn func(item) bool) error {
	return s.query(&dynamodb.QueryInput{
		KeyConditionExpression:    aws.String("pk = :pk"),
		ExpressionAttributeValues: item{":pk": str(pk)},
	}, fn)
}

// batchWrite writes puts and deletes in batches of 25, retrying unprocessed
// items with backoff.
func (s *Store) batchWrite(reqs []types.WriteRequest) error {
	for len(reqs) > 0 {
		n := min(25, len(reqs))
		pending := reqs[:n]
		reqs = reqs[n:]
		for attempt := 0; len(pending) > 0; attempt++ {
			if attempt > 0 {
				if attempt > 8 {
					return fmt.Errorf("dynamostore: %d writes still unprocessed after retries", len(pending))
				}
				time.Sleep(time.Duration(25<<attempt) * time.Millisecond)
			}
			ctx, cancel := s.ctx()
			out, err := s.db.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{
				RequestItems: map[string][]types.WriteRequest{s.table: pending}})
			cancel()
			if err != nil {
				return err
			}
			pending = out.UnprocessedItems[s.table]
		}
	}
	return nil
}

func deleteReq(pk, sk string) types.WriteRequest {
	return types.WriteRequest{DeleteRequest: &types.DeleteRequest{Key: key(pk, sk)}}
}

func putReq(it item) types.WriteRequest {
	return types.WriteRequest{PutRequest: &types.PutRequest{Item: it}}
}

// deletePartition removes every item under pk.
func (s *Store) deletePartition(pk string) error {
	var reqs []types.WriteRequest
	err := s.query(&dynamodb.QueryInput{
		KeyConditionExpression:    aws.String("pk = :pk"),
		ExpressionAttributeValues: item{":pk": str(pk)},
		ProjectionExpression:      aws.String("pk, sk"),
	}, func(it item) bool {
		reqs = append(reqs, deleteReq(pk, getS(it, "sk")))
		return true
	})
	if err != nil {
		return err
	}
	return s.batchWrite(reqs)
}

// addCounter atomically adds n to a counter item and returns the new value.
func (s *Store) addCounter(name string, n int64) (int64, error) {
	ctx, cancel := s.ctx()
	defer cancel()
	out, err := s.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 &s.table,
		Key:                       key(pkSeq, name),
		UpdateExpression:          aws.String("ADD #n :n"),
		ExpressionAttributeNames:  map[string]string{"#n": "n"},
		ExpressionAttributeValues: item{":n": num(n)},
		ReturnValues:              types.ReturnValueUpdatedNew,
	})
	if err != nil {
		return 0, err
	}
	return getN(out.Attributes, "n"), nil
}

// --- attribute readers ---------------------------------------------------------

func getS(it item, k string) string {
	if v, ok := it[k].(*types.AttributeValueMemberS); ok {
		return v.Value
	}
	return ""
}

func getN(it item, k string) int64 {
	if v, ok := it[k].(*types.AttributeValueMemberN); ok {
		var n int64
		_, _ = fmt.Sscan(v.Value, &n)
		return n
	}
	return 0
}

func getB(it item, k string) []byte {
	if v, ok := it[k].(*types.AttributeValueMemberB); ok {
		return v.Value
	}
	return nil
}

func getBool(it item, k string) bool {
	if v, ok := it[k].(*types.AttributeValueMemberBOOL); ok {
		return v.Value
	}
	return false
}
