// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package main

import (
	"runtime/debug"
	"testing"
)

func TestResolveVersion(t *testing.T) {
	const rev = "0123456789abcdef0123456789abcdef01234567"
	vcs := func(modified string) []debug.BuildSetting {
		s := []debug.BuildSetting{{Key: "vcs", Value: "git"}, {Key: "vcs.revision", Value: rev}}
		if modified != "" {
			s = append(s, debug.BuildSetting{Key: "vcs.modified", Value: modified})
		}
		return s
	}
	info := func(mainVersion string, settings []debug.BuildSetting) *debug.BuildInfo {
		return &debug.BuildInfo{Main: debug.Module{Version: mainVersion}, Settings: settings}
	}

	tests := []struct {
		name    string
		ldflags string
		bi      *debug.BuildInfo
		want    string
	}{
		{"ldflags version wins over build info", "0.1.0", info("v9.9.9", vcs("")), "0.1.0"},
		{"ldflags version without build info", "0.1.0", nil, "0.1.0"},
		{"module install (go install pkg@v0.1.0)", "dev", info("v0.1.0", nil), "0.1.0"},
		{"module install of a pre-release", "dev", info("v0.2.0-rc.1", nil), "0.2.0-rc.1"},
		{"module install of a pseudo-version", "dev", info("v0.1.1-0.20261013120000-0123456789ab", nil), "0.1.1-0.20261013120000-0123456789ab"},
		{"module version wins over the vcs revision", "dev", info("v0.1.0", vcs("")), "0.1.0"},
		{"(devel) with a vcs revision", "dev", info("(devel)", vcs("")), "dev-0123456789ab"},
		{"(devel) with a modified tree", "dev", info("(devel)", vcs("true")), "dev-0123456789ab-dirty"},
		{"(devel) with a clean tree says nothing of dirty", "dev", info("(devel)", vcs("false")), "dev-0123456789ab"},
		{"(devel) without a vcs revision", "dev", info("(devel)", nil), "dev"},
		{"empty module version with a vcs revision", "dev", info("", vcs("")), "dev-0123456789ab"},
		{"a short vcs revision is not used", "dev", info("(devel)", []debug.BuildSetting{{Key: "vcs.revision", Value: "abc"}}), "dev"},
		{"a modified tree with no revision is just dev", "dev", info("(devel)", []debug.BuildSetting{{Key: "vcs.modified", Value: "true"}}), "dev"},
		{"nothing available", "dev", nil, "dev"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveVersion(tt.ldflags, tt.bi); got != tt.want {
				t.Errorf("resolveVersion(%q, ...) = %q, want %q", tt.ldflags, got, tt.want)
			}
		})
	}
}
