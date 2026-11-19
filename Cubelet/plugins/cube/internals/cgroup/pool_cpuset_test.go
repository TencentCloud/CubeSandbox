// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cgroup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// applyCpusetFiles is what repairs a per-NUMA slot on every start, so cover the
// three states it can find: empty, drifted and already correct.
func TestApplyCpusetFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"cpuset.cpus", "cpuset.mems"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), nil, 0644))
	}

	require.NoError(t, applyCpusetFiles(dir, "16-31,208-223", 1))
	require.Equal(t, "16-31,208-223", readCpusetFile(t, dir, "cpuset.cpus"))
	require.Equal(t, "1", readCpusetFile(t, dir, "cpuset.mems"))

	// A CPU hot-add or a manual edit leaves a stale mask behind.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cpuset.cpus"), []byte("0-3"), 0644))
	require.NoError(t, applyCpusetFiles(dir, "16-31,208-223", 1))
	require.Equal(t, "16-31,208-223", readCpusetFile(t, dir, "cpuset.cpus"))

	// A second run against a healthy slot is a no-op, not an error.
	require.NoError(t, applyCpusetFiles(dir, "16-31,208-223", 1))

	// A missing directory is an error: the caller must learn that the mask was
	// never applied instead of starting with an unbound pool.
	require.Error(t, applyCpusetFiles(filepath.Join(dir, "missing"), "0", 0))
}

func readCpusetFile(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	require.NoError(t, err)
	return string(b)
}
