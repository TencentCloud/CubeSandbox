// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubes

import (
	"context"
	"fmt"
	"strings"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	jsoniter "github.com/json-iterator/go"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/restartpolicy"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func (l *local) RecoverAllCubebox(ctx context.Context, afterRecover func(ctx context.Context, cb *cubeboxstore.CubeBox) error) error {
	l.cacheRecoveryReady.Store(false)
	complete := true
	start := time.Now()
	defer func() {
		log.G(ctx).Debugf("Recover cubebox cost %v", time.Since(start))
	}()

	all, err := l.db.ReadAll(cubeboxstore.DBBucketSandbox)
	if err != nil {
		return fmt.Errorf("load from cubebox db: %w", err)
	}
	for id, sandboxBytes := range all {
		var cb = new(cubeboxstore.CubeBox)
		if err := jsoniter.Unmarshal(sandboxBytes, &cb); err != nil {
			complete = false
			log.G(ctx).Errorf("Unmarshal to cubebox %s from meta: %v", id, err)
			continue
		}
		cb.ReconcileRecoveredMemoryLineage(legacyRestoreBaseLabel(cb))

		if cb.Namespace == "" {
			cb.Namespace = namespaces.Default
		}
		podCtx := namespaces.WithNamespace(ctx, cb.Namespace)
		podCtx = context.WithValue(podCtx, constants.CubeboxID, cb.ID)

		if err := RecoverPod(podCtx, l.client, cb); err != nil {
			complete = false
			log.G(podCtx).Errorf("failed to recover pod, skip this pod: %v", err)
			continue
		}
		cb.ReconcileRecoveredWritableLayer(podCtx)

		log.G(podCtx).Infof("Loaded sandbox %v", utils.InterfaceToString(&cb))
		if cb.GetStatus() != nil && cb.GetStatus().IsTerminated() {
			pid := cb.GetStatus().Get().Pid
			if pid != 0 {
				if utils.ProcessExists(podCtx, int(pid)) {
					log.G(podCtx).Warnf("sandbox %s is terminating, but process still exists", cb.ID)
				} else {
					log.G(podCtx).Warnf("sandbox %s is terminating, but process not exists, please delete it after check", cb.ID)
				}
			}
		}

		l.cubeboxStore.Add(cb)
		err = afterRecover(podCtx, cb)
		if err != nil {
			complete = false
			log.G(podCtx).Errorf("failed to recover pod, skip this pod: %v", err)
			continue
		}
	}
	l.cacheRecoveryReady.Store(complete)
	return nil
}

// legacyRestoreBaseLabel reads the pre-MemoryLineage restore binding. Labels
// only: create-request annotations are user controllable and were never
// written by Cubelet, so they are not evidence. The row was just unmarshalled
// and is not in the store yet, so reading Labels needs no lock.
func legacyRestoreBaseLabel(cb *cubeboxstore.CubeBox) string {
	if cb == nil {
		return ""
	}
	return cb.Labels[constants.MasterAnnotationRuntimeRestoreSnapshotID]
}

func RecoverPod(ctx context.Context, client *containerd.Client, cb *cubeboxstore.CubeBox) (err error) {
	ctx, cancel := context.WithTimeout(ctx, loadContainerTimeout)
	defer func() {
		cancel()
	}()

	if cb.Version == "" {
		cb.Version = cb.GetVersion()
	}
	// Paused tombstones have no live containerd/shim task; keep store as-is for List.
	if cb.GetStatus() != nil && cb.GetStatus().IsPaused() {
		if cb.SandboxID == "" {
			cb.SandboxID = cb.ID
		}
		return nil
	}

	var mainContainer *cubeboxstore.Container

	for id := range cb.AllContainers() {
		ci, err := cb.ContainersMap.Get(id)
		if err != nil {
			log.G(ctx).Errorf("failed to get container %q of sandbox %q: %v", id, cb.ID, err)
			continue
		}
		if ci.IsPod || cb.FirstContainerName == ci.ID {
			mainContainer = ci
			ci.IsPod = true
		}
		ci, err = RecoverContainer(ctx, client, cb, ci)
		if err != nil {
			return fmt.Errorf("failed to load container %q of sandbox %q: %v", ci.ID, cb.ID, err)
		}
		cb.ContainersMap.AddContainer(ci)
	}

	if mainContainer == nil && cb.GetVersion() == cubeboxstore.CubeboxVersionV1 {
		sandboxContainer := &cubeboxstore.Container{
			Metadata: cb.Metadata,
			IP:       cb.IP,
			IsPod:    true,
		}
		ci, err := RecoverContainer(ctx, client, cb, sandboxContainer)
		if err != nil {
			return fmt.Errorf("failed to load v1 sandbox container %q of sandbox %q: %v", ci.ID, cb.ID, err)
		}
		mainContainer = ci
		cb.ContainersMap.AddContainer(ci)
	}

	if mainContainer == nil {
		return fmt.Errorf("failed to load main container of sandbox %q : %v", cb.ID, err)
	}

	if cb.SandboxID == "" {
		cb.SandboxID = cb.ID
	}

	cb.SandboxID = cb.ID
	return nil
}

