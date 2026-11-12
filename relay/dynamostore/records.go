package dynamostore

import (
	"cmp"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/sierragridteam/sierra-meshrelay/relay"
)

// attemptTTLSlack keeps attempts a day past AttemptRetention before TTL may
// delete them; reads filter by time anyway.
const attemptTTLSlack = 24 * time.Hour

// --- leases --------------------------------------------------------------------

func leaseSK(started time.Time, id string) string { return pad(ms(started)) + "#" + id }

func leaseItem(l relay.LeaseRecord) item {
	it := key(pkLease, leaseSK(l.StartedAt, l.ID))
	it["id"] = str(l.ID)
	it["gateway"] = str(l.Gateway)
	it["client"] = str(l.Client)
	it["role"] = str(string(l.Role))
	it["startedAt"] = num(ms(l.StartedAt))
	if l.EndedAt != nil {
		it["endedAt"] = num(ms(*l.EndedAt))
		it["endReason"] = str(l.EndReason)
		it["framesTo"] = num(l.FramesToNode)
		it["framesFrom"] = num(l.FramesFromNode)
		it["bytesTo"] = num(l.BytesToNode)
		it["bytesFrom"] = num(l.BytesFromNode)
	}
	return it
}

func leaseOpenItem(l relay.LeaseRecord) item {
	it := key(pkLeaseOpen, l.ID)
	it["leaseSK"] = str(leaseSK(l.StartedAt, l.ID))
	return it
}

func leaseFrom(it item) relay.LeaseRecord {
	l := relay.LeaseRecord{ID: getS(it, "id"), Gateway: getS(it, "gateway"), Client: getS(it, "client"),
		Role: relay.Role(getS(it, "role")), StartedAt: fromMS(getN(it, "startedAt")), EndReason: getS(it, "endReason"),
		FramesToNode: getN(it, "framesTo"), FramesFromNode: getN(it, "framesFrom"),
		BytesToNode: getN(it, "bytesTo"), BytesFromNode: getN(it, "bytesFrom")}
	if _, ok := it["endedAt"]; ok {
		l.EndedAt = timePtr(getN(it, "endedAt"))
	}
	return l
}

// InsertLease records a lease's start, with an open-lease marker that
// CloseOrphanLeases reads instead of the whole log.
func (s *Store) InsertLease(l relay.LeaseRecord) error {
	if err := s.check(); err != nil {
		return err
	}
	l.EndedAt = nil
	return s.transact(
		types.TransactWriteItem{Put: &types.Put{TableName: &s.table, Item: leaseItem(l)}},
		types.TransactWriteItem{Put: &types.Put{TableName: &s.table, Item: leaseOpenItem(l)}},
	)
}

