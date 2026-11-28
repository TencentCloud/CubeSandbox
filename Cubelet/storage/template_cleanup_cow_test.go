// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// CoW storage never initializes the legacy format pool, so its path stays
// empty. Cleanup must not turn that into a path relative to the working
// directory and remove whatever happens to share the template's name there.
func TestCleanupTemplateLocalDataSkipsUnsetFormatPath(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	templateID := "tpl-cow-cleanup"
	bystander := filepath.Join(cwd, templateID)
	require.NoError(t, os.MkdirAll(bystander, 0o755))

	previousDir, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(cwd))
	t.Cleanup(func() { _ = os.Chdir(previousDir) })

	previousLocalStorage := localStorage
	localStorage = &local{config: &Config{RootPath: root, DataPath: root}}
	t.Cleanup(func() { localStorage = previousLocalStorage })

	require.NoError(t, CleanupTemplateLocalData(context.Background(), templateID, ""))
	_, err = os.Stat(bystander)
	require.NoError(t, err, "a directory relative to the working directory was removed")
}