func RecoverContainer(ctx context.Context, client *containerd.Client, cb *cubeboxstore.CubeBox, ctr *cubeboxstore.Container) (*cubeboxstore.Container, error) {
	ctx, cancel := context.WithTimeout(ctx, loadContainerTimeout)
	defer cancel()

	if ctr.DeletedTime != nil {
		return ctr, nil
	}

	cntr, err := client.LoadContainer(ctx, ctr.ID)
	if errdefs.IsNotFound(err) {
		if ctr.Status == nil {
			ctr.Status = cubeboxstore.StoreStatus(cubeboxstore.Status{
				Unknown: true,
			})
		}
		ctr.Status.Update(func(s cubeboxstore.Status) (cubeboxstore.Status, error) {
			if s.FinishedAt == 0 {
				// Container vanished without a recorded exit; mark FinishedAt as unobserved.
				s.FinishedAt = time.Now().UnixNano()
				s.ExitUnobserved = true
			}
			return s, nil
		})
		return ctr, nil
	} else if err != nil {
		return ctr, err
	}

	var oldStatus cubeboxstore.Status
	if ctr.Status != nil {
		oldStatus = ctr.Status.Get()
	} else {
		oldStatus = cubeboxstore.Status{
			CreatedAt: ctr.CreatedAt,
		}
	}

	if (cb.FirstContainerName != "" && cb.FirstContainerName == ctr.ID) || cb.ID == ctr.ID {
		ctr.IsPod = true
	}

	newStatus, err := loadStatus(ctx, cntr, &oldStatus)
	switch {
	case err != nil && shimLiveness(cb, ctr.IsPod) == utils.LivenessGone:
		log.G(ctx).Errorf("Failed to load container status for %q and shim pid %d is gone: %v", cntr.ID(), cb.Endpoint.Pid, err)
		newStatus = unobservedExitStatus(oldStatus)
	case err != nil:
		log.G(ctx).Warnf("Failed to load container status for %q, keeping previous status: %v", cntr.ID(), err)
		newStatus = fallbackStatus(oldStatus)
	case newStatus.Unknown && shimLiveness(cb, ctr.IsPod) == utils.LivenessAlive:
		log.G(ctx).Warnf("Task not found for %q, but shim pid %d is still alive; keeping previous status", cntr.ID(), cb.Endpoint.Pid)
		newStatus = fallbackStatus(oldStatus)
	}

	if ctr.SandboxID == "" {
		ctr.SandboxID = cb.ID
	}
	// Prefer the fresh reason derived by loadStatus over the previously stored one.
	if newStatus.Reason == "" {
		newStatus.Reason = oldStatus.Reason
	}
	// Exit reasons belong to a finished run; non-exit reasons such as PauseFailed can remain live.
	if newStatus.FinishedAt == 0 && restartpolicy.IsExitReason(restartpolicy.Reason(newStatus.Reason)) {
		newStatus.Reason = ""
	}
	ctr.Status = cubeboxstore.StoreStatus(*newStatus)
	ctr.Container = cntr
	return ctr, nil
}

func unobservedExitStatus(old cubeboxstore.Status) *cubeboxstore.Status {
	s := old
	if s.FinishedAt == 0 {
		s.FinishedAt = time.Now().UnixNano()
		s.ExitUnobserved = true
	}
	s.Unknown = true
	return &s
}

// fallbackStatus preserves a known prior status when task state cannot be
// refreshed (e.g. ttrpc timeout). UNKNOWN and CREATED (never confirmed started)
// synthesize an unobserved exit so interrupted creates do not stay stuck in
// CREATED.
func fallbackStatus(old cubeboxstore.Status) *cubeboxstore.Status {
	switch old.State() {
	case cubebox.ContainerState_CONTAINER_RUNNING,
		cubebox.ContainerState_CONTAINER_PAUSED,
		cubebox.ContainerState_CONTAINER_PAUSING,
		cubebox.ContainerState_CONTAINER_EXITED:
		s := old
		return &s
	default:
		return unobservedExitStatus(old)
	}
}

