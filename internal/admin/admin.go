// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

// Package admin is the management of inboxes: create, change, rotate a token,
// delete, prune, back up. The admin HTTP API and the command line's direct mode
// are both thin layers over it, so the two cannot disagree about what is valid
// or what is secret.
//
// Nothing here returns a secret except the one new agent token that Create and
// RotateToken hand back, once. Views of an inbox never carry the verifier
// secret, its sealed form, or any token hash.
package admin

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hookinlet/hookinlet/pkg/auth/token"
	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/verify"
)

var (
	// ErrNoSecretBox means a verifier secret was supplied but no master key is
	// configured to seal it with.
	ErrNoSecretBox = errors.New("no master key is configured, so verifier secrets cannot be stored")
	// ErrBusy means the operation is already running.
	ErrBusy = errors.New("already running")
	// ErrNoBackup means the Store cannot produce a backup.
	ErrNoBackup = errors.New("this store does not support backups")
)

// MaxSecretBytes bounds a verifier secret.
const MaxSecretBytes = 4096

// maxPatchRetries is how often an update is retried when another update to the
// same inbox lands between its read and its write.
const maxPatchRetries = 5

// Service manages inboxes in a Store.
type Service struct {
	Store inbox.Store
	// Box seals verifier secrets. It may be nil, in which case supplying a
	// secret fails with ErrNoSecretBox and everything else works.
	Box inbox.SecretBox
	// Verifiers checks verifier names and configurations. Defaults to the
	// built-in presets.
	Verifiers *verify.Registry
	// Now is the clock for retention. Defaults to time.Now.
	Now func() time.Time

	pruneMu sync.Mutex
	// editMu serializes this process's read-modify-write edits, so that admins
	// changing one inbox together do not collide with each other. A change made
	// from outside (the command line in direct mode) can still land in between,
	// which the version check catches and the retry absorbs.
	editMu sync.Mutex
}

func (s *Service) registry() *verify.Registry {
	if s.Verifiers == nil {
		return verify.Default()
	}
	return s.Verifiers
}

func (s *Service) now() time.Time {
	if s.Now == nil {
		return time.Now()
	}
	return s.Now()
}

