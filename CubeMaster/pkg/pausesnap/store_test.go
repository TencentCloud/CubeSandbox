// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package pausesnap

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPluginVolumeIDsFromRequestJSON(t *testing.T) {
	t.Parallel()

	require.Nil(t, pluginVolumeIDsFromRequestJSON(""))
	require.Nil(t, pluginVolumeIDsFromRequestJSON("   "))
	require.Nil(t, pluginVolumeIDsFromRequestJSON("{not-json"))
	require.Nil(t, pluginVolumeIDsFromRequestJSON(`{"plugin_volume_ids":[]}`))

	raw, err := json.Marshal(pauseRequestJSON{PluginVolumeIDs: []string{" vol-a ", "", "vol-a", "vol-b"}})
	require.NoError(t, err)
	require.Equal(t, []string{"vol-a", "vol-b"}, pluginVolumeIDsFromRequestJSON(string(raw)))
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
