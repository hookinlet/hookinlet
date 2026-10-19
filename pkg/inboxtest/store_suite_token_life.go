// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inboxtest

import (
	"time"

	"testing"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// closeTo reports whether two times are within a second, which is as exactly as a
// Store need keep one: a backend may keep fewer digits than a time.Time has.
func closeTo(a, b time.Time) bool { return a.Sub(b).Abs() < time.Second }

// testTokenExpiry checks Token.ExpiresAt. A Store that does not keep it (it returns
// the token without one) skips the case, so that a Store written before expiry
// existed is not failed by a newer suite; one that keeps it must also enforce it.
func testTokenExpiry(t *testing.T, s inbox.Store, clk *Clock) {
	ctx := t.Context()
	expiry := clk.Now().Add(time.Hour)
	probe := mkToken("expiring", func(tk *inbox.Token) { tk.ExpiresAt = &expiry })
	if err := s.CreateToken(ctx, probe); err != nil {
		t.Fatalf("CreateToken with an expiry in the future: %v", err)
	}
	got, err := s.GetTokenByHash(ctx, tokenHash("expiring"))
	if err != nil {
		t.Fatal(err)
	}
	if got.ExpiresAt == nil {
		t.Skip("this Store does not keep Token.ExpiresAt; see inbox.Token")
	}
	if !closeTo(*got.ExpiresAt, expiry) {
		t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, expiry)
	}
	if probe.ExpiresAt == nil || !closeTo(*probe.ExpiresAt, expiry) {
		t.Errorf("CreateToken left ExpiresAt as %v in the token it was given", probe.ExpiresAt)
	}
	forever := mkToken("forever")
	if err := s.CreateToken(ctx, forever); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetTokenByHash(ctx, tokenHash("forever")); err != nil || got.ExpiresAt != nil {
		t.Errorf("a token made without an expiry: %+v, %v", got, err)
	}

	// An expiry that is not in the future is refused, by the Store's clock.
	for name, at := range map[string]time.Time{"now": clk.Now(), "the past": clk.Now().Add(-time.Hour)} {
		bad := mkToken("bad-"+name, func(tk *inbox.Token) { tk.ExpiresAt = &at })
		isErr(t, "an expiry of "+name, s.CreateToken(ctx, bad), inbox.ErrInvalid)
		_, err := s.GetTokenByHash(ctx, tokenHash("bad-"+name))
		isErr(t, "a refused token", err, inbox.ErrNotFound)
	}

	// ListTokens shows it.
	listed, err := s.ListTokens(ctx, inbox.TokenListParams{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tk := range listed {
		switch tk.Name {
		case "expiring":
			if tk.ExpiresAt == nil || !closeTo(*tk.ExpiresAt, expiry) {
				t.Errorf("ListTokens shows ExpiresAt %v for the expiring token", tk.ExpiresAt)
			}
		case "forever":
			if tk.ExpiresAt != nil {
				t.Errorf("ListTokens shows ExpiresAt %v for a token without one", tk.ExpiresAt)
			}
		}
	}

	// It works until the moment, and not from the moment.
	clk.Advance(time.Hour - time.Minute)
	if _, err := s.GetTokenByHash(ctx, tokenHash("expiring")); err != nil {
		t.Errorf("a minute before it expires: %v", err)
	}
	clk.Advance(time.Minute)
	_, err = s.GetTokenByHash(ctx, tokenHash("expiring"))
	isErr(t, "a token at its expiry", err, inbox.ErrNotFound)
	clk.Advance(24 * time.Hour)
	_, err = s.GetTokenByHash(ctx, tokenHash("expiring"))
	isErr(t, "a token long after its expiry", err, inbox.ErrNotFound)
	if _, err := s.GetTokenByHash(ctx, tokenHash("forever")); err != nil {
		t.Errorf("a token with no expiry stopped working: %v", err)
	}

	// An expired token is still listed, so that it can be seen and revoked.
	listed, err = s.ListTokens(ctx, inbox.TokenListParams{})
	if err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, tk := range listed {
		seen = seen || tk.Name == "expiring"
	}
	if !seen {
		t.Error("an expired token is no longer listed")
	}
	if err := s.DeleteToken(ctx, probe.ID); err != nil {
		t.Errorf("revoking an expired token: %v", err)
	}
}

// testTokenLastUsed checks inbox.TokenUseRecorder. A Store that does not implement
// it skips the case: it has no last-used time, which is allowed.
func testTokenLastUsed(t *testing.T, s inbox.Store, clk *Clock) {
	rec, ok := s.(inbox.TokenUseRecorder)
	if !ok {
		t.Skip("this Store does not implement inbox.TokenUseRecorder, so it records no last use")
	}
	ctx := t.Context()
	lastUsed := func(name string) *time.Time {
		t.Helper()
		got, err := s.GetTokenByHash(ctx, tokenHash(name))
		if err != nil {
			t.Fatal(err)
		}
		return got.LastUsedAt
	}
	tk := mkToken("used", func(tk *inbox.Token) { tk.LastUsedAt = new(clk.Now().Add(-time.Hour)) })
	if err := s.CreateToken(ctx, tk); err != nil {
		t.Fatal(err)
	}
	if got := lastUsed("used"); got != nil {
		t.Errorf("a new token has LastUsedAt %v; CreateToken must ignore the one it was given", got)
	}
	other := mkToken("unused")
	if err := s.CreateToken(ctx, other); err != nil {
		t.Fatal(err)
	}

	first := clk.Now().Add(time.Minute)
	if err := rec.TouchToken(ctx, tk.ID, first); err != nil {
		t.Fatal(err)
	}
	if got := lastUsed("used"); got == nil || !closeTo(*got, first) {
		t.Errorf("after a touch at %v, LastUsedAt = %v", first, got)
	}
	if got := lastUsed("unused"); got != nil {
		t.Errorf("touching one token set LastUsedAt of another: %v", got)
	}

	// It only moves forward, and an earlier touch is not an error.
	if err := rec.TouchToken(ctx, tk.ID, first.Add(-10*time.Minute)); err != nil {
		t.Errorf("a touch earlier than the last: %v", err)
	}
	if got := lastUsed("used"); got == nil || !closeTo(*got, first) {
		t.Errorf("an earlier touch moved LastUsedAt to %v", got)
	}
	later := first.Add(10 * time.Minute)
	if err := rec.TouchToken(ctx, tk.ID, later); err != nil {
		t.Fatal(err)
	}
	if got := lastUsed("used"); got == nil || !closeTo(*got, later) {
		t.Errorf("a later touch left LastUsedAt at %v, want %v", got, later)
	}

	listed, err := s.ListTokens(ctx, inbox.TokenListParams{})
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range listed {
		switch {
		case l.Name == "used" && (l.LastUsedAt == nil || !closeTo(*l.LastUsedAt, later)):
			t.Errorf("ListTokens shows LastUsedAt %v for a used token", l.LastUsedAt)
		case l.Name == "unused" && l.LastUsedAt != nil:
			t.Errorf("ListTokens shows LastUsedAt %v for an unused token", l.LastUsedAt)
		}
	}

	if err := s.DeleteToken(ctx, tk.ID); err != nil {
		t.Fatal(err)
	}
	isErr(t, "a touch of a revoked token", rec.TouchToken(ctx, tk.ID, later.Add(time.Minute)), inbox.ErrNotFound)
	isErr(t, "a touch of a token that never was", rec.TouchToken(ctx, 999999, later), inbox.ErrNotFound)
}
