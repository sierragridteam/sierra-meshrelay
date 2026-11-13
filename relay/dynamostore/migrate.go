package dynamostore

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/sierragridteam/sierra-meshrelay/relay"
)

// importedLogTTL ages out copied bridge log lines: their IDs come from the
// SQLite file's global sequence, so the per-gateway cap (which deletes by
// contiguous ID) never reaches them.
const importedLogTTL = 30 * 24 * time.Hour

// ErrNotEmpty means Import found relay data already in the table.
var ErrNotEmpty = errors.New("dynamostore: the table already has relay data")

// Import copies a relay.db snapshot into the store's (empty) table. The
// store holds the instance lease, so no relay is running on the table
// meanwhile. Rows are written as they were: names, token hashes, created
// times, sealed passwords byte for byte, invite use counts, lease IDs.
func (s *Store) Import(snap *relay.Snapshot) error {
	if err := s.check(); err != nil {
		return err
	}
	for _, pk := range []string{pkGateway, pkClient, pkInvite, pkRepeater, pkSetting, pkLease} {
		empty := true
		if err := s.query(&dynamodb.QueryInput{
			KeyConditionExpression:    aws.String("pk = :pk"),
			ExpressionAttributeValues: item{":pk": str(pk)},
			Limit:                     aws.Int32(1),
		}, func(item) bool { empty = false; return false }); err != nil {
			return err
		}
		if !empty {
			return fmt.Errorf("%w (partition %s)", ErrNotEmpty, pk)
		}
	}

	var reqs []types.WriteRequest
	add := func(it item) { reqs = append(reqs, putReq(it)) }
	for _, g := range snap.Gateways {
		add(gatewayItem(g))
		add(tokenItem(bridgeTokenSK(g.TokenHash), g.Name))
	}
	for _, c := range snap.Clients {
		add(clientItem(c.ClientRecord, c.TokenHash))
		add(tokenItem(clientTokenSK(c.TokenHash), c.Name))
	}
	for _, i := range snap.Invites {
		add(inviteItem(*i))
	}
	for _, r := range snap.Repeaters {
		add(repeaterItem(r))
	}
	for pk, raw := range snap.PollStates {
		it := key(pkPollState, pk)
		it["state"] = str(string(raw))
		it["updatedAt"] = num(ms(s.now()))
		add(it)
	}
	for _, a := range snap.Attempts {
		add(s.attemptItem(a))
	}
	for gw, es := range snap.Events {
		for i, e := range es {
			add(eventItem(gw, int64(i+1), e))
		}
		add(counterItem("EV#"+gw, int64(len(es))))
	}
	logExpires := s.now().Add(importedLogTTL).Unix()
	for gw, es := range snap.BridgeLogs {
		for _, e := range es {
			it := logItem(gw, e.ReceivedAt, e)
			it[TTLAttribute] = num(logExpires)
			add(it)
		}
	}
	// Every gateway's log counter starts past the file's high-water mark, so
	// an admin's `after` cursor from the old relay never hides new lines.
	logGateways := map[string]bool{}
	for _, g := range snap.Gateways {
		logGateways[g.Name] = true
	}
	for gw := range snap.BridgeLogs {
		logGateways[gw] = true
	}
	for gw := range logGateways {
		add(counterItem("LOG#"+gw, snap.MaxBridgeLogID))
	}
	for _, l := range snap.Leases {
		add(leaseItem(l))
		if l.EndedAt == nil {
			add(leaseOpenItem(l))
		}
	}
	for k, raw := range snap.Settings {
		it := key(pkSetting, k)
		it["value"] = str(string(raw))
		add(it)
	}
	return s.batchWrite(reqs)
}

func counterItem(name string, n int64) item {
	it := key(pkSeq, name)
	it["n"] = num(n)
	return it
}

// VerifyReport is what Verify checked.
type VerifyReport struct {
	Counts map[string][2]int // table -> {snapshot, store}
	// SealedOK counts repeaters whose sealed password opened with the key;
	// KeyChecked is false when no key was given (nothing was opened).
	SealedOK   int
	KeyChecked bool
	Problems   []string
}

// OK reports whether nothing failed.
func (r VerifyReport) OK() bool { return len(r.Problems) == 0 }

func (r VerifyReport) String() string {
	var b strings.Builder
	names := make([]string, 0, len(r.Counts))
	for k := range r.Counts {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		c := r.Counts[k]
		mark := "ok"
		if c[0] != c[1] {
			mark = "MISMATCH"
		}
		fmt.Fprintf(&b, "  %-20s snapshot %6d  dynamodb %6d  %s\n", k, c[0], c[1], mark)
	}
	if r.KeyChecked {
		fmt.Fprintf(&b, "  sealed passwords that open with the key: %d\n", r.SealedOK)
	} else {
		fmt.Fprintf(&b, "  sealed passwords NOT checked against a key (copied byte for byte only)\n")
	}
	for _, p := range r.Problems {
		fmt.Fprintf(&b, "  PROBLEM: %s\n", p)
	}
	return b.String()
}

