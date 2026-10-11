// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hookinlet/hookinlet/internal/admin"
	"github.com/hookinlet/hookinlet/internal/applyfile"
	"github.com/hookinlet/hookinlet/pkg/store/sqlite"
)

const applyUsage = `Usage: hookinlet apply -f FILE [flags]

Brings the inboxes named in FILE (a YAML file; "-" reads standard input) to the
state it declares. An inbox that does not exist is created, with its secret; one
that does is changed only in the settings that differ. A setting the file leaves
out is left alone on an existing inbox and gets the server's default on a new
one. apply never deletes an inbox or a message.

The whole file and every secret it names are checked before anything is
changed. Verifier secrets are read from an environment variable (secret_env) or
a file (secret_file), never from the YAML, and are written to the server on
every run because they cannot be read back.

The agent tokens of newly created inboxes are written once to the file given
with --tokens-out (JSON, mode 0600, never overwritten) and never printed;
creating an inbox without --tokens-out is refused.

Flags:
`

func applyCmd(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hookinlet apply", flag.ContinueOnError)
	fs.SetOutput(stderr)
	c := addConn(fs, getenv)
	var (
		file      = fs.String("f", "", "the YAML file to apply, or - for standard input (required)")
		dryRun    = fs.Bool("dry-run", false, "check everything and print what would happen, changing nothing")
		tokensOut = fs.String("tokens-out", "", "file to write the agent tokens of newly created inboxes to, as JSON {slug: token}; must not exist")
	)
	fs.Usage = func() {
		fmt.Fprint(stderr, applyUsage)
		fs.PrintDefaults()
	}
	if code, done := parseFlags(fs, args, stderr); done {
		return code
	}
	if *file == "" {
		fmt.Fprintln(stderr, "hookinlet: apply needs -f FILE (or -f - for standard input)")
		return exitUsage
	}

	// 1. Everything that can be checked without the server.
	data, err := readApplyFile(*file)
	if err != nil {
		fmt.Fprintf(stderr, "hookinlet: %v\n", err)
		return exitUsage
	}
	specs, err := applyfile.Parse(data)
	if err != nil {
		fmt.Fprintf(stderr, "hookinlet: %s is not valid:\n  %s\n", displayName(*file), indentLines(err))
		return exitUsage
	}
	secrets, err := applyfile.ResolveSecrets(specs, getenv)
	if err != nil {
		fmt.Fprintf(stderr, "hookinlet: the secrets for %s cannot be read:\n  %s\n", displayName(*file), indentLines(err))
		return exitUsage
	}
	if len(secrets) > 0 {
		if code := c.checkSealing(stderr); code != 0 {
			return code
		}
	}

	ctx, stop := notifyContext()
	defer stop()
	var api adminAPI
	if *dryRun && c.noDatabaseYet() {
		api = emptyAdmin{} // a dry run must not create the database it would plan against
	} else {
		var closeAPI func()
		var code int
		if api, closeAPI, code = c.open(ctx, stderr, !*dryRun); code != 0 {
			return code
		}
		defer closeAPI()
	}

	// 2. Read the current state and work out the plan.
	listCtx, cancel := context.WithTimeout(ctx, callTimeout)
	views, err := api.List(listCtx)
	cancel()
	if err != nil {
		return fail(stderr, err)
	}
	current := make(map[string]*admin.View, len(views))
	for i := range views {
		current[views[i].Slug] = &views[i]
	}
	declared := make(map[string]bool, len(specs))
	items := make([]applyfile.Item, 0, len(specs))
	creates := 0
	for _, s := range specs {
		declared[s.Slug] = true
		it := applyfile.Plan(s, current[s.Slug])
		if it.Action == applyfile.Create {
			creates++
		}
		items = append(items, it)
	}
	var notManaged []string
	for _, v := range views {
		if !declared[v.Slug] {
			notManaged = append(notManaged, v.Slug)
		}
	}
	sort.Strings(notManaged)

	// 3. Where the new tokens go is settled before anything changes.
	var sink *tokenSink
	if creates > 0 {
		if *tokensOut == "" {
			fmt.Fprintf(stderr, "hookinlet: %d inbox(es) would be created, and their agent tokens are shown only once: pass --tokens-out FILE to receive them (nothing was changed)\n", creates)
			return exitUsage
		}
		if *dryRun {
			err = checkTokensPath(*tokensOut)
		} else {
			sink, err = openTokenSink(*tokensOut)
		}
		if err != nil {
			fmt.Fprintf(stderr, "hookinlet: --tokens-out: %v (nothing was changed)\n", err)
			return exitUsage
		}
		if sink != nil {
			defer sink.close()
		}
	}

	// 4. Do it, reporting each step as it completes.
	rep := &applyReport{dryRun: *dryRun, w: stdout, asJSON: *c.asJSON, NotManaged: notManaged}
	for _, it := range items {
		if err := applyOne(ctx, api, it, secrets[it.Spec.Slug], *dryRun, sink, rep, stderr); err != nil {
			rep.finish(*tokensOut, sink != nil)
			fmt.Fprintf(stderr, "hookinlet: stopped at inbox %q; changes already made stay in place, and running apply again picks up from here\n", it.Spec.Slug)
			return fail(stderr, err)
		}
	}
	rep.finish(*tokensOut, sink != nil)
	return 0
}

