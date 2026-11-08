package relay

// Poll bookkeeping for shared repeaters: every check's outcome (attempts,
// kept a week) and the poller's saved schedule. The poller (sierra-data)
// owns the policy; the relay keeps what it needs across restarts and shows
// admins how each gateway's link to a repeater is doing.

import (
	"database/sql"
	"encoding/json"
	stderrors "errors"
	"slices"
	"time"

	"github.com/sierragridteam/sierra-meshrelay/meshops"
)

const repeaterPollSchema = `
CREATE TABLE IF NOT EXISTS repeater_poll_state (
	pubkey     TEXT PRIMARY KEY,
	state      TEXT NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS repeater_attempts (
	pubkey      TEXT NOT NULL,
	at          INTEGER NOT NULL,
	gateway     TEXT NOT NULL,
	client      TEXT NOT NULL DEFAULT '',
	outcome     TEXT NOT NULL,
	step        TEXT NOT NULL DEFAULT '',
	flooded     INTEGER NOT NULL DEFAULT 0,
	path_reset  INTEGER NOT NULL DEFAULT 0,
	duration_ms INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_repeater_attempts ON repeater_attempts (pubkey, at);
`

// attemptRetention is how long attempts are kept.
const attemptRetention = 7 * 24 * time.Hour

// Attempt outcomes. Only ok, no_reply and refused sent anything over the
// mesh; the rest say nothing about the repeater.
const (
	AttemptOK          = "ok"
	AttemptNoReply     = "no_reply" // the repeater didn't answer (login or status)
	AttemptRefused     = "refused"  // wrong password
	AttemptNotKnown    = "not_known"
	AttemptUnavailable = "unavailable" // the gateway was busy, paused, offline or taken over
	AttemptError       = "error"       // the node itself failed (contacts full, no reply from the node)
)

// RepeaterAttempt is one check of a repeater through one gateway.
type RepeaterAttempt struct {
	Pubkey    string        `json:"pubkey"`
	At        time.Time     `json:"at"`
	Gateway   string        `json:"gateway"`
	Client    string        `json:"client"`
	Outcome   string        `json:"outcome"`
	Step      string        `json:"step,omitempty"` // where it stopped: contact, login, status
	Flooded   bool          `json:"flooded"`        // the login went by flood (no route, or reset)
	PathReset bool          `json:"path_reset,omitempty"`
	Duration  time.Duration `json:"duration_ms"`
}

// attemptOutcome classifies a check's error.
func attemptOutcome(err error) string {
	var ue *UnavailableError
	switch {
	case err == nil:
		return AttemptOK
	case stderrors.Is(err, ErrNotKnown):
		return AttemptNotKnown
	case stderrors.Is(err, meshops.ErrNoMeshReply):
		return AttemptNoReply
	case stderrors.Is(err, meshops.ErrLoginFailed):
		return AttemptRefused
	case stderrors.Is(err, ErrBusy), stderrors.As(err, &ue), stderrors.Is(err, ErrLeaseEnded):
		return AttemptUnavailable
	}
	return AttemptError
}

// RepeaterLink is how one gateway's checks of a repeater went over a window.
type RepeaterLink struct {
	Gateway string `json:"gateway"`
	// Tries counts checks that went over the mesh; OK of them succeeded.
	Tries     int        `json:"tries"`
	OK        int        `json:"ok"`
	Flooded   int        `json:"flooded"`
	MedianMS  int64      `json:"median_ms,omitempty"` // of successful checks
	LastOK    *time.Time `json:"last_ok,omitempty"`
	LastTry   *time.Time `json:"last_try,omitempty"`
	LastError string     `json:"last_outcome,omitempty"` // the last try's outcome when it failed
}

// RepeaterPollState is the poller's saved schedule for a repeater. The
// relay stores it and shows it to admins; the poller decides what it means.
type RepeaterPollState struct {
	// Primary is the gateway tried first: the one that last reached it.
	Primary string `json:"primary,omitempty"`
	// NextCycle is when the next polling cycle starts.
	NextCycle time.Time `json:"next_cycle"`
	// FailedCycles counts consecutive cycles that went over the mesh and
	// never read it. A cycle in which no gateway could try doesn't count.
	FailedCycles int `json:"failed_cycles,omitempty"`
	// HibernatingSince is set while it's polled once a day: it hasn't been
	// read for a while and nothing else reports it alive.
	HibernatingSince *time.Time `json:"hibernating_since,omitempty"`
	// RefusedAt is when the repeater last refused the password; polling
	// stops until the share is updated.
	RefusedAt *time.Time `json:"refused_at,omitempty"`
	// WaitingSince is set while cycles end with no gateway able to try (its
	// bridge offline, its node off or paused, in use): nothing is known about
	// the repeater meanwhile. Waiting says why, per gateway.
	WaitingSince *time.Time `json:"waiting_since,omitempty"`
	Waiting      string     `json:"waiting,omitempty"`
	// Reading is the poller's last successful reading, opaque to the relay,
	// so a restarted poller reports it until the next read.
	Reading json.RawMessage `json:"reading,omitempty"`
}

