// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The tools an MCP client is shown are the ones the document describes: the
// server takes their names, descriptions, and the schemas of their arguments from
// it.
func TestToolsListIsTheDocument(t *testing.T) {
	ae, base, _ := mcpEnv(t)
	_, _, body := mcpPost(t, base, ae.tokenA, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	var got struct {
		Result struct {
			Tools []struct {
				Name        string         `json:"name"`
				Description string         `json:"description"`
				InputSchema map[string]any `json:"inputSchema"`
				Annotations struct {
					ReadOnlyHint bool `json:"readOnlyHint"`
				} `json:"annotations"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	want := loadSpec(t).doc.MCPTools()
	if len(got.Result.Tools) != len(want) || len(want) == 0 {
		t.Fatalf("the server lists %d tools, the document describes %d", len(got.Result.Tools), len(want))
	}
	for _, w := range want {
		found := false
		for _, g := range got.Result.Tools {
			if g.Name != w.Name {
				continue
			}
			found = true
			var wantSchema map[string]any
			b, _ := json.Marshal(w.Input)
			_ = json.Unmarshal(b, &wantSchema)
			if g.Description != w.Description || g.Annotations.ReadOnlyHint != w.ReadOnly || !reflect.DeepEqual(g.InputSchema, wantSchema) {
				t.Errorf("%s: what the server lists differs from the document:\n server: %+v\n document: %+v", w.Name, g, w)
			}
		}
		if !found {
			t.Errorf("the document describes the tool %s and the server does not list it", w.Name)
		}
	}
}

// docs/mcp.md says what each tool takes. It has a section for each tool of the
// document, and names every argument in it.
func TestMCPDocumentationNamesEveryToolAndArgument(t *testing.T) {
	md, err := os.ReadFile("../../docs/mcp.md")
	if err != nil {
		t.Fatal(err)
	}
	sections := map[string]string{}
	heading := regexp.MustCompile("(?m)^### `([a-z_]+)`$")
	locs := heading.FindAllStringSubmatchIndex(string(md), -1)
	for i, l := range locs {
		end := len(md)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		} else if next := strings.Index(string(md[l[1]:]), "\n## "); next >= 0 {
			end = l[1] + next
		}
		sections[string(md[l[2]:l[3]])] = string(md[l[1]:end])
	}
	for _, tool := range loadSpec(t).doc.MCPTools() {
		sec, ok := sections[tool.Name]
		if !ok {
			t.Errorf("docs/mcp.md has no section for the tool %s", tool.Name)
			continue
		}
		props, _ := tool.Input["properties"].(map[string]any)
		var names []string
		for n := range props {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			if !strings.Contains(sec, "`"+n+"`") {
				t.Errorf("the section for %s does not mention its argument %s", tool.Name, n)
			}
		}
	}
}
