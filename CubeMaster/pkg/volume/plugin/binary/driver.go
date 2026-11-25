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
// Exit code 0 + empty "error" field without "error_kind" = success.
// Exit code != 0 or non-empty "error" field = failure.
// Create accepts error_kind="invalid_argument" with a non-empty public error
// message on normal exits, including non-zero exits. A normally exiting
// Destroy may return destroy_not_started or destroy_retryable with a non-empty
// error field. An already absent target is a successful Destroy.
//
// # Operations and flags
//
//	create
//	  --volume-id  <volumeID>   pre-generated UUIDv4
//	  --name       <name>       human-readable label
//	  --private-data <data>     opaque caller input; omitted when empty
//	  stdout JSON: {"token":"...","private_data":"...","error":""}
//
//	destroy
//	  --volume-id  <volumeID>
//	  --private-data <data>     state returned by Create; omitted when empty
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
	"errors"
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
	volumeID, name, privateData string,
) (*plugin.VolumeInfo, error) {
	if strings.ContainsRune(privateData, '\x00') {
		return nil, fmt.Errorf("binary plugin %q create: volume %q private_data must not contain NUL", p.name, volumeID)
	}
	var resp struct {
		Token       string          `json:"token"`
		PrivateData string          `json:"private_data"`
		Error       string          `json:"error"`
		ErrorKind   json.RawMessage `json:"error_kind"`
	}
	args := []string{
		"--op", "create",
		"--volume-id", volumeID,
		"--name", name,
	}
	if privateData != "" {
		args = append(args, "--private-data", privateData)
	}
	stdout, stderr, runErr := p.execute(ctx, args...)
	// Do not replace a successful exit with a later context cancellation.
	if runErr != nil {
		// On Windows, canceled processes can report a non-negative exit code.
		if ctx.Err() != nil {
			return nil, fmt.Errorf("binary plugin %q create: %w", p.name, ctx.Err())
		}
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) || exitErr.ExitCode() < 0 {
			return nil, fmt.Errorf("binary plugin %q create: %w", p.name, executionError(runErr, stdout, stderr))
		}
	}

	decodeErr := json.Unmarshal(stdout, &resp)
	var kind string
	if decodeErr == nil && len(resp.ErrorKind) != 0 {
		// RawMessage distinguishes an absent field from null or an empty value.
		if err := json.Unmarshal(resp.ErrorKind, &kind); err == nil && kind == "invalid_argument" && resp.Error != "" {
			return nil, fmt.Errorf("binary plugin %q create: %w", p.name, &plugin.InvalidArgumentError{Message: resp.Error})
		}
	}
	if runErr != nil {
		return nil, fmt.Errorf("binary plugin %q create: %w", p.name, executionError(runErr, stdout, stderr))
	}
	if decodeErr != nil {
		return nil, fmt.Errorf("binary plugin %q create: decode stdout: %w (raw: %s)", p.name, decodeErr, strings.TrimSpace(string(stdout)))
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("binary plugin %q create: %s", p.name, resp.Error)
	}
	if len(resp.ErrorKind) != 0 {
		return nil, fmt.Errorf("binary plugin %q create: invalid error_kind response", p.name)
	}
	return &plugin.VolumeInfo{
		VolumeID:    volumeID,
		Name:        name,
		Token:       resp.Token,
		PrivateData: resp.PrivateData,
		PluginName:  p.name,
	}, nil
}

// Destroy forks the binary with --op destroy.
func (p *Plugin) Destroy(ctx context.Context, volumeID, privateData string) error {
	if strings.ContainsRune(privateData, '\x00') {
		return &plugin.NotStartedError{
			Cause: fmt.Errorf("binary plugin %q destroy: volume %q private_data must not contain NUL", p.name, volumeID),
		}
	}
	var resp struct {
		Error     string          `json:"error"`
		ErrorKind json.RawMessage `json:"error_kind"`
	}
	args := []string{
		"--op", "destroy",
		"--volume-id", volumeID,
	}
	if privateData != "" {
		args = append(args, "--private-data", privateData)
	}
	stdout, stderr, runErr := p.execute(ctx, args...)
	decodeErr := json.Unmarshal(stdout, &resp)
	var kind string
	_ = json.Unmarshal(resp.ErrorKind, &kind)
	knownFailure := kind == "destroy_not_started" || kind == "destroy_retryable"
	if runErr == nil && decodeErr == nil && knownFailure && resp.Error == "" {
		return fmt.Errorf("binary plugin %q destroy: missing error for %q", p.name, kind)
	}
	var exitErr *exec.ExitError
	if ctx.Err() == nil && (runErr == nil || (errors.As(runErr, &exitErr) && exitErr.ExitCode() > 0)) &&
		decodeErr == nil && resp.Error != "" {
		switch kind {
		case "destroy_not_started":
			return &plugin.NotStartedError{Cause: errors.New(resp.Error)}
		case "destroy_retryable":
			return fmt.Errorf("%w: %s", plugin.ErrDestroyRetryable, resp.Error)
		}
	}
	if runErr != nil {
		return fmt.Errorf("binary plugin %q destroy: %w", p.name, executionError(runErr, stdout, stderr))
	}
	if decodeErr != nil {
		return fmt.Errorf("binary plugin %q destroy: decode stdout: %w (raw: %s)", p.name, decodeErr, strings.TrimSpace(string(stdout)))
	}
	if resp.Error != "" {
		return fmt.Errorf("binary plugin %q destroy: %s", p.name, resp.Error)
	}
	return nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// execute only captures process output. Each operation interprets its reply.
func (p *Plugin) execute(ctx context.Context, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, p.binaryPath, args...) //nolint:gosec
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, nil, &plugin.NotStartedError{Cause: err}
	}
	err := cmd.Wait()
	return stdout.Bytes(), stderr.Bytes(), err
}

func executionError(err error, stdout, stderr []byte) error {
	detail := strings.TrimSpace(string(stderr))
	if detail == "" {
		detail = strings.TrimSpace(string(stdout))
	}
	return fmt.Errorf("exit error: %w: %s", err, detail)
}
