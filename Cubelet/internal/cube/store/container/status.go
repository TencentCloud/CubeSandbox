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

package container

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/containerd/continuity"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

const statusVersion = "v1"

type versionedStatus struct {
	Version string
	Status
}

type Status struct {
	Pid uint32

	CreatedAt int64

	StartedAt int64

	FinishedAt int64

	ExitCode int32

	Reason string

	Message string

	Starting bool `json:"-"`

	Removing bool `json:"-"`

	Unknown bool `json:"-"`

	Resources *runtime.ContainerResources
}

func (s Status) State() runtime.ContainerState {
	if s.Unknown {
		return runtime.ContainerState_CONTAINER_UNKNOWN
	}
	if s.FinishedAt != 0 {
		return runtime.ContainerState_CONTAINER_EXITED
	}
	if s.StartedAt != 0 {
		return runtime.ContainerState_CONTAINER_RUNNING
	}
	if s.CreatedAt != 0 {
		return runtime.ContainerState_CONTAINER_CREATED
	}
	return runtime.ContainerState_CONTAINER_UNKNOWN
}

func (s *Status) encode() ([]byte, error) {
	return json.Marshal(&versionedStatus{
		Version: statusVersion,
		Status:  *s,
	})
}

func (s *Status) decode(data []byte) error {
	versioned := &versionedStatus{}
	if err := json.Unmarshal(data, versioned); err != nil {
		return err
	}

	switch versioned.Version {
	case statusVersion:
		*s = versioned.Status
		return nil
	}
	return errors.New("unsupported version")
}

type UpdateFunc func(Status) (Status, error)

type StatusStorage interface {
	Get() Status

	UpdateSync(UpdateFunc) error

	Update(UpdateFunc) error

	Delete() error
}

func StoreStatus(root, id string, status Status) (StatusStorage, error) {
	data, err := status.encode()
	if err != nil {
		return nil, fmt.Errorf("failed to encode status: %w", err)
	}
	path := filepath.Join(root, "status")
	if err := continuity.AtomicWriteFile(path, data, 0600); err != nil {
		return nil, fmt.Errorf("failed to checkpoint status to %q: %w", path, err)
	}
	return &statusStorage{
		path:   path,
		status: status,
	}, nil
}

func LoadStatus(root, id string) (Status, error) {
	path := filepath.Join(root, "status")
	data, err := os.ReadFile(path)
	if err != nil {
		return Status{}, fmt.Errorf("failed to read status from %q: %w", path, err)
	}
	var status Status
	if err := status.decode(data); err != nil {
		return Status{}, fmt.Errorf("failed to decode status %q: %w", data, err)
	}
	return status, nil
}

type statusStorage struct {
	sync.RWMutex
	path   string
	status Status
}

func (s *statusStorage) Get() Status {
	s.RLock()
	defer s.RUnlock()

	return deepCopyOf(s.status)
}

func deepCopyOf(s Status) Status {
	copy := s

	if s.Resources == nil {
		return copy
	}
	copy.Resources = &runtime.ContainerResources{}
	if s.Resources != nil && s.Resources.Linux != nil {
		hugepageLimits := make([]*runtime.HugepageLimit, 0, len(s.Resources.Linux.HugepageLimits))
		for _, l := range s.Resources.Linux.HugepageLimits {
			if l != nil {
				hugepageLimits = append(hugepageLimits, &runtime.HugepageLimit{
					PageSize: l.PageSize,
					Limit:    l.Limit,
				})
			}
		}
		copy.Resources = &runtime.ContainerResources{
			Linux: &runtime.LinuxContainerResources{
				CpuPeriod:              s.Resources.Linux.CpuPeriod,
				CpuQuota:               s.Resources.Linux.CpuQuota,
				CpuShares:              s.Resources.Linux.CpuShares,
				CpusetCpus:             s.Resources.Linux.CpusetCpus,
				CpusetMems:             s.Resources.Linux.CpusetMems,
				MemoryLimitInBytes:     s.Resources.Linux.MemoryLimitInBytes,
				MemorySwapLimitInBytes: s.Resources.Linux.MemorySwapLimitInBytes,
				OomScoreAdj:            s.Resources.Linux.OomScoreAdj,
				Unified:                s.Resources.Linux.Unified,
				HugepageLimits:         hugepageLimits,
			},
		}
	}

	if s.Resources != nil && s.Resources.Windows != nil {
		copy.Resources = &runtime.ContainerResources{
			Windows: &runtime.WindowsContainerResources{
				CpuShares:          s.Resources.Windows.CpuShares,
				CpuCount:           s.Resources.Windows.CpuCount,
				CpuMaximum:         s.Resources.Windows.CpuMaximum,
				MemoryLimitInBytes: s.Resources.Windows.MemoryLimitInBytes,
				RootfsSizeInBytes:  s.Resources.Windows.RootfsSizeInBytes,
			},
		}
	}
	return copy
}

func (s *statusStorage) UpdateSync(u UpdateFunc) error {
	s.Lock()
	defer s.Unlock()
	newStatus, err := u(s.status)
	if err != nil {
		return err
	}
	data, err := newStatus.encode()
	if err != nil {
		return fmt.Errorf("failed to encode status: %w", err)
	}
	if err := continuity.AtomicWriteFile(s.path, data, 0600); err != nil {
		return fmt.Errorf("failed to checkpoint status to %q: %w", s.path, err)
	}
	s.status = newStatus
	return nil
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

func (s *statusStorage) Delete() error {
	temp := filepath.Dir(s.path) + ".del-" + filepath.Base(s.path)
	if err := os.Rename(s.path, temp); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.RemoveAll(temp)
}
