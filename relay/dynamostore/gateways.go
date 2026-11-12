package dynamostore

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/sierragridteam/sierra-meshrelay/meshproto"
	"github.com/sierragridteam/sierra-meshrelay/relay"
)

// Partition keys. See docs/dynamodb.md for the whole layout.
const (
	pkGateway   = "GW"        // sk name
	pkClient    = "CL"        // sk name
	pkToken     = "TOK"       // sk B#<hash> (bridge) or C#<hash> (client) -> name
	pkLease     = "LEASE"     // sk <started ms>#<id>
	pkLeaseOpen = "LEASEOPEN" // sk id -> the open lease's sk
	pkInvite    = "INV"       // sk id
	pkRepeater  = "REP"       // sk pubkey
	pkPollState = "RPS"       // sk pubkey
	pkSetting   = "SET"       // sk key
	pkSeq       = "SEQ"       // sk EV#<gateway> / LOG#<gateway>: counters
	// Per-entity partitions: EV#<gateway> (events), LOG#<gateway> (bridge
	// logs), RA#<pubkey> (repeater attempts).
)

func bridgeTokenSK(hash string) string { return "B#" + hash }
func clientTokenSK(hash string) string { return "C#" + hash }
func eventsPK(gw string) string        { return "EV#" + gw }
func logsPK(gw string) string          { return "LOG#" + gw }
func attemptsPK(pubkey string) string  { return "RA#" + pubkey }

// --- gateways ------------------------------------------------------------------

func gatewayItem(g *relay.GatewayRecord) item {
	it := key(pkGateway, g.Name)
	it["tokenHash"] = str(g.TokenHash)
	it["pinned"] = str(g.PinnedPubkey)
	it["mismatch"] = str(g.MismatchPubkey)
	it["notes"] = str(g.Notes)
	it["createdAt"] = num(ms(g.CreatedAt))
	it["lastSeen"] = num(ms(g.LastSeen))
	it["lastStatsAt"] = num(ms(g.LastStatsAt))
	hello, stats := "", ""
	if g.LastHello != nil {
		b, _ := json.Marshal(g.LastHello)
		hello = string(b)
	}
	if g.LastStats != nil {
		b, _ := json.Marshal(g.LastStats)
		stats = string(b)
	}
	it["lastHello"] = str(hello)
	it["lastStats"] = str(stats)
	return it
}

func tokenItem(sk, name string) item {
	it := key(pkToken, sk)
	it["name"] = str(name)
	return it
}

func gatewayFrom(it item) *relay.GatewayRecord {
	g := &relay.GatewayRecord{
		Name: getS(it, "sk"), TokenHash: getS(it, "tokenHash"), PinnedPubkey: getS(it, "pinned"),
		MismatchPubkey: getS(it, "mismatch"), Notes: getS(it, "notes"),
		CreatedAt: fromMS(getN(it, "createdAt")), LastSeen: fromMS(getN(it, "lastSeen")),
		LastStatsAt: fromMS(getN(it, "lastStatsAt")),
	}
	if h := getS(it, "lastHello"); h != "" {
		var v meshproto.Hello
		if json.Unmarshal([]byte(h), &v) == nil {
			g.LastHello = &v
		}
	}
	if st := getS(it, "lastStats"); st != "" {
		var v relay.Stats
		if json.Unmarshal([]byte(st), &v) == nil {
			g.LastStats = &v
		}
	}
	return g
}

func (s *Store) putNew(it item) types.TransactWriteItem {
	return types.TransactWriteItem{Put: &types.Put{TableName: &s.table, Item: it,
		ConditionExpression: aws.String("attribute_not_exists(pk)")}}
}

func (s *Store) transact(items ...types.TransactWriteItem) error {
	ctx, cancel := s.ctx()
	defer cancel()
	_, err := s.db.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: items})
	return err
}

