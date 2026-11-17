// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func mustCatalog(t *testing.T) Catalog {
	t.Helper()
	cat, err := LoadCatalog("")
	if err != nil {
		t.Fatal(err)
	}
	return cat
}

func TestResolveCatalogServers(t *testing.T) {
	servers, warnings, err := Resolve([]byte(`{
		"duckduckgo": {},
		"arxiv": {"storagePath": "/data"},
		"filesystem": {"paths": ["/a", "/b"]},
		"time": null
	}`), mustCatalog(t), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	got := map[string][]string{}
	for _, s := range servers {
		got[s.Name] = s.Command
		if s.Install != nil {
			t.Errorf("%s: catalog servers must not run install steps at start", s.Name)
		}
		if len(s.Pull) == 0 {
			t.Errorf("%s: expected pull commands", s.Name)
		}
	}
	want := map[string][]string{
		"arxiv":      {"uvx", "arxiv-mcp-server", "--storage-path", "/data"},
		"duckduckgo": {"uvx", "duckduckgo-mcp-server"},
		"filesystem": {"npx", "-y", "@modelcontextprotocol/server-filesystem", "/a", "/b"},
		"time":       {"uvx", "mcp-server-time"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
	if servers[0].Name != "arxiv" || servers[3].Name != "time" {
		t.Fatalf("servers not sorted by name: %v", servers)
	}
}

func TestResolveDropsUnsetOptionalGroup(t *testing.T) {
	servers, _, err := Resolve([]byte(`{"arxiv": {}}`), mustCatalog(t), "")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"uvx", "arxiv-mcp-server"}; !reflect.DeepEqual(servers[0].Command, want) {
		t.Fatalf("command = %v, want %v", servers[0].Command, want)
	}
}

func TestResolveUnknownPropertiesAreWarnings(t *testing.T) {
	_, warnings, err := Resolve([]byte(`{"duckduckgo": {"futureOption": 1}}`), mustCatalog(t), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "futureOption") {
		t.Fatalf("warnings = %v", warnings)
	}
}

func TestResolveGitHubServer(t *testing.T) {
	servers, _, err := Resolve([]byte(`{
		"github/acme/weather-mcp": {
			"installCmd": "npm install",
			"runCmd": "npm run start",
			"envs": {"API_TOKEN": "secret"}
		}
	}`), mustCatalog(t), "/srv")
	if err != nil {
		t.Fatal(err)
	}
	s := servers[0]
	if s.Repo != "https://github.com/acme/weather-mcp.git" {
		t.Errorf("repo = %q", s.Repo)
	}
	if s.Dir != "/srv/github/acme/weather-mcp" {
		t.Errorf("dir = %q", s.Dir)
	}
	if !reflect.DeepEqual(s.Command, []string{"/bin/sh", "-c", "npm run start"}) {
		t.Errorf("command = %v", s.Command)
	}
	if !reflect.DeepEqual(s.Install, [][]string{{"/bin/sh", "-c", "npm install"}}) {
		t.Errorf("install = %v", s.Install)
	}
	if s.Env["API_TOKEN"] != "secret" {
		t.Errorf("env = %v", s.Env)
	}
}

func TestResolveLocalCustomServer(t *testing.T) {
	servers, _, err := Resolve([]byte(`{"my-tools": {"runCmd": "python3 /opt/tools/server.py"}}`), mustCatalog(t), "")
	if err != nil {
		t.Fatal(err)
	}
	if servers[0].Repo != "" || servers[0].Dir != "/" {
		t.Fatalf("unexpected local server: %+v", servers[0])
	}
}

func TestResolveErrors(t *testing.T) {
	cases := map[string]struct {
		config string
		want   string
	}{
		"not an object":         {`["duckduckgo"]`, "JSON object"},
		"null config":           {`null`, "JSON object"},
		"server not an object":  {`{"duckduckgo": "yes"}`, "must be a JSON object"},
		"unknown server":        {`{"nope": {}}`, `unknown MCP server "nope"`},
		"empty name":            {`{"": {}}`, "must not be empty"},
		"missing required prop": {`{"filesystem": {}}`, `property "paths" is required`},
		"empty required array":  {`{"filesystem": {"paths": []}}`, "must not be empty"},
		"wrong prop type":       {`{"arxiv": {"storagePath": 1}}`, "must be a string"},
		"wrong array item":      {`{"filesystem": {"paths": [1]}}`, "array of strings"},
		"github missing runCmd": {`{"github/a/b": {"installCmd": "make"}}`, "runCmd is required"},
		"github bad path":       {`{"github/a": {"runCmd": "x"}}`, "github/<owner>/<repo>"},
		"github traversal":      {`{"github/../b": {"runCmd": "x"}}`, "invalid GitHub"},
		"github runCmd type":    {`{"github/a/b": {"runCmd": 1}}`, "must be strings"},
		"env value type":        {`{"github/a/b": {"runCmd": "x", "envs": {"A": 1}}}`, "must be strings"},
		"env name":              {`{"github/a/b": {"runCmd": "x", "envs": {"A-B": "1"}}}`, "invalid environment variable"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := Resolve([]byte(tc.config), mustCatalog(t), "")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestLoadCatalogOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(path, []byte(`{
		"weather": {"command": [["weather-mcp"], ["--city", "{{city}}"]], "properties": {"city": {"type": "string", "default": "Shenzhen"}}},
		"time": {"command": [["/usr/local/bin/time-mcp"]]}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cat, err := LoadCatalog(path)
	if err != nil {
		t.Fatal(err)
	}
	servers, _, err := Resolve([]byte(`{"weather": {}, "time": {}}`), cat, "")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/usr/local/bin/time-mcp"}; !reflect.DeepEqual(servers[0].Command, want) {
		t.Errorf("override not applied: %v", servers[0].Command)
	}
	if want := []string{"weather-mcp", "--city", "Shenzhen"}; !reflect.DeepEqual(servers[1].Command, want) {
		t.Errorf("default not applied: %v", servers[1].Command)
	}
	if _, err := LoadCatalog(filepath.Join(t.TempDir(), "missing.json")); err != nil {
		t.Errorf("missing override file must be ignored: %v", err)
	}
}

func TestLoadCatalogRejectsInvalidEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(path, []byte(`{"bad": {"command": []}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCatalog(path); err == nil || !strings.Contains(err.Error(), "command is empty") {
		t.Fatalf("err = %v", err)
	}
}
