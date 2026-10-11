// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Relay reads an inbox through the query API and passes every message on to one
// destination, in order, at least once.
//
// It is an agent like any other: it holds an agent token for one inbox and a
// cursor, the seq of the last message it has handled, and asks for what came
// after. The cursor is kept in a file, so a restart carries on where it stopped.
type Relay struct {
	// Source is the hookinlet server, such as "http://localhost:8080".
	Source string
	// Token is the inbox's agent token.
	Token string
	// Inbox is the inbox's slug.
	Inbox string
	// Destination receives each message as a POST.
	Destination string
	// StateFile holds the cursor. Required.
	StateFile string
	// Start is where to begin when there is no state file: "beginning" or "now".
	Start string
	// IncludeUnverified also forwards messages that failed verification. They
	// are untrusted input, so by default they are not forwarded.
	IncludeUnverified bool
	// PollInterval is how long to wait when there is nothing new.
	PollInterval time.Duration
	// MaxBackoff caps the wait between attempts to deliver one message.
	MaxBackoff time.Duration
	// StripHeaders names more headers not to pass on, besides the ones that are
	// never passed on (see sensitiveHeader).
	StripHeaders []string

	Log    *slog.Logger
	Client *http.Client // used for both sides; nil means a default
}

// hopHeaders are not passed on: they describe one connection, or are set by the
// HTTP client for this one.
var hopHeaders = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Connection": true, "Te": true, "Trailer": true,
	"Transfer-Encoding": true, "Upgrade": true, "Host": true, "Content-Length": true, "Accept-Encoding": true,
}

// sensitiveHeader reports whether a stored header is one a receiving service
// might trust, and so must not be forwarded as the sender wrote it. Every header
// of a stored message was chosen by whoever sent the webhook, or by a proxy in
// front of hookinlet. The relay sets its own X-Hookinlet-* headers, so a sender's
// must not get through; and the headers that say where a request came from
// (X-Forwarded-For and friends) would, replayed, tell an internal service that
// the request came from wherever the sender said.
//
// The remaining headers are the sender's, passed on so that its signature can be
// checked. A receiving service must not take any other header on trust.
func sensitiveHeader(name string) bool {
	n := strings.ToLower(name)
	switch {
	case strings.HasPrefix(n, "x-hookinlet-"), strings.HasPrefix(n, "x-forwarded-"):
		return true
	}
	switch n {
	case "forwarded", "x-real-ip", "via", "x-original-forwarded-for", "true-client-ip", "cf-connecting-ip", "x-client-ip":
		return true
	}
	return false
}

type message struct {
	Seq        int64               `json:"seq"`
	Verified   bool                `json:"verified"`
	DeliveryID *string             `json:"delivery_id"`
	EventType  *string             `json:"event_type"`
	ReceivedAt string              `json:"received_at"`
	Path       string              `json:"path"`
	Query      string              `json:"query"`
	Method     string              `json:"method"`
	Headers    map[string][]string `json:"headers"`
}

type page struct {
	Messages  []message `json:"messages"`
	LatestSeq int64     `json:"latest_seq"`
}

func (r *Relay) client() *http.Client {
	if r.Client != nil {
		return r.Client
	}
	return &http.Client{
		Timeout: 60 * time.Second,
		// A redirect would send the message, or the token, somewhere nobody chose.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Run relays until ctx ends. It returns nil when stopped by ctx.
func (r *Relay) Run(ctx context.Context) error {
	if r.StateFile == "" {
		return errors.New("a state file is required, or a restart would lose its place")
	}
	cursor, err := r.loadCursor(ctx)
	if err != nil {
		return err
	}
	r.Log.Info("relaying", "inbox", r.Inbox, "from_seq", cursor, "to", r.Destination)

	for ctx.Err() == nil {
		pg, err := r.fetchPage(ctx, cursor)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			r.Log.Warn("could not read the inbox; will retry", "err", err)
			r.sleep(ctx, r.MaxBackoff)
			continue
		}
		for _, m := range pg.Messages {
			if m.Verified || r.IncludeUnverified {
				if err := r.deliver(ctx, m); err != nil {
					return nil // ctx ended while trying
				}
			}
			cursor = m.Seq
			if err := r.saveCursor(cursor); err != nil {
				return err
			}
		}
		if len(pg.Messages) == 0 {
			// Nothing matched since the cursor. Everything up to latest_seq was looked
			// at (or does not match), so the next look can start there.
			if pg.LatestSeq > cursor {
				cursor = pg.LatestSeq
				if err := r.saveCursor(cursor); err != nil {
					return err
				}
			}
			r.sleep(ctx, r.PollInterval)
		}
	}
	return nil
}

func (r *Relay) sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

func (r *Relay) get(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(r.Source, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+r.Token)
	return r.client().Do(req)
}

func (r *Relay) fetchPage(ctx context.Context, after int64) (*page, error) {
	resp, err := r.get(ctx, fmt.Sprintf("/v1/inboxes/%s/messages?after=%d&limit=50&fields=summary", url.PathEscape(r.Inbox), after))
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the inbox answered %d", resp.StatusCode)
	}
	var pg page
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&pg); err != nil {
		return nil, fmt.Errorf("reading the page: %w", err)
	}
	return &pg, nil
}