// View is an inbox as the admin API shows it. It has no secrets: the verifier
// secret appears only as the fact that one is set.
type View struct {
	ID      int64  `json:"id"`
	Slug    string `json:"slug"`
	Name    string `json:"name"`
	Version int64  `json:"version"`
	// IngestKey is the key in the inbox's ingest URL. Anyone who has it can post to
	// the inbox, so it is shown to the admin API and to no one else.
	IngestKey         string          `json:"ingest_key"`
	Verifier          string          `json:"verifier"`
	VerifierConfig    json.RawMessage `json:"verifier_config"`
	HasVerifierSecret bool            `json:"has_verifier_secret"`
	OnVerifyFail      string          `json:"on_verify_fail"`
	MaxBodyBytes      int64           `json:"max_body_bytes"`
	RetentionDays     int             `json:"retention_days"`
	RetentionMaxCount int64           `json:"retention_max_count"`
	EventTypePointer  string          `json:"event_type_pointer"`
	Disabled          bool            `json:"disabled"`
	LastSeq           int64           `json:"last_seq"`
	// MessageCount is set when a single inbox is shown, not in lists.
	MessageCount *int64 `json:"message_count,omitempty"`
	IngestPath   string `json:"ingest_path"`
	// IngestURL is set when the externally visible base URL is known.
	IngestURL string `json:"ingest_url,omitempty"`
	// Warnings lists settings that will make deliveries fail. It is set by the
	// calls that can cause them, and is empty when there is nothing to say.
	Warnings  []string `json:"warnings,omitempty"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
}

const timeLayout = "2006-01-02T15:04:05.000000Z"

// ViewOf converts an inbox. It copies nothing secret.
func ViewOf(in *inbox.Inbox) View {
	cfg := in.VerifierConfig
	if len(cfg) == 0 {
		cfg = json.RawMessage(`{}`)
	}
	return View{
		ID: int64(in.ID), Slug: in.Slug, IngestKey: in.IngestKey, Name: in.Name, Version: in.Version,
		Verifier: in.Verifier, VerifierConfig: append(json.RawMessage(nil), cfg...),
		HasVerifierSecret: len(in.VerifierSecret) > 0, OnVerifyFail: in.OnVerifyFail,
		MaxBodyBytes: in.MaxBodyBytes, RetentionDays: in.RetentionDays, RetentionMaxCount: in.RetentionMaxCount,
		EventTypePointer: in.EventTypePointer, Disabled: in.Disabled, LastSeq: in.LastSeq,
		IngestPath: "/in/" + in.IngestKey,
		CreatedAt:  in.CreatedAt.UTC().Format(timeLayout), UpdatedAt: in.UpdatedAt.UTC().Format(timeLayout),
	}
}

// WithBase sets IngestURL from a base URL such as "https://hooks.example.com".
func (v View) WithBase(base string) View {
	if base != "" {
		v.IngestURL = strings.TrimRight(base, "/") + v.IngestPath
	}
	return v
}

// CreateParams describes a new inbox. Zero values mean the default.
// The fields are omitted when empty, so that a client built on these types sends
// only what it means and a zero value is never mistaken for a wrong one: the
// document gives each field a type, and `null` is not one of them.
type CreateParams struct {
	Slug string `json:"slug,omitempty"`
	// IngestKey is the key in the ingest URL. Empty means a random one, which is
	// what an inbox should have: a key chosen to be readable is a key anyone can
	// guess.
	IngestKey         string          `json:"ingest_key,omitempty"`
	Name              string          `json:"name,omitempty"`
	Verifier          string          `json:"verifier,omitempty"`
	VerifierConfig    json.RawMessage `json:"verifier_config,omitempty"`
	VerifierSecret    string          `json:"verifier_secret,omitempty"`
	OnVerifyFail      string          `json:"on_verify_fail,omitempty"`
	MaxBodyBytes      int64           `json:"max_body_bytes,omitempty"`
	RetentionDays     int             `json:"retention_days,omitempty"`
	RetentionMaxCount int64           `json:"retention_max_count,omitempty"`
	EventTypePointer  string          `json:"event_type_pointer,omitempty"`
}

// Created is the result of Create and RotateToken. AgentToken is the only copy
// of the token there will ever be.
type Created struct {
	Inbox      View     `json:"inbox"`
	AgentToken string   `json:"agent_token"`
	Warnings   []string `json:"warnings,omitempty"`
}

// Create makes an inbox and a token for agents to read it with.
func (s *Service) Create(ctx context.Context, p CreateParams) (*Created, error) {
	if p.Verifier == "" {
		return nil, inbox.Invalidf("verifier is required: name how deliveries are checked (stripe, github, slack, standardwebhooks, hmac, token), or none to accept anything unchecked")
	}
	if p.Name == "" {
		p.Name = p.Slug
	}
	if p.IngestKey == "" {
		key, err := newIngestKey()
		if err != nil {
			return nil, err
		}
		p.IngestKey = key
	}
	if err := s.checkVerifier(p.Verifier, p.VerifierConfig); err != nil {
		return nil, err
	}
	if p.VerifierSecret != "" {
		if err := s.checkSecret(p.Verifier, p.VerifierConfig, p.VerifierSecret); err != nil {
			return nil, err
		}
	}
	sealed, err := s.seal(p.VerifierSecret)
	if err != nil {
		return nil, err
	}
	tok, hash, err := token.Generate()
	if err != nil {
		return nil, fmt.Errorf("admin: generate token: %w", err)
	}
	in := &inbox.Inbox{
		Slug: p.Slug, IngestKey: p.IngestKey, Name: p.Name, Verifier: p.Verifier, VerifierConfig: p.VerifierConfig,
		VerifierSecret: sealed, OnVerifyFail: p.OnVerifyFail, AgentTokenHash: hash,
		MaxBodyBytes: p.MaxBodyBytes, RetentionDays: p.RetentionDays, RetentionMaxCount: p.RetentionMaxCount,
		EventTypePointer: p.EventTypePointer,
	}
	if err := s.Store.CreateInbox(ctx, in); err != nil {
		return nil, err
	}
	return &Created{Inbox: ViewOf(in), AgentToken: tok, Warnings: warnings(in)}, nil
}

// newIngestKey makes a key for an ingest URL: 128 random bits, URL-safe.
func newIngestKey() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("admin: generate ingest key: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// RotateIngestKey gives the inbox a new random ingest key. The old key stops
// working at once, so senders have to be given the new URL.
func (s *Service) RotateIngestKey(ctx context.Context, slug string) (View, error) {
	key, err := newIngestKey()
	if err != nil {
		return View{}, err
	}
	s.editMu.Lock()
	defer s.editMu.Unlock()
	for attempt := 0; ; attempt++ {
		in, err := s.Store.GetInboxBySlug(ctx, slug)
		if err != nil {
			return View{}, err
		}
		in.IngestKey = key
		err = s.Store.UpdateInbox(ctx, in)
		if errors.Is(err, inbox.ErrConflict) && attempt < maxPatchRetries {
			continue
		}
		if err != nil {
			return View{}, err
		}
		return ViewOf(in), nil
	}
}

// warnings lists settings that will make deliveries fail, so an admin who set
// an inbox up in several steps is not surprised.
func warnings(in *inbox.Inbox) []string {
	if in.Verifier != "none" && inbox.KnownVerifier(in.Verifier) && len(in.VerifierSecret) == 0 {
		return []string{fmt.Sprintf("the %q verifier needs a secret; deliveries fail until one is set", in.Verifier)}
	}
	return nil
}

// viewWithWarnings is the view of an inbox that has just been changed.
func viewWithWarnings(in *inbox.Inbox) View {
	v := ViewOf(in)
	v.Warnings = warnings(in)
	return v
}

// Get returns one inbox, with its message count.
func (s *Service) Get(ctx context.Context, slug string) (View, error) {
	in, err := s.Store.GetInboxBySlug(ctx, slug)
	if err != nil {
		return View{}, err
	}
	stats, err := s.Store.Stats(ctx, in.ID)
	if err != nil {
		return View{}, err
	}
	v := ViewOf(in)
	v.MessageCount = &stats.MessageCount
	return v, nil
}

// List returns a page of inboxes in ID order. next is the ID to pass as after
// for the following page, or 0 when this was the last.
func (s *Service) List(ctx context.Context, after inbox.ID, limit int) (views []View, next inbox.ID, err error) {
	p := inbox.ListParams{AfterID: after, Limit: limit}
	page, err := s.Store.ListInboxes(ctx, p)
	if err != nil {
		return nil, 0, err
	}
	views = make([]View, 0, len(page))
	for _, in := range page {
		views = append(views, ViewOf(in))
	}
	if len(page) > 0 && len(page) >= p.EffectiveLimit() {
		next = page[len(page)-1].ID
	}
	return views, next, nil
}

// Patch changes settings. A nil field is left as it is. The slug and the
// secrets are not here: the slug never changes, and the secret and the token
// have their own operations.
type Patch struct {
	Name              *string          `json:"name,omitempty"`
	Verifier          *string          `json:"verifier,omitempty"`
	VerifierConfig    *json.RawMessage `json:"verifier_config,omitempty"`
	OnVerifyFail      *string          `json:"on_verify_fail,omitempty"`
	MaxBodyBytes      *int64           `json:"max_body_bytes,omitempty"`
	RetentionDays     *int             `json:"retention_days,omitempty"`
	RetentionMaxCount *int64           `json:"retention_max_count,omitempty"`
	EventTypePointer  *string          `json:"event_type_pointer,omitempty"`
	Disabled          *bool            `json:"disabled,omitempty"`
}

func (p Patch) empty() bool { return p == Patch{} }

// Update applies a patch. If another change to the same inbox lands first, the
// patch is applied again on top of it.
func (s *Service) Update(ctx context.Context, slug string, p Patch) (View, error) {
	if p.empty() {
		return View{}, inbox.Invalidf("nothing to change")
	}
	s.editMu.Lock()
	defer s.editMu.Unlock()
	for attempt := 0; ; attempt++ {
		in, err := s.Store.GetInboxBySlug(ctx, slug)
		if err != nil {
			return View{}, err
		}
		if p.Name != nil {
			in.Name = *p.Name
		}
		var cleared bool
		if p.Verifier != nil {
			if *p.Verifier != in.Verifier && len(in.VerifierSecret) > 0 {
				// The secret was set for another scheme. Keeping it would make it a
				// credential of the new one: a GitHub signing key accepted as a shared token.
				in.VerifierSecret = nil
				cleared = true
			}
			in.Verifier = *p.Verifier
		}
		if p.VerifierConfig != nil {
			in.VerifierConfig = *p.VerifierConfig
		}
		if p.Verifier != nil || p.VerifierConfig != nil {
			if err := s.checkVerifier(in.Verifier, in.VerifierConfig); err != nil {
				return View{}, err
			}
		}
		if p.OnVerifyFail != nil {
			in.OnVerifyFail = *p.OnVerifyFail
		}
		if p.MaxBodyBytes != nil {
			in.MaxBodyBytes = *p.MaxBodyBytes
		}
		if p.RetentionDays != nil {
			in.RetentionDays = *p.RetentionDays
		}
		if p.RetentionMaxCount != nil {
			in.RetentionMaxCount = *p.RetentionMaxCount
		}
		if p.EventTypePointer != nil {
			in.EventTypePointer = *p.EventTypePointer
		}
		if p.Disabled != nil {
			in.Disabled = *p.Disabled
		}
		err = s.Store.UpdateInbox(ctx, in)
		if errors.Is(err, inbox.ErrConflict) && attempt < maxPatchRetries {
			continue
		}
		if err != nil {
			return View{}, err
		}
		v := viewWithWarnings(in)
		if cleared {
			v.Warnings = append([]string{"the verifier changed, so its secret was removed: set one for the new verifier"}, v.Warnings...)
		}
		return v, nil
	}
}

// SetSecret sets or replaces an inbox's verifier secret.
func (s *Service) SetSecret(ctx context.Context, slug, secret string) (View, error) {
	if secret == "" {
		return View{}, inbox.Invalidf("secret must not be empty")
	}
	sealed, err := s.seal(secret)
	if err != nil {
		return View{}, err
	}
	s.editMu.Lock()
	defer s.editMu.Unlock()
	for attempt := 0; ; attempt++ {
		in, err := s.Store.GetInboxBySlug(ctx, slug)
		if err != nil {
			return View{}, err
		}
		if err := s.checkSecret(in.Verifier, in.VerifierConfig, secret); err != nil {
			return View{}, err
		}
		in.VerifierSecret = sealed
		err = s.Store.UpdateInbox(ctx, in)
		if errors.Is(err, inbox.ErrConflict) && attempt < maxPatchRetries {
			continue
		}
		if err != nil {
			return View{}, err
		}
		return viewWithWarnings(in), nil
	}
}

// checkSecret refuses a secret the inbox's verifier cannot use, when the
// verifier can tell: a mistake is found when it is made and not on the first
// delivery, which the sender may never retry.
func (s *Service) checkSecret(name string, config json.RawMessage, secret string) error {
	v, err := s.registry().New(name, config)
	if err != nil {
		return nil // the verifier itself is checked elsewhere
	}
	if c, ok := v.(verify.SecretChecker); ok {
		if err := c.CheckSecret([]byte(secret)); err != nil {
			return inbox.Invalidf("%s", strings.TrimPrefix(err.Error(), verify.ErrConfig.Error()+": "))
		}
	}
	return nil
}

// CheckSecret is checkSecret for a caller that has no Service, such as the
// command line checking a file before it changes anything.
func CheckSecret(verifier string, config json.RawMessage, secret string) error {
	return new(Service).checkSecret(verifier, config, secret)
}

// RotateToken replaces the inbox's agent token. The old token stops working at
// once; the new one is returned here and never again.
func (s *Service) RotateToken(ctx context.Context, slug string) (*Created, error) {
	tok, hash, err := token.Generate()
	if err != nil {
		return nil, fmt.Errorf("admin: generate token: %w", err)
	}
	s.editMu.Lock()
	defer s.editMu.Unlock()
	for attempt := 0; ; attempt++ {
		in, err := s.Store.GetInboxBySlug(ctx, slug)
		if err != nil {
			return nil, err
		}
		in.AgentTokenHash = hash
		err = s.Store.UpdateInbox(ctx, in)
		if errors.Is(err, inbox.ErrConflict) && attempt < maxPatchRetries {
			continue
		}
		if err != nil {
			return nil, err
		}
		return &Created{Inbox: ViewOf(in), AgentToken: tok}, nil
	}
}

// Delete removes an inbox and all of its messages.
func (s *Service) Delete(ctx context.Context, slug string) error {
	in, err := s.Store.GetInboxBySlug(ctx, slug)
	if err != nil {
		return err
	}
	return s.Store.DeleteInbox(ctx, in.ID)
}

// RunRetention applies every inbox's retention policy now. It fails with
// ErrBusy if a sweep is already running.
func (s *Service) RunRetention(ctx context.Context) (deleted int64, err error) {
	if !s.pruneMu.TryLock() {
		return 0, ErrBusy
	}
	defer s.pruneMu.Unlock()
	return s.Store.Prune(ctx, s.now())
}

// SweepRetention is RunRetention for the scheduled sweep: if someone is
// pruning already it waits its turn instead of failing.
func (s *Service) SweepRetention(ctx context.Context) (deleted int64, err error) {
	s.pruneMu.Lock()
	defer s.pruneMu.Unlock()
	return s.Store.Prune(ctx, s.now())
}

// Backup takes a snapshot of the Store. The caller closes the reader.
func (s *Service) Backup(ctx context.Context) (io.ReadCloser, int64, error) {
	b, ok := s.Store.(inbox.Backupper) // the documented optional capability
	if !ok {
		return nil, 0, ErrNoBackup
	}
	return b.Backup(ctx)
}

func (s *Service) seal(secret string) ([]byte, error) {
	if secret == "" {
		return nil, nil
	}
	if len(secret) > MaxSecretBytes {
		return nil, inbox.Invalidf("secret must be at most %d bytes", MaxSecretBytes)
	}
	if s.Box == nil {
		return nil, ErrNoSecretBox
	}
	return s.Box.Seal([]byte(secret))
}

// checkVerifier makes sure the verifier exists and accepts its configuration,
// so a mistake is reported when it is made and not on a sender's first delivery.
func (s *Service) checkVerifier(name string, config json.RawMessage) error {
	if !utf8.ValidString(name) {
		return inbox.Invalidf("verifier must be valid UTF-8")
	}
	if len(config) > 0 && !json.Valid(config) {
		return inbox.Invalidf("verifier_config must be valid JSON")
	}
	if _, err := s.registry().New(name, config); err != nil {
		if errors.Is(err, verify.ErrUnknown) {
			return inbox.Invalidf("unknown verifier %q (known: %s)", name, strings.Join(s.registry().Names(), ", "))
		}
		return inbox.Invalidf("%s", strings.TrimPrefix(err.Error(), verify.ErrConfig.Error()+": "))
	}
	return nil
}
