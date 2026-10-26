// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package sandbox

import (
	"strconv"
	"sync"
	"time"

	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

type State uint32

const (
	StateReady State = iota

	StateNotReady

	StateUnknown
)

func (s State) String() string {
	switch s {
	case StateReady:
		return runtime.PodSandboxState_SANDBOX_READY.String()
	case StateNotReady:
		return runtime.PodSandboxState_SANDBOX_NOTREADY.String()
	case StateUnknown:

		return "SANDBOX_UNKNOWN"
	default:
		return "invalid sandbox state value: " + strconv.Itoa(int(s))
	}
}

type Status struct {
	Pid uint32

	CreatedAt time.Time

	ExitedAt time.Time

	ExitStatus uint32

	State State
}

type UpdateFunc func(Status) (Status, error)

type StatusStorage interface {
	Get() Status

	Update(UpdateFunc) error
}

func StoreStatus(status Status) StatusStorage {
	return &statusStorage{status: status}
}

type statusStorage struct {
	sync.RWMutex
	status Status
}

func (s *statusStorage) Get() Status {
	s.RLock()
	defer s.RUnlock()
	return s.status
}

func (s *statusStorage) Update(u UpdateFunc) error {
	s.Lock()
	defer s.Unlock()
	newStatus, err := u(s.status)
	if err != nil {
		return err
	}
	s.status = newStatus
	return nil
}
