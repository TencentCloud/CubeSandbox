// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package terminalcore

import (
	"context"
	"errors"
	"testing"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/require"
)

func TestBuildTerminalProcessSpecPreservesSecurityContextAndDoesNotMutateBase(t *testing.T) {
	base := &specs.Process{
		Terminal: false,
		Args:     []string{"old"},
		Env:      []string{"PATH=/bin", "TERM=vt100", "TERM=duplicate"},
		Cwd:      "/workspace",
		User:     specs.User{UID: 1000, GID: 1000},
		Capabilities: &specs.LinuxCapabilities{
			Bounding: []string{"CAP_CHOWN"},
		},
	}
	derived := buildTerminalProcessSpec(base, []string{"/bin/sh", "-il"})

	require.True(t, derived.Terminal)
	require.Equal(t, []string{"/bin/sh", "-il"}, derived.Args)
	require.Equal(t, []string{"PATH=/bin", "TERM=xterm-256color"}, derived.Env)
	require.Equal(t, "/workspace", derived.Cwd)
	require.Equal(t, base.User, derived.User)
	require.Same(t, base.Capabilities, derived.Capabilities)

	require.False(t, base.Terminal)
	require.Equal(t, []string{"old"}, base.Args)
	require.Equal(t, []string{"PATH=/bin", "TERM=vt100", "TERM=duplicate"}, base.Env)
}

func TestExecutableNotFoundClassification(t *testing.T) {
	require.True(t, isExecutableNotFound(errors.New("exec: /bin/bash: no such file or directory"), "/bin/bash"))
	require.True(t, isExecutableNotFound(errors.New("exec: /bin/bash: executable file not found in $PATH"), "/bin/bash"))
	require.False(t, isExecutableNotFound(errors.New("open terminal fifo: no such file or directory"), "/bin/bash"))
	require.False(t, isExecutableNotFound(errors.New("permission denied"), "/bin/bash"))
}

func TestContainerdPTYProcessRestoresNamespaceOnRuntimeCalls(t *testing.T) {
	process := &containerdPTYProcess{namespace: "tenant-a"}
	ctx := process.namespacedContext(context.Background())
	namespace, ok := namespaces.Namespace(ctx)
	require.True(t, ok)
	require.Equal(t, "tenant-a", namespace)
}
