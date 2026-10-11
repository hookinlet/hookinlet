// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

// Package config loads hookinlet's settings from flags and environment
// variables. Every setting is a flag (--listen-addr) and an environment
// variable (HOOKINLET_LISTEN_ADDR). Flags win over the environment, which wins
// over the built-in default.
package config

import (
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// EnvPrefix is prepended to the upper-cased, underscore-separated flag name to
// form the environment variable name.
const EnvPrefix = "HOOKINLET_"

// masterKeyLen is the required length in bytes of the decoded master key.
const masterKeyLen = 32

// minAdminTokenLen is the shortest admin token accepted. 32 characters from a
// random source is 192 bits or more; it rules out anything a person would type.
const minAdminTokenLen = 32

// ErrUsage is returned (wrapped) when flag parsing failed. The flag package has
// already written the problem and the usage text to the output writer, so
// callers should not print it again.
var ErrUsage = errors.New("invalid command line")

// Secret holds a value that must never reach logs or error messages. Its
// String, GoString and LogValue methods all redact; use Reveal at the point of
// use.
type Secret string

// Reveal returns the secret value.
func (s Secret) Reveal() string { return string(s) }

// String implements fmt.Stringer.
func (Secret) String() string { return "[redacted]" }

// GoString implements fmt.GoStringer so that %#v does not leak the value.
func (Secret) GoString() string { return "config.Secret([redacted])" }

// LogValue implements slog.LogValuer.
func (Secret) LogValue() slog.Value { return slog.StringValue("[redacted]") }

// Config is the validated runtime configuration.
type Config struct {
	ListenAddr         string
	DataDir            string
	AdminToken         Secret
	MasterKey          Secret // the raw 32 key bytes, not the base64 form
	PublicBaseURL      string
	TrustedProxies     []netip.Prefix
	LogLevel           slog.Level
	LogFormat          string
	MaxStreams         int
	MaxStreamsPerInbox int
	SSEHeartbeat       time.Duration
	SSERecheckInterval time.Duration
	MCPEnabled         bool
	MCPInlineBodyBytes int
	RetentionInterval  time.Duration
	ShutdownTimeout    time.Duration
}

// LogValue implements slog.LogValuer. Secrets are redacted by their type.
func (c *Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("listen_addr", c.ListenAddr),
		slog.String("data_dir", c.DataDir),
		slog.Any("admin_token", c.AdminToken),
		slog.Any("master_key", c.MasterKey),
		slog.String("public_base_url", c.PublicBaseURL),
		slog.Int("trusted_proxies", len(c.TrustedProxies)),
		slog.String("log_level", c.LogLevel.String()),
		slog.String("log_format", c.LogFormat),
		slog.Int("max_streams", c.MaxStreams),
		slog.Int("max_streams_per_inbox", c.MaxStreamsPerInbox),
		slog.Duration("sse_heartbeat", c.SSEHeartbeat),
		slog.Duration("sse_recheck_interval", c.SSERecheckInterval),
		slog.Bool("mcp_enabled", c.MCPEnabled),
		slog.Int("mcp_inline_body_bytes", c.MCPInlineBodyBytes),
		slog.Duration("retention_interval", c.RetentionInterval),
		slog.Duration("shutdown_timeout", c.ShutdownTimeout),
	)
}

type spec struct {
	name     string // flag name; the env name is derived from it
	def      string
	usage    string
	required bool
}

var specs = []spec{
	{name: "listen-addr", def: ":8080", usage: "address the HTTP server listens on"},
	{name: "data-dir", def: "./data", usage: "directory for persistent data"},
	{name: "admin-token", required: true, usage: "bearer token that can read every inbox and use the admin API; at least 32 characters, e.g. from `openssl rand -base64 32` (prefer the environment variable: flags are visible in process listings)"},
	{name: "master-key", required: true, usage: "base64-encoded 32-byte key that encrypts stored secrets (prefer the environment variable)"},
	{name: "public-base-url", usage: "externally visible base URL, used for ingest URLs in admin responses; derived from the request if unset"},
	{name: "trusted-proxies", usage: "comma-separated CIDRs whose X-Forwarded-For header is honored"},
	{name: "log-level", def: "info", usage: "log level: debug, info, warn, error"},
	{name: "log-format", def: "json", usage: "log format: json or text"},
	{name: "max-streams", def: "1000", usage: "most SSE streams open at once; further ones get 503"},
	{name: "max-streams-per-inbox", def: "0", usage: "most SSE streams that may follow one inbox; 0 means half of max-streams"},
	{name: "sse-heartbeat", def: "15s", usage: "interval between SSE heartbeat comments"},
	{name: "sse-recheck-interval", def: "0", usage: "re-read the store on this interval even without a notifier signal; 0 disables"},
	{name: "mcp-enabled", def: "true", usage: "serve the MCP endpoint at /mcp"},
	{name: "mcp-inline-body-bytes", def: "16384", usage: "largest message body returned inline by MCP tools (at most 65536)"},
	{name: "retention-interval", def: "1h", usage: "interval between retention sweeps"},
	{name: "shutdown-timeout", def: "10s", usage: "time allowed for in-flight requests to finish on shutdown"},
}

