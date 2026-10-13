// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package docs

import (
	"io/fs"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

var (
	linkRE     = regexp.MustCompile(`\[[^\]]*\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)
	headingRE  = regexp.MustCompile(`^ {0,3}#{1,6}\s+(.*?)(?:\s+#+)?\s*$`)
	mdLinkRE   = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	htmlTagRE  = regexp.MustCompile(`<[^>]*>`)
	codeSpanRE = regexp.MustCompile("`[^`]*`")
)

// TestLinks fails when a relative link or #anchor in README.md or docs/*.md does
// not resolve. Anchors are the ones GitHub derives from headings (lowercase,
// punctuation dropped, spaces to hyphens, repeats suffixed -1, -2); an id set
// by hand in HTML is not accepted, because tools that build a site from these
// files do not all see it.
func TestLinks(t *testing.T) {
	// Paths are relative to the repository root, which is what the links resolve against.
	root := os.DirFS("..")
	files := []string{"README.md"}
	more, err := fs.Glob(root, "docs/*.md")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, more...)

	anchors := map[string]map[string]bool{}
	anchorsOf := func(file string) map[string]bool {
		file = path.Clean(file)
		if a, ok := anchors[file]; ok {
			return a
		}
		a := map[string]bool{}
		anchors[file] = a
		if b, err := fs.ReadFile(root, file); err == nil {
			seen := map[string]int{}
			forEachLine(string(b), func(line string, inCode bool) {
				if inCode {
					return
				}
				m := headingRE.FindStringSubmatch(line)
				if m == nil {
					return
				}
				s := slug(m[1])
				if n := seen[s]; n > 0 {
					a[s+"-"+strconv.Itoa(n)] = true
				} else {
					a[s] = true
				}
				seen[s]++
			})
		}
		return a
	}

	for _, f := range files {
		b, err := fs.ReadFile(root, f)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		forEachLine(string(b), func(line string, inCode bool) {
			n++
			if inCode {
				return
			}
			line = codeSpanRE.ReplaceAllString(line, "")
			for _, m := range linkRE.FindAllStringSubmatch(line, -1) {
				dest := m[1]
				if strings.Contains(dest, "://") || strings.HasPrefix(dest, "mailto:") {
					continue
				}
				pathPart, frag, _ := strings.Cut(dest, "#")
				target := f
				if pathPart != "" {
					target = path.Join(path.Dir(f), pathPart)
					if _, err := fs.Stat(root, target); err != nil {
						t.Errorf("%s:%d: link %q: %s does not exist", f, n, dest, target)
						continue
					}
				}
				if frag == "" || !strings.HasSuffix(target, ".md") {
					continue
				}
				if !anchorsOf(target)[strings.ToLower(frag)] {
					t.Errorf("%s:%d: link %q: no heading in %s has the anchor #%s", f, n, dest, target, frag)
				}
			}
		})
	}
}

// forEachLine calls fn for every line, saying whether it is inside a fenced code block.
func forEachLine(s string, fn func(line string, inCode bool)) {
	fence := ""
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimLeft(line, " ")
		if marker := fenceOf(t); marker != "" {
			switch {
			case fence == "":
				fence = marker
			case strings.HasPrefix(marker, fence[:1]) && len(marker) >= len(fence):
				fence = ""
			}
			fn(line, true)
			continue
		}
		fn(line, fence != "")
	}
}

func fenceOf(t string) string {
	for _, c := range []string{"`", "~"} {
		n := len(t) - len(strings.TrimLeft(t, c))
		if n >= 3 && strings.HasPrefix(t, c) {
			return strings.Repeat(c, n)
		}
	}
	return ""
}

// slug is GitHub's heading anchor.
func slug(h string) string {
	h = mdLinkRE.ReplaceAllString(h, "$1")
	h = htmlTagRE.ReplaceAllString(h, "")
	var b strings.Builder
	for _, r := range strings.ToLower(h) {
		switch {
		case r == ' ' || r == '-':
			b.WriteByte('-')
		case r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		}
	}
	return b.String()
}
