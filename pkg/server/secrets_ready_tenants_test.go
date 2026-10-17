// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// tenantKeyBox is a SecretBox whose key for some tenants is gone, as when a key
// service has lost one organization's key: Open fails for the secrets of the inboxes
// in down (by their sealing context, which names the inbox's UID) and works for the
// rest. It counts the opens.
type tenantKeyBox struct {
	inbox.SecretBox
	down  map[string]bool
	opens int
}

func (b *tenantKeyBox) Open(ct, context []byte) ([]byte, error) {
	b.opens++
	if b.down[string(context)] {
		return nil, errors.New("key service: no key for this tenant")
	}
	return b.SecretBox.Open(ct, context)
}

// tenantSecrets makes inboxes in the order given, each with a verifier secret, in
// the tenant named for it, and returns a box over the env's own whose key is gone for
// the tenants in failing. The secrets are "plain-<slug>", which a log must never show,
// and the sealed values are returned too, for the same reason.
func tenantSecrets(e *env, tenants []string, failing ...string) (*tenantKeyBox, [][]byte) {
	e.t.Helper()
	box := &tenantKeyBox{SecretBox: e.box, down: map[string]bool{}}
	var sealed [][]byte
	for i, tenant := range tenants {
		slug := fmt.Sprintf("hook-%03d", i)
		in := e.newInbox(slug, "github", "plain-"+slug, func(in *inbox.Inbox) { in.Tenant = tenant })
		sealed = append(sealed, in.VerifierSecret)
		for _, f := range failing {
			if f == tenant {
				box.down[string(inbox.VerifierSecretContext(in.UID))] = true
			}
		}
	}
	e.server.adm.Box = box
	return box, sealed
}

func repeat(tenant string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = tenant
	}
	return out
}

// secretLogLines returns the log lines about secrets that do not open, by the
// tenant they name ("" for a line without a tenant field).
func secretLogLines(t *testing.T, e *env) map[string][]map[string]any {
	t.Helper()
	lines := map[string][]map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(e.logs.String()), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		if _, ok := m["unreadable"]; !ok {
			continue
		}
		tenant, _ := m["tenant"].(string)
		lines[tenant] = append(lines[tenant], m)
	}
	return lines
}

// noSecretsLogged fails if the log shows any secret, opened or sealed.
func noSecretsLogged(t *testing.T, e *env, sealed [][]byte) {
	t.Helper()
	logs := e.logs.String()
	if strings.Contains(logs, "plain-") {
		t.Error("the log shows a verifier secret")
	}
	for _, s := range sealed {
		for _, enc := range []string{base64.StdEncoding.EncodeToString(s), base64.RawURLEncoding.EncodeToString(s), fmt.Sprintf("%x", s)} {
			if strings.Contains(logs, enc[:24]) {
				t.Fatal("the log shows a sealed verifier secret")
			}
		}
	}
}

// One tenant whose key is gone does not take the server, and every other tenant on
// it, out of rotation, even when that tenant owns the oldest inboxes. It is named in
// the log, with how many of its sampled secrets did not open.
func TestReadyzStaysUpWhenOneTenantsSecretsDoNotOpen(t *testing.T) {
	e := newEnv(t)
	box, sealed := tenantSecrets(e, append(repeat("acme", 30), repeat("globex", 3)...), "acme")
	if rec := e.do(req{method: http.MethodGet, path: "/readyz"}); rec.Code != http.StatusOK {
		t.Fatalf("/readyz with one tenant's key gone: %d %s", rec.Code, rec.Body)
	}
	if box.opens > readySecretSample {
		t.Errorf("readiness opened %d secrets, more than its sample of %d", box.opens, readySecretSample)
	}
	lines := secretLogLines(t, e)
	if len(lines["acme"]) != 1 {
		t.Fatalf("log lines naming acme: %v (all: %v)", lines["acme"], lines)
	}
	if n, _ := lines["acme"][0]["unreadable"].(float64); n < 1 {
		t.Errorf("the line does not count acme's unreadable secrets: %v", lines["acme"][0])
	}
	if len(lines["globex"]) != 0 || len(lines[""]) != 0 {
		t.Errorf("a tenant whose secrets open was logged as failing: %v", lines)
	}
	noSecretsLogged(t, e, sealed)

	// The answer is believed for a while: neither opened nor logged again.
	opens := box.opens
	for range 3 {
		if rec := e.do(req{method: http.MethodGet, path: "/readyz"}); rec.Code != http.StatusOK {
			t.Fatalf("/readyz again: %d", rec.Code)
		}
	}
	if box.opens != opens || len(secretLogLines(t, e)["acme"]) != 1 {
		t.Errorf("later probes opened %d more secrets and logged %d lines", box.opens-opens, len(secretLogLines(t, e)["acme"]))
	}
}