// raw returns the exact bytes the sender posted, and the content type it used.
func (r *Relay) raw(ctx context.Context, seq int64) ([]byte, string, error) {
	resp, err := r.get(ctx, fmt.Sprintf("/v1/inboxes/%s/messages/%d/raw", url.PathEscape(r.Inbox), seq))
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, "", errGone
	case resp.StatusCode != http.StatusOK:
		return nil, "", fmt.Errorf("the inbox answered %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 11<<20))
	return body, resp.Header.Get("Content-Type"), err
}

// errGone means retention removed the message before it was relayed.
var errGone = errors.New("the message is gone")

// deliver passes one message on, retrying until the destination takes it or
// refuses it for good. It returns an error only if ctx ends first.
func (r *Relay) deliver(ctx context.Context, m message) error {
	wait := time.Second
	for attempt := 1; ; attempt++ {
		outcome, err := r.attempt(ctx, m)
		switch outcome {
		case delivered:
			r.Log.Info("relayed", "seq", m.Seq, "attempt", attempt)
			return nil
		case dropped:
			r.Log.Warn("not relayed", "seq", m.Seq, "reason", err.Error())
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r.Log.Warn("will retry", "seq", m.Seq, "attempt", attempt, "err", err.Error(), "in", wait.String())
		r.sleep(ctx, wait)
		wait = min(wait*2, r.MaxBackoff)
	}
}

type outcome int

const (
	retry outcome = iota
	delivered
	dropped // the destination refuses this message for good, or it no longer exists
)

func (r *Relay) attempt(ctx context.Context, m message) (outcome, error) {
	body, contentType, err := r.raw(ctx, m.Seq)
	switch {
	case errors.Is(err, errGone):
		return dropped, err
	case err != nil:
		return retry, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.Destination, bytes.NewReader(body))
	if err != nil {
		return dropped, err
	}
	// The original headers go along, so a destination that checks the sender's
	// signature can do so: the body is the exact bytes that were signed.
	for k, vs := range m.Headers {
		if hopHeaders[http.CanonicalHeaderKey(k)] || sensitiveHeader(k) || r.stripped(k) {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("X-Hookinlet-Seq", strconv.FormatInt(m.Seq, 10))
	req.Header.Set("X-Hookinlet-Inbox", r.Inbox)
	req.Header.Set("X-Hookinlet-Received-At", m.ReceivedAt)
	req.Header.Set("X-Hookinlet-Verified", strconv.FormatBool(m.Verified))
	if m.DeliveryID != nil {
		req.Header.Set("X-Hookinlet-Delivery-Id", *m.DeliveryID)
	}
	if m.EventType != nil {
		req.Header.Set("X-Hookinlet-Event-Type", *m.EventType)
	}

	resp, err := r.client().Do(req)
	if err != nil {
		return retry, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return delivered, nil
	case refusedForGood[resp.StatusCode]:
		// The destination looked at this message and will not take it. Asking again
		// will not change that, and one such message must not hold up all the rest.
		return dropped, fmt.Errorf("the destination answered %d", resp.StatusCode)
	default:
		// Everything else is retried: a server error, a timeout, a rate limit, and
		// also what a wrong setting looks like (a redirect, 401, 403, 404). Those
		// would otherwise discard the whole backlog, one message at a time, on a
		// mistyped URL or an expired credential. The log says what is wrong, and
		// nothing is lost while it is fixed.
		return retry, fmt.Errorf("the destination answered %d", resp.StatusCode)
	}
}

// refusedForGood are the statuses that mean the destination rejected this
// particular message, as opposed to being unavailable or misconfigured.
var refusedForGood = map[int]bool{
	http.StatusBadRequest: true, http.StatusGone: true, http.StatusRequestEntityTooLarge: true,
	http.StatusUnsupportedMediaType: true, http.StatusUnprocessableEntity: true,
}

func (r *Relay) stripped(name string) bool {
	for _, s := range r.StripHeaders {
		if strings.EqualFold(strings.TrimSpace(s), name) {
			return true
		}
	}
	return false
}

// ---- the cursor ----

func (r *Relay) loadCursor(ctx context.Context) (int64, error) {
	b, err := os.ReadFile(r.StateFile)
	switch {
	case err == nil:
		n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("%s does not hold a position: %q", r.StateFile, strings.TrimSpace(string(b)))
		}
		return n, nil
	case !errors.Is(err, os.ErrNotExist):
		return 0, err
	}
	if r.Start == "beginning" {
		return 0, nil
	}
	// Start from now: ask for the newest message and begin after it.
	pg, err := r.fetchLatest(ctx)
	if err != nil {
		return 0, fmt.Errorf("finding the end of the inbox: %w", err)
	}
	if err := r.saveCursor(pg); err != nil {
		return 0, err
	}
	return pg, nil
}

func (r *Relay) fetchLatest(ctx context.Context) (int64, error) {
	resp, err := r.get(ctx, "/v1/inboxes/"+url.PathEscape(r.Inbox))
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("the inbox answered %d (is the token right?)", resp.StatusCode)
	}
	var info struct {
		LatestSeq int64 `json:"latest_seq"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info); err != nil {
		return 0, err
	}
	return info.LatestSeq, nil
}

// saveCursor writes the position atomically, so a crash leaves the old one or
// the new one and never half of either.
func (r *Relay) saveCursor(seq int64) error {
	dir := filepath.Dir(r.StateFile)
	tmp, err := os.CreateTemp(dir, ".relay-state-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, err = fmt.Fprintf(tmp, "%d\n", seq)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Rename(name, r.StateFile)
}
