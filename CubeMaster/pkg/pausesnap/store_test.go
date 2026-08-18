// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package pausesnap

import (
	"context"
	"encoding/json"
	"errors"
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

func TestIsReadyPauseSnapshot(t *testing.T) {
	t.Parallel()

	require.True(t, isReadyPauseSnapshot("READY"))
	require.True(t, isReadyPauseSnapshot(" ready "))
	require.False(t, isReadyPauseSnapshot(statusFailed))
	require.False(t, isReadyPauseSnapshot(statusCreating))
	require.False(t, isReadyPauseSnapshot(""))
}

// A READY binding means the sandbox is genuinely paused: Begin must wrap
// ErrAlreadyExists (via %w) so the caller can detect it with errors.Is and
// treat the pause as idempotent already-paused.
func TestBeginReadyBindingWrapsErrAlreadyExists(t *testing.T) {
	db := setupPauseDeleteTest(t)
	seedPauseBinding(t, db, "sb-ready", "snap-ready", statusReady, "10.0.0.1")

	_, err := Begin(context.Background(), "sb-ready", "node-1", "10.0.0.1", "cubebox")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrAlreadyExists))
	require.Contains(t, err.Error(), "snap-ready")
}

// A CREATING/FAILED binding is not a clean paused state: Begin must NOT wrap
// ErrAlreadyExists, so the caller keeps the generic failure path instead of
// masking an in-flight or terminally-failed pause as already-paused.
func TestBeginNonReadyBindingDoesNotWrapErrAlreadyExists(t *testing.T) {
	for _, status := range []string{statusCreating, statusFailed} {
		t.Run(status, func(t *testing.T) {
			db := setupPauseDeleteTest(t)
			seedPauseBinding(t, db, "sb-x", "snap-x", status, "10.0.0.1")

			_, err := Begin(context.Background(), "sb-x", "node-1", "10.0.0.1", "cubebox")
			require.Error(t, err)
			require.False(t, errors.Is(err, ErrAlreadyExists))
			require.Contains(t, err.Error(), status)
		})
	}
}
