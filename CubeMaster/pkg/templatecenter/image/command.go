// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package image

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

var executableLookPath = exec.LookPath

func dockerLogin(ctx context.Context, configDir, imageRef, username, password string) error {
	registry := registryHostFromImageRef(imageRef)
	cmd := exec.CommandContext(ctx, "docker", "--config", configDir, "login", registry, "-u", username, "--password-stdin")
	cmd.Stdin = strings.NewReader(password)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func dockerRun(ctx context.Context, configDir string, args ...string) error {
	_, err := dockerOutput(ctx, configDir, args...)
	return err
}

func dockerOutput(ctx context.Context, configDir string, args ...string) ([]byte, error) {
	cmdArgs := make([]string, 0, len(args)+2)
	if configDir != "" {
		cmdArgs = append(cmdArgs, "--config", configDir)
	}
	cmdArgs = append(cmdArgs, args...)
	cmd := exec.CommandContext(ctx, "docker", cmdArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func skopeoOutput(ctx context.Context, authFile string, args ...string) ([]byte, error) {
	// skopeo expects global-ish flags such as --authfile to follow the
	// subcommand (e.g. `skopeo inspect --authfile X docker://...`).
	cmdArgs := make([]string, 0, len(args)+2)
	if authFile != "" && len(args) > 0 {
		cmdArgs = append(cmdArgs, args[0], "--authfile", authFile)
		cmdArgs = append(cmdArgs, args[1:]...)
	} else {
		cmdArgs = append(cmdArgs, args...)
	}
	cmd := exec.CommandContext(ctx, "skopeo", cmdArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func runCommand(ctx context.Context, dir, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}
