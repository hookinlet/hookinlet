// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package config

import (
	"bytes"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"strings"
	"testing"
	"time"
)

var testKey = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func validEnv() map[string]string {
	return map[string]string{
		"HOOKINLET_ADMIN_TOKEN": "admin-secret-value-0123456789-abcdefghij",
		"HOOKINLET_MASTER_KEY":  testKey,
	}
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(nil, env(validEnv()), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenAddr != ":8080" || c.DataDir != "./data" || c.LogFormat != "json" ||
		c.LogLevel != slog.LevelInfo || c.SSEHeartbeat != 15*time.Second ||
		c.SSERecheckInterval != 0 || !c.MCPEnabled || c.MCPInlineBodyBytes != 16384 ||
		c.RetentionInterval != time.Hour || c.ShutdownTimeout != 10*time.Second {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	if got := c.MasterKey.Reveal(); got != string(bytes.Repeat([]byte{7}, 32)) {
		t.Fatalf("master key not decoded: %q", got)
	}
}

func TestFlagsBeatEnvBeatDefault(t *testing.T) {
	e := validEnv()
	e["HOOKINLET_LISTEN_ADDR"] = ":9000"
	e["HOOKINLET_DATA_DIR"] = "/env/data"
	c, err := Load([]string{"--listen-addr=:7000"}, env(e), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenAddr != ":7000" {
		t.Errorf("flag should beat env: got %q", c.ListenAddr)
	}
	if c.DataDir != "/env/data" {
		t.Errorf("env should beat default: got %q", c.DataDir)
	}
}

func TestFlagCanSetEmptyOverEnv(t *testing.T) {
	// An explicitly passed empty flag wins, so a required value cannot be
	// silently satisfied by the environment.
	_, err := Load([]string{"--admin-token="}, env(validEnv()), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "--admin-token") {
		t.Fatalf("want admin-token error, got %v", err)
	}
}

func TestMissingRequiredReportsAllAndNamesEnvVars(t *testing.T) {
	_, err := Load(nil, env(nil), io.Discard)
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{
		"--admin-token (HOOKINLET_ADMIN_TOKEN) is required",
		"--master-key (HOOKINLET_MASTER_KEY) is required",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestInvalidValues(t *testing.T) {
	short := base64.StdEncoding.EncodeToString([]byte("too-short"))
	tests := []struct {
		name string
		set  map[string]string
		want string
	}{
		{"master key not base64", map[string]string{"HOOKINLET_MASTER_KEY": "%%%not-base64%%%"}, "--master-key"},
		{"master key wrong length", map[string]string{"HOOKINLET_MASTER_KEY": short}, "exactly 32 bytes, got 9"},
		{"bad duration", map[string]string{"HOOKINLET_SHUTDOWN_TIMEOUT": "soon"}, "--shutdown-timeout"},
		{"zero shutdown timeout", map[string]string{"HOOKINLET_SHUTDOWN_TIMEOUT": "0s"}, "greater than zero"},
		{"negative recheck", map[string]string{"HOOKINLET_SSE_RECHECK_INTERVAL": "-1s"}, "must not be negative"},
		{"bad log format", map[string]string{"HOOKINLET_LOG_FORMAT": "xml"}, "--log-format"},
		{"bad log level", map[string]string{"HOOKINLET_LOG_LEVEL": "loud"}, "--log-level"},
		{"bad bool", map[string]string{"HOOKINLET_MCP_ENABLED": "maybe"}, "--mcp-enabled"},
		{"bad body bytes", map[string]string{"HOOKINLET_MCP_INLINE_BODY_BYTES": "0"}, "--mcp-inline-body-bytes"},
		{"body bytes above the REST inline size", map[string]string{"HOOKINLET_MCP_INLINE_BODY_BYTES": "65537"}, "--mcp-inline-body-bytes"},
		{"bad cidr", map[string]string{"HOOKINLET_TRUSTED_PROXIES": "10.0.0.0/8, nonsense"}, `"nonsense" is not a CIDR`},
		{"relative base url", map[string]string{"HOOKINLET_PUBLIC_BASE_URL": "example.com"}, "--public-base-url"},
		{"non-http base url", map[string]string{"HOOKINLET_PUBLIC_BASE_URL": "ftp://example.com"}, "--public-base-url"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := validEnv()
			maps.Copy(e, tt.set)
			_, err := Load(nil, env(e), io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("want error containing %q, got %v", tt.want, err)
			}
		})
	}
}

func TestErrorsNeverEchoSecrets(t *testing.T) {
	e := validEnv()
	e["HOOKINLET_MASTER_KEY"] = "super-secret-but-not-base64!!"
	_, err := Load(nil, env(e), io.Discard)
	if err == nil {
		t.Fatal("want error")
	}
	for _, secret := range []string{"super-secret-but-not-base64", "admin-secret-value-0123456789-abcdefghij"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error leaks secret %q: %v", secret, err)
		}
	}
}

func TestParsedValues(t *testing.T) {
	e := validEnv()
	e["HOOKINLET_TRUSTED_PROXIES"] = "10.0.0.0/8, 192.168.0.0/16"
	e["HOOKINLET_PUBLIC_BASE_URL"] = "https://hooks.example.com/"
	e["HOOKINLET_SSE_RECHECK_INTERVAL"] = "5s"
	e["HOOKINLET_MCP_ENABLED"] = "false"
	c, err := Load(nil, env(e), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.TrustedProxies) != 2 || c.PublicBaseURL != "https://hooks.example.com" ||
		c.SSERecheckInterval != 5*time.Second || c.MCPEnabled {
		t.Fatalf("unexpected: %+v", c)
	}
}

func TestUsageErrors(t *testing.T) {
	var out bytes.Buffer
	_, err := Load([]string{"--no-such-flag"}, env(validEnv()), &out)
	if !errors.Is(err, ErrUsage) {
		t.Fatalf("want ErrUsage, got %v", err)
	}
	if !strings.Contains(out.String(), "no-such-flag") {
		t.Errorf("flag package should have explained the problem: %q", out.String())
	}
	if _, err := Load([]string{"stray"}, env(validEnv()), io.Discard); !errors.Is(err, ErrUsage) {
		t.Fatalf("positional argument: want ErrUsage, got %v", err)
	}
	if _, err := Load([]string{"-h"}, env(validEnv()), io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("want flag.ErrHelp, got %v", err)
	}
}

func TestSecretsAreRedactedEverywhere(t *testing.T) {
	c, err := Load(nil, env(validEnv()), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("cfg", "config", c)
	outputs := []string{
		buf.String(),
		fmt.Sprintf("%v", c), fmt.Sprintf("%+v", c), fmt.Sprintf("%#v", c),
		fmt.Sprint(c.AdminToken), fmt.Sprint(c.MasterKey),
		fmt.Sprintf("%#v", c.AdminToken),
	}
	for _, out := range outputs {
		if strings.Contains(out, "admin-secret-value-0123456789-abcdefghij") || strings.Contains(out, string(bytes.Repeat([]byte{7}, 32))) {
			t.Errorf("secret leaked in %q", out)
		}
	}
	if !strings.Contains(buf.String(), "[redacted]") {
		t.Errorf("expected redaction marker in log output: %s", buf.String())
	}
}

func TestParseMasterKey(t *testing.T) {
	good, err := ParseMasterKey(testKey)
	if err != nil || good.Reveal() != string(bytes.Repeat([]byte{7}, 32)) {
		t.Fatalf("valid key: %v", err)
	}
	for name, in := range map[string]string{
		"empty":       "",
		"not base64":  "%%%secret-sentinel%%%",
		"too short":   base64.StdEncoding.EncodeToString([]byte("short")),
		"too long":    base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 33)),
		"url-safe":    base64.URLEncoding.EncodeToString(bytes.Repeat([]byte{0xfb, 0xff}, 16)),
		"unpadded 32": strings.TrimRight(testKey, "="),
	} {
		_, err := ParseMasterKey(in)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), "secret-sentinel") {
			t.Errorf("%s: error echoes the value: %v", name, err)
		}
	}
}