// CreateGateway inserts a gateway and its token pointer; ErrExists if the
// name or the token hash is taken.
func (s *Store) CreateGateway(name, tokenHash, pinnedPubkey, notes string, now time.Time) error {
	if err := s.check(); err != nil {
		return err
	}
	g := &relay.GatewayRecord{Name: name, TokenHash: tokenHash, PinnedPubkey: pinnedPubkey, Notes: notes, CreatedAt: now}
	err := s.transact(s.putNew(gatewayItem(g)), s.putNew(tokenItem(bridgeTokenSK(tokenHash), name)))
	if _, ok := txFailed(err); ok {
		return relay.ErrExists
	}
	return err
}

// Gateway loads one gateway.
func (s *Store) Gateway(name string) (*relay.GatewayRecord, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	it, err := s.get(pkGateway, name)
	if err != nil {
		return nil, err
	}
	return gatewayFrom(it), nil
}

// GatewayByToken resolves a bridge token hash through its pointer item.
func (s *Store) GatewayByToken(hash string) (*relay.GatewayRecord, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	p, err := s.get(pkToken, bridgeTokenSK(hash))
	if err != nil {
		return nil, err
	}
	g, err := s.Gateway(getS(p, "name"))
	if err != nil {
		return nil, err
	}
	if g.TokenHash != hash {
		return nil, relay.ErrNotFound // a stale pointer: never authenticate through one
	}
	return g, nil
}

// Gateways lists every gateway, by name.
func (s *Store) Gateways() ([]*relay.GatewayRecord, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	var out []*relay.GatewayRecord
	err := s.queryPK(pkGateway, func(it item) bool { out = append(out, gatewayFrom(it)); return true })
	return out, err
}

// SetGatewayToken replaces a gateway's bridge token hash: the gateway row
// and both pointers change in one transaction, so the old token stops
// working at once.
func (s *Store) SetGatewayToken(name, hash string) error {
	if err := s.check(); err != nil {
		return err
	}
	for range 3 {
		g, err := s.Gateway(name)
		if err != nil {
			return err
		}
		if g.TokenHash == hash {
			return nil
		}
		err = s.transact(
			types.TransactWriteItem{Update: &types.Update{TableName: &s.table, Key: key(pkGateway, name),
				UpdateExpression:          aws.String("SET tokenHash = :new"),
				ConditionExpression:       aws.String("tokenHash = :old"),
				ExpressionAttributeValues: item{":new": str(hash), ":old": str(g.TokenHash)}}},
			types.TransactWriteItem{Delete: &types.Delete{TableName: &s.table, Key: key(pkToken, bridgeTokenSK(g.TokenHash))}},
			s.putNew(tokenItem(bridgeTokenSK(hash), name)),
		)
		failed, ok := txFailed(err)
		switch {
		case !ok:
			return err
		case slices.Contains(failed, 2):
			return fmt.Errorf("gateway %s: %w (token hash in use)", name, relay.ErrExists)
		}
		// The gateway changed underneath (deleted or re-tokened): look again.
	}
	return errors.New("dynamostore: gateway token kept changing; giving up")
}

func (s *Store) updateExisting(pk, sk, expr string, names map[string]string, vals item) error {
	ctx, cancel := s.ctx()
	defer cancel()
	in := &dynamodb.UpdateItemInput{TableName: &s.table, Key: key(pk, sk), UpdateExpression: aws.String(expr),
		ConditionExpression: aws.String("attribute_exists(pk)"), ExpressionAttributeValues: vals}
	if len(names) > 0 {
		in.ExpressionAttributeNames = names
	}
	_, err := s.db.UpdateItem(ctx, in)
	if condFailed(err) {
		return relay.ErrNotFound
	}
	return err
}

// ignoreMissing turns ErrNotFound into nil, for the Record* updates.
func ignoreMissing(err error) error {
	if errors.Is(err, relay.ErrNotFound) {
		return nil
	}
	return err
}

