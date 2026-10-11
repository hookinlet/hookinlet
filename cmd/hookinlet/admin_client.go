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
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/hookinlet/hookinlet/internal/admin"
	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// adminAPI is what the management commands need, however they reach the data:
// through a running server's admin API or straight in the data directory.
type adminAPI interface {
	Create(ctx context.Context, p admin.CreateParams) (*admin.Created, error)
	Get(ctx context.Context, slug string) (admin.View, error)
	List(ctx context.Context) ([]admin.View, error)
	Update(ctx context.Context, slug string, p admin.Patch) (admin.View, error)
	SetSecret(ctx context.Context, slug, secret string) (admin.View, error)
	RotateToken(ctx context.Context, slug string) (*admin.Created, error)
	RotateIngestKey(ctx context.Context, slug string) (admin.View, error)
	Delete(ctx context.Context, slug string) error
	RunRetention(ctx context.Context) (deleted int64, err error)
	Backup(ctx context.Context) (io.ReadCloser, int64, error)
}

// ---- direct: the data directory ----

type directAdmin struct{ svc *admin.Service }

func (d directAdmin) Create(ctx context.Context, p admin.CreateParams) (*admin.Created, error) {
	return d.svc.Create(ctx, p)
}
func (d directAdmin) Get(ctx context.Context, slug string) (admin.View, error) {
	return d.svc.Get(ctx, slug)
}
func (d directAdmin) List(ctx context.Context) ([]admin.View, error) {
	var all []admin.View
	var after inbox.ID
	for {
		page, next, err := d.svc.List(ctx, after, 0)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if next == 0 {
			return all, nil
		}
		after = next
	}
}
func (d directAdmin) Update(ctx context.Context, slug string, p admin.Patch) (admin.View, error) {
	return d.svc.Update(ctx, slug, p)
}
func (d directAdmin) SetSecret(ctx context.Context, slug, secret string) (admin.View, error) {
	return d.svc.SetSecret(ctx, slug, secret)
}
func (d directAdmin) RotateIngestKey(ctx context.Context, slug string) (admin.View, error) {
	return d.svc.RotateIngestKey(ctx, slug)
}
func (d directAdmin) RotateToken(ctx context.Context, slug string) (*admin.Created, error) {
	return d.svc.RotateToken(ctx, slug)
}
func (d directAdmin) Delete(ctx context.Context, slug string) error { return d.svc.Delete(ctx, slug) }
func (d directAdmin) RunRetention(ctx context.Context) (int64, error) {
	return d.svc.RunRetention(ctx)
}
func (d directAdmin) Backup(ctx context.Context) (io.ReadCloser, int64, error) {
	return d.svc.Backup(ctx)
}

// ---- remote: a running server ----

// apiError is an error the server answered with.
type apiError struct {
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("%s (HTTP %d, %s)", e.Message, e.Status, e.Code)
}

// remoteAdmin talks to a server's admin API with the admin token.
type remoteAdmin struct {
	base  string
	token string
	http  *http.Client
}

