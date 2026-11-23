// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package volume

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/volume/refcount"
	bolt "go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
)

type reportingDriver struct {
	VolumePlugin
	detachErr   error
	attachCalls int
}

func (*reportingDriver) Name() string { return "fixture" }
func (p *reportingDriver) Attach(context.Context, *AttachRequest) (*AttachResult, error) {
	p.attachCalls++
	return &AttachResult{}, nil
}
func (p *reportingDriver) Detach(context.Context, *DetachRequest) error {
	return p.detachErr
}

func TestSharedFirstAttachReportFollowsSuccessfulCreate(t *testing.T) {
	for _, firstRollsBackBeforeReply := range []bool{false, true} {
		name := "success_before_rollback"
		if firstRollsBackBeforeReply {
			name = "success_after_rollback"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			db, err := bolt.Open(filepath.Join(t.TempDir(), "volume.db"), 0600, nil)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			store, err := refcount.New(db)
			require.NoError(t, err)
			m := &Manager{byName: make(map[string]VolumePlugin)}
			driver := &reportingDriver{}
			m.Register(driver)
			m.SetRefCountStore(store)

			attach := func(id string, before int64) {
				req := &AttachRequest{Namespace: "default", VolumeID: "shared", SandboxID: id, Driver: driver.Name()}
				_, err := m.Attach(ctx, req)
				require.NoError(t, err)
				require.Equal(t, before, req.RefCount)
			}
			detach := func(id string) (*DetachRequest, error) {
				req := &DetachRequest{Namespace: "default", VolumeID: "shared", SandboxID: id, Driver: driver.Name()}
				return req, m.Detach(ctx, req)
			}
			attach("first", 0)
			attach("second", 1)
			rollbackFirst := func() {
				req, err := detach("first")
				require.NoError(t, err)
				require.EqualValues(t, 1, req.RefCount)
				require.False(t, req.NodeRefLastDetach)
			}
			if firstRollsBackBeforeReply {
				rollbackFirst()
			}
			ids, err := m.TakeCreateReports("default", "second", []string{"shared"})
			require.NoError(t, err)
			require.Equal(t, []string{"shared"}, ids)
			ids, err = m.TakeCreateReports("default", "second", []string{"shared"})
			require.NoError(t, err)
			require.Empty(t, ids, "a shared mount can report +1 only once")
			if !firstRollsBackBeforeReply {
				rollbackFirst()
			}

			driver.detachErr = errors.New("cannot detach")
			req, err := detach("second")
			require.ErrorIs(t, err, driver.detachErr)
			require.False(t, req.NodeRefLastDetach)
			require.False(t, req.CreateReportClaimed)
			ids, err = m.TakeCreateReports("default", "second", []string{"shared"})
			require.NoError(t, err)
			require.Empty(t, ids, "a failed last detach must not reset the +1")
			driver.detachErr = nil
			req, err = detach("second")
			require.NoError(t, err)
			require.True(t, req.NodeRefLastDetach)
			require.True(t, req.CreateReportClaimed)
			require.Zero(t, req.RefCount)

			attach("failed", 0)
			req, err = detach("failed")
			require.NoError(t, err)
			require.False(t, req.NodeRefLastDetach, "no -1 for a mount that never reported +1")

			require.NoError(t, store.Restore("default", "shared", "legacy", driver.Name(), nil))
			req, err = detach("legacy")
			require.NoError(t, err)
			require.True(t, req.NodeRefLastDetach)
			require.False(t, req.CreateReportClaimed)
		})
	}
}

func TestAttachClassifiesClosedHolderDatabaseBeforePlugin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "volume.db")
	db, err := bolt.Open(path, 0600, nil)
	require.NoError(t, err)
	store, err := refcount.New(db)
	require.NoError(t, err)
	m := &Manager{byName: make(map[string]VolumePlugin)}
	driver := &reportingDriver{}
	m.Register(driver)
	m.SetRefCountStore(store)
	_, err = store.Acquire("default", "shared", "existing", driver.Name())
	require.NoError(t, err)
	require.NoError(t, db.Close())

	_, err = m.Attach(t.Context(), &AttachRequest{
		Namespace: "default", VolumeID: "shared", SandboxID: "new", Driver: driver.Name(),
	})
	var notStarted *AttachNotStartedError
	require.ErrorAs(t, err, &notStarted)
	require.ErrorIs(t, err, bolterrors.ErrDatabaseNotOpen)
	require.Zero(t, driver.attachCalls)

	db, err = bolt.Open(path, 0600, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	store, err = refcount.New(db)
	require.NoError(t, err)
	record, found, err := store.GetRecord("default", "shared")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, map[string]struct{}{"existing": {}}, record.SandboxIDs)
}
