// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/volume"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/volume/binary"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/volume/refcount"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	bolt "go.etcd.io/bbolt"
)

func TestLookupPluginVolumeSource(t *testing.T) {
	t.Parallel()

	raw := `[{"name":"vol-a","driver":"cos","private_data":"volumes/vol-a/"},{"name":"vol-b","driver":"cos-rpc"}]`
	annotations := map[string]string{"plugin-volume-sources": raw}

	driver, pd, ok := lookupPluginVolumeSource(annotations, "vol-a")
	if !ok || driver != "cos" || pd != "volumes/vol-a/" {
		t.Fatalf("vol-a: driver=%q private_data=%q ok=%v", driver, pd, ok)
	}
	driver, pd, ok = lookupPluginVolumeSource(annotations, "vol-b")
	if !ok || driver != "cos-rpc" || pd != "" {
		t.Fatalf("vol-b: driver=%q private_data=%q ok=%v", driver, pd, ok)
	}
	if _, _, ok := lookupPluginVolumeSource(annotations, "missing"); ok {
		t.Fatal("expected ok=false for missing volume")
	}
	if _, _, ok := lookupPluginVolumeSource(nil, "vol-a"); ok {
		t.Fatal("expected ok=false for nil annotations")
	}
	if _, _, ok := lookupPluginVolumeSource(map[string]string{"plugin-volume-sources": "{bad"}, "vol-a"); ok {
		t.Fatal("expected ok=false for malformed JSON")
	}
}

func TestIsPluginVolume(t *testing.T) {
	t.Parallel()

	annotations := map[string]string{
		"plugin-volume-sources": `[{"name":"vol-a","driver":"cos","private_data":"x"}]`,
	}
	if !isPluginVolume(annotations, "vol-a") {
		t.Fatal("expected vol-a to be a plugin volume")
	}
	if isPluginVolume(annotations, "vol-b") {
		t.Fatal("expected vol-b not to be a plugin volume")
	}
}

type rollbackVolumeDriver struct {
	volume.VolumePlugin
	detachErr error
}

func (*rollbackVolumeDriver) Name() string { return "test-create-rollback" }
func (*rollbackVolumeDriver) Attach(context.Context, *volume.AttachRequest) (*volume.AttachResult, error) {
	return &volume.AttachResult{}, nil
}
func (p *rollbackVolumeDriver) Detach(context.Context, *volume.DetachRequest) error {
	return p.detachErr
}
func (*rollbackVolumeDriver) Close() error { return nil }

func TestPluginVolumeRollbackReportsConfirmedRelease(t *testing.T) {
	db, err := bolt.Open(filepath.Join(t.TempDir(), "volumes.db"), 0600, nil)
	require.NoError(t, err)
	store, err := refcount.New(db)
	require.NoError(t, err)
	mgr := volume.Global()
	mgr.SetRefCountStore(store)
	driver := &rollbackVolumeDriver{}
	mgr.Register(driver)
	t.Cleanup(func() {
		mgr.SetRefCountStore(nil)
		require.NoError(t, db.Close())
	})

	const namespace = "rollback-tests"
	for _, tc := range []struct {
		name    string
		shared  bool
		failure bool
	}{
		{name: "unreported first mount"},
		{name: "last holder after successful sandbox leaves", shared: true},
		{name: "failed detach leaves cleanup unknown", failure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			id := tc.name
			_, err := mgr.Attach(ctx, &volume.AttachRequest{SandboxID: "failed", Namespace: namespace, VolumeID: id, Driver: driver.Name()})
			require.NoError(t, err)
			if tc.shared {
				_, err := mgr.Attach(ctx, &volume.AttachRequest{SandboxID: "successful", Namespace: namespace, VolumeID: id, Driver: driver.Name()})
				require.NoError(t, err)
				ids, err := mgr.TakeCreateReports(namespace, "successful", []string{id})
				require.NoError(t, err)
				require.Equal(t, []string{id}, ids)
				req := &volume.DetachRequest{SandboxID: "successful", Namespace: namespace, VolumeID: id, Driver: driver.Name()}
				require.NoError(t, mgr.Detach(ctx, req))
				require.False(t, req.NodeRefLastDetach)
			}
			create := &workflow.CreateContext{BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: "failed"}, PluginVolumeIDs: []string{id}}
			rollback := &workflow.DestroyContext{BaseWorkflowInfo: workflow.BaseWorkflowInfo{IsRollBack: true}, CreateVolumeRollback: create}
			info := &StorageInfo{SandboxID: "failed", Namespace: namespace, PluginVolumeBackendInfos: map[string]*PluginVolumeBackendInfo{
				id: {VolumeID: id, Driver: driver.Name()},
			}}
			if tc.failure {
				driver.detachErr = errors.New("backend detach failed")
				t.Cleanup(func() { driver.detachErr = nil })
			}
			err = (&local{}).detachPluginVolumes(ctx, info, rollback)
			if tc.failure {
				require.Error(t, err)
				require.Nil(t, create.PluginVolumeRollback)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, create.PluginVolumeRollback)
			if tc.shared {
				require.Equal(t, []workflow.VolumeRefEvent{{VolumeID: id, Referenced: 0, CreateReportClaimed: true}}, create.PluginVolumeRollback)
			} else {
				require.Empty(t, create.PluginVolumeRollback)
			}
		})
	}
}