// EndLease records a lease's end and clears its open marker. A lease that
// was never recorded is a no-op, as in SQLite.
func (s *Store) EndLease(l relay.LeaseRecord) error {
	if err := s.check(); err != nil {
		return err
	}
	if l.EndedAt == nil {
		return errors.New("dynamostore: EndLease needs EndedAt")
	}
	sk := ""
	if l.StartedAt.IsZero() {
		p, err := s.get(pkLeaseOpen, l.ID)
		if errors.Is(err, relay.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		sk = getS(p, "leaseSK")
	} else {
		sk = leaseSK(l.StartedAt, l.ID)
	}
	return s.endLease(sk, l.ID, ms(*l.EndedAt), l.EndReason, l.FramesToNode, l.FramesFromNode,
		l.BytesToNode, l.BytesFromNode, "attribute_exists(pk)")
}

func (s *Store) endLease(sk, id string, ended int64, reason string, ft, ff, bt, bf int64, cond string) error {
	err := s.transact(
		types.TransactWriteItem{Update: &types.Update{TableName: &s.table, Key: key(pkLease, sk),
			UpdateExpression: aws.String("SET endedAt = :e, endReason = :r, framesTo = :ft, framesFrom = :ff, " +
				"bytesTo = :bt, bytesFrom = :bf"),
			ConditionExpression: aws.String(cond),
			ExpressionAttributeValues: item{":e": num(ended), ":r": str(reason), ":ft": num(ft), ":ff": num(ff),
				":bt": num(bt), ":bf": num(bf)}}},
		types.TransactWriteItem{Delete: &types.Delete{TableName: &s.table, Key: key(pkLeaseOpen, id)}},
	)
	if _, ok := txFailed(err); ok {
		return nil // never recorded (or already ended): nothing to do
	}
	return err
}

// CloseOrphanLeases ends every lease the previous process left open.
func (s *Store) CloseOrphanLeases(now time.Time) (int64, error) {
	if err := s.check(); err != nil {
		return 0, err
	}
	type open struct{ id, sk string }
	var opens []open
	if err := s.queryPK(pkLeaseOpen, func(it item) bool {
		opens = append(opens, open{getS(it, "sk"), getS(it, "leaseSK")})
		return true
	}); err != nil {
		return 0, err
	}
	var n int64
	for _, o := range opens {
		// Count only leases this call ended; a lease row that is missing or
		// already ended just loses its stale marker.
		l, err := s.get(pkLease, o.sk)
		if err != nil && !errors.Is(err, relay.ErrNotFound) {
			return n, err
		}
		if err == nil {
			if _, ended := l["endedAt"]; !ended {
				if err := s.endLease(o.sk, o.id, ms(now), "relay_restart", 0, 0, 0, 0,
					"attribute_exists(pk) AND attribute_not_exists(endedAt)"); err != nil {
					return n, err
				}
				n++
				continue
			}
		}
		ctx, cancel := s.ctx()
		_, err = s.db.DeleteItem(ctx, &dynamodb.DeleteItemInput{TableName: &s.table, Key: key(pkLeaseOpen, o.id)})
		cancel()
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// RecentLeases returns finished leases, newest start first.
func (s *Store) RecentLeases(limit int) ([]relay.LeaseRecord, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	out := []relay.LeaseRecord{}
	if limit <= 0 {
		return out, nil
	}
	err := s.query(&dynamodb.QueryInput{
		KeyConditionExpression:    aws.String("pk = :pk"),
		FilterExpression:          aws.String("attribute_exists(endedAt)"),
		ExpressionAttributeValues: item{":pk": str(pkLease)},
		ScanIndexForward:          aws.Bool(false),
		Limit:                     aws.Int32(int32(min(limit+16, 1000))),
	}, func(it item) bool {
		out = append(out, leaseFrom(it))
		return len(out) < limit
	})
	return out, err
}

// --- invites -------------------------------------------------------------------

func inviteItem(i relay.InviteRecord) item {
	it := key(pkInvite, i.ID)
	it["passphraseHash"] = str(i.PassphraseHash)
	it["gateway"] = str(i.Gateway)
	it["namePrefix"] = str(i.NamePrefix)
	it["maxUses"] = num(int64(i.MaxUses))
	it["uses"] = num(int64(i.Uses))
	it["expiresAt"] = num(ms(i.ExpiresAt))
	it["expectedPubkey"] = str(i.ExpectedPubkey)
	it["note"] = str(i.Note)
	it["revoked"] = boolean(i.Revoked)
	it["createdBy"] = str(i.CreatedBy)
	it["createdAt"] = num(ms(i.CreatedAt))
	return it
}

func inviteFrom(it item) *relay.InviteRecord {
	return &relay.InviteRecord{ID: getS(it, "sk"), PassphraseHash: getS(it, "passphraseHash"),
		Gateway: getS(it, "gateway"), NamePrefix: getS(it, "namePrefix"), MaxUses: int(getN(it, "maxUses")),
		Uses: int(getN(it, "uses")), ExpiresAt: fromMS(getN(it, "expiresAt")),
		ExpectedPubkey: getS(it, "expectedPubkey"), Note: getS(it, "note"), Revoked: getBool(it, "revoked"),
		CreatedBy: getS(it, "createdBy"), CreatedAt: fromMS(getN(it, "createdAt"))}
}

// CreateInvite stores a new invite (no uses, not revoked).
func (s *Store) CreateInvite(i relay.InviteRecord) error {
	if err := s.check(); err != nil {
		return err
	}
	i.Uses, i.Revoked = 0, false
	ctx, cancel := s.ctx()
	defer cancel()
	_, err := s.db.PutItem(ctx, &dynamodb.PutItemInput{TableName: &s.table, Item: inviteItem(i),
		ConditionExpression: aws.String("attribute_not_exists(pk)")})
	if condFailed(err) {
		return relay.ErrExists
	}
	return err
}

func (s *Store) allInvites() ([]*relay.InviteRecord, error) {
	var out []*relay.InviteRecord
	err := s.queryPK(pkInvite, func(it item) bool { out = append(out, inviteFrom(it)); return true })
	return out, err
}

// Invites lists invites, newest first.
func (s *Store) Invites() ([]*relay.InviteRecord, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	out, err := s.allInvites()
	slices.SortStableFunc(out, func(a, b *relay.InviteRecord) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return out, err
}

// ActiveInvites lists invites that can still be redeemed at now, oldest first.
func (s *Store) ActiveInvites(now time.Time) ([]*relay.InviteRecord, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	all, err := s.allInvites()
	var out []*relay.InviteRecord
	for _, i := range all {
		if i.Active(now) {
			out = append(out, i)
		}
	}
	slices.SortStableFunc(out, func(a, b *relay.InviteRecord) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out, err
}

// RevokeInvite marks an invite unusable.
func (s *Store) RevokeInvite(id string) error {
	if err := s.check(); err != nil {
		return err
	}
	return s.updateExisting(pkInvite, id, "SET revoked = :t", nil, item{":t": boolean(true)})
}

// UseInvite consumes one use in a single conditional update, so two racing
// redemptions can't both take the last use.
func (s *Store) UseInvite(id string, now time.Time) error {
	if err := s.check(); err != nil {
		return err
	}
	ctx, cancel := s.ctx()
	defer cancel()
	_, err := s.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:        &s.table,
		Key:              key(pkInvite, id),
		UpdateExpression: aws.String("ADD uses :one"),
		ConditionExpression: aws.String("attribute_exists(pk) AND revoked = :f AND uses < maxUses " +
			"AND expiresAt > :now"),
		ExpressionAttributeValues: item{":one": num(1), ":f": boolean(false), ":now": num(ms(now))},
	})
	if condFailed(err) {
		return relay.ErrNotFound
	}
	return err
}

// --- repeaters -----------------------------------------------------------------

func repeaterFrom(it item) *relay.RepeaterRecord {
	return &relay.RepeaterRecord{Pubkey: getS(it, "sk"), Name: getS(it, "name"), Access: getS(it, "access"),
		AddedByGateway: getS(it, "addedByGateway"), AddedByClient: getS(it, "addedByClient"),
		CreatedAt: fromMS(getN(it, "createdAt")), UpdatedAt: fromMS(getN(it, "updatedAt")),
		LastAttemptAt: timePtr(getN(it, "lastAttemptAt")), LastOKAt: timePtr(getN(it, "lastOkAt")),
		LastGateway: getS(it, "lastGateway"), LastError: getS(it, "lastError"), Sealed: getB(it, "sealed")}
}

func repeaterItem(r *relay.RepeaterRecord) item {
	it := key(pkRepeater, r.Pubkey)
	it["name"] = str(r.Name)
	it["sealed"] = &types.AttributeValueMemberB{Value: r.Sealed}
	it["access"] = str(r.Access)
	it["addedByGateway"] = str(r.AddedByGateway)
	it["addedByClient"] = str(r.AddedByClient)
	it["createdAt"] = num(ms(r.CreatedAt))
	it["updatedAt"] = num(ms(r.UpdatedAt))
	var attempt, ok int64
	if r.LastAttemptAt != nil {
		attempt = ms(*r.LastAttemptAt)
	}
	if r.LastOKAt != nil {
		ok = ms(*r.LastOKAt)
	}
	it["lastAttemptAt"] = num(attempt)
	it["lastOkAt"] = num(ok)
	it["lastGateway"] = str(r.LastGateway)
	it["lastError"] = str(r.LastError)
	return it
}

// Repeater returns one shared repeater.
func (s *Store) Repeater(pubkey string) (*relay.RepeaterRecord, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	it, err := s.get(pkRepeater, pubkey)
	if err != nil {
		return nil, err
	}
	return repeaterFrom(it), nil
}

// Repeaters lists shared repeaters by name, then pubkey.
func (s *Store) Repeaters() ([]*relay.RepeaterRecord, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	var out []*relay.RepeaterRecord
	err := s.queryPK(pkRepeater, func(it item) bool { out = append(out, repeaterFrom(it)); return true })
	slices.SortStableFunc(out, func(a, b *relay.RepeaterRecord) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.Pubkey, b.Pubkey))
	})
	return out, err
}

