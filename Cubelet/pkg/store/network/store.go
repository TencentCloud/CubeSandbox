// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package network

import (
	"fmt"
	"sync"

	"encoding/json"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/network/proto"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow/provider"
)

const DBBucketNetwork = "network/v1"

type Store struct {
	lock sync.RWMutex

	idToAlloc map[string]NetworkAllocation

	db *utils.CubeStore
}

func NewStore(db *utils.CubeStore) *Store {
	return &Store{
		idToAlloc: make(map[string]NetworkAllocation),
		db:        db,
	}
}

func RecoverFromDB(db *utils.CubeStore) (*Store, error) {
	s := NewStore(db)

	all, err := db.ReadAll(DBBucketNetwork)
	if err != nil {
		return nil, err
	}

	for id, netBytes := range all {
		var net NetworkAllocation
		if err := json.Unmarshal(netBytes, &net); err != nil {
			return nil, err
		}
		var metadata provider.NetworkProvider
		switch net.NetworkType {
		case cubebox.NetworkType_tap.String():
			metadata = &proto.ShimNetReq{}
		default:
			return nil, fmt.Errorf("unknown instance type %s", net.NetworkType)
		}
		metadata.FromPersistMetadata(net.PersistentMetadata)
		net.Metadata = metadata
		s.idToAlloc[id] = net
	}

	return s, nil
}

func (s *Store) Add(net NetworkAllocation) {
	s.lock.Lock()
	defer s.lock.Unlock()
	if net.PersistentMetadata == nil {
		net.PersistentMetadata = net.Metadata.GetPersistMetadata()
	}
	s.idToAlloc[net.SandboxID] = net
}

func (s *Store) Get(id string) (NetworkAllocation, error) {
	s.lock.RLock()
	defer s.lock.RUnlock()
	if c, ok := s.idToAlloc[id]; ok {
		return c, nil
	}
	return NetworkAllocation{}, utils.ErrorKeyNotFound
}

func (s *Store) Delete(id string) {
	s.lock.Lock()
	defer s.lock.Unlock()

	delete(s.idToAlloc, id)
}

func (s *Store) List() []NetworkAllocation {
	s.lock.RLock()
	defer s.lock.RUnlock()
	var allocs []NetworkAllocation
	for _, c := range s.idToAlloc {
		allocs = append(allocs, c)
	}
	return allocs
}

func (s *Store) Len() int {
	s.lock.RLock()
	defer s.lock.RUnlock()
	return len(s.idToAlloc)
}

func (s *Store) Sync(id string) error {
	s.lock.RLock()
	defer s.lock.RUnlock()

	net, ok := s.idToAlloc[id]
	if !ok {
		return utils.ErrorKeyNotFound
	}

	bs, err := json.Marshal(net)
	if err != nil {
		return err
	}

	return s.db.Set(DBBucketNetwork, id, bs)
}

func (s *Store) DeleteSync(id string) error {
	if err := s.db.Delete(DBBucketNetwork, id); err != nil && err != utils.ErrorKeyNotFound &&
		err != utils.ErrorBucketNotFound {
		return err
	}
	s.Delete(id)
	return nil
}
