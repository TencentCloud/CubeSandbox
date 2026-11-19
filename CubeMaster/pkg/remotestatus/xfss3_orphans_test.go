// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package remotestatus

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/pausesnap"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxspec"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	cubeleterrorcode "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

type orphanWorld struct {
	snapshots  map[string][]*cubebox.LocalSnapshotInfo // node -> packages
	sandboxes  map[string][]*cubebox.CubeSandbox       // node -> sandboxes
	bindings   map[string]bool                         // snapshot id -> bound
	sbBindings map[string]bool                         // sandbox id -> bound
	backends   map[string]string
	running    map[string]string
	dbErr      error
	cleaned    []string
	dropped    []string
}

func withOrphanWorld(t *testing.T, w *orphanWorld) {
	t.Helper()
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	prev := []any{listNodeIPs, listLocalSnapshots, listNodeSandboxes, pauseBySnapshot, pauseBySandbox,
		cleanupPausePackage, dropPausedTombstone, sandboxBackend, sandboxRunningHostIP, nowFn, cubeletAddr}
	listNodeIPs = func() []string { return []string{"10.0.0.1", "10.0.0.2"} }
	cubeletAddr = func(ip string) string { return ip }
	nowFn = func() time.Time { return now }
	listLocalSnapshots = func(_ context.Context, ip string, _ *cubebox.ListLocalSnapshotsRequest) (*cubebox.ListLocalSnapshotsResponse, error) {
		return &cubebox.ListLocalSnapshotsResponse{Snapshots: w.snapshots[ip]}, nil
	}
	listNodeSandboxes = func(_ context.Context, ip string, _ *cubebox.ListCubeSandboxRequest) (*cubebox.ListCubeSandboxResponse, error) {
		return &cubebox.ListCubeSandboxResponse{Items: w.sandboxes[ip]}, nil
	}
	pauseBySnapshot = func(_ context.Context, id string) (*pausesnap.Record, error) {
		if w.dbErr != nil {
			return nil, w.dbErr
		}
		if w.bindings[id] {
			return &pausesnap.Record{SnapshotID: id}, nil
		}
		return nil, pausesnap.ErrNotFound
	}
	pauseBySandbox = func(_ context.Context, id string) (*pausesnap.Record, error) {
		if w.sbBindings[id] {
			return &pausesnap.Record{SandboxID: id}, nil
		}
		return nil, pausesnap.ErrNotFound
	}
	cleanupPausePackage = func(_ context.Context, _, ip, snap, backend string) error {
		if backend != constants.SnapshotBackendXFSS3 {
			t.Errorf("cleanup with backend %q", backend)
		}
		w.cleaned = append(w.cleaned, ip+"/"+snap)
		return nil
	}
	dropPausedTombstone = func(_ context.Context, _, sid, ip string) error {
		w.dropped = append(w.dropped, ip+"/"+sid)
		return nil
	}
	sandboxBackend = func(_ context.Context, sid string) (string, error) {
		b, ok := w.backends[sid]
		if !ok {
			return "", sandboxspec.ErrSandboxSpecNotFound
		}
		return b, nil
	}
	sandboxRunningHostIP = func(_ context.Context, sid string) string { return w.running[sid] }
	t.Cleanup(func() {
		listNodeIPs = prev[0].(func() []string)
		listLocalSnapshots = prev[1].(func(context.Context, string, *cubebox.ListLocalSnapshotsRequest) (*cubebox.ListLocalSnapshotsResponse, error))
		listNodeSandboxes = prev[2].(func(context.Context, string, *cubebox.ListCubeSandboxRequest) (*cubebox.ListCubeSandboxResponse, error))
		pauseBySnapshot = prev[3].(func(context.Context, string) (*pausesnap.Record, error))
		pauseBySandbox = prev[4].(func(context.Context, string) (*pausesnap.Record, error))
		cleanupPausePackage = prev[5].(func(context.Context, string, string, string, string) error)
		dropPausedTombstone = prev[6].(func(context.Context, string, string, string) error)
		sandboxBackend = prev[7].(func(context.Context, string) (string, error))
		sandboxRunningHostIP = prev[8].(func(context.Context, string) string)
		nowFn = prev[9].(func() time.Time)
		cubeletAddr = prev[10].(func(string) string)
	})
}

func pkg(id, backend, kind string, age time.Duration) *cubebox.LocalSnapshotInfo {
	at := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC).Add(-age).Format(time.RFC3339)
	return &cubebox.LocalSnapshotInfo{SnapshotID: id, Backend: backend, Kind: kind, CreatedAt: at}
}

