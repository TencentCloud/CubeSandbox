// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/pkgs/snapshotmode"
)

const (
	shimSnapshotCaptureAction = "SnapshotCapture"
	shimSnapshotResumeAction  = "SnapshotResume"
	shimSnapshotCaptureConfig = "cube.shimapi.update.snapshot.capture_config"
	shimSnapshotIDAnnotation  = "cube.shimapi.update.snapshot.id"
)

var errSnapshotShimIncompatible = errors.New("incompatible CubeShim: SnapshotCapture is unavailable; upgrade the sandbox shim to create a consistent memory/rootfs snapshot")

// errFilesystemOnlyNeedsCoordinatedShim is what the legacy capture path returns
// for a filesystem-only request: that path always writes guest memory, so it
// can never stand in for a capture that must not produce memory.
var errFilesystemOnlyNeedsCoordinatedShim = errors.New("filesystem-only snapshot requires the coordinated shim path")

// errFilesystemOnlyCapturedMemory reports a filesystem-only capture whose
// artifact contains guest memory; the artifact, not a version number, proves
// which capture path ran.
var errFilesystemOnlyCapturedMemory = errors.New("shim captured guest memory for a filesystem-only request")

// snapshotVMMStateFile is the file a memory capture always writes next to the
// VM config (hypervisor SnapshotConfig: "state.json" plus the memory ranges).
// A filesystem-only capture writes only "config.json", so its presence is
// positive evidence that guest memory was dumped.
const snapshotVMMStateFile = "state.json"

// capturedGuestMemory reports whether the shim wrote a VM state file for this
// capture, and separately whether the capture directory could be inspected at
// all. See snapshotVMMStateFile.
//
// A leftover file from an earlier attempt cannot produce a false positive:
// each attempt starts with a cleared work directory and a create_new config
// write, so a re-run into a dirty directory fails outright.
func capturedGuestMemory(specDir string) (bool, error) {
	if strings.TrimSpace(specDir) == "" {
		return false, nil
	}
	_, err := os.Stat(filepath.Join(specDir, "snapshot", snapshotVMMStateFile))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		// The package cannot be inspected, which says nothing about whether
		// the shim supports filesystem-only captures.
		return false, fmt.Errorf("inspect snapshot capture %s: %w", specDir, err)
	}
}

func snapshotCaptureError(err error) error {
	if shimSnapshotUnsupported(err) {
		return fmt.Errorf("%w: %v", errSnapshotShimIncompatible, err)
	}
	return err
}

type frozenSnapshotConfig struct {
	SnapshotID     string `json:"snapshot_id"`
	DestinationURL string `json:"destination_url"`
	MemoryVolURL   string `json:"memory_vol_url,omitempty"`
	SnapshotType   string `json:"snapshot_type"`
	RenewOnly      bool   `json:"renew_only,omitempty"`
	// SnapshotMode is only set for filesystem-only captures; full captures
	// keep the historical payload shape.
	SnapshotMode string `json:"snapshot_mode,omitempty"`
	// RootfsHostPath is the writable rootfs disk path; the shim resolves it
	// to the guest mount point.
	RootfsHostPath string `json:"rootfs_host_path,omitempty"`
}

func (s *service) updateSnapshotShim(ctx context.Context, cb *cubeboxstore.CubeBox, annotations map[string]string) error {
	if cb == nil || cb.FirstContainer() == nil || cb.FirstContainer().Container == nil {
		return fmt.Errorf("sandbox task is unavailable")
	}
	ns := cb.Namespace
	if ns == "" {
		ns = namespaces.Default
	}
	ctx = namespaces.WithNamespace(ctx, ns)
	task, err := cb.FirstContainer().Container.Task(ctx, nil)
	if err != nil {
		return fmt.Errorf("load snapshot shim task: %w", err)
	}
	if err := task.Update(ctx, containerd.WithAnnotations(annotations)); err != nil {
		return fmt.Errorf("update snapshot shim: %w", err)
	}
	return nil
}

// buildFrozenSnapshotConfig renders the SnapshotCapture payload. A
// filesystem-only capture must not name a memory volume at all.
func buildFrozenSnapshotConfig(
	snapshotID, destination, memoryDevPath, snapshotType, rootfsHostPath string,
	mode snapshotmode.Mode,
) ([]byte, error) {
	cfg := frozenSnapshotConfig{
		SnapshotID: snapshotID, DestinationURL: destination,
		SnapshotType: normalizeSnapshotType(snapshotType),
	}
	if mode.IsFsOnly() {
		cfg.SnapshotMode = mode.String()
		cfg.RootfsHostPath = rootfsHostPath
	} else {
		cfg.MemoryVolURL = snapshotMemoryVolURL(memoryDevPath)
	}
	return json.Marshal(cfg)
}

