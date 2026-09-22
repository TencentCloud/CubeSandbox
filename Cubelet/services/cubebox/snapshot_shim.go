// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
)

const (
	shimSnapshotCaptureAction = "SnapshotCapture"
	shimSnapshotResumeAction  = "SnapshotResume"
	shimSnapshotCaptureConfig = "cube.shimapi.update.snapshot.capture_config"
	shimSnapshotIDAnnotation  = "cube.shimapi.update.snapshot.id"
)

var errSnapshotShimIncompatible = errors.New("incompatible CubeShim: SnapshotCapture is unavailable; upgrade the sandbox shim to create a consistent memory/rootfs snapshot")

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

func (s *service) captureSnapshotWithShim(ctx context.Context, cb *cubeboxstore.CubeBox, snapshotID, path, memoryVol, snapshotType string) error {
	config, err := json.Marshal(frozenSnapshotConfig{
		SnapshotID: snapshotID, DestinationURL: path,
		MemoryVolURL: snapshotMemoryVolURL(memoryVol), SnapshotType: normalizeSnapshotType(snapshotType),
	})
	if err != nil {
		return err
	}
	return s.updateSnapshotShim(ctx, cb, map[string]string{
		shimUpdateActionAnnotation: shimSnapshotCaptureAction,
		shimSnapshotCaptureConfig:  string(config),
	})
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
