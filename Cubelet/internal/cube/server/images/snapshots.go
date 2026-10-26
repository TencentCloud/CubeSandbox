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

package images

import (
	"context"
	"fmt"
	"time"

	snapshot "github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"

	snapshotstore "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/store/snapshot"
	ctrdutil "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/util"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
)

type snapshotsSyncer struct {
	store        *snapshotstore.Store
	snapshotters map[string]snapshot.Snapshotter
	syncPeriod   time.Duration
}

func newSnapshotsSyncer(store *snapshotstore.Store, snapshotters map[string]snapshot.Snapshotter,
	period time.Duration) *snapshotsSyncer {
	return &snapshotsSyncer{
		store:        store,
		snapshotters: snapshotters,
		syncPeriod:   period,
	}
}

func (s *snapshotsSyncer) start() {
	tick := time.NewTicker(s.syncPeriod)
	go func() {
		defer tick.Stop()

		for {
			if err := s.sync(); err != nil {
				log.L.WithError(err).Error("Failed to sync snapshot stats")
			}
			<-tick.C
		}
	}()
}

func (s *snapshotsSyncer) sync() error {
	ctx := ctrdutil.NamespacedContext()
	start := time.Now().UnixNano()

	for key, snapshotter := range s.snapshotters {
		var snapshots []snapshot.Info

		if err := snapshotter.Walk(ctx, func(ctx context.Context, info snapshot.Info) error {
			snapshots = append(snapshots, info)
			return nil
		}); err != nil {
			return fmt.Errorf("walk all snapshots for %q failed: %w", key, err)
		}
		for _, info := range snapshots {
			snapshotKey := snapshotstore.Key{
				Key:         info.Name,
				Snapshotter: key,
			}
			sn, err := s.store.Get(snapshotKey)
			if err == nil {

				if sn.Kind == info.Kind && sn.Kind != snapshot.KindActive {
					sn.Timestamp = time.Now().UnixNano()
					s.store.Add(sn)
					continue
				}
			}

			sn = snapshotstore.Snapshot{
				Key: snapshotstore.Key{
					Key:         info.Name,
					Snapshotter: key,
				},
				Kind:      info.Kind,
				Timestamp: time.Now().UnixNano(),
			}
			usage, err := snapshotter.Usage(ctx, info.Name)
			if err != nil {
				if !errdefs.IsNotFound(err) {
					log.L.WithError(err).Errorf("Failed to get usage for snapshot %q", info.Name)
				}
				continue
			}
			sn.Size = uint64(usage.Size)
			sn.Inodes = uint64(usage.Inodes)
			s.store.Add(sn)
		}
	}

	for _, sn := range s.store.List() {
		if sn.Timestamp >= start {
			continue
		}

		s.store.Delete(sn.Key)
	}
	return nil
}

func (s *snapshotsSyncer) cleanup(ctx context.Context, nses []string) error {
	for _, ns := range nses {
		for key, snapshotter := range s.snapshotters {
			var tree = newSnapshotTree()
			nsCtx := namespaces.WithNamespace(ctx, ns)

			if err := snapshotter.Walk(nsCtx, func(ctx context.Context, info snapshot.Info) error {
				tree.add(info)
				return nil
			}); err != nil {
				return fmt.Errorf("walk all snapshots for %q failed: %w", key, err)
			}
			stack := &snapshotStack{}
			printTree(tree, stack)

			for node := stack.pop(); node != nil; node = stack.pop() {
				if err := snapshotter.Remove(nsCtx, node.info.Name); err != nil {
					if !errdefs.IsNotFound(err) {
						continue
					}
					log.L.WithError(err).Errorf("Failed to remove snapshot %s with namespace %s", node.info.Name, ns)
					continue
				}
			}
		}
	}
	log.L.Info("complete to cleanup all snapshots")
	return nil
}

type snapshotTree struct {
	nodes []*snapshotTreeNode
	index map[string]*snapshotTreeNode
}

func newSnapshotTree() *snapshotTree {
	return &snapshotTree{
		index: make(map[string]*snapshotTreeNode),
	}
}

type snapshotTreeNode struct {
	info     snapshot.Info
	children []string
}

func (st *snapshotTree) add(info snapshot.Info) *snapshotTreeNode {
	entry, ok := st.index[info.Name]
	if !ok {
		entry = &snapshotTreeNode{info: info}
		st.nodes = append(st.nodes, entry)
		st.index[info.Name] = entry
	} else {
		entry.info = info
	}

	if info.Parent != "" {
		pn := st.get(info.Parent)
		if pn == nil {

			pn = st.add(snapshot.Info{Name: info.Parent})
		}

		pn.children = append(pn.children, info.Name)
	}
	return entry
}

func (st *snapshotTree) get(name string) *snapshotTreeNode {
	return st.index[name]
}

func printTree(st *snapshotTree, stack *snapshotStack) {
	for _, node := range st.nodes {

		if node.info.Parent == "" {
			printNode(node.info.Name, st, stack)
		}
	}
}

type snapshotStack struct {
	stack []*snapshotTreeNode
}

func (s *snapshotStack) push(node *snapshotTreeNode) {
	s.stack = append(s.stack, node)
}

func (s *snapshotStack) pop() *snapshotTreeNode {
	if len(s.stack) == 0 {
		return nil
	}
	node := s.stack[len(s.stack)-1]
	s.stack = s.stack[:len(s.stack)-1]
	return node
}

func printNode(name string, tree *snapshotTree, stack *snapshotStack) {
	node := tree.index[name]
	stack.push(node)
	for _, child := range node.children {
		printNode(child, tree, stack)
	}
}