func (s *service) captureSnapshotWithShim(
	ctx context.Context,
	cb *cubeboxstore.CubeBox,
	snapshotID, path, memoryDevPath, snapshotType, rootfsHostPath string,
	mode snapshotmode.Mode,
) error {
	config, err := buildFrozenSnapshotConfig(snapshotID, path, memoryDevPath, snapshotType, rootfsHostPath, mode)
	if err != nil {
		return err
	}
	err = s.updateSnapshotShim(ctx, cb, map[string]string{
		shimUpdateActionAnnotation: shimSnapshotCaptureAction,
		shimSnapshotCaptureConfig:  string(config),
	})
	if mode.IsFsOnly() {
		// The artifact, not the reply, proves which capture path ran.
		captured, inspectErr := capturedGuestMemory(path)
		switch {
		case inspectErr != nil:
			// Unverifiable artifact: refuse it without blaming the shim.
			return errors.Join(inspectErr, err)
		case captured:
			if err == nil {
				return errFilesystemOnlyCapturedMemory
			}
			return fmt.Errorf("%w: %v", errFilesystemOnlyCapturedMemory, err)
		}
	}
	return err
}

func (s *service) resumeSnapshotWithShim(ctx context.Context, cb *cubeboxstore.CubeBox, snapshotID string) error {
	return s.updateSnapshotShim(ctx, cb, map[string]string{
		shimUpdateActionAnnotation: shimSnapshotResumeAction,
		shimSnapshotIDAnnotation:   snapshotID,
	})
}

type snapshotFreezeLease struct {
	stop chan struct{}
	done chan struct{}
	once sync.Once
	err  error
}

// Stop waits for any in-flight renewal, so its result includes every failure
// that occurred before Cubelet asks the shim to resume the VM.
func (lease *snapshotFreezeLease) Stop() error {
	lease.once.Do(func() { close(lease.stop) })
	<-lease.done
	return lease.err
}

func newSnapshotFreezeLease(interval time.Duration, renew func() error, invalidate func()) *snapshotFreezeLease {
	lease := &snapshotFreezeLease{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(lease.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-lease.stop:
				return
			case <-ticker.C:
				if err := renew(); err != nil {
					lease.err = fmt.Errorf("snapshot freeze renewal failed: %w", err)
					invalidate()
					return
				}
			}
		}
	}()
	return lease
}

// Keep the shim's recovery lease alive while Cubelet owns the frozen VM.
// A failed renewal invalidates the rootfs work, even if the shim later resumes
// automatically and the rootfs operation itself reports success.
func (s *service) startSnapshotLeaseRenewal(ctx context.Context, cb *cubeboxstore.CubeBox, snapshotID string, invalidate context.CancelFunc) *snapshotFreezeLease {
	config, _ := json.Marshal(frozenSnapshotConfig{SnapshotID: snapshotID, RenewOnly: true})
	return newSnapshotFreezeLease(10*time.Second, func() error {
		renewCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		err := s.updateSnapshotShim(renewCtx, cb, map[string]string{
			shimUpdateActionAnnotation: shimSnapshotCaptureAction,
			shimSnapshotCaptureConfig:  string(config),
		})
		if err != nil {
			log.G(ctx).Warnf("failed to renew snapshot freeze for %s: %v", snapshotID, err)
		}
		return err
	}, invalidate)
}

func shimSnapshotUnsupported(err error) bool {
	return err != nil && strings.Contains(err.Error(), "unknown update ext action: "+shimSnapshotCaptureAction)
}

// filesystemOnlyUnsupportedMessage reports the stable, user-facing rejection
// for a sandbox whose shim cannot capture without guest memory, or "" when err
// is not that case. Shim capability is judged by the artifact and error
// identity, never by a version comparison.
func filesystemOnlyUnsupportedMessage(err error, shimVersion string, mode snapshotmode.Mode) string {
	if err == nil || !mode.IsFsOnly() {
		return ""
	}
	if !errors.Is(err, errSnapshotShimIncompatible) &&
		!errors.Is(err, errFilesystemOnlyNeedsCoordinatedShim) &&
		!errors.Is(err, errFilesystemOnlyCapturedMemory) {
		return ""
	}
	return fmt.Sprintf("%s: shim %q does not support filesystem-only snapshots",
		snapshotmode.ErrFilesystemOnlyUnsupported, shimVersion)
}

// filesystemOnlyPrecondition reports a filesystem-only rejection that happened
// before the guest or the disk was touched. These must surface as 409: the
// request is impossible on this sandbox, not a server fault to retry.
func filesystemOnlyPrecondition(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, snapshotmode.ErrFilesystemOnlyFreezeUnsupported) ||
		strings.Contains(message, snapshotmode.ErrFilesystemOnlyRootfsUnsupported)
}
