// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package capability

import (
	"context"
	"testing"

	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/stretchr/testify/assert"

	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func TestGenOpt_Add(t *testing.T) {
	testCap := "SYS_ADMIN"

	c := &cubebox.ContainerConfig{
		SecurityContext: &cubebox.ContainerSecurityContext{
			Capabilities: &cubebox.Capability{
				AddCapabilities: []string{testCap},
			},
		},
	}
	ctx := context.Background()

	opts := GenOpt(ctx, c)

	if len(opts) != 1 {
		t.Errorf("unexpected number of options: got %v, want %v", len(opts), 1)
	}

	spec := &oci.Spec{}
	err := opts[0](ctx, nil, nil, spec)
	assert.NoError(t, err)

	assert.NotNil(t, spec.Process)
	assert.NotNil(t, spec.Process.Capabilities)

	expected := "CAP_" + testCap
	assert.Equal(t, spec.Process.Capabilities.Effective, []string{expected})
	assert.Equal(t, spec.Process.Capabilities.Bounding, []string{expected})
	assert.Equal(t, spec.Process.Capabilities.Permitted, []string{expected})
}

func TestGenOpt_Drop(t *testing.T) {
	testCap := "NET_RAW"

	c := &cubebox.ContainerConfig{
		SecurityContext: &cubebox.ContainerSecurityContext{
			Capabilities: &cubebox.Capability{
				DropCapabilities: []string{testCap},
				AddCapabilities:  []string{"ALL"},
			},
		},
	}
	ctx := context.Background()

	opts := GenOpt(ctx, c)

	if len(opts) != 1 {
		t.Errorf("unexpected number of options: got %v, want %v", len(opts), 1)
	}

	spec := &oci.Spec{}
	err := opts[0](ctx, nil, nil, spec)
	assert.NoError(t, err)

	assert.NotNil(t, spec.Process)
	assert.NotNil(t, spec.Process.Capabilities)

	expected := "CAP_" + testCap
	assert.NotContains(t, spec.Process.Capabilities.Bounding, expected)
	assert.NotContains(t, spec.Process.Capabilities.Effective, expected)
	assert.NotContains(t, spec.Process.Capabilities.Permitted, expected)
}

func TestGenOpt_WithoutSecurityContext(t *testing.T) {

	c := &cubebox.ContainerConfig{}
	ctx := context.Background()

	opts := GenOpt(ctx, c)

	if len(opts) != 0 {
		t.Errorf("unexpected number of options: got %v, want %v", len(opts), 0)
	}
}
