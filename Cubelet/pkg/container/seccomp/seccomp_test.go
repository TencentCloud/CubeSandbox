// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package seccomp

import (
	"context"
	"testing"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"

	cseccomp "github.com/containerd/containerd/v2/contrib/seccomp"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
)

func TestGenOpt(t *testing.T) {
	in := []*cubebox.SysCall{
		{
			Names:  []string{"ptrace"},
			Action: string(specs.ActAllow),
		},
	}
	specFunc := GenOpt(context.Background(), in)
	if specFunc == nil {
		assert.FailNow(t, "should not nil")
	}
	s := oci.Spec{}
	err := specFunc(context.Background(), nil, nil, &s)
	assert.Nil(t, err)
	assert.NotNil(t, s.Linux)
	assert.NotNil(t, s.Linux.Seccomp)
	assert.Equal(t, specs.ActErrno, s.Linux.Seccomp.DefaultAction)
	syscall := specs.LinuxSyscall{
		Names:  []string{"ptrace"},
		Action: specs.ActAllow,
	}
	assert.Equal(t, syscall, s.Linux.Seccomp.Syscalls[len(s.Linux.Seccomp.Syscalls)-1])
}

func TestGenOptInitializesContainerdDefaultProfileWhenSeccompMissing(t *testing.T) {
	in := []*cubebox.SysCall{
		{
			Names:  []string{"getpid"},
			Action: string(specs.ActAllow),
		},
	}
	specFunc := GenOpt(context.Background(), in)
	s := oci.Spec{
		Linux: &specs.Linux{},
		Process: &specs.Process{
			Capabilities: &specs.LinuxCapabilities{
				Bounding: []string{"CAP_SYS_ADMIN"},
			},
		},
	}
	expectedBase := cseccomp.DefaultProfile(&specs.Spec{
		Linux: &specs.Linux{},
		Process: &specs.Process{
			Capabilities: &specs.LinuxCapabilities{
				Bounding: []string{"CAP_SYS_ADMIN"},
			},
		},
	})

	err := specFunc(context.Background(), nil, nil, &s)
	assert.NoError(t, err)
	assert.NotNil(t, s.Linux.Seccomp)
	assert.Equal(t, expectedBase.DefaultAction, s.Linux.Seccomp.DefaultAction)
	assert.Equal(t, expectedBase.Architectures, s.Linux.Seccomp.Architectures)
	assert.Equal(t, append(expectedBase.Syscalls, specs.LinuxSyscall{
		Names:  []string{"getpid"},
		Action: specs.ActAllow,
	}), s.Linux.Seccomp.Syscalls)
}

func TestGenOptAppendsToExistingSeccomp(t *testing.T) {
	in := []*cubebox.SysCall{
		{
			Names:  []string{"getpid"},
			Action: string(specs.ActAllow),
		},
	}
	specFunc := GenOpt(context.Background(), in)
	s := oci.Spec{
		Linux: &specs.Linux{
			Seccomp: &specs.LinuxSeccomp{
				DefaultAction: specs.ActErrno,
				Syscalls: []specs.LinuxSyscall{
					{
						Names:  []string{"ptrace"},
						Action: specs.ActAllow,
					},
				},
			},
		},
	}

	err := specFunc(context.Background(), nil, nil, &s)
	assert.NoError(t, err)
	assert.Equal(t, specs.ActErrno, s.Linux.Seccomp.DefaultAction)
	assert.Equal(t, []specs.LinuxSyscall{
		{
			Names:  []string{"ptrace"},
			Action: specs.ActAllow,
		},
		{
			Names:  []string{"getpid"},
			Action: specs.ActAllow,
		},
	}, s.Linux.Seccomp.Syscalls)
}

func TestGenOptNoSyscallsIsNoop(t *testing.T) {
	specFunc := GenOpt(context.Background(), nil)
	s := oci.Spec{}

	err := specFunc(context.Background(), nil, nil, &s)
	assert.NoError(t, err)
	assert.Nil(t, s.Linux)
}

func TestGenOptKeepsExistingSeccompWhenSyscallsEmpty(t *testing.T) {
	specFunc := GenOpt(context.Background(), nil)
	s := oci.Spec{
		Linux: &specs.Linux{
			Seccomp: &specs.LinuxSeccomp{
				DefaultAction: specs.ActAllow,
			},
		},
	}

	err := specFunc(context.Background(), nil, nil, &s)
	assert.NoError(t, err)
	assert.NotNil(t, s.Linux)
	assert.NotNil(t, s.Linux.Seccomp)
	assert.Equal(t, specs.ActAllow, s.Linux.Seccomp.DefaultAction)
}