func TestPluginVolumeAttachClassifiesProcessStart(t *testing.T) {
	dir := t.TempDir()
	db, err := bolt.Open(filepath.Join(dir, "volumes.db"), 0600, nil)
	require.NoError(t, err)
	store, err := refcount.New(db)
	require.NoError(t, err)
	mgr := volume.Global()
	mgr.SetRefCountStore(store)
	t.Cleanup(func() {
		mgr.SetRefCountStore(nil)
		require.NoError(t, db.Close())
	})

	path := filepath.Join(dir, "controller")
	driver := binary.New("test-attach-process-start")
	require.NoError(t, driver.Init(t.Context(), volume.PluginConfig{BinaryPath: path}))
	mgr.Register(driver)
	ctx := namespaces.WithNamespace(t.Context(), "default")
	const report = "printf '%s\\n' '{\"error\":\"mount rolled back\",\"error_kind\":\"attach_rolled_back\"}'\n"
	for _, tc := range []struct {
		name, script       string
		prior, existing    bool
		uncertain, settled bool
	}{
		{name: "process not started"},
		{name: "generic failure", script: "exit 1\n", uncertain: true},
		{name: "process killed after printing", script: report + "kill -KILL $$\n", uncertain: true},
		{name: "clean attach failure", script: report + "exit 1\n", settled: true},
		{name: "must detach other volumes first", script: report + "exit 1\n", prior: true, settled: true},
		{name: "existing holder is preserved", script: report + "exit 1\n", existing: true, uncertain: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			started := tc.script != ""
			if started {
				body := "#!/bin/sh\nif [ \"$2\" = detach ]; then printf '{}\\n'; exit 0; fi\n" + tc.script
				require.NoError(t, os.WriteFile(path, []byte(body), 0700))
			}
			opts := &workflow.CreateContext{
				BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: "sb"},
				ReqInfo: &cubebox.RunCubeSandboxRequest{Annotations: map[string]string{
					cubebox.AnnotationPluginVolumeSources: `[{"name":"vol","driver":"test-attach-process-start"}]`,
				}},
			}
			info := &StorageInfo{SandboxID: opts.SandboxID, Namespace: "default"}
			if tc.prior {
				_, err := store.Acquire(info.Namespace, "prior", opts.SandboxID, driver.Name())
				require.NoError(t, err)
				opts.PluginVolumeIDs = []string{"prior"}
				info.PluginVolumeBackendInfos = map[string]*PluginVolumeBackendInfo{
					"prior": {VolumeID: "prior", Driver: driver.Name()},
				}
			}
			if tc.existing {
				_, err := store.Acquire(info.Namespace, "vol", opts.SandboxID, driver.Name())
				require.NoError(t, err)
			}
			l := &local{config: &Config{VolumePluginBaseDir: dir}}
			err := l.attachPluginVolume(ctx, opts, &cubebox.Volume{Name: "vol"}, info)
			require.Error(t, err)
			var notStarted *volume.AttachNotStartedError
			require.Equal(t, !started, errors.As(err, &notStarted))
			require.Equal(t, started, opts.PluginVolumeAttachAttempted)
			require.Equal(t, tc.uncertain, opts.PluginVolumeAttachUncertain)
			if tc.prior {
				require.Nil(t, opts.PluginVolumeRollback)
				rollback := &workflow.DestroyContext{BaseWorkflowInfo: workflow.BaseWorkflowInfo{IsRollBack: true}, CreateVolumeRollback: opts}
				require.NoError(t, l.detachPluginVolumes(ctx, info, rollback))
			}
			require.Equal(t, tc.settled, opts.PluginVolumeRollback != nil)
			require.Empty(t, opts.PluginVolumeRollback)
			_, held, err := store.GetRecord(info.Namespace, "vol")
			require.NoError(t, err)
			require.Equal(t, tc.existing, held)
		})
	}
}