// RecordRepeaterAttempt appends one attempt and prunes old ones.
func (s *Store) RecordRepeaterAttempt(a RepeaterAttempt) error {
	_, err := s.db.Exec(`INSERT INTO repeater_attempts (pubkey, at, gateway, client, outcome, step, flooded,
			path_reset, duration_ms) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.Pubkey, ms(a.At), a.Gateway, a.Client, a.Outcome, a.Step, a.Flooded, a.PathReset, a.Duration.Milliseconds())
	if err != nil {
		return err
	}
	s.attemptMu.Lock()
	due := a.At.Sub(s.attemptPruned) > time.Hour
	if due {
		s.attemptPruned = a.At
	}
	s.attemptMu.Unlock()
	if due {
		_, err = s.db.Exec(`DELETE FROM repeater_attempts WHERE at < ?`, ms(a.At.Add(-attemptRetention)))
	}
	return err
}

// RepeaterAttempts lists a repeater's attempts since a time, oldest first.
func (s *Store) RepeaterAttempts(pubkey string, since time.Time) ([]RepeaterAttempt, error) {
	rows, err := s.db.Query(`SELECT at, gateway, client, outcome, step, flooded, path_reset, duration_ms
		FROM repeater_attempts WHERE pubkey = ? AND at >= ? ORDER BY at`, pubkey, ms(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RepeaterAttempt
	for rows.Next() {
		a := RepeaterAttempt{Pubkey: pubkey}
		var at, dur int64
		if err := rows.Scan(&at, &a.Gateway, &a.Client, &a.Outcome, &a.Step, &a.Flooded, &a.PathReset, &dur); err != nil {
			return nil, err
		}
		a.At, a.Duration = fromMS(at), time.Duration(dur)*time.Millisecond
		out = append(out, a)
	}
	return out, rows.Err()
}

// RepeaterLinks summarizes a repeater's attempts since a time, by gateway
// (most tries first).
func (s *Store) RepeaterLinks(pubkey string, since time.Time) ([]RepeaterLink, error) {
	as, err := s.RepeaterAttempts(pubkey, since)
	if err != nil {
		return nil, err
	}
	return summarizeLinks(as), nil
}

func summarizeLinks(as []RepeaterAttempt) []RepeaterLink {
	byGW := map[string]*RepeaterLink{}
	okMS := map[string][]int64{}
	var order []string
	for _, a := range as {
		if a.Outcome != AttemptOK && a.Outcome != AttemptNoReply && a.Outcome != AttemptRefused {
			continue // nothing went over the mesh
		}
		l := byGW[a.Gateway]
		if l == nil {
			l = &RepeaterLink{Gateway: a.Gateway}
			byGW[a.Gateway] = l
			order = append(order, a.Gateway)
		}
		l.Tries++
		if a.Flooded {
			l.Flooded++
		}
		l.LastTry, l.LastError = timePtr(a.At), ""
		if a.Outcome == AttemptOK {
			l.OK++
			l.LastOK = timePtr(a.At)
			okMS[a.Gateway] = append(okMS[a.Gateway], a.Duration.Milliseconds())
		} else {
			l.LastError = a.Outcome
		}
	}
	out := make([]RepeaterLink, 0, len(order))
	for _, gw := range order {
		l := byGW[gw]
		if d := okMS[gw]; len(d) > 0 {
			slices.Sort(d)
			l.MedianMS = d[len(d)/2]
		}
		out = append(out, *l)
	}
	slices.SortStableFunc(out, func(a, b RepeaterLink) int { return b.Tries - a.Tries })
	return out
}

// RepeaterPollState returns a repeater's saved schedule; the zero state if
// none is saved.
func (s *Store) RepeaterPollState(pubkey string) (RepeaterPollState, error) {
	var st RepeaterPollState
	var raw string
	err := s.db.QueryRow(`SELECT state FROM repeater_poll_state WHERE pubkey = ?`, pubkey).Scan(&raw)
	if err != nil {
		if stderrors.Is(err, sql.ErrNoRows) {
			return st, nil
		}
		return st, err
	}
	return st, json.Unmarshal([]byte(raw), &st)
}

// SetRepeaterPollState saves a repeater's schedule.
func (s *Store) SetRepeaterPollState(pubkey string, st RepeaterPollState, now time.Time) error {
	b, _ := json.Marshal(st)
	_, err := s.db.Exec(`INSERT INTO repeater_poll_state (pubkey, state, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (pubkey) DO UPDATE SET state = excluded.state, updated_at = excluded.updated_at`,
		pubkey, string(b), ms(now))
	return err
}

// deleteRepeaterPolling forgets a repeater's attempts and schedule.
func (s *Store) deleteRepeaterPolling(pubkey string) error {
	if _, err := s.db.Exec(`DELETE FROM repeater_attempts WHERE pubkey = ?`, pubkey); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM repeater_poll_state WHERE pubkey = ?`, pubkey)
	return err
}

// SetRepeaterPollState saves the poller's schedule for a repeater (written
// through the relay's write queue, like poll outcomes).
func (r *Relay) SetRepeaterPollState(pubkey string, st RepeaterPollState) {
	now := r.now()
	r.audit("repeater poll state", func(s *Store) error { return s.SetRepeaterPollState(pubkey, st, now) })
}

func (r *Relay) recordAttempt(a RepeaterAttempt) {
	r.audit("repeater attempt", func(s *Store) error { return s.RecordRepeaterAttempt(a) })
}
