// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package seccomp

import (
	"context"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"

	cseccomp "github.com/containerd/containerd/v2/contrib/seccomp"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/opencontainers/runtime-spec/specs-go"
)

func GenOpt(_ context.Context, reqSysCalls []*cubebox.SysCall) oci.SpecOpts {
	if len(reqSysCalls) == 0 {
		return func(context.Context, oci.Client, *containers.Container, *specs.Spec) error {
			return nil
		}
	}

	sysCalls := make([]specs.LinuxSyscall, 0, len(reqSysCalls))
	for _, sysCall := range reqSysCalls {
		var args []specs.LinuxSeccompArg
		for _, arg := range sysCall.GetArgs() {
			args = append(args, specs.LinuxSeccompArg{
				Index:    uint(arg.Index),
				Value:    arg.Value,
				ValueTwo: arg.ValueTwo,
				Op:       specs.LinuxSeccompOperator(arg.Op),
			})
		}

		var errno *uint
		if sysCall.Errno != 0 {
			errnoValue := uint(sysCall.Errno)
			errno = &errnoValue
		}

		sysCalls = append(sysCalls, specs.LinuxSyscall{
			Names:    sysCall.Names,
			Action:   specs.LinuxSeccompAction(sysCall.Action),
			ErrnoRet: errno,
			Args:     args,
		})
	}
	return withSeccomp(sysCalls)
}

func withSeccomp(sysCalls []specs.LinuxSyscall) oci.SpecOpts {
	return func(ctx context.Context, client oci.Client, c *containers.Container, s *specs.Spec) error {
		if s.Linux == nil {
			s.Linux = &specs.Linux{}
		}
		if s.Linux.Seccomp == nil {
			ensureSeccompProfilePrereqs(s)
			s.Linux.Seccomp = cseccomp.DefaultProfile(s)
		}

		s.Linux.Seccomp.Syscalls = append(s.Linux.Seccomp.Syscalls, sysCalls...)
		return nil
	}
}

func ensureSeccompProfilePrereqs(s *specs.Spec) {
	if s.Process == nil {
		s.Process = &specs.Process{}
	}
	if s.Process.Capabilities == nil {
		s.Process.Capabilities = &specs.LinuxCapabilities{}
	}
}
