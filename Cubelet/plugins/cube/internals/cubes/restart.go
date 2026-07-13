// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubes

import (
	"context"
	"fmt"
	"strings"
	"time"

	"encoding/json"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
)

func (l *local) RecoverAllCubebox(ctx context.Context, afterRecover func(ctx context.Context, cb *cubeboxstore.CubeBox) error) error {
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
		if err := json.Unmarshal(sandboxBytes, &cb); err != nil {
			log.G(ctx).Errorf("Unmarshal to cubebox %s from meta: %v", id, err)
			continue
		}

		if cb.Namespace == "" {
			cb.Namespace = namespaces.Default
		}
		podCtx := namespaces.WithNamespace(ctx, cb.Namespace)
		podCtx = context.WithValue(podCtx, constants.CubeboxID, cb.ID)

		if err := RecoverPod(podCtx, l.client, cb); err != nil {
			log.G(podCtx).Errorf("failed to recover pod, skip this pod: %v", err)
			continue
		}

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
			log.G(podCtx).Errorf("failed to recover pod, skip this pod: %v", err)
			continue
		}
	}
	return nil
}

func RecoverPod(ctx context.Context, client *containerd.Client, cb *cubeboxstore.CubeBox) (err error) {
	ctx, cancel := context.WithTimeout(ctx, loadContainerTimeout)
	defer func() {
		cancel()
	}()

	if cb.Version == "" {
		cb.Version = cb.GetVersion()
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
				s.FinishedAt = time.Now().UnixNano()
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

	newStatus, err := loadStatus(ctx, cntr, &oldStatus)
	if err != nil {
		log.G(ctx).Errorf("Failed to load container status for %q: %v", cntr.ID(), err)
		if newStatus == nil {
			newStatus = &cubeboxstore.Status{}
		}
		if newStatus.FinishedAt == 0 {
			newStatus.FinishedAt = time.Now().UnixNano()
		}
		newStatus.Unknown = true
	}

	if ctr.SandboxID == "" {
		ctr.SandboxID = cb.ID
	}
	ctr.Status = cubeboxstore.StoreStatus(*newStatus)
	ctr.Container = cntr

	if (cb.FirstContainerName != "" && cb.FirstContainerName == ctr.ID) || cb.ID == ctr.ID {
		ctr.IsPod = true
	}
	return ctr, nil
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
			newStatus.FinishedAt = status.FinishedAt
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

			if _, err := t.Delete(ctx, containerd.WithProcessKill); err != nil && !errdefs.IsNotFound(err) {
				if strings.Contains(err.Error(), "ttrpc: closed") {
					t, err = cntr.Task(ctx, nil)
					if err != nil && !errdefs.IsNotFound(err) {
						return nil, fmt.Errorf("failed to load task: %w", err)
					} else if err == nil {
						if _, err := t.Delete(ctx, containerd.WithProcessKill); err != nil && !errdefs.IsNotFound(err) {
							return nil, fmt.Errorf("failed to delete task: %w", err)
						}
					}
				} else {
					return nil, fmt.Errorf("failed to delete task: %w", err)
				}
			}
			newStatus.FinishedAt = s.ExitTime.UnixNano()
			newStatus.ExitCode = int32(s.ExitStatus)
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
