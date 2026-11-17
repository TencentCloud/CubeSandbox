// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRootfsWritableLayerSurvivesStoreSync(t *testing.T) {
	const layer = "disk/tpl-2f7b0d7b7caa476c98cdf416_0"

	store, cleanup := createTestStore(t)
	defer cleanup()

	box := createTestCubeBox("box1", "")
	box.RootfsWritableLayer = layer
	store.Add(box)
	require.NoError(t, store.Sync("box1"))

	raw, err := store.db.Get(DBBucketSandbox, "box1")
	require.NoError(t, err)

	var restored CubeBox
	require.NoError(t, json.Unmarshal(raw, &restored))
	require.Equal(t, layer, restored.RootfsWritableLayer)
}
