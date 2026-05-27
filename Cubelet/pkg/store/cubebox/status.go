// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"sync"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
)

type Status struct {
	Pid uint32

	CreatedAt int64

	StartedAt int64

	FinishedAt int64

	PausedAt  int64
	PausingAt int64

	ExitCode int32

	Reason string

	Message string

	Unknown bool `json:"-"`

	Removing               bool `json:"-"`
	PostStop               bool `json:"-"`
	LifeTimeMetricReported bool `json:"-"`

	// RollingBack signals that a snapshot rollback is in flight against this
	// sandbox. While set, the shim holds its sandbox mutex doing
	// delete_vm + resume_vm_with_config and ttrpc state() may transiently
	// time out or report Unknown. Background scanners (DeadGC) MUST skip
	// the cubebox to avoid stamping Unknown=true / FinishedAt=now into the
	// in-memory Status, which would otherwise make IsTerminated() return
	// true and break a follow-up pause/resume request. Mirrors the
	// IsPaused() skip already in scanDeadContainer. Non-persistent.
	RollingBack bool `json:"-"`
}

func (s Status) State() cubebox.ContainerState {
	if s.Unknown {
		return cubebox.ContainerState_CONTAINER_UNKNOWN
	}
	if s.FinishedAt != 0 {
		return cubebox.ContainerState_CONTAINER_EXITED
	}
	if s.PausingAt != 0 {
		return cubebox.ContainerState_CONTAINER_PAUSING
	}
	if s.PausedAt != 0 {
		return cubebox.ContainerState_CONTAINER_PAUSED
	}
	if s.StartedAt != 0 {
		return cubebox.ContainerState_CONTAINER_RUNNING
	}
	if s.CreatedAt != 0 {
		return cubebox.ContainerState_CONTAINER_CREATED
	}
	return cubebox.ContainerState_CONTAINER_UNKNOWN
}

type UpdateFunc func(Status) (Status, error)

func StoreStatus(status Status) *StatusStorage {
	return &StatusStorage{Status: status}
}

type StatusStorage struct {
	sync.RWMutex `json:"-"`
	Status       Status
}

func (s *StatusStorage) Get() Status {
	s.RLock()
	defer s.RUnlock()
	return s.Status
}

func (s *StatusStorage) Update(u UpdateFunc) error {
	s.Lock()
	defer s.Unlock()
	newStatus, err := u(s.Status)
	if err != nil {
		return err
	}
	s.Status = newStatus
	return nil
}

func (s *StatusStorage) IsTerminated() bool {
	state := s.Get().State()
	return state == cubebox.ContainerState_CONTAINER_UNKNOWN || state == cubebox.ContainerState_CONTAINER_EXITED
}

func (s *StatusStorage) IsPaused() bool {
	state := s.Get().State()
	return state == cubebox.ContainerState_CONTAINER_PAUSING || state == cubebox.ContainerState_CONTAINER_PAUSED
}
