// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func TestCheckAndGetAnnotationOverridesUserHostMountAllowlist(t *testing.T) {
	cfg := ensureSandboxTestConfig(t)
	origExtraConf := cfg.ExtraConf
	cfg.ExtraConf = &config.ExtraConf{
		AllowedHostMountPrefixes: []string{"/srv/cubelet/shared/"},
	}
	t.Cleanup(func() { cfg.ExtraConf = origExtraConf })

	req := &types.CreateCubeSandboxReq{
		Annotations: map[string]string{
			cubebox.AnnotationHostMountAllowedPrefixes: `["/"]`,
		},
	}
	out := &cubebox.RunCubeSandboxRequest{}
	require.NoError(t, checkAndGetAnnotation(req, out))

	prefixes, ok, err := cubebox.HostMountAllowedPrefixes(out.Annotations)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, []string{"/srv/cubelet/shared/"}, prefixes)
}

func TestFillResumeRecreateFieldsStampsCurrentHostMountAllowlist(t *testing.T) {
	cfg := ensureSandboxTestConfig(t)
	origExtraConf := cfg.ExtraConf
	cfg.ExtraConf = &config.ExtraConf{
		AllowedHostMountPrefixes: []string{"/srv/cubelet/shared/"},
	}
	t.Cleanup(func() { cfg.ExtraConf = origExtraConf })

	out := &cubebox.RunCubeSandboxRequest{}
	fillResumeRecreateFields(t.Context(), "sb-hostdir", out, nil)

	prefixes, ok, err := cubebox.HostMountAllowedPrefixes(out.Annotations)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, []string{"/srv/cubelet/shared/"}, prefixes)
}
