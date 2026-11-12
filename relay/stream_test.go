package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dpup/prefab/errors"
	"google.golang.org/grpc/codes"
)

func streamLines(t *testing.T, h http.HandlerFunc) (*httptest.ResponseRecorder, []StreamLine) {
	t.Helper()
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/x/stream", nil))
	var out []StreamLine
	for _, l := range strings.Split(strings.TrimSpace(rec.Body.String()), "\n") {
		var sl StreamLine
		if err := json.Unmarshal([]byte(l), &sl); err != nil {
			t.Fatalf("line %q: %v", l, err)
		}
		out = append(out, sl)
	}
	return rec, out
}

func TestStreamJSONStepsHeartbeatAndResult(t *testing.T) {
	defer func(d time.Duration) { streamHeartbeat = d }(streamHeartbeat)
	streamHeartbeat = 10 * time.Millisecond
	rec, lines := streamLines(t, streamJSON(func(req *http.Request) (any, error) {
		progressFrom(req.Context())("logging in")
		time.Sleep(60 * time.Millisecond)
		return map[string]string{"access": "guest"}, nil
	}, nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != StreamContentType {
		t.Fatalf("%d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if lines[0].Progress != "logging in" {
		t.Errorf("first line %+v", lines[0])
	}
	waits := 0
	for _, l := range lines[1 : len(lines)-1] {
		if l.Waiting > 0 {
			waits++
		}
	}
	if waits == 0 {
		t.Errorf("no heartbeat: %+v", lines)
	}
	last := lines[len(lines)-1]
	if m, _ := last.Result.(map[string]any); m["access"] != "guest" {
		t.Errorf("last line %+v", last)
	}
}

func TestStreamJSONError(t *testing.T) {
	var logged error
	_, lines := streamLines(t, streamJSON(func(*http.Request) (any, error) {
		return nil, errors.NewC("the repeater refused that password", codes.PermissionDenied)
	}, func(_ *http.Request, err error) { logged = err }))
	if logged == nil {
		t.Error("the error wasn't logged")
	}
	last := lines[len(lines)-1]
	if last.Error == nil || last.Error.Status != http.StatusForbidden || !strings.Contains(last.Error.Message, "refused") {
		t.Fatalf("%+v", lines)
	}
}

// Without a stream, steps go nowhere.
func TestProgressFromWithoutStream(t *testing.T) {
	progressFrom(httptest.NewRequest(http.MethodGet, "/", nil).Context())("nobody hears this")
}
