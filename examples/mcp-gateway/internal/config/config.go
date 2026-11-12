// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package config turns an E2B-compatible `mcp` map into launch specs for
// stdio MCP servers.
package config

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// GitHubPrefix marks a server that is cloned from github.com and started
// with a caller-provided runCmd, mirroring E2B's GitHubMcpServerConfig.
const GitHubPrefix = "github/"

// DefaultServersDir is where github/ servers are cloned.
const DefaultServersDir = "/opt/mcp-gateway/servers"

const maxServerNameLen = 128

var (
	envNameRE     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	githubPartRE  = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	placeholderRE = regexp.MustCompile(`^\{\{([A-Za-z0-9_]+)\}\}$`)
)

//go:embed catalog.json
var builtinCatalog []byte

// Property describes one server-specific configuration value.
type Property struct {
	// Type is one of "string", "array", "number" or "boolean".
	Type     string `json:"type"`
	Required bool   `json:"required,omitempty"`
	Default  any    `json:"default,omitempty"`
}

// Entry is a built-in server definition.
type Entry struct {
	Description string `json:"description,omitempty"`
	// Command is a list of argument groups. A group that references an unset
	// optional property is dropped as a whole, so flag/value pairs can be
	// expressed as one group.
	Command    [][]string          `json:"command"`
	Env        map[string]string   `json:"env,omitempty"`
	Install    [][]string          `json:"install,omitempty"`
	Properties map[string]Property `json:"properties,omitempty"`
}

// Catalog maps server names to their definitions.
type Catalog map[string]Entry

// Server is a fully resolved, ready-to-start MCP server.
type Server struct {
	Name    string
	Command []string
	Env     map[string]string
	Dir     string
	// Install holds commands run once, in Dir, before Command starts.
	Install [][]string
	// Pull holds commands that pre-fetch the server, run by `mcp-gateway pull`
	// at template build time.
	Pull [][]string
	// Repo is a git URL cloned into Dir before Install runs.
	Repo string
}

// customConfig is the E2B GitHubMcpServerConfig shape.
type customConfig struct {
	RunCmd     string            `json:"runCmd"`
	InstallCmd string            `json:"installCmd"`
	Envs       map[string]string `json:"envs"`
}

var customKeys = map[string]bool{"runCmd": true, "installCmd": true, "envs": true}

// LoadCatalog returns the built-in catalog merged with the optional JSON file
// at overridePath. Entries in the file replace built-in entries of the same
// name. A missing file is not an error.
func LoadCatalog(overridePath string) (Catalog, error) {
	cat, err := parseCatalog(builtinCatalog)
	if err != nil {
		return nil, fmt.Errorf("built-in catalog: %w", err)
	}
	if overridePath == "" {
		return cat, nil
	}
	data, err := os.ReadFile(overridePath)
	if errors.Is(err, os.ErrNotExist) {
		return cat, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read catalog %s: %w", overridePath, err)
	}
	extra, err := parseCatalog(data)
	if err != nil {
		return nil, fmt.Errorf("catalog %s: %w", overridePath, err)
	}
	for name, entry := range extra {
		cat[name] = entry
	}
	return cat, nil
}

func parseCatalog(data []byte) (Catalog, error) {
	var cat Catalog
	if err := json.Unmarshal(data, &cat); err != nil {
		return nil, err
	}
	for name, entry := range cat {
		if len(entry.Command) == 0 || len(entry.Command[0]) == 0 {
			return nil, fmt.Errorf("server %q: command is empty", name)
		}
		for prop, p := range entry.Properties {
			switch p.Type {
			case "string", "array", "number", "boolean":
			default:
				return nil, fmt.Errorf("server %q: property %q has unsupported type %q", name, prop, p.Type)
			}
		}
	}
	return cat, nil
}

