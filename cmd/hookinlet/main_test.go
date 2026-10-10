// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package main

import (
	"bytes"
	"strings"
	"testing"
)

func runCmd(t *testing.T, env map[string]string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(args, func(k string) string { return env[k] }, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestServeWithoutRequiredSettingsFailsReadably(t *testing.T) {
	code, _, stderr := runCmd(t, nil, "serve")
	if code != exitUsage {
		t.Fatalf("exit code %d, want %d", code, exitUsage)
	}
	for _, want := range []string{
		"invalid configuration",
		"--admin-token (HOOKINLET_ADMIN_TOKEN) is required",
		"--master-key (HOOKINLET_MASTER_KEY) is required",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr)
		}
	}
}

func TestServeBadFlagExitsUsage(t *testing.T) {
	code, _, stderr := runCmd(t, nil, "serve", "--bogus")
	if code != exitUsage || !strings.Contains(stderr, "bogus") {
		t.Fatalf("code %d, stderr %q", code, stderr)
	}
}

func TestServeHelpExitsZero(t *testing.T) {
	code, _, stderr := runCmd(t, nil, "serve", "-h")
	if code != 0 || !strings.Contains(stderr, "HOOKINLET_LISTEN_ADDR") {
		t.Fatalf("code %d, stderr %q", code, stderr)
	}
}

func TestVersionAndHelp(t *testing.T) {
	if code, out, _ := runCmd(t, nil, "version"); code != 0 || strings.TrimSpace(out) != version {
		t.Errorf("version: code %d, out %q", code, out)
	}
	if code, out, _ := runCmd(t, nil, "help"); code != 0 || !strings.Contains(out, "Usage: hookinlet") {
		t.Errorf("help: code %d, out %q", code, out)
	}
}

func TestNoArgsAndUnknownCommand(t *testing.T) {
	if code, _, _ := runCmd(t, nil); code != exitUsage {
		t.Errorf("no args: code %d", code)
	}
	code, _, stderr := runCmd(t, nil, "frobnicate")
	if code != exitUsage || !strings.Contains(stderr, `unknown command "frobnicate"`) {
		t.Errorf("unknown: code %d, stderr %q", code, stderr)
	}
}
