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

package snapshot

import (
	"sync"

	snapshot "github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/errdefs"
)

type Key struct {
	Key string

	Snapshotter string
}

type Snapshot struct {
	Key Key

	Kind snapshot.Kind

	Size uint64

	Inodes uint64

	Timestamp int64
}

type Store struct {
	lock      sync.RWMutex
	snapshots map[Key]Snapshot
}

func NewStore() *Store {
	return &Store{snapshots: make(map[Key]Snapshot)}
}

func (s *Store) Add(snapshot Snapshot) {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.snapshots[snapshot.Key] = snapshot
}

func (s *Store) Get(key Key) (Snapshot, error) {
	s.lock.RLock()
	defer s.lock.RUnlock()
	if sn, ok := s.snapshots[key]; ok {
		return sn, nil
	}
	return Snapshot{}, errdefs.ErrNotFound
}

func (s *Store) List() []Snapshot {
	s.lock.RLock()
	defer s.lock.RUnlock()
	var snapshots []Snapshot
	for _, sn := range s.snapshots {
		snapshots = append(snapshots, sn)
	}
	return snapshots
}

func (s *Store) Delete(key Key) {
	s.lock.Lock()
	defer s.lock.Unlock()
	delete(s.snapshots, key)
}