// SetPubkeys sets a gateway's pinned and mismatching pubkeys.
func (s *Store) SetPubkeys(name, pinned, mismatch string) error {
	if err := s.check(); err != nil {
		return err
	}
	return s.updateExisting(pkGateway, name, "SET pinned = :p, mismatch = :m", nil,
		item{":p": str(pinned), ":m": str(mismatch)})
}

// DeleteGateway removes a gateway, its token pointer, its events and its
// bridge logs. Lease audit rows and the event/log counters stay (the
// counters keep a recreated gateway's log IDs past old cursors).
func (s *Store) DeleteGateway(name string) error {
	if err := s.check(); err != nil {
		return err
	}
	g, err := s.Gateway(name)
	if err != nil {
		return err
	}
	err = s.transact(
		types.TransactWriteItem{Delete: &types.Delete{TableName: &s.table, Key: key(pkGateway, name),
			ConditionExpression: aws.String("attribute_exists(pk)")}},
		types.TransactWriteItem{Delete: &types.Delete{TableName: &s.table, Key: key(pkToken, bridgeTokenSK(g.TokenHash))}},
	)
	if _, ok := txFailed(err); ok {
		return relay.ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := s.deletePartition(eventsPK(name)); err != nil {
		return err
	}
	return s.deletePartition(logsPK(name))
}

// RecordHello stores a bridge's hello and marks it seen.
func (s *Store) RecordHello(name string, h *meshproto.Hello, at time.Time) error {
	if err := s.check(); err != nil {
		return err
	}
	b, _ := json.Marshal(h)
	return ignoreMissing(s.updateExisting(pkGateway, name, "SET lastHello = :h, lastSeen = :at", nil,
		item{":h": str(string(b)), ":at": num(ms(at))}))
}

// RecordSeen marks a gateway seen.
func (s *Store) RecordSeen(name string, at time.Time) error {
	if err := s.check(); err != nil {
		return err
	}
	return ignoreMissing(s.updateExisting(pkGateway, name, "SET lastSeen = :at", nil, item{":at": num(ms(at))}))
}

// RecordStats stores a bridge's stats and marks it seen.
func (s *Store) RecordStats(name string, st relay.Stats, at time.Time) error {
	if err := s.check(); err != nil {
		return err
	}
	b, _ := json.Marshal(st)
	return ignoreMissing(s.updateExisting(pkGateway, name, "SET lastStats = :s, lastStatsAt = :at, lastSeen = :at", nil,
		item{":s": str(string(b)), ":at": num(ms(at))}))
}

// --- gateway events ------------------------------------------------------------

// AddGatewayEvent appends an event under the gateway's counter and drops
// the one that falls off the cap, so exactly MaxEventsPerGateway remain.
func (s *Store) AddGatewayEvent(name string, e relay.GatewayEvent) error {
	if err := s.check(); err != nil {
		return err
	}
	n, err := s.addCounter("EV#"+name, 1)
	if err != nil {
		return err
	}
	if err := s.putEvent(name, n, e); err != nil {
		return err
	}
	if old := n - relay.MaxEventsPerGateway; old > 0 {
		ctx, cancel := s.ctx()
		defer cancel()
		_, err = s.db.DeleteItem(ctx, &dynamodb.DeleteItemInput{TableName: &s.table, Key: key(eventsPK(name), pad(old))})
	}
	return err
}

func eventItem(gw string, id int64, e relay.GatewayEvent) item {
	it := key(eventsPK(gw), pad(id))
	it["at"] = num(ms(e.At))
	it["kind"] = str(e.Kind)
	it["detail"] = str(e.Detail)
	return it
}

func (s *Store) putEvent(gw string, id int64, e relay.GatewayEvent) error {
	ctx, cancel := s.ctx()
	defer cancel()
	_, err := s.db.PutItem(ctx, &dynamodb.PutItemInput{TableName: &s.table, Item: eventItem(gw, id, e)})
	return err
}

// GatewayEvents returns a gateway's most recent events, newest first.
func (s *Store) GatewayEvents(name string, limit int) ([]relay.GatewayEvent, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	out := []relay.GatewayEvent{}
	if limit <= 0 {
		return out, nil
	}
	err := s.query(&dynamodb.QueryInput{
		KeyConditionExpression:    aws.String("pk = :pk"),
		ExpressionAttributeValues: item{":pk": str(eventsPK(name))},
		ScanIndexForward:          aws.Bool(false),
		Limit:                     aws.Int32(int32(min(limit, 1000))),
	}, func(it item) bool {
		out = append(out, relay.GatewayEvent{At: fromMS(getN(it, "at")), Kind: getS(it, "kind"), Detail: getS(it, "detail")})
		return len(out) < limit
	})
	return out, err
}

// --- clients -------------------------------------------------------------------

func clientItem(name string, role relay.Role, hash string, created time.Time) item {
	it := key(pkClient, name)
	it["role"] = str(string(role))
	it["tokenHash"] = str(hash)
	it["createdAt"] = num(ms(created))
	return it
}

func clientFrom(it item) relay.ClientRecord {
	return relay.ClientRecord{Name: getS(it, "sk"), Role: relay.Role(getS(it, "role")), CreatedAt: fromMS(getN(it, "createdAt"))}
}

// CreateClient inserts a client and its token pointer.
func (s *Store) CreateClient(name string, role relay.Role, hash string, now time.Time) error {
	if err := s.check(); err != nil {
		return err
	}
	err := s.transact(s.putNew(clientItem(name, role, hash, now)), s.putNew(tokenItem(clientTokenSK(hash), name)))
	if _, ok := txFailed(err); ok {
		return relay.ErrExists
	}
	return err
}

// ClientByToken resolves a client token hash.
func (s *Store) ClientByToken(hash string) (*relay.ClientRecord, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	p, err := s.get(pkToken, clientTokenSK(hash))
	if err != nil {
		return nil, err
	}
	it, err := s.get(pkClient, getS(p, "name"))
	if err != nil {
		return nil, err
	}
	if getS(it, "tokenHash") != hash {
		return nil, relay.ErrNotFound
	}
	c := clientFrom(it)
	return &c, nil
}

// Clients lists client tokens by name (never their hashes).
func (s *Store) Clients() ([]relay.ClientRecord, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	out := []relay.ClientRecord{}
	err := s.queryPK(pkClient, func(it item) bool { out = append(out, clientFrom(it)); return true })
	return out, err
}

// DeleteClient revokes a client token.
func (s *Store) DeleteClient(name string) error {
	if err := s.check(); err != nil {
		return err
	}
	it, err := s.get(pkClient, name)
	if err != nil {
		return err
	}
	err = s.transact(
		types.TransactWriteItem{Delete: &types.Delete{TableName: &s.table, Key: key(pkClient, name),
			ConditionExpression: aws.String("attribute_exists(pk)")}},
		types.TransactWriteItem{Delete: &types.Delete{TableName: &s.table, Key: key(pkToken, clientTokenSK(getS(it, "tokenHash")))}},
	)
	if _, ok := txFailed(err); ok {
		return relay.ErrNotFound
	}
	return err
}

// --- settings ------------------------------------------------------------------

// Setting returns a setting's JSON value, or ErrNotFound.
func (s *Store) Setting(k string) ([]byte, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	it, err := s.get(pkSetting, k)
	if err != nil {
		return nil, err
	}
	return []byte(getS(it, "value")), nil
}

// SetSetting stores a setting's JSON value.
func (s *Store) SetSetting(k string, value []byte) error {
	if err := s.check(); err != nil {
		return err
	}
	it := key(pkSetting, k)
	it["value"] = str(string(value))
	ctx, cancel := s.ctx()
	defer cancel()
	_, err := s.db.PutItem(ctx, &dynamodb.PutItemInput{TableName: &s.table, Item: it})
	return err
}
