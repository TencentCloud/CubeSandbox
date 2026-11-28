// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestLoadKernelCmdlineFile(t *testing.T) {
	t.Run("top-level list", func(t *testing.T) {
		path := writeTemp(t, "cmdline.yaml", "- page_reporting.page_reporting_order=9\n- damon_reclaim.min_age=100000\n")
		got, err := loadKernelCmdlineFile(path)
		require.NoError(t, err)
		assert.Equal(t, []string{
			"page_reporting.page_reporting_order=9",
			"damon_reclaim.min_age=100000",
		}, got)
	})

	t.Run("json list", func(t *testing.T) {
		path := writeTemp(t, "cmdline.json", `["damon_reclaim.enabled=Y"]`)
		got, err := loadKernelCmdlineFile(path)
		require.NoError(t, err)
		assert.Equal(t, []string{"damon_reclaim.enabled=Y"}, got)
	})

	t.Run("missing file", func(t *testing.T) {
		_, err := loadKernelCmdlineFile(filepath.Join(t.TempDir(), "absent.yaml"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "read kernel cmdline file")
	})

	t.Run("empty file", func(t *testing.T) {
		got, err := loadKernelCmdlineFile(writeTemp(t, "empty.yaml", ""))
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("mapping rejected", func(t *testing.T) {
		_, err := loadKernelCmdlineFile(writeTemp(t, "wrapper.yaml", "cmdline:\n  - damon_reclaim.enabled=Y\n"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "top-level list")
	})
}

func TestMergeKernelCmdline(t *testing.T) {
	t.Run("file then cli", func(t *testing.T) {
		got := mergeKernelCmdline(
			[]string{"page_reporting.page_reporting_order=6"},
			[]string{"damon_reclaim.enabled=Y"},
		)
		assert.Equal(t, []string{
			"page_reporting.page_reporting_order=6",
			"damon_reclaim.enabled=Y",
		}, got)
	})

	t.Run("cli overrides duplicate key", func(t *testing.T) {
		got := mergeKernelCmdline(
			[]string{"page_reporting.page_reporting_order=6", "damon_reclaim.enabled=Y"},
			[]string{"page_reporting.page_reporting_order=9"},
		)
		assert.Equal(t, []string{
			"page_reporting.page_reporting_order=9",
			"damon_reclaim.enabled=Y",
		}, got)
	})

	t.Run("blank entries dropped", func(t *testing.T) {
		got := mergeKernelCmdline([]string{" ", ""}, []string{"damon_reclaim.enabled=Y", "  "})
		assert.Equal(t, []string{"damon_reclaim.enabled=Y"}, got)
	})

	t.Run("empty inputs", func(t *testing.T) {
		assert.Empty(t, mergeKernelCmdline(nil, nil))
	})
}
