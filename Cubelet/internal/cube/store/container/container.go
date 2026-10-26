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
	"sync"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/errdefs"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/store"
	"github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/store/label"
	"github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/store/stats"
	"github.com/tencentcloud/CubeSandbox/Cubelet/internal/truncindex"
)

type Container struct {
	Metadata

	Status StatusStorage

	Container containerd.Container

	*store.StopCh

	IsStopSignaledWithTimeout *uint32

	Stats *stats.ContainerStats
}

type Opts func(*Container) error

func WithContainer(cntr containerd.Container) Opts {
	return func(c *Container) error {
		c.Container = cntr
		return nil
	}
}

func WithStatus(status Status, root string) Opts {
	return func(c *Container) error {
		s, err := StoreStatus(root, c.ID, status)
		if err != nil {
			return err
		}
		c.Status = s
		if s.Get().State() == runtime.ContainerState_CONTAINER_EXITED {
			c.Stop()
		}
		return nil
	}
}

func NewContainer(metadata Metadata, opts ...Opts) (Container, error) {
	c := Container{
		Metadata:                  metadata,
		StopCh:                    store.NewStopCh(),
		IsStopSignaledWithTimeout: new(uint32),
	}
	for _, o := range opts {
		if err := o(&c); err != nil {
			return Container{}, err
		}
	}
	return c, nil
}

func (c *Container) Delete() error {
	return c.Status.Delete()
}

type Store struct {
	lock       sync.RWMutex
	containers map[string]Container
	idIndex    *truncindex.TruncIndex
	labels     *label.Store
}

func NewStore(labels *label.Store) *Store {
	return &Store{
		containers: make(map[string]Container),
		idIndex:    truncindex.NewTruncIndex([]string{}),
		labels:     labels,
	}
}

func (s *Store) Add(c Container) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	if _, ok := s.containers[c.ID]; ok {
		return errdefs.ErrAlreadyExists
	}
	if err := s.labels.Reserve(c.ProcessLabel); err != nil {
		return err
	}
	if err := s.idIndex.Add(c.ID); err != nil {
		return err
	}
	s.containers[c.ID] = c
	return nil
}

func (s *Store) Get(id string) (Container, error) {
	s.lock.RLock()
	defer s.lock.RUnlock()
	id, err := s.idIndex.Get(id)
	if err != nil {
		if err == truncindex.ErrNotExist {
			err = errdefs.ErrNotFound
		}
		return Container{}, err
	}
	if c, ok := s.containers[id]; ok {
		return c, nil
	}
	return Container{}, errdefs.ErrNotFound
}

func (s *Store) List() []Container {
	s.lock.RLock()
	defer s.lock.RUnlock()
	var containers []Container
	for _, c := range s.containers {
		containers = append(containers, c)
	}
	return containers
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

	if _, ok := s.containers[id]; !ok {
		return errdefs.ErrNotFound
	}

	c := s.containers[id]
	c.Stats = newContainerStats
	s.containers[id] = c
	return nil
}

func (s *Store) Delete(id string) {
	s.lock.Lock()
	defer s.lock.Unlock()
	id, err := s.idIndex.Get(id)
	if err != nil {

		return
	}
	c := s.containers[id]
	s.labels.Release(c.ProcessLabel)
	s.idIndex.Delete(id)
	delete(s.containers, id)
}
