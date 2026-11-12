package relay

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/dpup/prefab"
	"github.com/dpup/prefab/errors"
	"github.com/dpup/prefab/logging"
)

// StreamContentType is the media type of a streamed answer: one JSON object
// per line.
const StreamContentType = "application/x-ndjson"

// streamHeartbeat is how often a streamed answer says it's still working.
// Proxies drop a connection that is quiet for long (the deployment's load
// balancer, after 60 seconds), and a repeater check can take minutes.
var streamHeartbeat = 15 * time.Second

// StreamLine is one line of a streamed answer. Exactly one field is set:
// Progress as the handler works, Waiting (seconds so far) between steps,
// then Result or Error to finish.
type StreamLine struct {
	Progress string       `json:"progress,omitempty"`
	Waiting  int          `json:"waiting,omitempty"`
	Result   any          `json:"result,omitempty"`
	Error    *StreamError `json:"error,omitempty"`
}

// StreamError is how a streamed answer failed: the status and message the
// unstreamed route would have answered with.
type StreamError struct {
	Status  int    `json:"status"`
	Message string `json:"message"`
}

type progressKey struct{}

// progressFrom returns the function that reports a step of the request in
// ctx, or one that does nothing when nobody is listening.
func progressFrom(ctx context.Context) func(string) {
	if f, ok := ctx.Value(progressKey{}).(func(string)); ok {
		return f
	}
	return func(string) {}
}

// streamJSON serves a JSON handler as a stream: the steps it reports
// (progressFrom), a heartbeat while it works, then its answer. The status is
// always 200; the last line says how it went.
func (r *Relay) streamJSON(h prefab.JSONHandler) http.HandlerFunc {
	return streamJSON(h, func(req *http.Request, err error) {
		logging.Errorw(r.ctx, "JSON handler error", "error", err, "req.method", req.Method, "req.url", req.URL.String())
	})
}

func streamJSON(h prefab.JSONHandler, logErr func(*http.Request, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		var mu sync.Mutex
		started := false
		write := func(l StreamLine) {
			mu.Lock()
			defer mu.Unlock()
			if !started {
				w.Header().Set("Content-Type", StreamContentType)
				w.Header().Set("Cache-Control", "no-store")
				w.Header().Set("X-Accel-Buffering", "no")
				w.WriteHeader(http.StatusOK)
				started = true
			}
			b, _ := json.Marshal(l)
			_, _ = w.Write(append(b, '\n'))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		ctx := context.WithValue(req.Context(), progressKey{}, func(step string) { write(StreamLine{Progress: step}) })

		start := time.Now()
		done := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := time.NewTicker(streamHeartbeat)
			defer t.Stop()
			for {
				select {
				case <-done:
					return
				case <-t.C:
					write(StreamLine{Waiting: max(1, int(time.Since(start).Seconds()))})
				}
			}
		}()
		res, err := h(req.WithContext(ctx))
		close(done)
		wg.Wait()
		if err != nil {
			if logErr != nil {
				logErr(req, err)
			}
			write(StreamLine{Error: &StreamError{Status: errors.HTTPStatusCode(err), Message: err.Error()}})
			return
		}
		write(StreamLine{Result: res})
	}
}
