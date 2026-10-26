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

package mount

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	bolt "go.etcd.io/bbolt"

	"github.com/containerd/log"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"

	"github.com/containerd/containerd/v2/core/metadata"
	"github.com/containerd/containerd/v2/core/metadata/boltutil"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/mount/manager"
	"github.com/containerd/containerd/v2/plugins"
)

func init() {
	registry.Register(&plugin.Registration{
		Type: plugins.MountManagerPlugin,
		ID:   "bolt",
		Requires: []plugin.Type{
			plugins.MountHandlerPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			hp, err := ic.GetByType(plugins.MountHandlerPlugin)
			if err != nil && !errors.Is(err, plugin.ErrPluginNotFound) {
				return nil, err
			}
			var opts []manager.Opt
			for k, v := range hp {
				opts = append(opts, manager.WithMountHandler(k, v.(mount.Handler)))
			}

			root := ic.Properties[plugins.PropertyStateDir]

			targets := filepath.Join(root, "t")
			if merr := os.MkdirAll(targets, 0700); merr != nil {
				return nil, merr
			}

			opts = append(opts, manager.WithAllowedRoot(filepath.Dir(ic.Properties[plugins.PropertyRootDir])))

			metadb := filepath.Join(root, "mounts.db")

			db, err := bolt.Open(metadb, 0600, nil)
			if err != nil {
				return nil, fmt.Errorf("failed to open database file: %w", err)
			}

			mm, err := manager.NewManager(db, targets, opts...)
			if err != nil {
				db.Close()
				return nil, fmt.Errorf("failed to create mount manager: %w", err)
			}

			if sync, ok := mm.(interface{ Sync(context.Context) error }); ok {

				tx, err := db.Begin(true)
				if err != nil {
					db.Close()
					return nil, fmt.Errorf("failed to open database for write: %w", err)
				}
				ctx := boltutil.WithTransaction(ic.Context, tx)

				ready := ic.RegisterReadiness()
				go func() {
					defer ready()
					if err := sync.Sync(ctx); err == nil {
						tx.Commit()
					} else {
						log.G(ctx).WithError(err).Errorf("failed to sync mounts")
						tx.Rollback()
					}
				}()
			}

			return mm, nil
		},
	})

	registry.Register(&plugin.Registration{
		Type: constants.CubeServicePlugin,
		ID:   "mount-gc-register",
		Requires: []plugin.Type{
			plugins.MountManagerPlugin,
			plugins.MetadataPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			md, error := ic.GetSingle(plugins.MetadataPlugin)
			if error != nil {
				return nil, error
			}

			mm, error := ic.GetSingle(plugins.MountManagerPlugin)
			if error != nil {
				return nil, error
			}
			collector, ok := mm.(metadata.Collector)
			if !ok {
				return nil, fmt.Errorf("mount manager must implement metadata.Collector")
			}

			md.(*metadata.DB).RegisterCollectibleResource(metadata.ResourceMount, collector)
			return nil, nil
		},
	})
}
