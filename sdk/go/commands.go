// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"context"
	"fmt"
)

type processStarter interface {
	startProcess(context.Context, processStartRequest, CommandOptions) (*processStartResult, error)
}

type Commands struct {
	starter processStarter
}

func (c *Commands) Run(ctx context.Context, cmd string, opts CommandOptions) (*CommandResult, error) {
	if c == nil || c.starter == nil {
		return nil, fmt.Errorf("commands is not attached to a sandbox")
	}

	envs := opts.Envs
	if envs == nil {
		envs = map[string]string{}
	}
	stdin := false
	process, err := c.starter.startProcess(ctx, processStartRequest{
		Process: processConfig{
			Cmd:  "/bin/bash",
			Args: []string{"-l", "-c", cmd},
			Envs: envs,
			Cwd:  opts.Cwd,
		},
		Stdin: &stdin,
	}, opts)
	if err != nil {
		return nil, err
	}

	return &CommandResult{
		Stdout:   process.Stdout,
		Stderr:   process.Stderr,
		ExitCode: process.ExitCode,
	}, nil
}