// PutRepeater inserts or replaces a repeater's password and who added it,
// keeping its created time and last attempt.
func (s *Store) PutRepeater(r relay.RepeaterRecord, now time.Time) error {
	if err := s.check(); err != nil {
		return err
	}
	ctx, cancel := s.ctx()
	defer cancel()
	sealed := r.Sealed
	if sealed == nil {
		sealed = []byte{}
	}
	_, err := s.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: &s.table,
		Key:       key(pkRepeater, r.Pubkey),
		UpdateExpression: aws.String("SET #n = :name, sealed = :sealed, #a = :access, addedByGateway = :bg, " +
			"addedByClient = :bc, updatedAt = :now, lastOkAt = :now, lastGateway = :lg, lastError = :empty, " +
			"createdAt = if_not_exists(createdAt, :now), lastAttemptAt = if_not_exists(lastAttemptAt, :zero)"),
		ExpressionAttributeNames: map[string]string{"#n": "name", "#a": "access"},
		ExpressionAttributeValues: item{":name": str(r.Name), ":sealed": &types.AttributeValueMemberB{Value: sealed},
			":access": str(r.Access), ":bg": str(r.AddedByGateway), ":bc": str(r.AddedByClient),
			":now": num(ms(now)), ":lg": str(r.LastGateway), ":empty": str(""), ":zero": num(0)},
	})
	return err
}