// Tenants whose inboxes come in turn: the one whose key is gone is logged, here the
// default tenant, whose lines carry no tenant field.
func TestReadyzStaysUpWhenInterleavedTenantsHaveOneFailing(t *testing.T) {
	e := newEnv(t)
	var tenants []string
	for range 20 {
		tenants = append(tenants, "", "acme")
	}
	_, sealed := tenantSecrets(e, tenants, "")
	if rec := e.do(req{method: http.MethodGet, path: "/readyz"}); rec.Code != http.StatusOK {
		t.Fatalf("/readyz: %d %s", rec.Code, rec.Body)
	}
	lines := secretLogLines(t, e)
	if len(lines[""]) != 1 || len(lines["acme"]) != 0 {
		t.Fatalf("log lines: %v", lines)
	}
	if _, has := lines[""][0]["tenant"]; has {
		t.Errorf("the default tenant's line has a tenant field: %v", lines[""][0])
	}
	noSecretsLogged(t, e, sealed)
}

// When nothing opens, whatever the tenant, the server is not ready: a wrong master
// key, or a key service that fails for everyone, looks like this. The reason does not
// claim to know which, and each tenant is logged.
func TestReadyzFailsWhenEveryTenantsSecretsFail(t *testing.T) {
	e := newEnv(t)
	_, sealed := tenantSecrets(e, append(repeat("acme", 30), repeat("globex", 3)...), "acme", "globex")
	rec := e.do(req{method: http.MethodGet, path: "/readyz"})
	if rec.Code != http.StatusServiceUnavailable || errorCode(t, rec) != "not_ready" {
		t.Fatalf("/readyz with every key gone: %d %s", rec.Code, rec.Body)
	}
	if body := rec.Body.String(); !strings.Contains(body, "key service") || strings.Contains(body, "hook-") || strings.Contains(body, "acme") {
		t.Errorf("the reason: %s", body)
	}
	lines := secretLogLines(t, e)
	if len(lines["acme"]) != 1 || len(lines["globex"]) != 1 {
		t.Errorf("log lines: %v", lines)
	}
	noSecretsLogged(t, e, sealed)
}

// With one tenant only, its key being gone cannot be told from a wrong master key,
// and the server is not ready.
func TestReadyzFailsWhenTheOnlyTenantsSecretsFail(t *testing.T) {
	e := newEnv(t)
	_, sealed := tenantSecrets(e, repeat("acme", 10), "acme")
	if rec := e.do(req{method: http.MethodGet, path: "/readyz"}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz: %d %s", rec.Code, rec.Body)
	}
	lines := secretLogLines(t, e)
	if len(lines["acme"]) != 1 {
		t.Fatalf("log lines: %v", lines)
	}
	if msg, _ := lines["acme"][0]["msg"].(string); !strings.Contains(msg, "one tenant") {
		t.Errorf("the line does not say that one tenant's key alone looks the same: %q", msg)
	}
	noSecretsLogged(t, e, sealed)
}
