// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func TestResolvePauseSnapshotID(t *testing.T) {
	t.Parallel()
	_, err := resolvePauseSnapshotID(&cubebox.UpdateCubeSandboxRequest{})
	if err == nil {
		t.Fatal("expected error when snapshot id missing")
	}

	id, err := resolvePauseSnapshotID(&cubebox.UpdateCubeSandboxRequest{
		Annotations: map[string]string{
			constants.MasterAnnotationPauseSnapshotID: "snap-abc123def456abc123def456",
		},
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if id != "snap-abc123def456abc123def456" {
		t.Fatalf("id=%q", id)
	}

	_, err = resolvePauseSnapshotID(&cubebox.UpdateCubeSandboxRequest{
		Annotations: map[string]string{
			constants.MasterAnnotationPauseSnapshotID: "pause-sbx1",
		},
	})
	if err == nil {
		t.Fatal("expected error for non snap- prefix")
	}

	id, err = resolvePauseSnapshotID(&cubebox.UpdateCubeSandboxRequest{
		Annotations: map[string]string{
			constants.MasterAnnotationRuntimeSnapshotID: "snap-from-runtime000000000001",
		},
	})
	if err != nil {
		t.Fatalf("runtime fallback err: %v", err)
	}
	if id != "snap-from-runtime000000000001" {
		t.Fatalf("id=%q", id)
	}
}

func TestStampPauseSnapshotID(t *testing.T) {
	t.Parallel()
	stampPauseSnapshotID(nil, "snap-abc123def456abc123def456")

	sb := newCubeboxWithStatusForTest("sb-stamp", cubeboxstore.Status{StartedAt: time.Now().UnixNano()})
	stampPauseSnapshotID(sb, "snap-abc123def456abc123def456")
	if sb.Labels[constants.MasterAnnotationPauseSnapshotID] != "snap-abc123def456abc123def456" {
		t.Fatalf("labels=%v", sb.Labels)
	}
	if sb.Annotations[constants.MasterAnnotationPauseSnapshotID] != "snap-abc123def456abc123def456" {
		t.Fatalf("annotations=%v", sb.Annotations)
	}
	if got := stampedPauseSnapshotID(sb); got != "snap-abc123def456abc123def456" {
		t.Fatalf("stampedPauseSnapshotID=%q", got)
	}
}

func TestReplacedLivePauseSnapshotID(t *testing.T) {
	t.Parallel()
	if got := replacedLivePauseSnapshotID("", "snap-b00000000000000000000000001"); got != "" {
		t.Fatalf("first pause has no previous live snap, got %q", got)
	}
	if got := replacedLivePauseSnapshotID("snap-a00000000000000000000000001", "snap-a00000000000000000000000001"); got != "" {
		t.Fatalf("same id is not a replacement, got %q", got)
	}
	if got := replacedLivePauseSnapshotID("snap-a00000000000000000000000001", "snap-b00000000000000000000000001"); got != "snap-a00000000000000000000000001" {
		t.Fatalf("resume leftover should GC after next pause, got %q", got)
	}
}

func TestPauseSnapshotIDForGCCatalogMissUsesPauseLabel(t *testing.T) {
	t.Parallel()
	snapID := "snap-deadbeefdeadbeefdeadbeef"
	sb := newCubeboxWithStatusForTest("sb-unknown", cubeboxstore.Status{Unknown: true})
	stampPauseSnapshotID(sb, snapID)
	if got := pauseSnapshotIDForGC(sb); got != snapID {
		t.Fatalf("pauseSnapshotIDForGC=%q want %s (half-finished pause has no catalog)", got, snapID)
	}
}

func TestPauseSnapshotIDForGCIgnoresRuntimeSnapshotWithoutCatalog(t *testing.T) {
	t.Parallel()
	sb := newCubeboxWithStatusForTest("sb-runtime", cubeboxstore.Status{StartedAt: time.Now().UnixNano()})
	sb.AddLabels(map[string]string{
		constants.MasterAnnotationRuntimeSnapshotID: "snap-usercommit00000000000001",
	})
	if got := pauseSnapshotIDForGC(sb); got != "" {
		t.Fatalf("must not GC a runtime/commit snap on catalog miss, got %q", got)
	}
}

func TestPauseSnapIDToGCOnDestroyUnknown(t *testing.T) {
	t.Parallel()
	snapID := "snap-deadbeefdeadbeefdeadbeef"
	sb := newCubeboxWithStatusForTest("sb-gc-unknown", cubeboxstore.Status{Unknown: true})
	stampPauseSnapshotID(sb, snapID)

	if got := pauseSnapIDToGCOnDestroy(&cubebox.DestroyCubeSandboxRequest{SandboxID: sb.ID}, sb); got != snapID {
		t.Fatalf("unknown delete should GC half-finished pause snap, got %q", got)
	}

	keep := &cubebox.DestroyCubeSandboxRequest{
		SandboxID: sb.ID,
		Annotations: map[string]string{
			constants.AnnotationPauseKeepTombstone: "true",
		},
	}
	if got := pauseSnapIDToGCOnDestroy(keep, sb); got != "" {
		t.Fatalf("keep_tombstone must not GC, got %q", got)
	}

	tomb := &cubebox.DestroyCubeSandboxRequest{
		SandboxID: sb.ID,
		Annotations: map[string]string{
			constants.AnnotationPauseDeleteTombstone: "true",
		},
	}
	if got := pauseSnapIDToGCOnDestroy(tomb, sb); got != "" {
		t.Fatalf("delete_tombstone must not GC, got %q", got)
	}
}

func TestPauseSnapIDToGCOnDestroySkipsPaused(t *testing.T) {
	t.Parallel()
	sb := newCubeboxWithStatusForTest("sb-gc-paused", cubeboxstore.Status{
		PausedAt: time.Now().UnixNano(),
	})
	stampPauseSnapshotID(sb, "snap-paused000000000000000001")
	if got := pauseSnapIDToGCOnDestroy(&cubebox.DestroyCubeSandboxRequest{SandboxID: sb.ID}, sb); got != "" {
		t.Fatalf("PAUSED destroy without tombstone flags must not GC, got %q", got)
	}
}

func TestPauseSnapIDToGCOnDestroyPausingAndRunning(t *testing.T) {
	t.Parallel()
	snapID := "snap-pausing00000000000000001"
	pausing := newCubeboxWithStatusForTest("sb-gc-pausing", cubeboxstore.Status{
		PausingAt: time.Now().UnixNano(),
	})
	stampPauseSnapshotID(pausing, snapID)
	if got := pauseSnapIDToGCOnDestroy(&cubebox.DestroyCubeSandboxRequest{SandboxID: pausing.ID}, pausing); got != snapID {
		t.Fatalf("PAUSING delete should GC in-progress pause snap, got %q", got)
	}

	running := newCubeboxWithStatusForTest("sb-gc-running", cubeboxstore.Status{
		StartedAt: time.Now().UnixNano(),
	})
	stampPauseSnapshotID(running, "snap-leftover0000000000000001")
	if got := pauseSnapIDToGCOnDestroy(&cubebox.DestroyCubeSandboxRequest{SandboxID: running.ID}, running); got != "snap-leftover0000000000000001" {
		t.Fatalf("RUNNING leftover pause snap should still GC, got %q", got)
	}
}

func TestPauseCatalogBackendPrefersAnnotationThenLabel(t *testing.T) {
	t.Parallel()
	sb := newCubeboxWithStatusForTest("sb-be", cubeboxstore.Status{})
	if got := pauseCatalogBackend(sb); got != "" {
		t.Fatalf("empty backend=%q", got)
	}
	sb.AddLabels(map[string]string{constants.MasterAnnotationStorageBackend: "s3"})
	if got := pauseCatalogBackend(sb); got != "s3" {
		t.Fatalf("label backend=%q", got)
	}
	sb.AddAnnotations(map[string]string{constants.MasterAnnotationStorageBackend: "xfs"})
	if got := pauseCatalogBackend(sb); got != "xfs" {
		t.Fatalf("annotation should win, got %q", got)
	}
}
