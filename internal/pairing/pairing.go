// Package pairing is the bridge side of PROTOCOL.md "Pairing": it gets this
// bridge a token from the relay, by passphrase (approved at once) or by a
// device code an admin approves.
package pairing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/sierragridteam/sierra-meshrelay/meshproto"
)

// Errors a host may see, each with a plain explanation.
var (
	// ErrInvalidPassphrase: wrong, expired, used up or revoked. The relay
	// deliberately doesn't say which.
	ErrInvalidPassphrase = errors.New("that passphrase didn't work; check the words, or ask your SIERRA contact for a new one")
	// ErrRateLimited: too many attempts from this network.
	ErrRateLimited = errors.New("too many pairing attempts from this network; wait a while and try again")
	// ErrDenied: an admin rejected the code.
	ErrDenied = errors.New("your SIERRA contact declined this pairing request")
	// ErrExpired: the code timed out (or was already used).
	ErrExpired = errors.New("the pairing code expired; get a new one")
)

// Client talks to the relay's pairing endpoints.
type Client struct {
	// APIBase is the relay API, e.g. https://data.sierragridteam.org/mesh/relay/api.
	APIBase string
	HTTP    *http.Client
}

func (c *Client) post(ctx context.Context, path string, body, out any) (int, error) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.APIBase+path, bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) && !errors.Is(err, context.Canceled) {
			return 0, fmt.Errorf("couldn't reach SIERRA's relay (%s): %w", req.URL.Host, ue.Err)
		}
		return 0, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return resp.StatusCode, json.NewDecoder(resp.Body).Decode(out)
	case http.StatusForbidden:
		var e meshproto.PairError
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == meshproto.ErrInvalidPassphrase {
			return resp.StatusCode, ErrInvalidPassphrase
		}
	case http.StatusTooManyRequests:
		return resp.StatusCode, ErrRateLimited
	}
	return resp.StatusCode, fmt.Errorf("relay answered HTTP %d", resp.StatusCode)
}

// Redeem pairs with a passphrase. It returns the approved result, or
// ErrInvalidPassphrase.
func (c *Client) Redeem(ctx context.Context, req meshproto.PairStartRequest) (meshproto.PairResult, error) {
	var res meshproto.PairResult
	if req.Passphrase == "" {
		return res, errors.New("no passphrase")
	}
	req.Passphrase = meshproto.NormalizePassphrase(req.Passphrase)
	if _, err := c.post(ctx, "/pair/start", req, &res); err != nil {
		return res, err
	}
	if res.Status != meshproto.PairApproved || res.Token == "" {
		return res, fmt.Errorf("relay answered %q without a token", res.Status)
	}
	return res, nil
}

// Start begins the device-code flow.
func (c *Client) Start(ctx context.Context, req meshproto.PairStartRequest) (meshproto.PairStartResponse, error) {
	req.Passphrase = ""
	var res meshproto.PairStartResponse
	_, err := c.post(ctx, "/pair/start", req, &res)
	if err == nil && (res.PairID == "" || res.UserCode == "") {
		err = errors.New("relay's pairing response had no code")
	}
	return res, err
}

// Wait polls until the code is approved (returning the token), denied,
// expired, or ctx ends.
func (c *Client) Wait(ctx context.Context, start meshproto.PairStartResponse) (meshproto.PairResult, error) {
	interval := time.Duration(start.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	deadline := time.Now().Add(time.Duration(start.ExpiresIn)*time.Second + interval)
	for {
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return meshproto.PairResult{}, ctx.Err()
		case <-t.C:
		}
		var res meshproto.PairResult
		_, err := c.post(ctx, "/pair/poll", meshproto.PairPollRequest{PairID: start.PairID, PollSecret: start.PollSecret}, &res)
		switch {
		case errors.Is(err, ErrRateLimited):
			interval *= 2 // the relay asked us to slow down
			continue
		case err != nil:
			// A network blip shouldn't lose the code: keep polling until it expires.
			if time.Now().After(deadline) {
				return res, err
			}
			continue
		}
		switch res.Status {
		case meshproto.PairApproved:
			if res.Token == "" {
				return res, errors.New("relay approved the pairing without a token")
			}
			return res, nil
		case meshproto.PairDenied:
			return res, ErrDenied
		case meshproto.PairExpired:
			return res, ErrExpired
		}
		if time.Now().After(deadline) {
			return res, ErrExpired
		}
	}
}