// DeleteRepeater removes a repeater, then its attempts and poll state.
func (s *Store) DeleteRepeater(pubkey string) error {
	if err := s.check(); err != nil {
		return err
	}
	ctx, cancel := s.ctx()
	_, err := s.db.DeleteItem(ctx, &dynamodb.DeleteItemInput{TableName: &s.table, Key: key(pkRepeater, pubkey),
		ConditionExpression: aws.String("attribute_exists(pk)")})
	cancel()
	if condFailed(err) {
		return relay.ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := s.deletePartition(attemptsPK(pubkey)); err != nil {
		return err
	}
	ctx, cancel = s.ctx()
	defer cancel()
	_, err = s.db.DeleteItem(ctx, &dynamodb.DeleteItemInput{TableName: &s.table, Key: key(pkPollState, pubkey)})
	return err
}

// RecordRepeaterPoll notes a poll's outcome: errText "" is a success.
func (s *Store) RecordRepeaterPoll(pubkey, gateway string, at time.Time, errText string) error {
	if err := s.check(); err != nil {
		return err
	}
	if errText == "" {
		return ignoreMissing(s.updateExisting(pkRepeater, pubkey,
			"SET lastAttemptAt = :at, lastOkAt = :at, lastGateway = :gw, lastError = :empty", nil,
			item{":at": num(ms(at)), ":gw": str(gateway), ":empty": str("")}))
	}
	return ignoreMissing(s.updateExisting(pkRepeater, pubkey, "SET lastAttemptAt = :at, lastError = :e", nil,
		item{":at": num(ms(at)), ":e": str(errText)}))
}

// --- repeater polling ----------------------------------------------------------

func (s *Store) attemptItem(a relay.RepeaterAttempt) item {
	it := key(attemptsPK(a.Pubkey), pad(ms(a.At))+"#"+pad(int64(s.nextSeq()&(1<<62-1))))
	it["at"] = num(ms(a.At))
	it["gateway"] = str(a.Gateway)
	it["client"] = str(a.Client)
	it["outcome"] = str(a.Outcome)
	it["step"] = str(a.Step)
	it["flooded"] = boolean(a.Flooded)
	it["pathReset"] = boolean(a.PathReset)
	it["durationMs"] = num(a.Duration.Milliseconds())
	it["expires"] = num(a.At.Add(relay.AttemptRetention + attemptTTLSlack).Unix())
	return it
}

// RecordRepeaterAttempt appends one attempt; TTL deletes it after
// AttemptRetention (plus a day), in place of SQLite's hourly prune.
func (s *Store) RecordRepeaterAttempt(a relay.RepeaterAttempt) error {
	if err := s.check(); err != nil {
		return err
	}
	ctx, cancel := s.ctx()
	defer cancel()
	_, err := s.db.PutItem(ctx, &dynamodb.PutItemInput{TableName: &s.table, Item: s.attemptItem(a)})
	return err
}

// RepeaterAttempts lists a repeater's attempts since a time, oldest first.
func (s *Store) RepeaterAttempts(pubkey string, since time.Time) ([]relay.RepeaterAttempt, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	var out []relay.RepeaterAttempt
	err := s.query(&dynamodb.QueryInput{
		KeyConditionExpression:    aws.String("pk = :pk AND sk >= :since"),
		ExpressionAttributeValues: item{":pk": str(attemptsPK(pubkey)), ":since": str(pad(ms(since)))},
	}, func(it item) bool {
		out = append(out, relay.RepeaterAttempt{Pubkey: pubkey, At: fromMS(getN(it, "at")),
			Gateway: getS(it, "gateway"), Client: getS(it, "client"), Outcome: getS(it, "outcome"),
			Step: getS(it, "step"), Flooded: getBool(it, "flooded"), PathReset: getBool(it, "pathReset"),
			Duration: time.Duration(getN(it, "durationMs")) * time.Millisecond})
		return true
	})
	return out, err
}

// RepeaterPollState returns a repeater's saved schedule; the zero state if
// none is saved.
func (s *Store) RepeaterPollState(pubkey string) (relay.RepeaterPollState, error) {
	var st relay.RepeaterPollState
	if err := s.check(); err != nil {
		return st, err
	}
	it, err := s.get(pkPollState, pubkey)
	if errors.Is(err, relay.ErrNotFound) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal([]byte(getS(it, "state")), &st)
}

// SetRepeaterPollState saves a repeater's schedule.
func (s *Store) SetRepeaterPollState(pubkey string, st relay.RepeaterPollState, now time.Time) error {
	if err := s.check(); err != nil {
		return err
	}
	b, _ := json.Marshal(st)
	it := key(pkPollState, pubkey)
	it["state"] = str(string(b))
	it["updatedAt"] = num(ms(now))
	ctx, cancel := s.ctx()
	defer cancel()
	_, err := s.db.PutItem(ctx, &dynamodb.PutItemInput{TableName: &s.table, Item: it})
	return err
}

// --- bridge logs ---------------------------------------------------------------

func logItem(gw string, at time.Time, e relay.BridgeLog) item {
	it := key(logsPK(gw), pad(e.ID))
	it["id"] = num(e.ID)
	it["receivedAt"] = num(ms(at))
	it["t"] = str(e.T)
	it["level"] = str(e.Level)
	it["msg"] = str(e.Msg)
	attrs := ""
	if len(e.Attrs) > 0 {
		b, _ := json.Marshal(e.Attrs)
		attrs = string(b)
	}
	it["attrs"] = str(attrs)
	return it
}

// AddBridgeLogs stores lines under IDs from the gateway's counter, and
// deletes the lines those IDs push past MaxLogsPerGateway: IDs are
// contiguous, so the cap is exact without reading anything.
func (s *Store) AddBridgeLogs(gateway string, at time.Time, es []relay.BridgeLog) error {
	if err := s.check(); err != nil {
		return err
	}
	if len(es) == 0 {
		return nil
	}
	n := int64(len(es))
	last, err := s.addCounter("LOG#"+gateway, n)
	if err != nil {
		return err
	}
	reqs := make([]types.WriteRequest, 0, 2*len(es))
	for i, e := range es {
		e.ID = last - n + 1 + int64(i)
		reqs = append(reqs, putReq(logItem(gateway, at, e)))
	}
	keep := int64(relay.MaxLogsPerGateway)
	for id := max(1, last-n+1-keep); id <= last-keep; id++ {
		reqs = append(reqs, deleteReq(logsPK(gateway), pad(id)))
	}
	return s.batchWrite(reqs)
}

// BridgeLogs returns a gateway's lines after afterID, oldest first, at most
// limit of the newest.
func (s *Store) BridgeLogs(gateway string, afterID int64, limit int) ([]relay.BridgeLog, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	out := []relay.BridgeLog{}
	if limit <= 0 {
		return out, nil
	}
	err := s.query(&dynamodb.QueryInput{
		KeyConditionExpression:    aws.String("pk = :pk AND sk > :after"),
		ExpressionAttributeValues: item{":pk": str(logsPK(gateway)), ":after": str(pad(max(afterID, 0)))},
		ScanIndexForward:          aws.Bool(false),
		Limit:                     aws.Int32(int32(min(limit, 1000))),
	}, func(it item) bool {
		e := relay.BridgeLog{ID: getN(it, "id"), ReceivedAt: fromMS(getN(it, "receivedAt")), T: getS(it, "t"),
			Level: getS(it, "level"), Msg: getS(it, "msg")}
		if a := getS(it, "attrs"); a != "" {
			_ = json.Unmarshal([]byte(a), &e.Attrs)
		}
		out = append(out, e)
		return len(out) < limit
	})
	slices.Reverse(out)
	return out, err
}