// Names returns the sorted server names in the catalog.
func (c Catalog) Names() []string {
	names := make([]string, 0, len(c))
	for name := range c {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Resolve validates raw, the JSON value of the `mcp` option, and returns the
// servers to start sorted by name. Unknown server-specific properties are
// ignored for forward compatibility and reported as warnings.
func Resolve(raw []byte, cat Catalog, serversDir string) ([]Server, []string, error) {
	var servers map[string]json.RawMessage
	if err := decodeObject(raw, &servers); err != nil {
		return nil, nil, fmt.Errorf("mcp config must be a JSON object keyed by server name: %w", err)
	}
	if serversDir == "" {
		serversDir = DefaultServersDir
	}

	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)

	var (
		out      []Server
		warnings []string
	)
	for _, name := range names {
		if err := validateServerName(name); err != nil {
			return nil, nil, err
		}
		props, err := decodeProps(servers[name])
		if err != nil {
			return nil, nil, fmt.Errorf("mcp server %q: %w", name, err)
		}
		var (
			srv  Server
			warn []string
		)
		switch {
		case strings.HasPrefix(name, GitHubPrefix):
			srv, warn, err = resolveGitHub(name, props, serversDir)
		case cat[name].Command != nil:
			srv, warn, err = resolveCatalog(name, cat[name], props)
		case props["runCmd"] != nil:
			srv, warn, err = resolveCustom(name, props, "/")
		default:
			err = fmt.Errorf("unknown MCP server %q; supported servers: %s, or %s<owner>/<repo> with runCmd",
				name, strings.Join(cat.Names(), ", "), GitHubPrefix)
		}
		if err != nil {
			return nil, nil, err
		}
		out = append(out, srv)
		warnings = append(warnings, warn...)
	}
	return out, warnings, nil
}

func decodeObject(raw []byte, v any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("not a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	return dec.Decode(v)
}

// decodeProps accepts an object or null (treated as {}).
func decodeProps(raw json.RawMessage) (map[string]any, error) {
	if trimmed := bytes.TrimSpace(raw); bytes.Equal(trimmed, []byte("null")) {
		return map[string]any{}, nil
	}
	props := map[string]any{}
	if err := decodeObject(raw, &props); err != nil {
		return nil, errors.New("configuration must be a JSON object")
	}
	return props, nil
}

func validateServerName(name string) error {
	if name == "" {
		return errors.New("mcp server name must not be empty")
	}
	if len(name) > maxServerNameLen {
		return fmt.Errorf("mcp server name %q is longer than %d characters", name, maxServerNameLen)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("mcp server name %q contains control characters", name)
		}
	}
	return nil
}

func resolveGitHub(name string, props map[string]any, serversDir string) (Server, []string, error) {
	parts := strings.Split(strings.TrimPrefix(name, GitHubPrefix), "/")
	if len(parts) != 2 {
		return Server{}, nil, fmt.Errorf("mcp server %q: expected %s<owner>/<repo>", name, GitHubPrefix)
	}
	for _, p := range parts {
		if !githubPartRE.MatchString(p) || p == "." || p == ".." {
			return Server{}, nil, fmt.Errorf("mcp server %q: invalid GitHub owner or repository name", name)
		}
	}
	dir := path.Join(serversDir, "github", parts[0], parts[1])
	srv, warn, err := resolveCustom(name, props, dir)
	if err != nil {
		return Server{}, nil, err
	}
	srv.Repo = "https://github.com/" + parts[0] + "/" + parts[1] + ".git"
	return srv, warn, nil
}

func resolveCustom(name string, props map[string]any, dir string) (Server, []string, error) {
	var cfg customConfig
	known := map[string]any{}
	var warnings []string
	for k, v := range props {
		if customKeys[k] {
			known[k] = v
		} else {
			warnings = append(warnings, fmt.Sprintf("mcp server %q: ignoring unknown property %q", name, k))
		}
	}
	data, _ := json.Marshal(known)
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Server{}, nil, fmt.Errorf("mcp server %q: runCmd and installCmd must be strings and envs must map names to strings", name)
	}
	if strings.TrimSpace(cfg.RunCmd) == "" {
		return Server{}, nil, fmt.Errorf("mcp server %q: runCmd is required", name)
	}
	if err := validateEnv(name, cfg.Envs); err != nil {
		return Server{}, nil, err
	}
	srv := Server{
		Name:    name,
		Command: []string{"/bin/sh", "-c", cfg.RunCmd},
		Env:     cfg.Envs,
		Dir:     dir,
	}
	if strings.TrimSpace(cfg.InstallCmd) != "" {
		srv.Install = [][]string{{"/bin/sh", "-c", cfg.InstallCmd}}
	}
	sort.Strings(warnings)
	return srv, warnings, nil
}

