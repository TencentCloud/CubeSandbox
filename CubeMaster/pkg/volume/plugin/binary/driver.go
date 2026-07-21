// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package binary implements a ControllerPlugin that forks an external binary
// once per operation.
//
// # Calling convention
//
//	<binary_path> --op <op> [--<key> <value> ...]
//
// The binary writes a single JSON object to stdout and exits.
// Exit code 0 + empty "error" field = success.
// Exit code != 0 or non-empty "error" field = failure.
//
// # Operations and flags
//
//	create
//	  --volume-id  <volumeID>   pre-generated UUIDv4
//	  --name       <name>       human-readable label
//	  stdout JSON: {"token":"...","error":""}
//
//	destroy
//	  --volume-id  <volumeID>
//	  stdout JSON: {"error":""}
//
// # Configuration
//
// The binary reads its own credentials / config from wherever it likes
// (file, env vars, etc.).  CubeMaster only passes the operation-specific
// fields above.
package binary

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/volume/plugin"
)

func init() {
	plugin.SetBinaryFactory(func(name, binaryPath string) plugin.ControllerPlugin {
		return New(name, binaryPath)
	})
}

// Plugin is a ControllerPlugin that forks a subprocess for each operation.
type Plugin struct {
	name       string
	binaryPath string
}

// New creates a Plugin.
func New(name, binaryPath string) *Plugin {
	return &Plugin{name: name, binaryPath: binaryPath}
}

func (p *Plugin) Name() string { return p.name }

// Create forks the binary with --op create.
func (p *Plugin) Create(
	ctx context.Context,
	volumeID, name string,
) (*plugin.VolumeInfo, error) {
	var resp struct {
		Token string `json:"token"`
		Error string `json:"error"`
	}
	if err := p.run(ctx, &resp,
		"--op", "create",
		"--volume-id", volumeID,
		"--name", name,
	); err != nil {
		return nil, fmt.Errorf("binary plugin %q create: %w", p.name, err)
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("binary plugin %q create: %s", p.name, resp.Error)
	}
	return &plugin.VolumeInfo{
		VolumeID:   volumeID,
		Name:       name,
		Token:      resp.Token,
		PluginName: p.name,
	}, nil
}

// Destroy forks the binary with --op destroy.
func (p *Plugin) Destroy(ctx context.Context, volumeID string) error {
	var resp struct {
		Error string `json:"error"`
	}
	if err := p.run(ctx, &resp,
		"--op", "destroy",
		"--volume-id", volumeID,
	); err != nil {
		return fmt.Errorf("binary plugin %q destroy: %w", p.name, err)
	}
	if resp.Error != "" {
		return fmt.Errorf("binary plugin %q destroy: %s", p.name, resp.Error)
	}
	return nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func (p *Plugin) run(ctx context.Context, out any, args ...string) error {
	cmd := exec.CommandContext(ctx, p.binaryPath, args...) //nolint:gosec
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		return fmt.Errorf("exit error: %w: %s", err, detail)
	}
	if out != nil {
		if err := json.Unmarshal(stdout.Bytes(), out); err != nil {
			return fmt.Errorf("decode stdout: %w (raw: %s)", err, strings.TrimSpace(stdout.String()))
		}
	}
	return nil
}
