package relay

// Bridge logs: recent log lines a bridge sends as `log` messages, kept per
// gateway for admins (GET /gateways/<name>/logs, `meshbridge admin logs`).
// They let someone helping a host see why a bridge is stuck without reaching
// the host's computer. Admin-only: they hold LAN addresses and host names.

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/dpup/prefab/errors"
	"google.golang.org/grpc/codes"

	"github.com/sierragridteam/sierra-meshrelay/meshproto"
)

const bridgeLogSchema = `
CREATE TABLE IF NOT EXISTS bridge_logs (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	gateway     TEXT NOT NULL,
	received_at INTEGER NOT NULL,
	t           TEXT NOT NULL,
	level       TEXT NOT NULL,
	msg         TEXT NOT NULL,
	attrs       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS bridge_logs_by_gw ON bridge_logs (gateway, id);
`

// Bridge log limits. A bridge logs a handful of lines an hour when healthy;
// these only bite on a bridge in a tight failure loop.
const (
	maxLogsPerGateway   = 1000
	maxLogsPerMinute    = 120
	maxLogEntriesPerMsg = 100
	maxLogField         = 400
)

// BridgeLog is one stored line.
type BridgeLog struct {
	ID         int64             `json:"id"`
	ReceivedAt time.Time         `json:"received_at"`
	T          string            `json:"t"` // the bridge's clock
	Level      string            `json:"level"`
	Msg        string            `json:"msg"`
	Attrs      map[string]string `json:"attrs,omitempty"`
}

func (s *Store) addBridgeLogs(gateway string, at time.Time, es []BridgeLog) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // after Commit, a no-op
	for _, e := range es {
		attrs := ""
		if len(e.Attrs) > 0 {
			b, _ := json.Marshal(e.Attrs)
			attrs = string(b)
		}
		if _, err := tx.Exec(`INSERT INTO bridge_logs (gateway, received_at, t, level, msg, attrs) VALUES (?, ?, ?, ?, ?, ?)`,
			gateway, ms(at), e.T, e.Level, e.Msg, attrs); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM bridge_logs WHERE gateway = ? AND id <= (
		SELECT id FROM bridge_logs WHERE gateway = ? ORDER BY id DESC LIMIT 1 OFFSET ?)`,
		gateway, gateway, maxLogsPerGateway); err != nil {
		return err
	}
	return tx.Commit()
}

// BridgeLogs returns a gateway's lines after afterID, oldest first, at most
// limit of the newest.
func (s *Store) BridgeLogs(gateway string, afterID int64, limit int) ([]BridgeLog, error) {
	rows, err := s.db.Query(`SELECT id, received_at, t, level, msg, attrs FROM (
			SELECT * FROM bridge_logs WHERE gateway = ? AND id > ? ORDER BY id DESC LIMIT ?
		) ORDER BY id`, gateway, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BridgeLog{}
	for rows.Next() {
		var e BridgeLog
		var at int64
		var attrs string
		if err := rows.Scan(&e.ID, &at, &e.T, &e.Level, &e.Msg, &attrs); err != nil {
			return nil, err
		}
		e.ReceivedAt = fromMS(at)
		if attrs != "" {
			_ = json.Unmarshal([]byte(attrs), &e.Attrs)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) deleteBridgeLogs(gateway string) error {
	_, err := s.db.Exec(`DELETE FROM bridge_logs WHERE gateway = ?`, gateway)
	return err
}

// logBudget rate-limits one bridge session's log lines.
type logBudget struct {
	mu      sync.Mutex
	window  time.Time
	used    int
	dropped int
}

// take admits up to n lines in the current minute, returning how many, and
// how many were dropped since the last admitted batch.
func (b *logBudget) take(now time.Time, n int) (ok, dropped int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if now.Sub(b.window) >= time.Minute {
		b.window, b.used = now, 0
	}
	ok = min(n, maxLogsPerMinute-b.used)
	b.used += ok
	b.dropped += n - ok
	if ok > 0 {
		dropped, b.dropped = b.dropped, 0
	}
	return ok, dropped
}

func clip(s string) string {
	if len(s) <= maxLogField {
		return s
	}
	return s[:maxLogField] + "…"
}

// bridgeLog stores a `log` message from a bridge.
func (r *Relay) bridgeLog(s *bridgeSession, data []byte) {
	var m meshproto.LogMsg
	if json.Unmarshal(data, &m) != nil {
		return
	}
	es := m.Entries
	if len(es) > maxLogEntriesPerMsg {
		m.Lost += len(es) - maxLogEntriesPerMsg
		es = es[:maxLogEntriesPerMsg]
	}
	now := r.now()
	n, dropped := s.logs.take(now, len(es))
	es = es[:n]
	lost := m.Lost + dropped
	if len(es) == 0 {
		return
	}
	out := make([]BridgeLog, 0, len(es)+1)
	if lost > 0 {
		out = append(out, BridgeLog{T: now.UTC().Format(time.RFC3339), Level: "WARN",
			Msg: "(" + strconv.Itoa(lost) + " earlier lines lost: the bridge's buffer or the relay's rate limit)"})
	}
	for _, e := range es {
		attrs := map[string]string{}
		for k, v := range e.Attrs {
			if len(attrs) >= 32 {
				break
			}
			attrs[clip(k)] = clip(v)
		}
		out = append(out, BridgeLog{T: clip(e.T), Level: clip(e.Level), Msg: clip(e.Msg), Attrs: attrs})
	}
	r.queueLogs(s.gateway, now, out)
}

// logFlushDelay batches bridge log lines into one write per gateway, so a
// chatty bridge costs one small transaction every couple of seconds rather
// than one per message.
const logFlushDelay = 2 * time.Second

type pendingLogs struct {
	at    time.Time
	lines []BridgeLog
}

func (r *Relay) queueLogs(gateway string, at time.Time, lines []BridgeLog) {
	r.logMu.Lock()
	defer r.logMu.Unlock()
	if r.logPending == nil {
		r.logPending = map[string]*pendingLogs{}
	}
	p := r.logPending[gateway]
	if p == nil {
		p = &pendingLogs{}
		r.logPending[gateway] = p
		time.AfterFunc(logFlushDelay, func() { r.flushLogs(gateway) })
	}
	p.at = at
	p.lines = append(p.lines, lines...)
}

func (r *Relay) flushLogs(gateway string) {
	r.logMu.Lock()
	p := r.logPending[gateway]
	delete(r.logPending, gateway)
	r.logMu.Unlock()
	if p != nil && len(p.lines) > 0 {
		r.audit("bridge log", func(st *Store) error { return st.addBridgeLogs(gateway, p.at, p.lines) })
	}
}

func (r *Relay) apiGatewayLogs(req *http.Request, _ *ClientRecord) (any, error) {
	name := req.PathValue("name")
	if _, err := r.store.Gateway(name); err != nil {
		return nil, storeErr(err, "no such gateway")
	}
	limit, after := 200, int64(0)
	if v, err := strconv.Atoi(req.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = min(v, maxLogsPerGateway)
	}
	if v, err := strconv.ParseInt(req.URL.Query().Get("after"), 10, 64); err == nil && v > 0 {
		after = v
	}
	logs, err := r.store.BridgeLogs(name, after, limit)
	if err != nil {
		return nil, errors.NewC("reading logs: "+err.Error(), codes.Internal)
	}
	events, err := r.store.GatewayEvents(name, 50)
	if err != nil {
		return nil, errors.Wrap(err, 0)
	}
	return map[string]any{"logs": logs, "events": events}, nil
}