func applyOne(ctx context.Context, api adminAPI, it applyfile.Item, secret string, dryRun bool, sink *tokenSink, rep *applyReport, stderr io.Writer) error {
	slug := it.Spec.Slug
	call := func(f func(context.Context) error) error {
		cctx, cancel := context.WithTimeout(ctx, callTimeout)
		defer cancel()
		return f(cctx)
	}
	switch it.Action {
	case applyfile.Create:
		if !dryRun {
			p := it.Create
			p.VerifierSecret = secret
			var created *admin.Created
			if err := call(func(c context.Context) (err error) { created, err = api.Create(c, p); return }); err != nil {
				return createError(err)
			}
			// The token is saved before anything else is done with the inbox.
			if err := sink.add(slug, created.AgentToken); err != nil {
				return fmt.Errorf("inbox %q was created but its agent token could not be saved (%w); rotate it with: hookinlet inbox rotate-token %s", slug, err, slug)
			}
			for _, w := range created.Warnings {
				fmt.Fprintf(stderr, "warning: %s: %s\n", slug, w)
			}
			if it.Patch != (admin.Patch{}) {
				if err := call(func(c context.Context) (err error) { _, err = api.Update(c, slug, it.Patch); return }); err != nil {
					return err
				}
			}
		}
		rep.created(slug)
	case applyfile.Update:
		if !dryRun {
			if err := call(func(c context.Context) (err error) { _, err = api.Update(c, slug, it.Patch); return }); err != nil {
				return err
			}
		}
		rep.updated(slug, it.Fields)
	default:
		rep.unchanged(slug)
	}
	if it.SetSecret {
		// A new inbox got its secret in the create call.
		if !dryRun && it.Action != applyfile.Create {
			if err := call(func(c context.Context) (err error) { _, err = api.SetSecret(c, slug, secret); return }); err != nil {
				return err
			}
		}
		rep.secretSet(slug)
	}
	return nil
}

// createError words the one failure that is likely when creating.
func createError(err error) error {
	if errors.Is(err, admin.ErrNoSecretBox) {
		return fmt.Errorf("a verifier secret is declared, so %s is required to seal it", masterKeyEnv)
	}
	return err
}

const masterKeyEnv = "HOOKINLET_MASTER_KEY"

// checkSealing stops, before any change, a direct-mode run that has secrets to
// store and no master key to seal them with. Through a server, the server is
// the one that has the key.
func (c *conn) checkSealing(stderr io.Writer) int {
	data, err := c.data()
	if err != nil {
		return configError(stderr, err)
	}
	if c.remoteURL() == "" && data.MasterKey == "" {
		fmt.Fprintf(stderr, "hookinlet: the file declares verifier secrets, so %s is required to seal them in the data directory\n", masterKeyEnv)
		return exitUsage
	}
	return 0
}

func (c *conn) remoteURL() string {
	if *c.url != "" {
		return *c.url
	}
	return c.getenv(urlEnv)
}

// noDatabaseYet reports a direct-mode data directory that has no database.
func (c *conn) noDatabaseYet() bool {
	data, err := c.data()
	if err != nil || c.remoteURL() != "" {
		return false
	}
	_, err = os.Stat(filepath.Join(data.DataDir, sqlite.DBFileName))
	return errors.Is(err, os.ErrNotExist)
}

// emptyAdmin stands in for a data directory that has no database yet, in a dry
// run: every inbox in the file would be created.
type emptyAdmin struct{ adminAPI }

func (emptyAdmin) List(context.Context) ([]admin.View, error) { return nil, nil }

