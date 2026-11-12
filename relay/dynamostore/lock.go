package dynamostore

// The instance lease: the relay's single-writer lock without a filesystem.
//
// One item (pk "LOCK", sk "relay") names the owner and carries a heartbeat
// (hb, the owner's clock in ms) and a nonce that changes on every renewal.
// Open takes it when it is absent, already ours, or stale; the owner renews
// it every Heartbeat with a write conditioned on still owning it; Close
// deletes it, so a clean handover is immediate.
//
// Stale is judged two ways, so neither clock skew nor a dead owner can
// wedge a deploy: the waiter's own clock sees the item unchanged for
// StaleAfter, or the item's heartbeat is more than 2*StaleAfter behind the
// waiter's clock (an owner that died long ago). The takeover write is
// conditioned on the exact owner and nonce observed, so two waiters cannot
// both win.
//
// A process that loses its lease (another took it after missed renewals,
// or renewals failed for StaleAfter) stops: every call returns ErrLeaseLost
// and Options.OnLost tells the owner to exit.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/dpup/prefab/logging"

	"github.com/sierragridteam/sierra-meshrelay/relay"
)

const (
	pkLock = "LOCK"
	skLock = "relay"
)

type instanceLease struct {
	owner string
	stop  chan struct{}
	done  chan struct{}
	once  sync.Once
	nonce string
}

func randomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// LockHolder describes the instance lease item, for diagnostics.
type LockHolder struct {
	Owner     string
	Heartbeat time.Time
}

func (s *Store) putLease(owner, nonce string, cond string, vals item) error {
	ctx, cancel := s.ctx()
	defer cancel()
	it := key(pkLock, skLock)
	it["owner"] = str(owner)
	it["hb"] = num(s.now().UnixMilli())
	it["nonce"] = str(nonce)
	_, err := s.db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:                 &s.table,
		Item:                      it,
		ConditionExpression:       aws.String(cond),
		ExpressionAttributeNames:  map[string]string{"#o": "owner"},
		ExpressionAttributeValues: vals,
	})
	return err
}

func (s *Store) acquire(ctx context.Context) (*instanceLease, error) {
	o := s.opts
	deadline := s.now().Add(o.LockTimeout)
	var seen struct {
		owner, nonce string
		since        time.Time
	}
	announced := false
	for {
		nonce := randomID()
		err := s.putLease(o.Owner, nonce, "attribute_not_exists(pk) OR #o = :me", item{":me": str(o.Owner)})
		if err == nil {
			return &instanceLease{owner: o.Owner, nonce: nonce, stop: make(chan struct{}), done: make(chan struct{})}, nil
		}
		if !condFailed(err) {
			return nil, fmt.Errorf("dynamostore: taking the instance lease: %w", err)
		}
		cur, gerr := s.get(pkLock, skLock)
		if gerr == relay.ErrNotFound {
			continue // released between the two calls
		}
		if gerr != nil {
			return nil, fmt.Errorf("dynamostore: reading the instance lease: %w", gerr)
		}
		owner, curNonce, hb := getS(cur, "owner"), getS(cur, "nonce"), fromMS(getN(cur, "hb"))
		now := s.now()
		if owner != seen.owner || curNonce != seen.nonce {
			seen.owner, seen.nonce, seen.since = owner, curNonce, now
		}
		if now.Sub(seen.since) >= o.StaleAfter || now.Sub(hb) > 2*o.StaleAfter {
			nonce := randomID()
			err := s.putLease(o.Owner, nonce, "#o = :owner AND nonce = :nonce",
				item{":owner": str(owner), ":nonce": str(curNonce)})
			if err == nil {
				logging.Warnw(ctx, "took over the relay's instance lease from a relay that stopped renewing it",
					"table", s.table, "previous_owner", owner, "last_heartbeat", hb)
				return &instanceLease{owner: o.Owner, nonce: nonce, stop: make(chan struct{}), done: make(chan struct{})}, nil
			}
			if !condFailed(err) {
				return nil, fmt.Errorf("dynamostore: taking over the instance lease: %w", err)
			}
			continue // someone else moved first; look again
		}
		if !now.Before(deadline) {
			return nil, fmt.Errorf("dynamostore: table %s's instance lease is still held by %s after %s "+
				"(on a deploy, the previous relay task has not exited; last heartbeat %s)",
				s.table, owner, o.LockTimeout, hb.Format(time.RFC3339))
		}
		if !announced {
			logging.Warnw(ctx, "relay instance lease is held by another process; waiting for it to exit",
				"table", s.table, "owner", owner, "timeout", o.LockTimeout.String())
			announced = true
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(min(time.Second, o.Heartbeat/2)):
		}
	}
}

