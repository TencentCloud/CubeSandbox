// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package refcount

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReleasedVolumeIDs(t *testing.T) {
	t.Parallel()

	require.Nil(t, ReleasedVolumeIDs(nil))
	require.Nil(t, ReleasedVolumeIDs(map[string][]byte{}))

	raw, err := json.Marshal([]Event{
		{VolumeID: "vol-a", Referenced: 0},
		{VolumeID: "vol-b", Referenced: 1},
		{VolumeID: "vol-a", Referenced: 0},
		{VolumeID: "", Referenced: 0},
	})
	require.NoError(t, err)
	got := ReleasedVolumeIDs(map[string][]byte{ExtInfoKey: raw})
	require.Equal(t, []string{"vol-a"}, got)

	require.Nil(t, ReleasedVolumeIDs(map[string][]byte{ExtInfoKey: []byte("{bad")}))
	require.Nil(t, ReleasedVolumeIDs(map[string][]byte{"other": raw}))
}