func envName(flagName string) string {
	return EnvPrefix + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// source resolves settings from flags, then the environment, then defaults.
type source struct {
	raw    map[string]*string
	set    map[string]bool
	getenv func(string) string
}

// register adds the named settings as flags on fs. Call resolve after fs.Parse.
func register(fs *flag.FlagSet, getenv func(string) string, names ...string) *source {
	src := &source{raw: make(map[string]*string, len(names)), getenv: getenv}
	for _, name := range names {
		s := specByName(name)
		usage := s.usage + " [env " + envName(s.name) + "]"
		if s.def != "" {
			usage += " (default " + s.def + ")"
		}
		src.raw[s.name] = fs.String(s.name, "", usage)
	}
	return src
}

// resolve records which flags were set explicitly. An explicitly passed empty
// flag wins, so a required value cannot be silently satisfied by the
// environment.
func (src *source) resolve(fs *flag.FlagSet) {
	src.set = make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { src.set[f.Name] = true })
}

func (src *source) value(name string) string {
	s := specByName(name)
	switch {
	case src.set[name]:
		return *src.raw[name]
	case src.getenv(envName(name)) != "":
		return src.getenv(envName(name))
	default:
		return s.def
	}
}

func specByName(name string) spec {
	for _, s := range specs {
		if s.name == name {
			return s
		}
	}
	panic("config: unknown setting " + name)
}

// ParseMasterKey decodes a base64 master key and checks its length. The value
// is deliberately never echoed in an error.
func ParseMasterKey(v string) (Secret, error) {
	key, err := base64.StdEncoding.DecodeString(v)
	switch {
	case err != nil:
		return "", fieldErr("master-key", "is not valid base64")
	case len(key) != masterKeyLen:
		return "", fieldErr("master-key", "must decode to exactly %d bytes, got %d", masterKeyLen, len(key))
	}
	return Secret(key), nil
}

// DataSettings are the settings commands that open the data directory need.
type DataSettings struct {
	DataDir string
	// MasterKey is empty unless one was supplied.
	MasterKey Secret
}

// AddDataFlags registers --data-dir and --master-key on fs and returns a
// function that resolves them. Call the returned function after fs.Parse. The
// master key is optional here; a command that needs it must check for it.
func AddDataFlags(fs *flag.FlagSet, getenv func(string) string) func() (*DataSettings, error) {
	src := register(fs, getenv, "data-dir", "master-key")
	return func() (*DataSettings, error) {
		src.resolve(fs)
		d := &DataSettings{DataDir: src.value("data-dir")}
		var errs []error
		if d.DataDir == "" {
			errs = append(errs, fieldErr("data-dir", "must not be empty"))
		}
		if v := src.value("master-key"); v != "" {
			key, err := ParseMasterKey(v)
			if err != nil {
				errs = append(errs, err)
			}
			d.MasterKey = key
		}
		if len(errs) > 0 {
			return nil, errors.Join(errs...)
		}
		return d, nil
	}
}