func readApplyFile(path string) ([]byte, error) {
	r := stdin
	if path != "-" {
		f, err := os.Open(path) //nolint:gosec // the operator names the file
		if err != nil {
			var pe *os.PathError
			if errors.As(err, &pe) {
				return nil, fmt.Errorf("cannot read %s: %w", path, pe.Err)
			}
			return nil, fmt.Errorf("cannot read %s", path)
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	data, err := io.ReadAll(io.LimitReader(r, applyfile.MaxFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", displayName(path), err)
	}
	if len(data) > applyfile.MaxFileBytes {
		return nil, fmt.Errorf("%s is larger than %d bytes", displayName(path), applyfile.MaxFileBytes)
	}
	return data, nil
}

func displayName(path string) string {
	if path == "-" {
		return "standard input"
	}
	return path
}

func indentLines(err error) string {
	return strings.ReplaceAll(strings.TrimSpace(err.Error()), "\n", "\n  ")
}

// ---- the file that receives the tokens ----

// tokenSink is the --tokens-out file. It is created exclusively and privately
// before anything changes, and rewritten after each inbox is created, so that
// the tokens of the inboxes made before a failure are not lost.
type tokenSink struct {
	f      *os.File
	tokens map[string]string
}

// checkTokensPath is the part of openTokenSink a dry run can do.
func checkTokensPath(path string) error {
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("%s already exists; it is never overwritten", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("cannot use %s: %w", path, pathReason(err))
	}
	if st, err := os.Stat(filepath.Dir(path)); err != nil || !st.IsDir() {
		return fmt.Errorf("the directory for %s does not exist", path)
	}
	return nil
}

func openTokenSink(path string) (*tokenSink, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // the operator names the file
	switch {
	case errors.Is(err, os.ErrExist):
		return nil, fmt.Errorf("%s already exists; it is never overwritten", path)
	case err != nil:
		return nil, fmt.Errorf("cannot create %s: %w", path, pathReason(err))
	}
	s := &tokenSink{f: f, tokens: map[string]string{}}
	if err := f.Chmod(0o600); err != nil {
		s.close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("cannot make %s private: %w", path, pathReason(err))
	}
	if err := s.write(); err != nil {
		s.close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("cannot write %s: %w", path, pathReason(err))
	}
	return s, nil
}

func pathReason(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

func (s *tokenSink) add(slug, tok string) error {
	s.tokens[slug] = tok
	return s.write()
}

func (s *tokenSink) write() error {
	b, err := json.MarshalIndent(s.tokens, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := s.f.Truncate(0); err != nil {
		return err
	}
	if _, err := s.f.WriteAt(b, 0); err != nil {
		return err
	}
	return s.f.Sync()
}

func (s *tokenSink) close() { _ = s.f.Close() }

// ---- the summary ----

// applyReport prints the summary line by line as work completes. It names
// slugs, fields, and counts, never a secret or a token.
type applyReport struct {
	dryRun bool
	asJSON bool
	w      io.Writer

	Created    []string            `json:"created"`
	Updated    map[string][]string `json:"updated"`
	Unchanged  []string            `json:"unchanged"`
	SecretSet  []string            `json:"secret_set"`
	NotManaged []string            `json:"not_managed"`
}

func (r *applyReport) line(verb, slug, rest string) {
	if r.asJSON {
		return
	}
	if rest != "" {
		rest = ": " + rest
	}
	fmt.Fprintf(r.w, "%-17s %s%s\n", verb, slug, rest)
}

func (r *applyReport) created(slug string) {
	r.Created = append(r.Created, slug)
	r.line(r.verb("created", "would create"), slug, "")
}

func (r *applyReport) updated(slug string, fields []string) {
	if r.Updated == nil {
		r.Updated = map[string][]string{}
	}
	r.Updated[slug] = fields
	r.line(r.verb("updated", "would update"), slug, strings.Join(fields, ", "))
}

func (r *applyReport) unchanged(slug string) {
	r.Unchanged = append(r.Unchanged, slug)
	r.line("unchanged", slug, "")
}

func (r *applyReport) secretSet(slug string) {
	r.SecretSet = append(r.SecretSet, slug)
	r.line(r.verb("secret set", "would set secret"), slug, "")
}

func (r *applyReport) verb(done, would string) string {
	if r.dryRun {
		return would
	}
	return done
}

func (r *applyReport) finish(tokensOut string, wroteTokens bool) {
	if r.asJSON {
		out := struct {
			DryRun bool `json:"dry_run"`
			*applyReport
		}{r.dryRun, r}
		// Lists that are empty are [] and not null.
		for _, l := range []*[]string{&r.Created, &r.Unchanged, &r.SecretSet, &r.NotManaged} {
			if *l == nil {
				*l = []string{}
			}
		}
		if r.Updated == nil {
			r.Updated = map[string][]string{}
		}
		printJSON(r.w, out)
		return
	}
	if len(r.NotManaged) > 0 {
		fmt.Fprintf(r.w, "not managed       %s (on the server, not in the file; left alone)\n", strings.Join(r.NotManaged, ", "))
	}
	fmt.Fprintf(r.w, "\n%d created, %d updated, %d unchanged, %d secrets set, %d not managed", len(r.Created), len(r.Updated), len(r.Unchanged), len(r.SecretSet), len(r.NotManaged))
	if r.dryRun {
		fmt.Fprint(r.w, " (dry run: nothing was changed)")
	}
	fmt.Fprintln(r.w)
	if wroteTokens && len(r.Created) > 0 {
		fmt.Fprintf(r.w, "agent tokens of the new inboxes were written to %s\n", tokensOut)
	}
}
