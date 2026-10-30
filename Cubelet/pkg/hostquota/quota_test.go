// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package hostquota

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
)

func TestAutomaticAndExplicitQuota(t *testing.T) {
	c := &config.HostConf{}
	require.Equal(t, int64(64000), CPU(c, 32))
	read := func() (int64, error) { return 63786, nil }
	mem, err := Memory(c, read)
	require.NoError(t, err)
	require.Equal(t, int64(79733), mem)
	c.Quota.Cpu, c.Quota.Mem = 2000, "4Gi"
	require.Equal(t, int64(2000), CPU(c, 32, 8000))
	mem, err = Memory(c, read, 8192)
	require.NoError(t, err)
	require.Equal(t, int64(4096), mem)
}

func TestFallbackAndDetectionFailures(t *testing.T) {
	require.Equal(t, int64(8000), CPU(nil, 32, 0, 8000))
	fail := func() (int64, error) { return 0, errors.New("unavailable") }
	mem, err := Memory(nil, fail, 0, 8192)
	require.NoError(t, err)
	require.Equal(t, int64(8192), mem)
	_, err = Memory(nil, fail)
	require.Error(t, err)
	c := &config.HostConf{}
	c.Quota.Mem = "broken"
	_, err = Memory(c, fail, 8192)
	require.ErrorContains(t, err, "invalid host memory quota")
}