// shimLiveness checks the sandbox's recorded pod-shim incarnation via /proc.
// Only pod containers map to cb.Endpoint; non-pod containers and unrecorded
// PIDs return LivenessUnknown. Records without PidStartTime also return
// LivenessUnknown when the PID exists, intentionally avoiding trusting a bare
// PID that may have been recycled after a host reboot.
func shimLiveness(cb *cubeboxstore.CubeBox, isPod bool) utils.Liveness {
	if !isPod || cb.Endpoint.Pid <= 1 {
		return utils.LivenessUnknown
	}
	return utils.ProcessIdentity{
		Pid:       int(cb.Endpoint.Pid),
		StartTime: cb.Endpoint.PidStartTime,
	}.Status()
}

const loadContainerTimeout = 10 * time.Second

func loadStatus(ctx context.Context, cntr containerd.Container, status *cubeboxstore.Status) (*cubeboxstore.Status, error) {
	var newStatus cubeboxstore.Status
	var s containerd.Status
	var notFound bool
	t, err := cntr.Task(ctx, nil)
	if IsNotFoundContainerError(err) {

		notFound = true
	} else if err != nil {
		return nil, fmt.Errorf("failed to load task: %w", err)
	} else {

		s, err = t.Status(ctx)
		if err != nil {

			if !errdefs.IsNotFound(err) {
				return nil, fmt.Errorf("failed to get task status: %w", err)
			}
			notFound = true
		}
	}

	if notFound {

		if status.FinishedAt != 0 {
			// Preserve the already-recorded exit metadata when the task is gone.
			newStatus.FinishedAt = status.FinishedAt
			newStatus.ExitCode = status.ExitCode
			newStatus.ExitUnobserved = status.ExitUnobserved
		} else {
			newStatus.Unknown = true
		}
	} else {
		switch s.Status {
		case containerd.Created:
			newStatus.CreatedAt = status.CreatedAt
		case containerd.Running:
			newStatus.StartedAt = status.StartedAt
			if newStatus.StartedAt == 0 {
				newStatus.StartedAt = time.Now().UnixNano()
			}
			newStatus.Pid = t.Pid()

		case containerd.Stopped:
			deleteStoppedTask(ctx, cntr, t)
			finishedAt := s.ExitTime.UnixNano()
			if finishedAt <= 0 {
				// Backfill zero ExitTime so State() recognizes the container as exited.
				finishedAt = time.Now().UnixNano()
			}
			newStatus.FinishedAt = finishedAt
			newStatus.ExitCode = int32(s.ExitStatus)
			// Derive exit reason from the stopped task, preserving same-run OOMKilled.
			if status.Reason == string(restartpolicy.ReasonOOMKilled) &&
				status.FinishedAt != 0 && status.FinishedAt >= status.StartedAt {
				newStatus.Reason = string(restartpolicy.ReasonOOMKilled)
			} else {
				newStatus.Reason = string(restartpolicy.ReasonForExit(newStatus.ExitCode))
			}
		case containerd.Paused:
			if status.PausedAt != 0 {
				newStatus.PausedAt = status.PausedAt
			} else {
				newStatus.PausedAt = time.Now().UnixNano()
			}
		case containerd.Pausing:
			if status.PausingAt != 0 {
				newStatus.PausingAt = status.PausingAt
			} else {
				newStatus.PausingAt = time.Now().UnixNano()
			}
		default:
			return nil, fmt.Errorf("unexpected task status %q", s.Status)
		}
	}

	return &newStatus, nil
}

// deleteStoppedTask cleans up an already-stopped task on a best-effort basis
// so a transient delete error does not discard the observed exit metadata.
func deleteStoppedTask(ctx context.Context, cntr containerd.Container, t containerd.Task) {
	_, err := t.Delete(ctx, containerd.WithProcessKill)
	if err == nil || errdefs.IsNotFound(err) {
		return
	}
	if strings.Contains(err.Error(), "ttrpc: closed") {
		t, err = cntr.Task(ctx, nil)
		if err != nil {
			if !errdefs.IsNotFound(err) {
				log.G(ctx).Warnf("Failed to reload stopped task %q for cleanup: %v", cntr.ID(), err)
			}
			return
		}
		_, err = t.Delete(ctx, containerd.WithProcessKill)
		if err == nil || errdefs.IsNotFound(err) {
			return
		}
	}
	log.G(ctx).Warnf("Failed to delete stopped task %q: %v", cntr.ID(), err)
}
