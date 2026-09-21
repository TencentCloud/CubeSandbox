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
	"sync"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/netns"
	"github.com/containerd/errdefs"

	"github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/store"
	"github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/store/label"
	"github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/store/stats"
	"github.com/tencentcloud/CubeSandbox/Cubelet/internal/truncindex"
)

type Sandbox struct {
	Metadata

	Status StatusStorage

	Container containerd.Container

	Sandboxer string

	NetNS *netns.NetNS

	*store.StopCh

	Stats *stats.ContainerStats

	Endpoint Endpoint
}

type Endpoint struct {
	Address string
	Version uint32
	Pid     uint32
	// PidStartTime pins Pid to one process incarnation (/proc/<pid>/stat
	// field 22). Without it a recycled pid number is indistinguishable from
	// our shim, in either direction.
	PidStartTime uint64
	// ShimSpawned records that a shim process was started for this sandbox.
	// It is set *before* the shim is spawned, so that a crash between spawn
	// and the Pid write still leaves evidence. Without it, "no pid recorded"
	// is ambiguous between "no shim ever ran" and "we lost track of one".
	ShimSpawned bool
}

func (e *Endpoint) IsValid() bool {
	return e.Address != ""
}

func NewSandbox(metadata Metadata, status Status) Sandbox {
	s := Sandbox{
		Metadata: metadata,
		Status:   StoreStatus(status),
		StopCh:   store.NewStopCh(),
	}
	if status.State == StateNotReady {
		s.Stop()
	}
	return s
}

type Store struct {
	lock      sync.RWMutex
	sandboxes map[string]Sandbox
	idIndex   *truncindex.TruncIndex
	labels    *label.Store
}

func NewStore(labels *label.Store) *Store {
	return &Store{
		sandboxes: make(map[string]Sandbox),
		idIndex:   truncindex.NewTruncIndex([]string{}),
		labels:    labels,
	}
}

func (s *Store) Add(sb Sandbox) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	if _, ok := s.sandboxes[sb.ID]; ok {
		return errdefs.ErrAlreadyExists
	}
	if err := s.labels.Reserve(sb.ProcessLabel); err != nil {
		return err
	}
	if err := s.idIndex.Add(sb.ID); err != nil {
		return err
	}
	s.sandboxes[sb.ID] = sb
	return nil
}

func (s *Store) Get(id string) (Sandbox, error) {
	s.lock.RLock()
	defer s.lock.RUnlock()
	id, err := s.idIndex.Get(id)
	if err != nil {
		if err == truncindex.ErrNotExist {
			err = errdefs.ErrNotFound
		}
		return Sandbox{}, err
	}
	if sb, ok := s.sandboxes[id]; ok {
		return sb, nil
	}
	return Sandbox{}, errdefs.ErrNotFound
}

func (s *Store) List() []Sandbox {
	s.lock.RLock()
	defer s.lock.RUnlock()
	var sandboxes []Sandbox
	for _, sb := range s.sandboxes {
		sandboxes = append(sandboxes, sb)
	}
	return sandboxes
}

func (s *Store) UpdateContainerStats(id string, newContainerStats *stats.ContainerStats) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	id, err := s.idIndex.Get(id)
	if err != nil {
		if err == truncindex.ErrNotExist {
			err = errdefs.ErrNotFound
		}
		return err
	}

	if _, ok := s.sandboxes[id]; !ok {
		return errdefs.ErrNotFound
	}

	c := s.sandboxes[id]
	c.Stats = newContainerStats
	s.sandboxes[id] = c
	return nil
}

func (s *Store) Delete(id string) {
	s.lock.Lock()
	defer s.lock.Unlock()
	id, err := s.idIndex.Get(id)
	if err != nil {

		return
	}
	s.labels.Release(s.sandboxes[id].ProcessLabel)
	s.idIndex.Delete(id)
	delete(s.sandboxes, id)
}