// Load resolves settings from args (the arguments after the subcommand) and
// getenv, validates them, and reports every problem at once. Flag usage and
// flag errors are written to usageOut.
func Load(args []string, getenv func(string) string, usageOut io.Writer) (*Config, error) {
	fs := flag.NewFlagSet("hookinlet serve", flag.ContinueOnError)
	fs.SetOutput(usageOut)
	names := make([]string, 0, len(specs))
	for _, s := range specs {
		names = append(names, s.name)
	}
	src := register(fs, getenv, names...)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %w", ErrUsage, err)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(usageOut, "unexpected argument %q\n", fs.Arg(0))
		fs.Usage()
		return nil, fmt.Errorf("%w: unexpected argument %q", ErrUsage, fs.Arg(0))
	}
	src.resolve(fs)

	vals := make(map[string]string, len(specs))
	var errs []error
	for _, s := range specs {
		vals[s.name] = src.value(s.name)
		if s.required && vals[s.name] == "" {
			errs = append(errs, fieldErr(s.name, "is required"))
		}
	}

	c := &Config{
		ListenAddr:    vals["listen-addr"],
		DataDir:       vals["data-dir"],
		AdminToken:    Secret(vals["admin-token"]),
		LogFormat:     vals["log-format"],
		PublicBaseURL: strings.TrimRight(vals["public-base-url"], "/"),
	}
	collect := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}

	if v := vals["admin-token"]; v != "" {
		// This one credential reads every inbox, so a guessable one is not allowed.
		// The value is never echoed.
		switch {
		case len(v) < minAdminTokenLen:
			collect(fieldErr("admin-token", "must be at least %d characters; generate one with: openssl rand -base64 32", minAdminTokenLen))
		case strings.IndexFunc(v, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0:
			collect(fieldErr("admin-token", "must not contain whitespace or control characters"))
		}
	}
	if c.ListenAddr == "" {
		collect(fieldErr("listen-addr", "must not be empty"))
	}
	if c.DataDir == "" {
		collect(fieldErr("data-dir", "must not be empty"))
	}
	if c.LogFormat != "json" && c.LogFormat != "text" {
		collect(fieldErr("log-format", "must be json or text"))
	}
	if err := c.LogLevel.UnmarshalText([]byte(vals["log-level"])); err != nil {
		collect(fieldErr("log-level", "must be debug, info, warn, or error"))
	}
	if v := vals["master-key"]; v != "" {
		key, err := ParseMasterKey(v)
		if err != nil {
			collect(err)
		}
		c.MasterKey = key
	}
	if v := c.PublicBaseURL; v != "" {
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			collect(fieldErr("public-base-url", "must be an absolute http or https URL"))
		}
	}
	for _, p := range splitList(vals["trusted-proxies"]) {
		prefix, err := netip.ParsePrefix(p)
		if err != nil {
			collect(fieldErr("trusted-proxies", "%q is not a CIDR", p))
			continue
		}
		c.TrustedProxies = append(c.TrustedProxies, prefix)
	}

	var err error
	if c.SSEHeartbeat, err = parseDuration(vals, "sse-heartbeat", true); err != nil {
		collect(err)
	}
	if c.SSERecheckInterval, err = parseDuration(vals, "sse-recheck-interval", false); err != nil {
		collect(err)
	}
	if c.RetentionInterval, err = parseDuration(vals, "retention-interval", true); err != nil {
		collect(err)
	}
	if c.ShutdownTimeout, err = parseDuration(vals, "shutdown-timeout", true); err != nil {
		collect(err)
	}
	if c.MCPEnabled, err = strconv.ParseBool(vals["mcp-enabled"]); err != nil {
		collect(fieldErr("mcp-enabled", "must be true or false"))
	}
	if c.MaxStreams, err = strconv.Atoi(vals["max-streams"]); err != nil || c.MaxStreams < 1 {
		collect(fieldErr("max-streams", "must be a positive integer"))
	}
	if c.MaxStreamsPerInbox, err = strconv.Atoi(vals["max-streams-per-inbox"]); err != nil || c.MaxStreamsPerInbox < 0 {
		collect(fieldErr("max-streams-per-inbox", "must be zero or a positive integer"))
	}
	if c.MCPInlineBodyBytes, err = strconv.Atoi(vals["mcp-inline-body-bytes"]); err != nil || c.MCPInlineBodyBytes <= 0 || c.MCPInlineBodyBytes > 64<<10 {
		collect(fieldErr("mcp-inline-body-bytes", "must be between 1 and 65536"))
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return c, nil
}

func parseDuration(vals map[string]string, name string, positive bool) (time.Duration, error) {
	d, err := time.ParseDuration(vals[name])
	switch {
	case err != nil:
		return 0, fieldErr(name, "must be a duration such as 15s or 1h")
	case d < 0 || (positive && d == 0):
		if positive {
			return 0, fieldErr(name, "must be greater than zero")
		}
		return 0, fieldErr(name, "must not be negative")
	}
	return d, nil
}

func splitList(s string) []string {
	var out []string
	for p := range strings.SplitSeq(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func fieldErr(name, format string, args ...any) error {
	return fmt.Errorf("--%s (%s) %s", name, envName(name), fmt.Sprintf(format, args...))
}