func (s *Store) heartbeat(l *instanceLease) {
	defer close(l.done)
	t := time.NewTicker(s.opts.Heartbeat)
	defer t.Stop()
	lastOK := s.now()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
		}
		nonce := randomID()
		ctx, cancel := s.ctx()
		_, err := s.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName:                 &s.table,
			Key:                       key(pkLock, skLock),
			UpdateExpression:          aws.String("SET hb = :hb, nonce = :new"),
			ConditionExpression:       aws.String("#o = :me"),
			ExpressionAttributeNames:  map[string]string{"#o": "owner"},
			ExpressionAttributeValues: item{":hb": num(s.now().UnixMilli()), ":new": str(nonce), ":me": str(l.owner)},
		})
		cancel()
		switch {
		case err == nil:
			lastOK = s.now()
			continue
		case condFailed(err):
			s.loseLease(fmt.Errorf("%w (it was taken over)", ErrLeaseLost))
			return
		case s.now().Sub(lastOK) >= s.opts.StaleAfter-s.opts.Heartbeat:
			// Another relay may take it any moment now: stop first.
			s.loseLease(fmt.Errorf("%w (renewals failing since %s: %w)", ErrLeaseLost, lastOK.Format(time.RFC3339), err))
			return
		default:
			logging.Warnw(s.base, "relay instance lease renewal failed; retrying", "table", s.table, "error", err)
		}
	}
}

func (s *Store) loseLease(err error) {
	s.mu.Lock()
	already := s.lost || s.closed
	s.lost = true
	s.mu.Unlock()
	if already {
		return
	}
	logging.Errorw(s.base, "relay instance lease lost; the store refuses further calls", "table", s.table, "error", err)
	if s.opts.OnLost != nil {
		s.opts.OnLost(err)
	}
}

// release stops the heartbeat and deletes the lease item if it is still
// ours.
func (s *Store) release(l *instanceLease) error {
	l.once.Do(func() { close(l.stop) })
	<-l.done
	ctx, cancel := s.ctx()
	defer cancel()
	_, err := s.db.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName:                 &s.table,
		Key:                       key(pkLock, skLock),
		ConditionExpression:       aws.String("#o = :me"),
		ExpressionAttributeNames:  map[string]string{"#o": "owner"},
		ExpressionAttributeValues: item{":me": str(l.owner)},
	})
	if err != nil && !condFailed(err) {
		return fmt.Errorf("dynamostore: releasing the instance lease: %w", err)
	}
	return nil
}

// abandon stops renewing without releasing, as a crashed process would.
// For tests.
func (s *Store) abandon() {
	s.mu.Lock()
	l := s.lease
	s.closed = true
	s.mu.Unlock()
	l.once.Do(func() { close(l.stop) })
	<-l.done
}

// Holder reads the instance lease item (ErrNotFound if none).
func (s *Store) Holder() (LockHolder, error) {
	it, err := s.get(pkLock, skLock)
	if err != nil {
		return LockHolder{}, err
	}
	return LockHolder{Owner: getS(it, "owner"), Heartbeat: fromMS(getN(it, "hb"))}, nil
}
