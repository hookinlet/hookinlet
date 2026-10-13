// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package config

import (
	"encoding/base64"
	"io"
	"strings"
	"testing"
	"time"
)

// The fuzz targets here run their seeds in a normal `go test`. To fuzz one:
//
//	go test -run='^$' -fuzz=FuzzLoad -fuzztime=90s ./internal/config

// FuzzLoad feeds arbitrary environment values and flag arguments to Load. It
// must not panic, must never put a secret into an error, and a configuration it
// accepts must satisfy the bounds the server relies on.
func FuzzLoad(f *testing.F) {
	f.Add("0.0.0.0:8080", "15s", "10.0.0.0/8, ::1/128", "https://hooks.example.com/", "5", "1048576", "off", "--log-level=debug")
	f.Add("", "-1s", "not-a-cidr", "ftp://x", "-1", "9999999999999999999", "0s", "--max-streams")
	f.Add(":0", "1e3h", ",,,", "http://", "0", "-5", "1ns", "--admin-ui=maybe")
	f.Add("[::1]:80", "9223372036854775807ns", "300.0.0.0/8", "http://u:p@h", "1", "1", "1h", "--")
	f.Fuzz(func(t *testing.T, listen, dur, proxies, baseURL, num, budget, retention, arg string) {
		const marker = "zQxJ7secretmarker"
		e := map[string]string{
			"HOOKINLET_ADMIN_TOKEN":           marker + listen,
			"HOOKINLET_MASTER_KEY":            testKey,
			"HOOKINLET_LISTEN_ADDR":           listen,
			"HOOKINLET_SSE_HEARTBEAT":         dur,
			"HOOKINLET_SSE_RECHECK_INTERVAL":  dur,
			"HOOKINLET_DRAIN_DELAY":           dur,
			"HOOKINLET_SHUTDOWN_TIMEOUT":      dur,
			"HOOKINLET_RETENTION_INTERVAL":    retention,
			"HOOKINLET_TRUSTED_PROXIES":       proxies,
			"HOOKINLET_PUBLIC_BASE_URL":       baseURL,
			"HOOKINLET_MAX_STREAMS":           num,
			"HOOKINLET_MAX_STREAMS_PER_INBOX": num,
			"HOOKINLET_INGEST_MEMORY_BUDGET":  budget,
			"HOOKINLET_MCP_INLINE_BODY_BYTES": num,
		}
		c, err := Load([]string{arg}, func(k string) string { return e[k] }, io.Discard)
		if err != nil {
			if strings.Contains(err.Error(), marker) {
				t.Fatalf("an error echoes a secret: %v", err)
			}
			return
		}
		if c.SSEHeartbeat <= 0 || c.ShutdownTimeout <= 0 || c.DrainDelay < 0 || c.SSERecheckInterval < 0 {
			t.Fatalf("durations out of range: %+v", c)
		}
		if c.RetentionInterval != -1 && c.RetentionInterval < time.Second {
			t.Fatalf("retention interval %v", c.RetentionInterval)
		}
		if c.MaxStreams < 1 || c.MaxStreamsPerInbox < 0 || c.IngestMemoryBudget < 0 || c.MCPInlineBodyBytes < 1 || c.MCPInlineBodyBytes > 64<<10 {
			t.Fatalf("limits out of range: %+v", c)
		}
		if len(c.AdminToken) < minAdminTokenLen || len(c.MasterKey) != masterKeyLen {
			t.Fatal("accepted a weak secret")
		}
		if strings.Contains(c.PublicBaseURL, "\n") || strings.HasSuffix(c.PublicBaseURL, "/") {
			t.Fatalf("public base url %q", c.PublicBaseURL)
		}
		_ = c.LogValue() // must not panic, and the secrets must stay out of it
		if s := c.LogValue().String(); strings.Contains(s, marker) {
			t.Fatalf("log value leaks a secret: %s", s)
		}
	})
}

// FuzzParseMasterKey: only 32 decoded bytes are accepted, and the error never
// repeats the input.
func FuzzParseMasterKey(f *testing.F) {
	f.Add(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	f.Add("")
	f.Add("====")
	f.Add(base64.StdEncoding.EncodeToString(make([]byte, 31)))
	f.Add("AAAA AAAA")
	f.Fuzz(func(t *testing.T, v string) {
		key, err := ParseMasterKey(v)
		if err == nil {
			if len(key) != masterKeyLen {
				t.Fatalf("accepted a %d byte key", len(key))
			}
			return
		}
		if len(v) >= 8 && strings.Contains(err.Error(), v) {
			t.Fatalf("the error repeats the key: %v", err)
		}
	})
}
