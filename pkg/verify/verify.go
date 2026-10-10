// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

// Package verify authenticates webhook deliveries. A Verifier checks a
// delivery's signature against the exact bytes received; a Registry maps the
// verifier name stored on an inbox to the code that implements it.
//
// The built-in presets are stripe, github, slack, standardwebhooks, hmac,
// token, and none. Embedders can register more with Registry.Register.
//
// Every secret or signature comparison in this package is constant-time: they
// all go through hmac.Equal or crypto/subtle.
package verify

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"sync"
	"time"
	"unicode/utf8"
)

// DefaultTolerance is how far a signed timestamp may differ from the server's
// clock, in either direction, before the delivery is rejected as stale (or from
// the future). It matches the 5 minutes Stripe and Slack recommend.
const DefaultTolerance = 5 * time.Minute

const (
	maxToleranceSeconds = 24 * 60 * 60
	// maxIDLen bounds identifiers lifted from deliveries, which end up indexed.
	maxIDLen = 256
)

// Verifier checks that a delivery is authentic.
type Verifier interface {
	// Verify checks r's signature over body, the exact bytes received. secret
	// is the inbox's decrypted verifier secret and may be empty for verifiers
	// that need none. now is the server's clock reading, used for timestamp
	// tolerance. Verify must not read r.Body.
	//
	// A failed check returns an error for which IsFailure reports true. Any
	// other error means the verifier cannot run at all (for example a missing
	// secret) and wraps ErrConfig.
	Verify(r *http.Request, body []byte, secret []byte, now time.Time) (Result, error)
}

// Result is what a successful Verify learned about the delivery.
type Result struct {
	// DeliveryID identifies the delivery for deduplication, or is empty.
	DeliveryID string
	// EventType names the kind of event, or is empty.
	EventType string
	// Challenge, when non-nil, is a handshake the sender expects echoed back
	// (Slack's url_verification). The delivery is answered with it and not
	// stored.
	Challenge []byte
}

// HeaderRedactor is implemented by verifiers whose credential travels in a
// request header. The ingest handler removes those headers before storing the
// delivery.
type HeaderRedactor interface {
	SecretHeaders() []string
}

// Factory builds a Verifier from an inbox's non-secret verifier_config, which
// is a JSON object (possibly "{}").
type Factory func(config json.RawMessage) (Verifier, error)

var (
	// ErrConfig marks a verifier that cannot run: bad configuration, a missing
	// or unusable secret, or an unknown verifier name. It is a server-side
	// problem, not a bad delivery, so it is never reported as a failed
	// signature.
	ErrConfig = errors.New("verifier misconfigured")
	// ErrUnknown is returned by Registry.New for an unregistered name. It wraps
	// ErrConfig.
	ErrUnknown = fmt.Errorf("unknown verifier: %w", ErrConfig)
)

// Failure is the error for a delivery that did not verify. Reason is short,
// fixed text that is safe to return to the sender and to store; it never
// contains secrets, signatures, or request data.
type Failure struct{ Reason string }

func (f *Failure) Error() string { return "verification failed: " + f.Reason }

func fail(reason string) error { return &Failure{Reason: reason} }

// IsFailure reports whether err is a verification failure and returns its
// reason.
func IsFailure(err error) (reason string, ok bool) {
	var f *Failure
	if errors.As(err, &f) {
		return f.Reason, true
	}
	return "", false
}

func configErr(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrConfig, fmt.Sprintf(format, args...))
}

var nameRE = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// Registry maps verifier names to factories. It is safe for concurrent use.
type Registry struct {
	mu sync.RWMutex
	m  map[string]Factory
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry { return &Registry{m: make(map[string]Factory)} }

// Default returns a new Registry holding every built-in preset. Each call
// returns an independent registry, so registering extras never affects others.
func Default() *Registry {
	r := NewRegistry()
	for name, f := range map[string]Factory{
		"none":             newNone,
		"token":            newToken,
		"hmac":             newHMAC,
		"stripe":           newStripe,
		"github":           newGitHub,
		"slack":            newSlack,
		"standardwebhooks": newStandardWebhooks,
	} {
		if err := r.Register(name, f); err != nil {
			panic(err) // the names above are valid and distinct
		}
	}
	return r
}

// Register adds a verifier. The name must be 1-32 characters from a-z, 0-9, and
// hyphen (the same rule Inbox.Validate applies) and not already registered.
func (r *Registry) Register(name string, f Factory) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("verify: invalid verifier name %q", name)
	}
	if f == nil {
		return fmt.Errorf("verify: nil factory for %q", name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.m[name]; dup {
		return fmt.Errorf("verify: verifier %q is already registered", name)
	}
	r.m[name] = f
	return nil
}

// Has reports whether name is registered.
func (r *Registry) Has(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.m[name]
	return ok
}

// Names returns the registered names in sorted order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.m))
	for n := range r.m {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// New builds the named verifier from config. The error wraps ErrUnknown for an
// unregistered name and ErrConfig for a bad config. An admin API can call New
// to validate a config when it is saved.
func (r *Registry) New(name string, config json.RawMessage) (Verifier, error) {
	r.mu.RLock()
	f, ok := r.m[name]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrUnknown, name)
	}
	if len(config) == 0 {
		config = json.RawMessage(`{}`)
	}
	v, err := f(config)
	if err != nil {
		if errors.Is(err, ErrConfig) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %w", ErrConfig, err)
	}
	return v, nil
}

// ---- helpers shared by the presets ----

// decodeConfig strictly decodes config into dst, so a misspelled key is an
// error and not silently ignored.
func decodeConfig(config json.RawMessage, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(config))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return configErr("invalid verifier_config: %v", err)
	}
	if dec.More() {
		return configErr("invalid verifier_config: trailing data")
	}
	return nil
}

// toleranceFrom converts an optional tolerance_seconds setting.
func toleranceFrom(seconds int) (time.Duration, error) {
	switch {
	case seconds == 0:
		return DefaultTolerance, nil
	case seconds < 0 || seconds > maxToleranceSeconds:
		return 0, configErr("tolerance_seconds must be between 1 and %d", maxToleranceSeconds)
	}
	return time.Duration(seconds) * time.Second, nil
}

// withinTolerance reports whether ts is within tol of now, in either direction.
func withinTolerance(ts, now time.Time, tol time.Duration) bool {
	d := now.Sub(ts)
	if d < 0 {
		d = -d
	}
	return d <= tol
}

// cleanID returns s if it is usable as an identifier taken from a delivery, and
// "" if it is empty, too long, or not valid UTF-8.
func cleanID(s string, max int) string {
	if s == "" || len(s) > max || !utf8.ValidString(s) {
		return ""
	}
	return s
}
