// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//go:build windows

package opts

import (
	"context"
	"errors"
	"strings"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/oci"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
	runtimespec "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/windows"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func escapeAndCombineArgsWindows(args []string) string {
	escaped := make([]string, len(args))
	for i, a := range args {
		escaped[i] = windows.EscapeArg(a)
	}
	return strings.Join(escaped, " ")
}

func WithProcessCommandLineOrArgsForWindows(config *runtime.ContainerConfig, image *imagespec.ImageConfig) oci.SpecOpts {
	if image.ArgsEscaped {
		return func(ctx context.Context, client oci.Client, c *containers.Container, s *runtimespec.Spec) (err error) {

			args, firstArgFromImg, err := getArgs(image.Entrypoint, image.Cmd, config.GetCommand(), config.GetArgs())
			if err != nil {
				return err
			}

			var cmdLine string
			if image.ArgsEscaped && firstArgFromImg {
				cmdLine = args[0]
				if len(args) > 1 {
					cmdLine += " " + escapeAndCombineArgsWindows(args[1:])
				}
			} else {
				cmdLine = escapeAndCombineArgsWindows(args)
			}

			return oci.WithProcessCommandLine(cmdLine)(ctx, client, c, s)
		}
	}

	return func(ctx context.Context, client oci.Client, c *containers.Container, s *runtimespec.Spec) (err error) {
		args, _, err := getArgs(image.Entrypoint, image.Cmd, config.GetCommand(), config.GetArgs())
		if err != nil {
			return err
		}
		return oci.WithProcessArgs(args...)(ctx, client, c, s)
	}
}

func getArgs(imgEntrypoint []string, imgCmd []string, ctrEntrypoint []string, ctrCmd []string) ([]string, bool, error) {

	var firstArgFromImg bool
	entrypoint, cmd := ctrEntrypoint, ctrCmd

	if len(entrypoint) == 0 {

		if len(cmd) == 0 {
			cmd = append([]string{}, imgCmd...)
			if len(imgCmd) > 0 {
				firstArgFromImg = true
			}
		}
		if entrypoint == nil {
			entrypoint = append([]string{}, imgEntrypoint...)
			if len(imgEntrypoint) > 0 || len(ctrCmd) == 0 {
				firstArgFromImg = true
			}
		}
	}
	if len(entrypoint) == 0 && len(cmd) == 0 {
		return nil, false, errors.New("no command specified")
	}
	return append(entrypoint, cmd...), firstArgFromImg, nil
}
