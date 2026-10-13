// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Pat Ramsey

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Message is the part of a hookinlet message this agent cares about. The full
// shape is in docs/api.md.
type Message struct {
	Seq       int64           `json:"seq"`
	Verified  bool            `json:"verified"`
	EventType *string         `json:"event_type"`
	Body      json.RawMessage `json:"body"`
	BodyText  *string         `json:"body_text"`
	Epoch     string          `json:"epoch"`
}

// Agent reads one inbox. Handle is called once per message, in order. The agent
// keeps a cursor, the seq of the last message handled, so that polling and a
// reconnecting stream both carry on exactly where they stopped.
type Agent struct {
	URL    string // the server, such as http://localhost:8080
	Token  string // the inbox's agent token
	Inbox  string // the inbox's slug
	After  int64  // start after this seq; 0 is the beginning
	Handle func(Message)

	// Interval is how long to wait between polls when nothing is new.
	Interval time.Duration
	// Limit stops after this many messages; 0 never stops.
	Limit  int
	Client *http.Client
}

func (a *Agent) client() *http.Client {
	if a.Client != nil {
		return a.Client
	}
	return &http.Client{} // no overall timeout: a stream lasts as long as it lasts
}

func (a *Agent) get(ctx context.Context, path string, header http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(a.URL, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	for k, vs := range header {
		req.Header[k] = vs
	}
	req.Header.Set("Authorization", "Bearer "+a.Token)
	resp, err := a.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		err := fmt.Errorf("the server answered %d", resp.StatusCode)
		switch resp.StatusCode {
		case http.StatusConflict:
			// epoch_changed: the cursor belongs to an earlier life of the inbox.
			return nil, errEpochChanged
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
			// The token or the inbox is wrong; asking again will not help.
			return nil, &fatalError{fmt.Errorf("%w: is the token right, and the inbox there?", err)}
		}
		return nil, err
	}
	return resp, nil
}

// errEpochChanged means the server refused the cursor: the inbox was deleted and made
// again, or the server was restored, since the cursor was issued (see docs/api.md).
var errEpochChanged = errors.New("the cursor is from an earlier epoch of the inbox")

// fatalError is a failure that retrying cannot fix.
type fatalError struct{ error }

// Poll asks for messages after the cursor until ctx ends or Limit is reached.
func (a *Agent) Poll(ctx context.Context) error {
	seen := 0
	after := a.After
	epoch := "" // which life of the inbox the cursor is in; known from the first page
	for ctx.Err() == nil {
		path := fmt.Sprintf("/v1/inboxes/%s/messages?after=%d&limit=50", url.PathEscape(a.Inbox), after)
		if epoch != "" {
			path += "&epoch=" + url.QueryEscape(epoch)
		}
		resp, err := a.get(ctx, path, nil)
		if errors.Is(err, errEpochChanged) {
			// A seq means nothing across lives of an inbox: carry on from the old
			// position and the new inbox's first messages would never be seen.
			fmt.Fprintln(os.Stderr, "the inbox was deleted and made again, or the server was restored; reading it from its beginning")
			after, epoch = 0, ""
			continue
		}
		if err != nil {
			var fatal *fatalError
			if errors.As(err, &fatal) {
				return err
			}
			// The server is down, restarting, or busy: wait and ask again.
			fmt.Fprintf(os.Stderr, "%v; retrying\n", err)
			select {
			case <-time.After(a.Interval):
			case <-ctx.Done():
			}
			continue
		}
		var page struct {
			Messages []Message `json:"messages"`
			Next     string    `json:"next"`
			Epoch    string    `json:"epoch"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&page)
		_ = resp.Body.Close()
		if err != nil {
			return fmt.Errorf("reading a page: %w", err)
		}
		if epoch == "" {
			epoch = page.Epoch
		}
		for _, m := range page.Messages {
			a.Handle(m)
			after = m.Seq
			if seen++; a.Limit > 0 && seen >= a.Limit {
				return nil
			}
		}
		// The cursor to keep, which is the same one again when the page was empty.
		if u, err := url.Parse(page.Next); err == nil {
			if n, err := strconv.ParseInt(u.Query().Get("after"), 10, 64); err == nil && n > after {
				after = n
			}
		}
		if len(page.Messages) == 0 {
			select {
			case <-time.After(a.Interval):
			case <-ctx.Done():
			}
		}
	}
	return nil
}

// Stream follows the inbox over Server-Sent Events, reconnecting with
// Last-Event-ID when the connection drops, until ctx ends or Limit is reached.
func (a *Agent) Stream(ctx context.Context) error {
	seen := 0
	last := strconv.FormatInt(a.After, 10)
	epoch := ""
	for ctx.Err() == nil {
		done, err := a.streamOnce(ctx, &last, &epoch, &seen)
		if done {
			return nil
		}
		if errors.Is(err, errEpochChanged) {
			fmt.Fprintln(os.Stderr, "the inbox was deleted and made again, or the server was restored; reading it from its beginning")
			last, epoch = "0", ""
			continue
		}
		var fatal *fatalError
		if errors.As(err, &fatal) {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		fmt.Fprintf(os.Stderr, "connection lost (%v); reconnecting after %s\n", err, last)
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
		}
	}
	return nil
}

func (a *Agent) streamOnce(ctx context.Context, last, epoch *string, seen *int) (done bool, err error) {
	path := "/v1/inboxes/" + url.PathEscape(a.Inbox) + "/stream"
	if *epoch != "" {
		path += "?epoch=" + url.QueryEscape(*epoch) // a reconnect is refused if the inbox has changed lives
	}
	resp, err := a.get(ctx, path, http.Header{"Last-Event-ID": {*last}})
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20) // one event is one line of JSON
	var id string
	var data []string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "id:"):
			id = strings.TrimSpace(line[3:])
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(line[5:], " "))
		case line == "" && len(data) > 0:
			var m Message
			if err := json.Unmarshal([]byte(strings.Join(data, "\n")), &m); err == nil {
				a.Handle(m)
				if *epoch == "" {
					*epoch = m.Epoch
				}
			}
			if id != "" {
				*last = id
			}
			data = nil
			if *seen++; a.Limit > 0 && *seen >= a.Limit {
				return true, nil
			}
		}
	}
	return false, sc.Err()
}
