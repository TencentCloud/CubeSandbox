// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sysctl

import (
	"context"
	"testing"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/opencontainers/runtime-spec/specs-go"
)

func TestGenOpt(t *testing.T) {
	sysctls := map[string]string{
		"net.ipv4.ip_forward":          "1",
		"net.ipv6.conf.all.forwarding": "1",
	}

	s := &specs.Spec{}

	err := GenOpt(sysctls)(context.Background(), nil, &containers.Container{}, s)
	if err != nil {
		t.Errorf("GenOpt() error = %v", err)
	}

	for k, v := range sysctls {
		if s.Linux.Sysctl[k] != v {
			t.Errorf("GenOpt() Sysctl[%s] = %s, want %s", k, s.Linux.Sysctl[k], v)
		}
	}
}
