// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

type readerJSON struct {
	Name      string `json:"name"`
	Seq       int    `json:"seq"`
	UpdatedAt string `json:"updated_at"`
	Lag       int    `json:"lag"`
	Gap       bool   `json:"gap"`
}

type readerListJSON struct {
	Epoch      string       `json:"epoch"`
	LatestSeq  int          `json:"latest_seq"`
	OldestSeq  int          `json:"oldest_seq"`
	MaxReaders int          `json:"max_readers"`
	Readers    []readerJSON `json:"readers"`
}

// readerEnv is a server with agent-a holding n messages.
type readerEnv struct {
	*adminEnv
	epoch  string
	b      *inbox.Inbox // agent-b, which tokenB reads
	tokenB string
}

func newReaderEnv(t *testing.T, n int, mods ...func(*Options)) *readerEnv {
	t.Helper()
	a := newAdminEnv(t, mods...)
	for range n {
		if rec := a.post("/in/agent-a", `{"n":1}`, "Content-Type", "application/json"); rec.Code != 200 {
			t.Fatalf("ingest: %d %s", rec.Code, rec.Body)
		}
	}
	in, err := a.store.GetInbox(t.Context(), a.a.ID)
	if err != nil {
		t.Fatal(err)
	}
	b := a.newInbox("agent-b", "trusted", "")
	return &readerEnv{adminEnv: a, epoch: in.Epoch, b: b, tokenB: a.tokenFor(b)}
}

func (r *readerEnv) ack(name string, seq int, tok string) (int, []byte) {
	r.t.Helper()
	status, _, body := r.call("PUT", "/v1/inboxes/agent-a/readers/"+name, map[string]any{"seq": seq, "epoch": r.epoch}, tok)
	return status, body
}

func (r *readerEnv) list(tok string) readerListJSON {
	r.t.Helper()
	status, _, body := r.call("GET", "/v1/inboxes/agent-a/readers", nil, tok)
	if status != 200 {
		r.t.Fatalf("GET readers: %d %s", status, body)
	}
	var out readerListJSON
	if err := json.Unmarshal(body, &out); err != nil {
		r.t.Fatal(err)
	}
	return out
}

func TestAReaderSeesItsLagAgainstTheNewestMessage(t *testing.T) {
	r := newReaderEnv(t, 5)
	if got := r.list(r.tokenA); len(got.Readers) != 0 || got.LatestSeq != 5 || got.OldestSeq != 1 || got.Epoch != r.epoch || got.MaxReaders != DefaultMaxReadersPerInbox {
		t.Errorf("before any acknowledgement: %+v", got)
	}
	if status, body := r.ack("worker-b", 2, r.tokenA); status != 200 {
		t.Fatalf("ack: %d %s", status, body)
	} else {
		var one readerJSON
		_ = json.Unmarshal(body, &one)
		if one.Name != "worker-b" || one.Seq != 2 || one.Lag != 3 || one.Gap || one.UpdatedAt == "" {
			t.Errorf("the acknowledged reader: %+v", one)
		}
	}
	r.ack("worker-a", 5, r.tokenA)
	got := r.list(r.tokenA)
	if len(got.Readers) != 2 || got.Readers[0].Name != "worker-a" || got.Readers[0].Lag != 0 || got.Readers[1].Name != "worker-b" || got.Readers[1].Lag != 3 {
		t.Errorf("readers: %+v", got.Readers)
	}
	// The lag moves as messages arrive, with no acknowledgement.
	r.post("/in/agent-a", `{"n":2}`, "Content-Type", "application/json")
	if got := r.list(r.tokenA); got.Readers[0].Lag != 1 || got.Readers[1].Lag != 4 || got.LatestSeq != 6 {
		t.Errorf("after another message: %+v", got)
	}
	// The admin token reads it too, and another inbox's token does not (404, not 403).
	if got := r.list(testAdminToken); len(got.Readers) != 2 {
		t.Errorf("the admin token sees %+v", got)
	}
	if status, _, _ := r.call("GET", "/v1/inboxes/agent-a/readers", nil, r.tokenB); status != http.StatusNotFound {
		t.Errorf("another inbox's token: status %d, want 404", status)
	}
	if status, _, _ := r.call("PUT", "/v1/inboxes/agent-a/readers/x", map[string]any{"seq": 1, "epoch": r.epoch}, r.tokenB); status != http.StatusNotFound {
		t.Errorf("another inbox's token acknowledging: status %d, want 404", status)
	}
}

