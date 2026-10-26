// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package pausesnap

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPluginVolumeIDsFromJSON(t *testing.T) {
	t.Parallel()

	require.Nil(t, pluginVolumeIDsFromJSON(""))
	require.Nil(t, pluginVolumeIDsFromJSON("   "))
	require.Nil(t, pluginVolumeIDsFromJSON("{not-json"))
	require.Nil(t, pluginVolumeIDsFromJSON(`[]`))

	raw, err := json.Marshal([]string{" vol-a ", "", "vol-a", "vol-b"})
	require.NoError(t, err)
	require.Equal(t, []string{"vol-a", "vol-b"}, pluginVolumeIDsFromJSON(string(raw)))
}

func TestUniqueNonEmpty(t *testing.T) {
	t.Parallel()

	require.Nil(t, uniqueNonEmpty(nil))
	require.Nil(t, uniqueNonEmpty([]string{}))
	require.Empty(t, uniqueNonEmpty([]string{"", "  "}))
	require.Equal(t, []string{"a", "b"}, uniqueNonEmpty([]string{" a ", "a", "", "b"}))
}

func TestGenerateSnapshotIDFormat(t *testing.T) {
	t.Parallel()

	id := GenerateSnapshotID()
	require.True(t, len(id) > len(snapshotIDPrefix))
	require.Equal(t, snapshotIDPrefix, id[:len(snapshotIDPrefix)])
}

func TestIsShimGonePauseSnapshot(t *testing.T) {
	t.Parallel()

	require.True(t, isShimGonePauseSnapshot("READY"))
	require.True(t, isShimGonePauseSnapshot(" ready "))
	require.True(t, isShimGonePauseSnapshot(statusDeleteFailed))
	require.False(t, isShimGonePauseSnapshot(statusFailed))
	require.False(t, isShimGonePauseSnapshot(statusCreating))
	require.False(t, isShimGonePauseSnapshot(""))
}