// Verify reads everything back through the Storage methods the relay uses
// and compares it with the snapshot: counts per table, every token hash
// resolving to its gateway or client, every sealed password byte-identical
// and, with secretKey, opening with it (secretKey nil skips that check and
// says so in the report).
func (s *Store) Verify(snap *relay.Snapshot, secretKey []byte) (VerifyReport, error) {
	rep := VerifyReport{Counts: map[string][2]int{}}
	problem := func(format string, a ...any) { rep.Problems = append(rep.Problems, fmt.Sprintf(format, a...)) }
	count := func(name string, want, got int) {
		rep.Counts[name] = [2]int{want, got}
		if want != got {
			problem("%s: %d in the snapshot, %d in DynamoDB", name, want, got)
		}
	}

	gws, err := s.Gateways()
	if err != nil {
		return rep, err
	}
	count("gateways", len(snap.Gateways), len(gws))
	for _, g := range snap.Gateways {
		got, err := s.GatewayByToken(g.TokenHash)
		if err != nil || got.Name != g.Name || got.PinnedPubkey != g.PinnedPubkey {
			problem("gateway %s: its bridge token does not resolve to it (%v)", g.Name, err)
		}
	}
	cs, err := s.Clients()
	if err != nil {
		return rep, err
	}
	count("clients", len(snap.Clients), len(cs))
	for _, c := range snap.Clients {
		got, err := s.ClientByToken(c.TokenHash)
		if err != nil || got.Name != c.Name || got.Role != c.Role || got.Gateway != c.Gateway || !got.ExpiresAt.Equal(c.ExpiresAt) {
			problem("client %s: its token does not resolve to it (%v)", c.Name, err)
		}
	}
	invs, err := s.Invites()
	if err != nil {
		return rep, err
	}
	count("invites", len(snap.Invites), len(invs))

	reps, err := s.Repeaters()
	if err != nil {
		return rep, err
	}
	count("repeaters", len(snap.Repeaters), len(reps))
	rep.KeyChecked = secretKey != nil
	for _, want := range snap.Repeaters {
		got, err := s.Repeater(want.Pubkey)
		if err != nil {
			problem("repeater %s: %v", want.Pubkey, err)
			continue
		}
		if !bytes.Equal(got.Sealed, want.Sealed) {
			problem("repeater %s: sealed password differs from the snapshot", want.Pubkey)
			continue
		}
		if secretKey != nil {
			if err := relay.CheckSealed(secretKey, got.Pubkey, got.Sealed); err != nil {
				problem("repeater %s: %v", got.Pubkey, err)
				continue
			}
			rep.SealedOK++
		}
	}

	polls := 0
	for pk := range snap.PollStates {
		if _, err := s.get(pkPollState, pk); err == nil {
			polls++
		}
	}
	count("repeater_poll_state", len(snap.PollStates), polls)

	attempts := 0
	byPK := map[string]bool{}
	for _, a := range snap.Attempts {
		byPK[a.Pubkey] = true
	}
	for pk := range byPK {
		as, err := s.RepeaterAttempts(pk, time.Time{})
		if err != nil {
			return rep, err
		}
		attempts += len(as)
	}
	count("repeater_attempts", len(snap.Attempts), attempts)

	events, logs := 0, 0
	wantEvents, wantLogs := 0, 0
	for gw, es := range snap.Events {
		wantEvents += len(es)
		got, err := s.GatewayEvents(gw, relay.MaxEventsPerGateway)
		if err != nil {
			return rep, err
		}
		events += len(got)
	}
	count("gateway_events", wantEvents, events)
	for gw, es := range snap.BridgeLogs {
		wantLogs += len(es)
		got, err := s.BridgeLogs(gw, 0, 10*relay.MaxLogsPerGateway)
		if err != nil {
			return rep, err
		}
		logs += len(got)
	}
	count("bridge_logs", wantLogs, logs)

	ended := 0
	open := 0
	for _, l := range snap.Leases {
		if l.EndedAt != nil {
			ended++
		} else {
			open++
		}
	}
	recent, err := s.RecentLeases(ended + 1)
	if err != nil {
		return rep, err
	}
	count("leases (ended)", ended, len(recent))
	gotOpen := 0
	if err := s.queryPK(pkLeaseOpen, func(item) bool { gotOpen++; return true }); err != nil {
		return rep, err
	}
	count("leases (open)", open, gotOpen)

	settings := 0
	for k, want := range snap.Settings {
		got, err := s.Setting(k)
		if err != nil {
			problem("setting %s: %v", k, err)
			continue
		}
		if !bytes.Equal(got, want) {
			problem("setting %s differs", k)
			continue
		}
		settings++
	}
	count("settings", len(snap.Settings), settings)
	return rep, nil
}
