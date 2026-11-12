package admin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type steps struct {
	mu  sync.Mutex
	got []string
}

func (s *steps) add(m string) { s.mu.Lock(); s.got = append(s.got, m); s.mu.Unlock() }

func TestDoStreamReadsSteps(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repeaters/stream" || r.Header.Get("Accept") != streamContentType {
			t.Errorf("request %s accept %q", r.URL.Path, r.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", streamContentType)
		_, _ = w.Write([]byte(`{"progress":"logging in"}` + "\n" + `{"waiting":30}` + "\n" + `{"result":{"check":{"access":"guest"}}}` + "\n"))
	}))
	defer srv.Close()
	c := &Client{APIBase: srv.URL, Token: "t"}
	var s steps
	res, err := c.AddRepeater(context.Background(), "pk", "gw", "pw", s.add)
	if err != nil || res.Check.Access != "guest" {
		t.Fatalf("%+v %v", res, err)
	}
	if !slices.Equal(s.got, []string{"logging in", "still waiting (30s)"}) {
		t.Fatalf("steps %q", s.got)
	}
}

func TestDoStreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", streamContentType)
		_, _ = w.Write([]byte(`{"progress":"logging in"}` + "\n" + `{"error":{"status":403,"message":"the repeater refused that password"}}` + "\n"))
	}))
	defer srv.Close()
	_, err := (&Client{APIBase: srv.URL}).ShareRepeater(context.Background(), "pk", "", "pw", nil)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 403 || !strings.Contains(ae.Message, "refused") {
		t.Fatalf("%v", err)
	}
}

func TestDoStreamCutOff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", streamContentType)
		_, _ = w.Write([]byte(`{"progress":"logging in"}` + "\n"))
	}))
	defer srv.Close()
	_, err := (&Client{APIBase: srv.URL}).ShareRepeater(context.Background(), "pk", "", "pw", nil)
	if err == nil || !strings.Contains(err.Error(), "ended before") {
		t.Fatalf("%v", err)
	}
}

// A relay from before streaming: the stream route isn't there (405, as
// the DELETE route on the same path answers), so it posts as before, and
// says it's still waiting.
func TestDoStreamFallsBack(t *testing.T) {
	defer func(d time.Duration) { waitingEvery = d }(waitingEvery)
	waitingEvery = 20 * time.Millisecond
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/stream") {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write([]byte(`{"check":{"access":"admin"}}`))
	}))
	defer srv.Close()
	var s steps
	res, err := (&Client{APIBase: srv.URL}).ShareRepeater(context.Background(), "pk", "", "pw", s.add)
	if err != nil || res.Check.Access != "admin" {
		t.Fatalf("%+v %v", res, err)
	}
	if !slices.Equal(paths, []string{"/gateway/repeaters/stream", "/gateway/repeaters"}) {
		t.Fatalf("paths %q", paths)
	}
	if len(s.got) == 0 || !strings.HasPrefix(s.got[0], "still waiting") {
		t.Fatalf("steps %q", s.got)
	}
}

// A proxy's own timeout (no message) is explained; the relay's isn't touched.
func TestDoStreamProxyTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/stream") {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusGatewayTimeout)
	}))
	defer srv.Close()
	_, err := (&Client{APIBase: srv.URL}).ShareRepeater(context.Background(), "pk", "", "pw", nil)
	if err == nil || !strings.Contains(err.Error(), "may still finish") {
		t.Fatalf("%v", err)
	}
}
