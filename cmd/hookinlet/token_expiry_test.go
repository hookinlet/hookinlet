// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/internal/admin"
	"github.com/hookinlet/hookinlet/pkg/auth/token"
	"github.com/hookinlet/hookinlet/pkg/store/sqlite"
)

func TestParseExpiry(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for name, tc := range map[string]struct {
		in, at string
		want   *time.Time
		bad    bool
	}{
		"neither":          {},
		"hours":            {in: "720h", want: new(now.Add(720 * time.Hour))},
		"days":             {in: "90d", want: new(now.Add(90 * 24 * time.Hour))},
		"minutes":          {in: "90m", want: new(now.Add(90 * time.Minute))},
		"an absolute time": {at: "2027-01-31T00:00:00Z", want: new(at("2027-01-31T00:00:00Z"))},
		"zero":             {in: "0s", bad: true},
		"negative":         {in: "-1h", bad: true},
		"negative days":    {in: "-2d", bad: true},
		"nonsense":         {in: "soon", bad: true},
		"a time not ahead": {at: "2026-05-01T12:00:00Z", bad: true},
		"a past time":      {at: "2020-01-01T00:00:00Z", bad: true},
		"not a time":       {at: "tomorrow", bad: true},
		"both":             {in: "1h", at: "2027-01-31T00:00:00Z", bad: true},
	} {
		got, err := parseExpiry(tc.in, tc.at, now)
		switch {
		case tc.bad && err == nil:
			t.Errorf("%s: want an error, got %v", name, got)
		case !tc.bad && err != nil:
			t.Errorf("%s: %v", name, err)
		case !tc.bad && (got == nil) != (tc.want == nil), !tc.bad && got != nil && !got.Equal(*tc.want):
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
}

func TestTokenCreateExpiryAndListColumns(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{"HOOKINLET_DATA_DIR": dir, "HOOKINLET_MASTER_KEY": masterKey()}
	if code, _, _ := runCmd(t, env, "migrate"); code != 0 {
		t.Fatal("migrate failed")
	}
	if code, _, e := runCmd(t, env, "inbox", "create", "--slug", "life-inbox", "--verifier", "none"); code != 0 {
		t.Fatalf("create inbox: %s", e)
	}

	code, out, e := runCmd(t, env, "token", "create", "--name", "brief", "--inbox", "life-inbox", "--expires-in", "2h", "--json")
	var created admin.TokenCreated
	if code != 0 || json.Unmarshal([]byte(out), &created) != nil || created.Record.ExpiresAt == nil {
		t.Fatalf("token create --expires-in: %d %s %s", code, out, e)
	}
	exp, err := time.Parse(admin.TimeLayout, *created.Record.ExpiresAt)
	if err != nil || time.Until(exp) < time.Hour+50*time.Minute || time.Until(exp) > 2*time.Hour+time.Minute {
		t.Errorf("expires_at = %q (%v), want about two hours ahead", *created.Record.ExpiresAt, err)
	}
	if code, out, _ := runCmd(t, env, "token", "create", "--name", "text", "--all-inboxes", "--expires-at", "2099-01-01T00:00:00Z"); code != 0 || !strings.Contains(out, "expires:      2099-01-01") {
		t.Errorf("token create --expires-at: %d\n%s", code, out)
	}
	if code, out, _ := runCmd(t, env, "token", "create", "--name", "plain", "--all-inboxes"); code != 0 || strings.Contains(out, "expires") {
		t.Errorf("a token without an expiry mentions one: %d\n%s", code, out)
	}
	for _, args := range [][]string{
		{"token", "create", "--name", "x", "--all-inboxes", "--expires-in", "1h", "--expires-at", "2099-01-01T00:00:00Z"},
		{"token", "create", "--name", "x", "--all-inboxes", "--expires-in", "-1h"},
		{"token", "create", "--name", "x", "--all-inboxes", "--expires-at", "2001-01-01T00:00:00Z"},
		{"token", "create", "--name", "x", "--all-inboxes", "--expires-in", "whenever"},
	} {
		if code, _, _ := runCmd(t, env, args...); code != exitUsage {
			t.Errorf("%v: exit %d, want a usage error", args, code)
		}
	}

	s, err := sqlite.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	rec, err := s.GetTokenByHash(t.Context(), token.Hash(tokenRE.FindStringSubmatch(mustCreate(t, env))[1]))
	if err != nil || rec.ExpiresAt != nil {
		t.Errorf("a token without an expiry: %+v, %v", rec, err)
	}

	code, out, _ = runCmd(t, env, "token", "list")
	if code != 0 || !strings.Contains(out, "EXPIRES") || !strings.Contains(out, "LAST USED") {
		t.Fatalf("token list: %d\n%s", code, out)
	}
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.Contains(line, "plain"), strings.Contains(line, "default"):
			if !strings.Contains(line, "never") {
				t.Errorf("a token that does not expire should say so: %q", line)
			}
		case strings.Contains(line, "brief"):
			if !strings.Contains(line, "Z") || strings.Contains(line, "never") {
				t.Errorf("the expiring token's line: %q", line)
			}
		}
	}
}

func mustCreate(t *testing.T, env map[string]string) string {
	t.Helper()
	code, out, e := runCmd(t, env, "token", "create", "--name", "another", "--all-inboxes")
	if code != 0 {
		t.Fatalf("token create: %d %s", code, e)
	}
	return out
}

func TestExpiryColumnMarksAnExpiredToken(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	past, future := now.Add(-time.Hour).Format(admin.TimeLayout), now.Add(time.Hour).Format(admin.TimeLayout)
	if got := expiryColumn(nil, now); got != "never" {
		t.Errorf("no expiry: %q", got)
	}
	if got := expiryColumn(&past, now); !strings.HasSuffix(got, "(expired)") {
		t.Errorf("past: %q", got)
	}
	if got := expiryColumn(&future, now); got != future {
		t.Errorf("future: %q", got)
	}
}