func validateEnv(name string, env map[string]string) error {
	for k := range env {
		if !envNameRE.MatchString(k) {
			return fmt.Errorf("mcp server %q: invalid environment variable name %q", name, k)
		}
	}
	return nil
}

func resolveCatalog(name string, entry Entry, props map[string]any) (Server, []string, error) {
	var warnings []string
	values := map[string][]string{}
	for key, p := range entry.Properties {
		v, ok := props[key]
		if !ok || v == nil {
			v = p.Default
		}
		if v == nil {
			if p.Required {
				return Server{}, nil, fmt.Errorf("mcp server %q: property %q is required", name, key)
			}
			continue
		}
		strs, err := propertyValues(p.Type, v)
		if err != nil {
			return Server{}, nil, fmt.Errorf("mcp server %q: property %q: %w", name, key, err)
		}
		if p.Required && len(strs) == 0 {
			return Server{}, nil, fmt.Errorf("mcp server %q: property %q must not be empty", name, key)
		}
		values[key] = strs
	}
	for key := range props {
		if _, ok := entry.Properties[key]; !ok {
			warnings = append(warnings, fmt.Sprintf("mcp server %q: ignoring unknown property %q", name, key))
		}
	}
	sort.Strings(warnings)

	var argv []string
	for _, group := range entry.Command {
		expanded, ok := expandGroup(group, values)
		if ok {
			argv = append(argv, expanded...)
		}
	}
	env := map[string]string{}
	for k, tmpl := range entry.Env {
		if m := placeholderRE.FindStringSubmatch(tmpl); m != nil {
			if v, ok := values[m[1]]; ok {
				env[k] = strings.Join(v, ",")
			}
			continue
		}
		env[k] = tmpl
	}
	return Server{Name: name, Command: argv, Env: env, Dir: "/", Pull: entry.Install}, warnings, nil
}

// expandGroup substitutes whole-argument {{name}} placeholders. Array values
// expand to one argument per item. ok is false when a placeholder refers to
// an unset property, in which case the group is dropped.
func expandGroup(group []string, values map[string][]string) ([]string, bool) {
	var out []string
	for _, arg := range group {
		m := placeholderRE.FindStringSubmatch(arg)
		if m == nil {
			out = append(out, arg)
			continue
		}
		v, ok := values[m[1]]
		if !ok {
			return nil, false
		}
		out = append(out, v...)
	}
	return out, true
}

func propertyValues(typ string, v any) ([]string, error) {
	switch typ {
	case "string":
		s, ok := v.(string)
		if !ok {
			return nil, errors.New("must be a string")
		}
		return []string{s}, nil
	case "number":
		switch n := v.(type) {
		case json.Number:
			return []string{n.String()}, nil
		case float64:
			return []string{strconv.FormatFloat(n, 'f', -1, 64)}, nil
		case int:
			return []string{strconv.Itoa(n)}, nil
		}
		return nil, errors.New("must be a number")
	case "boolean":
		b, ok := v.(bool)
		if !ok {
			return nil, errors.New("must be a boolean")
		}
		return []string{strconv.FormatBool(b)}, nil
	case "array":
		items, ok := v.([]any)
		if !ok {
			return nil, errors.New("must be an array of strings")
		}
		out := make([]string, 0, len(items))
		for _, item := range items {
			s, ok := item.(string)
			if !ok {
				return nil, errors.New("must be an array of strings")
			}
			out = append(out, s)
		}
		return out, nil
	}
	return nil, fmt.Errorf("unsupported type %q", typ)
}
