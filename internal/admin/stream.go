package admin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// streamContentType matches relay.StreamContentType.
const streamContentType = "application/x-ndjson"

// waitingEvery is how often a request to a relay that can't stream says
// it's still waiting.
var waitingEvery = 15 * time.Second

// doStream POSTs body to path+"/stream", which answers with the steps of
// the request as it goes, and decodes the result into out. A relay from
// before streaming doesn't have that route: then it POSTs to path as
// before, and progress hears only that it's still waiting.
func (c *Client) doStream(ctx context.Context, path string, body, out any, progress func(string)) error {
	if progress == nil {
		progress = func(string) {}
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.APIBase+path+"/stream", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", streamContentType)
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return unreachable(c.APIBase, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK && strings.HasPrefix(resp.Header.Get("Content-Type"), streamContentType) {
		return readStream(resp.Body, out, progress)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return c.doWaiting(ctx, path, body, out, progress)
}

// readStream reads a streamed answer (see relay.StreamLine).
func readStream(r io.Reader, out any, progress func(string)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var l struct {
			Progress string          `json:"progress"`
			Waiting  int             `json:"waiting"`
			Result   json.RawMessage `json:"result"`
			Error    *struct {
				Status  int    `json:"status"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			return fmt.Errorf("reading the relay's answer: %w", err)
		}
		switch {
		case l.Error != nil:
			return &APIError{Status: l.Error.Status, Message: l.Error.Message}
		case l.Result != nil:
			if out == nil {
				return nil
			}
			return json.Unmarshal(l.Result, out)
		case l.Progress != "":
			progress(l.Progress)
		case l.Waiting > 0:
			progress(fmt.Sprintf("still waiting (%s)", time.Duration(l.Waiting)*time.Second))
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("the relay's answer was cut off: %w", err)
	}
	return errors.New("the relay's answer ended before it said how the request went")
}

// doWaiting is do, saying every so often that it's still waiting.
func (c *Client) doWaiting(ctx context.Context, path string, body, out any, progress func(string)) error {
	start := time.Now()
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		t := time.NewTicker(waitingEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				progress(fmt.Sprintf("still waiting (%s)", time.Since(start).Round(time.Second)))
			}
		}
	}()
	err := c.do(ctx, http.MethodPost, path, body, out)
	close(done)
	<-stopped
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusGatewayTimeout && ae.Message == "" {
		// A proxy in front of the relay gave up, not the relay: the check
		// may still finish.
		ae.Message = "the relay's proxy stopped waiting for the answer, but the check may still finish; " +
			"see whether the repeater was shared in a minute"
	}
	return err
}
