// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package backup

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/containerd/containerd/v2/core/metadata"
	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	bolt "go.etcd.io/bbolt"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	cubelog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

type Config struct {
	BackupPeriod string `toml:"backup_period"`
	RootPath     string `toml:"root_path"`
}

var defaultBackupPeriod = 6 * time.Hour

func init() {
	registry.Register(&plugin.Registration{
		Type:   constants.InternalPlugin,
		ID:     constants.BackupID.ID(),
		Config: &Config{},
		Requires: []plugin.Type{
			plugins.MetadataPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			config := ic.Config.(*Config)
			if config.RootPath == "" {
				config.RootPath = ic.Properties[plugins.PropertyRootDir]
			}

			backupPeriod, err := time.ParseDuration(config.BackupPeriod)
			if err != nil || backupPeriod == 0 {
				backupPeriod = defaultBackupPeriod
			}

			m, err := ic.GetSingle(plugins.MetadataPlugin)
			if err != nil {
				return nil, err
			}
			md := m.(*metadata.DB)

			l := &Local{
				BackupPeriod: backupPeriod,
				triggerCh:    make(chan struct{}),
			}

			l.jobs = append(l.jobs, BackupFilePair{
				Locker: func(f func()) {
					_ = md.Update(func(tx *bolt.Tx) error {
						tx.DB().Sync()
						f()
						return nil
					})
				},

				Source: "/data/cubelet/state/io.containerd.metadata.v1.bolt/meta.db",

				TargetDir: filepath.Join(config.RootPath,
					fmt.Sprintf("%v.%v", plugins.MetadataPlugin, "bolt")),
			})

			for _, p := range l.jobs {
				if err := os.MkdirAll(path.Clean(p.TargetDir), os.ModeDir|0755); err != nil && !os.IsExist(err) {
					return nil, err
				}
			}

			rt := &cubelog.RequestTrace{
				Action: "Backup",
				Caller: constants.BackupID.ID(),
			}
			ctx := cubelog.WithRequestTrace(ic.Context, rt)
			ctx = log.ReNewLogger(ctx)
			go l.Run(ctx)

			return l, nil
		},
	})
}