// newRemoteAdmin checks the address and builds a client. The token is a
// credential for every inbox, so it is not sent over plain HTTP to another
// machine unless the caller says that is fine.
func newRemoteAdmin(rawURL, tok string, allowInsecure bool) (*remoteAdmin, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return nil, errors.New("--url must be an http:// or https:// address such as https://hooks.example.com")
	}
	if u.Scheme == "http" && !allowInsecure && !isLoopback(u.Hostname()) {
		return nil, errors.New("refusing to send the admin token over plain HTTP to another machine; use https://, or pass --insecure-http if the network is trusted")
	}
	return &remoteAdmin{
		base:  strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/"),
		token: tok,
		// No overall timeout: a backup takes as long as the database is big.
		// Callers bound the other calls with a context.
		http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (r *remoteAdmin) request(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.base+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := r.http.Do(req)
	if err != nil {
		// The URL error text can include the address but never the header.
		return nil, fmt.Errorf("cannot reach the server: %w", err)
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("the server redirected the request (HTTP %d); use the address it redirects to as --url", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		defer func() { _ = resp.Body.Close() }()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var e struct {
			Error struct{ Code, Message string }
		}
		if json.Unmarshal(data, &e) == nil && e.Error.Message != "" {
			return nil, &apiError{Status: resp.StatusCode, Code: e.Error.Code, Message: e.Error.Message}
		}
		return nil, &apiError{Status: resp.StatusCode, Code: "error", Message: "the server answered with an error"}
	}
	return resp, nil
}

func (r *remoteAdmin) do(ctx context.Context, method, path string, body, out any) error {
	resp, err := r.request(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(out); err != nil {
		return fmt.Errorf("the server's answer was not understood: %w", err)
	}
	return nil
}

func slugPath(slug string) string { return "/admin/inboxes/" + url.PathEscape(slug) }

func (r *remoteAdmin) Create(ctx context.Context, p admin.CreateParams) (*admin.Created, error) {
	var out admin.Created
	if err := r.do(ctx, http.MethodPost, "/admin/inboxes", p, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
func (r *remoteAdmin) Get(ctx context.Context, slug string) (admin.View, error) {
	var v admin.View
	err := r.do(ctx, http.MethodGet, slugPath(slug), nil, &v)
	return v, err
}
func (r *remoteAdmin) List(ctx context.Context) ([]admin.View, error) {
	var all []admin.View
	path := "/admin/inboxes"
	for pages := 0; path != "" && pages < 10_000; pages++ {
		var page struct {
			Inboxes []admin.View `json:"inboxes"`
			Next    *string      `json:"next"`
		}
		if err := r.do(ctx, http.MethodGet, path, nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Inboxes...)
		path = ""
		if page.Next != nil {
			// Only ever follow the server's cursor on its own admin path.
			if !strings.HasPrefix(*page.Next, "/admin/inboxes?") {
				return nil, errors.New("the server sent a next link that was not understood")
			}
			path = *page.Next
		}
	}
	return all, nil
}
func (r *remoteAdmin) Update(ctx context.Context, slug string, p admin.Patch) (admin.View, error) {
	var v admin.View
	err := r.do(ctx, http.MethodPatch, slugPath(slug), p, &v)
	return v, err
}
func (r *remoteAdmin) SetSecret(ctx context.Context, slug, secret string) (admin.View, error) {
	var v admin.View
	err := r.do(ctx, http.MethodPut, slugPath(slug)+"/secret", map[string]string{"secret": secret}, &v)
	return v, err
}
func (r *remoteAdmin) RotateToken(ctx context.Context, slug string) (*admin.Created, error) {
	var out admin.Created
	if err := r.do(ctx, http.MethodPost, slugPath(slug)+"/rotate-token", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
func (r *remoteAdmin) RotateIngestKey(ctx context.Context, slug string) (admin.View, error) {
	var v admin.View
	err := r.do(ctx, http.MethodPost, slugPath(slug)+"/rotate-ingest-key", nil, &v)
	return v, err
}
func (r *remoteAdmin) Delete(ctx context.Context, slug string) error {
	return r.do(ctx, http.MethodDelete, slugPath(slug), nil, nil)
}
func (r *remoteAdmin) RunRetention(ctx context.Context) (int64, error) {
	var out struct {
		Deleted int64 `json:"deleted"`
	}
	err := r.do(ctx, http.MethodPost, "/admin/retention/run", nil, &out)
	return out.Deleted, err
}
func (r *remoteAdmin) Backup(ctx context.Context) (io.ReadCloser, int64, error) {
	resp, err := r.request(ctx, http.MethodGet, "/admin/backup", nil)
	if err != nil {
		return nil, 0, err
	}
	size, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	if err != nil || size < 0 {
		_ = resp.Body.Close()
		return nil, 0, errors.New("the server did not say how big the backup is")
	}
	return resp.Body, size, nil
}