func TestAddDataFlags(t *testing.T) {
	load := func(args []string, env map[string]string) (*DataSettings, error) {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		resolve := AddDataFlags(fs, func(k string) string { return env[k] })
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		return resolve()
	}

	d, err := load(nil, nil)
	if err != nil || d.DataDir != "./data" || d.MasterKey != "" {
		t.Errorf("defaults: %+v, %v", d, err)
	}
	d, err = load(nil, map[string]string{"HOOKINLET_DATA_DIR": "/env", "HOOKINLET_MASTER_KEY": testKey})
	if err != nil || d.DataDir != "/env" || d.MasterKey == "" {
		t.Errorf("env: %+v, %v", d, err)
	}
	d, err = load([]string{"--data-dir", "/flag"}, map[string]string{"HOOKINLET_DATA_DIR": "/env"})
	if err != nil || d.DataDir != "/flag" {
		t.Errorf("flag should win: %+v, %v", d, err)
	}
	if _, err := load([]string{"--data-dir="}, nil); err == nil {
		t.Error("an explicitly empty --data-dir was accepted")
	}
	if _, err := load(nil, map[string]string{"HOOKINLET_MASTER_KEY": "bad"}); err == nil || !strings.Contains(err.Error(), "--master-key") {
		t.Errorf("bad master key: %v", err)
	}
	// Unlike serve, no admin token is demanded.
	if _, err := load(nil, nil); err != nil {
		t.Errorf("data settings should not require an admin token: %v", err)
	}
}

func TestAdminTokenMustBeStrong(t *testing.T) {
	sentinel := "short-secret-sentinel"
	for name, tok := range map[string]string{
		"one character":        "t",
		"a typed password":     "hunter2hunter2",
		"31 characters":        strings.Repeat("a", 31),
		"with a space":         strings.Repeat("a", 20) + " " + strings.Repeat("b", 20),
		"with a newline":       strings.Repeat("a", 40) + "\n",
		"with a tab":           strings.Repeat("a", 20) + "\t" + strings.Repeat("b", 20),
		"with a unicode space": strings.Repeat("a", 20) + "\u00a0" + strings.Repeat("b", 20),
		"with a control byte":  strings.Repeat("a", 40) + "\x01",
		"short sentinel":       sentinel,
	} {
		e := validEnv()
		e["HOOKINLET_ADMIN_TOKEN"] = tok
		_, err := Load(nil, env(e), io.Discard)
		if err == nil || !strings.Contains(err.Error(), "--admin-token") {
			t.Errorf("%s: accepted (%v)", name, err)
			continue
		}
		if strings.Contains(err.Error(), tok) && len(tok) > 3 {
			t.Errorf("%s: the error echoes the token: %v", name, err)
		}
	}
	for name, tok := range map[string]string{
		"exactly 32":       strings.Repeat("a", 32),
		"openssl base64":   "dGhpcyBpcyBhIGxvbmcgcmFuZG9tIHRva2VuIQ==" + "AAAA",
		"with punctuation": "correct-horse_battery.staple/0123456789+=",
	} {
		e := validEnv()
		e["HOOKINLET_ADMIN_TOKEN"] = tok
		if _, err := Load(nil, env(e), io.Discard); err != nil {
			t.Errorf("%s: rejected: %v", name, err)
		}
	}
}