// Reading is the operation most callers make most often, and the Store has a single writer:
// a read must not store a position, whatever it carries, and no reader parameter exists.
func TestReadingNeverStoresAPosition(t *testing.T) {
	r := newReaderEnv(t, 3)
	for _, path := range []string{
		"/v1/inboxes/agent-a/messages?after=0", "/v1/inboxes/agent-a/messages?after=1&epoch=" + r.epoch,
		"/v1/inboxes/agent-a/messages/2", "/v1/inboxes/agent-a", "/v1/inboxes/agent-a/messages",
	} {
		if status, _, body := r.call("GET", path, nil, r.tokenA); status != 200 {
			t.Fatalf("GET %s: %d %s", path, status, body)
		}
	}
	if got := r.list(r.tokenA); len(got.Readers) != 0 {
		t.Errorf("reading stored %+v", got.Readers)
	}
	for _, path := range []string{"/v1/inboxes/agent-a/messages?after=0&reader=w", "/v1/inboxes/agent-a/stream?reader=w", "/v1/inboxes/agent-a?reader=w"} {
		if status, _, _ := r.call("GET", path, nil, r.tokenA); status != http.StatusBadRequest {
			t.Errorf("GET %s: status %d, want 400 (unknown parameter)", path, status)
		}
	}
}

func TestAnAcknowledgementIsChecked(t *testing.T) {
	r := newReaderEnv(t, 3)
	for _, c := range []struct {
		what, name string
		body       any
		want       int
		code       string
	}{
		{"a seq beyond the newest", "w", map[string]any{"seq": 4, "epoch": r.epoch}, 400, "invalid_parameter"},
		{"a negative seq", "w", map[string]any{"seq": -1, "epoch": r.epoch}, 400, "invalid_request"},
		{"no epoch", "w", map[string]any{"seq": 1}, 400, "invalid_request"},
		{"no seq", "w", map[string]any{"epoch": r.epoch}, 400, "invalid_request"},
		{"an unknown field", "w", map[string]any{"seq": 1, "epoch": r.epoch, "reader": "x"}, 400, "invalid_request"},
		{"another epoch", "w", map[string]any{"seq": 1, "epoch": "0badc0de"}, 409, "epoch_changed"},
		{"a name that cannot exist", "-w", map[string]any{"seq": 1, "epoch": r.epoch}, 404, "not_found"},
		{"a name that is too long", strings.Repeat("a", inbox.MaxReaderNameLen+1), map[string]any{"seq": 1, "epoch": r.epoch}, 404, "not_found"},
	} {
		status, _, body := r.call("PUT", "/v1/inboxes/agent-a/readers/"+c.name, c.body, r.tokenA)
		if status != c.want || errorCodeOfBody(t, body) != c.code {
			t.Errorf("%s: %d %s, want %d %s", c.what, status, body, c.want, c.code)
		}
	}
	if got := r.list(r.tokenA); len(got.Readers) != 0 {
		t.Errorf("a refused acknowledgement stored %+v", got.Readers)
	}
	// A position may be set back: that is how a reader starts over.
	r.ack("w", 3, r.tokenA)
	r.ack("w", 1, r.tokenA)
	if got := r.list(r.tokenA); len(got.Readers) != 1 || got.Readers[0].Seq != 1 || got.Readers[0].Lag != 2 {
		t.Errorf("after setting a position back: %+v", got.Readers)
	}
}

func TestTheNumberOfReadersIsCappedPerInbox(t *testing.T) {
	r := newReaderEnv(t, 1, func(o *Options) { o.MaxReadersPerInbox = 2 })
	if got := r.list(r.tokenA); got.MaxReaders != 2 {
		t.Errorf("max_readers = %d, want the configured 2", got.MaxReaders)
	}
	for _, n := range []string{"a", "b"} {
		if status, body := r.ack(n, 0, r.tokenA); status != 200 {
			t.Fatalf("ack %s: %d %s", n, status, body)
		}
	}
	status, body := r.ack("c", 0, r.tokenA)
	if status != 409 || errorCodeOfBody(t, body) != "too_many_readers" {
		t.Errorf("a third reader: %d %s, want 409 too_many_readers", status, body)
	}
	if status, _ := r.ack("a", 1, r.tokenA); status != 200 {
		t.Errorf("an existing reader at the cap: %d", status)
	}
	// Another inbox has its own places.
	if status, _, body := r.call("PUT", "/v1/inboxes/agent-b/readers/c", map[string]any{"seq": 0, "epoch": r.b.Epoch}, r.tokenB); status != 200 {
		t.Errorf("a reader of another inbox: %d %s", status, body)
	}
	// Forgetting one frees a place.
	if status, _, _ := r.call("DELETE", "/v1/inboxes/agent-a/readers/b", nil, r.tokenA); status != http.StatusNoContent {
		t.Errorf("forget: %d", status)
	}
	if status, _ := r.ack("c", 0, r.tokenA); status != 200 {
		t.Errorf("a new reader after one was forgotten: %d", status)
	}
	if status, _, _ := r.call("DELETE", "/v1/inboxes/agent-a/readers/b", nil, r.tokenA); status != http.StatusNotFound {
		t.Errorf("forgetting a reader twice: %d, want 404", status)
	}
}

func TestTheCapHasADefaultAndAMaximum(t *testing.T) {
	for _, c := range []struct{ set, want int }{{0, DefaultMaxReadersPerInbox}, {5000, inbox.MaxNamedReaders}, {1000, 1000}, {7, 7}} {
		r := newReaderEnv(t, 0, func(o *Options) { o.MaxReadersPerInbox = c.set })
		if got := r.list(r.tokenA); got.MaxReaders != c.want {
			t.Errorf("MaxReadersPerInbox %d: max_readers = %d, want %d", c.set, got.MaxReaders, c.want)
		}
	}
	if DefaultMaxReadersPerInbox != 100 || inbox.MaxNamedReaders != 1000 {
		t.Error("docs/api.md and docs/openapi.json say a default of 100 and a maximum of 1000")
	}
}