func TestOrphanPackagesOnlyUnboundSettledXFSS3Pauses(t *testing.T) {
	w := &orphanWorld{
		snapshots: map[string][]*cubebox.LocalSnapshotInfo{
			"10.0.0.1": {
				pkg("snap-orphan", "xfs-s3", "pause_snapshot", time.Hour),
				pkg("snap-bound", "xfs-s3", "pause_snapshot", time.Hour),
				pkg("snap-young", "xfs-s3", "pause_snapshot", time.Minute),
				pkg("snap-plain", "xfs", "pause_snapshot", time.Hour),
				pkg("snap-user", "xfs-s3", "snapshot", time.Hour),
			},
		},
		bindings: map[string]bool{"snap-bound": true},
	}
	withOrphanWorld(t, w)
	ReconcileXFSS3Orphans(context.Background())
	if len(w.cleaned) != 1 || w.cleaned[0] != "10.0.0.1/snap-orphan" {
		t.Fatalf("cleaned %v, want only the unbound settled xfs-s3 pause package", w.cleaned)
	}
}

func TestOrphanPackagesUnreadableBindingIsNotAnOrphan(t *testing.T) {
	w := &orphanWorld{
		snapshots: map[string][]*cubebox.LocalSnapshotInfo{"10.0.0.1": {pkg("snap-x", "xfs-s3", "pause_snapshot", time.Hour)}},
		dbErr:     errors.New("db down"),
	}
	withOrphanWorld(t, w)
	ReconcileXFSS3Orphans(context.Background())
	if len(w.cleaned) != 0 {
		t.Fatalf("cleaned %v while the binding table was unreadable", w.cleaned)
	}
}

func paused(id string) *cubebox.CubeSandbox {
	at := time.Date(2026, 11, 16, 11, 0, 0, 0, time.UTC).UnixNano()
	return &cubebox.CubeSandbox{Id: id, PausedAt: at, Containers: []*cubebox.Container{{Id: id, State: cubebox.ContainerState_CONTAINER_PAUSED}}}
}

func TestOrphanTombstonesRespectGrace(t *testing.T) {
	young := paused("sb-young")
	young.PausedAt = time.Date(2026, 11, 16, 11, 58, 0, 0, time.UTC).UnixNano()
	w := &orphanWorld{
		sandboxes: map[string][]*cubebox.CubeSandbox{"10.0.0.1": {young}},
		backends:  map[string]string{"sb-young": "xfs-s3"},
		running:   map[string]string{"sb-young": "10.0.0.2"},
	}
	withOrphanWorld(t, w)
	ReconcileXFSS3Orphans(context.Background())
	if len(w.dropped) != 0 {
		t.Fatalf("dropped %v inside the grace period", w.dropped)
	}
}

func TestOrphanTombstonesDroppedOnlyWhenRunningElsewhere(t *testing.T) {
	w := &orphanWorld{
		sandboxes: map[string][]*cubebox.CubeSandbox{
			"10.0.0.1": {paused("sb-moved"), paused("sb-bound"), paused("sb-plain"), paused("sb-nowhere"), paused("sb-here"), paused("sb-deleted")},
		},
		sbBindings: map[string]bool{"sb-bound": true},
		backends:   map[string]string{"sb-moved": "xfs-s3", "sb-bound": "xfs-s3", "sb-plain": "xfs", "sb-nowhere": "xfs-s3", "sb-here": "xfs-s3"},
		running:    map[string]string{"sb-moved": "10.0.0.2", "sb-bound": "10.0.0.2", "sb-plain": "10.0.0.2", "sb-here": "10.0.0.1"},
	}
	withOrphanWorld(t, w)
	ReconcileXFSS3Orphans(context.Background())
	if len(w.dropped) != 2 || w.dropped[0] != "10.0.0.1/sb-moved" || w.dropped[1] != "10.0.0.1/sb-deleted" {
		t.Fatalf("dropped %v, want the moved xfs-s3 sandbox and the deleted sandbox", w.dropped)
	}
}

func TestOrphanListFailuresSkipTheNode(t *testing.T) {
	w := &orphanWorld{
		snapshots: map[string][]*cubebox.LocalSnapshotInfo{"10.0.0.2": {pkg("snap-x", "xfs-s3", "pause_snapshot", time.Hour)}},
	}
	withOrphanWorld(t, w)
	listLocalSnapshots = func(_ context.Context, ip string, _ *cubebox.ListLocalSnapshotsRequest) (*cubebox.ListLocalSnapshotsResponse, error) {
		if ip == "10.0.0.1" {
			return nil, errors.New("connection refused")
		}
		return &cubebox.ListLocalSnapshotsResponse{
			Ret:       &cubeleterrorcode.Ret{RetCode: cubeleterrorcode.ErrorCode_Unknown, RetMsg: "list local snapshots failed"},
			Snapshots: w.snapshots[ip],
		}, nil
	}
	ReconcileXFSS3Orphans(context.Background())
	if len(w.cleaned) != 0 {
		t.Fatalf("cleaned %v from a node whose listing failed", w.cleaned)
	}
}
