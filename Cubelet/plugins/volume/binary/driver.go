// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package binary provides a VolumePlugin driver that invokes an external
// binary once per operation.
//
// # Calling convention
//
// For every Attach / Detach call the driver forks a new subprocess:
//
//	<binary_path> --op <op> [--<key> <value> ...]
//
// The binary runs, writes a single JSON object to stdout, and exits.
// Exit code 0 + empty "error" field = success.
// Exit code != 0 or non-empty "error" field = failure.
//
// # Operations and their flags
//
//	attach
//	  --sandbox-id      <sandboxID>
//	  --namespace       <namespace>
//	  --volume-id       <volumeID>
//	  --ref-count       <int64>       pre-attach count; 0 = first attach
//	  --volume-base-dir <path>        required parent dir; host_path MUST be inside it
//	  stdout: {"host_path":"...","metadata":{...},"error":""}
//
//	detach
//	  --sandbox-id   <sandboxID>
//	  --namespace    <namespace>
//	  --volume-id    <volumeID>
//	  --ref-count    <int64>          post-detach count; 0 = last detach
//	  --metadata     <json-object>    opaque map from the matching Attach call
//	  stdout: {"error":""}
//
// # Credentials and configuration
//
// The binary is responsible for reading its own configuration (credentials,
// bucket names, mount base, etc.) from wherever it likes — a file, environment
// variables, etc.  Cubelet does not pass secrets; it only passes the
// operation-specific fields listed above.
package binary

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/volume"
)

// Plugin is a VolumePlugin that forks a new subprocess for every operation.
type Plugin struct {
	name       string
	binaryPath string
}

// New creates a Plugin for the given driver name.
// Init must be called before Attach/Detach.
func New(name string) *Plugin {
	return &Plugin{name: name}
}

func (p *Plugin) Name() string                  { return p.name }
func (p *Plugin) PluginType() volume.PluginType { return volume.PluginTypeBinary }

// Init records the binary path. No subprocess is started here;
// the binary is invoked on demand for each Attach / Detach call.
func (p *Plugin) Init(_ context.Context, cfg volume.PluginConfig) error {
	if cfg.BinaryPath == "" {
		return fmt.Errorf("binary volume plugin %q: binary_path is empty", p.name)
	}
	p.binaryPath = cfg.BinaryPath
	return nil
}

// Attach forks the binary with --op attach and the request fields as flags.
func (p *Plugin) Attach(ctx context.Context, req *volume.AttachRequest) (*volume.AttachResult, error) {
	args := []string{
		"--op", "attach",
		"--sandbox-id", req.SandboxID,
		"--namespace", req.Namespace,
		"--volume-id", req.VolumeID,
		"--ref-count", strconv.FormatInt(req.RefCount, 10),
		"--volume-base-dir", req.VolumeBaseDir,
	}

	var resp struct {
		HostPath string            `json:"host_path"`
		Metadata map[string]string `json:"metadata"`
		Error    string            `json:"error"`
	}
	if err := p.run(ctx, &resp, args...); err != nil {
		return nil, fmt.Errorf("binary volume plugin %q attach: %w", p.name, err)
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("binary volume plugin %q attach: %s", p.name, resp.Error)
	}
	return &volume.AttachResult{
		VolumeID: req.VolumeID,
		HostPath: resp.HostPath,
		Metadata: resp.Metadata,
	}, nil
}

// Detach forks the binary with --op detach and the request fields as flags.
func (p *Plugin) Detach(ctx context.Context, req *volume.DetachRequest) error {
	metaJSON, err := marshalStringMap(req.Metadata)
	if err != nil {
		return err
	}

	args := []string{
		"--op", "detach",
		"--sandbox-id", req.SandboxID,
		"--namespace", req.Namespace,
		"--volume-id", req.VolumeID,
		"--ref-count", strconv.FormatInt(req.RefCount, 10),
		"--metadata", metaJSON,
	}

	var resp struct {
		Error string `json:"error"`
	}
	if err := p.run(ctx, &resp, args...); err != nil {
		return fmt.Errorf("binary volume plugin %q detach: %w", p.name, err)
	}
	if resp.Error != "" {
		return fmt.Errorf("binary volume plugin %q detach: %s", p.name, resp.Error)
	}
	return nil
}

// Close is a no-op: there is no persistent subprocess to shut down.
func (p *Plugin) Close() error { return nil }

// ---------------------------------------------------------------------------
// internal helpers
// ---------------------------------------------------------------------------

// run forks binaryPath with args, waits for it to exit, decodes stdout JSON
// into out, and returns an error if the process exits non-zero.
// ctx cancellation is forwarded to the child process.
func (p *Plugin) run(ctx context.Context, out any, args ...string) error {
	var cmd *exec.Cmd
	if ctx != nil {
		cmd = exec.CommandContext(ctx, p.binaryPath, args...) //nolint:gosec
	} else {
		cmd = exec.Command(p.binaryPath, args...) //nolint:gosec
	}

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

// marshalStringMap encodes a map[string]string to a compact JSON string.
// Returns "{}" for nil maps.
func marshalStringMap(m map[string]string) (string, error) {
	if m == nil {
		return "{}", nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("marshal map: %w", err)
	}
	return string(b), nil
}