func TestRetentionPastAReaderIsAGap(t *testing.T) {
	r := newReaderEnv(t, 0)
	r.mustAdmin("PATCH", "/v1/admin/inboxes/agent-a", map[string]any{"retention_max_count": 2}, 200)
	for range 3 {
		r.post("/in/agent-a", `{"n":1}`, "Content-Type", "application/json")
	}
	r.ack("behind", 1, r.tokenA)
	r.ack("caught-up", 3, r.tokenA)
	r.ack("new", 0, r.tokenA)
	for range 3 {
		r.post("/in/agent-a", `{"n":1}`, "Content-Type", "application/json")
	}
	r.runRetention(t)
	got := r.list(r.tokenA)
	if got.LatestSeq != 6 || got.OldestSeq != 5 {
		t.Fatalf("retention kept %d..%d, want 5..6", got.OldestSeq, got.LatestSeq)
	}
	want := map[string]struct {
		lag int
		gap bool
	}{"behind": {5, true}, "caught-up": {3, true}, "new": {6, false}}
	for _, rd := range got.Readers {
		if w := want[rd.Name]; rd.Lag != w.lag || rd.Gap != w.gap {
			t.Errorf("%s: lag %d gap %v, want lag %d gap %v", rd.Name, rd.Lag, rd.Gap, w.lag, w.gap)
		}
	}
	// A reader at seq 4 has missed nothing: the oldest stored message is 5.
	r.ack("exact", 4, r.tokenA)
	for _, rd := range r.list(r.tokenA).Readers {
		if rd.Name == "exact" && rd.Gap {
			t.Errorf("a reader at 4 of 5..6 has missed nothing, but the gap is reported")
		}
	}
}

func TestANewEpochDropsTheReaders(t *testing.T) {
	r := newReaderEnv(t, 2)
	r.ack("w", 2, r.tokenA)
	if err := r.store.RenewEpochs(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := r.list(r.tokenA)
	if len(got.Readers) != 0 || got.Epoch == r.epoch {
		t.Errorf("after the epoch changed: %+v (was %s)", got, r.epoch)
	}
	status, body := r.ack("w", 2, r.tokenA)
	if status != 409 || errorCodeOfBody(t, body) != "epoch_changed" {
		t.Errorf("an acknowledgement with the old epoch: %d %s", status, body)
	}
	r.epoch = got.Epoch
	if status, _ := r.ack("w", 2, r.tokenA); status != 200 {
		t.Errorf("an acknowledgement with the new epoch: %d", status)
	}
}

func TestReadersAreRefusedWhenTheyCannotBeKept(t *testing.T) {
	for name, mod := range map[string]func(*Options){
		"a store without the capability": func(o *Options) { o.Store = storeOnly{o.Store} },
		"the feature turned off":         func(o *Options) { o.MaxReadersPerInbox = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			a := newAdminEnv(t, mod)
			for _, c := range []struct {
				method, path string
				body         any
			}{
				{"GET", "/v1/inboxes/agent-a/readers", nil},
				{"PUT", "/v1/inboxes/agent-a/readers/w", map[string]any{"seq": 0, "epoch": a.a.Epoch}},
				{"DELETE", "/v1/inboxes/agent-a/readers/w", nil},
			} {
				status, _, body := a.call(c.method, c.path, c.body, a.tokenA)
				if status != 400 || errorCodeOfBody(t, body) != "unsupported" {
					t.Errorf("%s %s: %d %s, want 400 unsupported", c.method, c.path, status, body)
				}
			}
			// The credential is still checked first.
			if status, _, _ := a.call("GET", "/v1/inboxes/agent-a/readers", nil, ""); status != 401 {
				t.Errorf("no credential: %d, want 401", status)
			}
		})
	}
}

// Metrics are labelled by route pattern, never by what a client chose.
func TestMetricsDoNotCarryReaderNames(t *testing.T) {
	r := newReaderEnv(t, 1)
	const name = "very-distinctive-reader-name"
	r.ack(name, 1, r.tokenA)
	r.call("DELETE", "/v1/inboxes/agent-a/readers/"+name, nil, r.tokenA)
	r.call("PUT", "/v1/inboxes/agent-a/readers/"+name+"?bad=1", map[string]any{"seq": 1, "epoch": r.epoch}, r.tokenA)
	status, _, body := r.call("GET", "/metrics", nil, testAdminToken)
	if status != 200 || strings.Contains(string(body), name) {
		t.Errorf("metrics: status %d, mentions the reader name: %v", status, strings.Contains(string(body), name))
	}
	if !strings.Contains(string(body), "readers/{name}") {
		t.Errorf("the reader routes are not counted by their pattern:\n%s", body)
	}
}
