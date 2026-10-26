// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package multimeta

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sync"

	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/multimetadb/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
)

type cubeboxConfig struct {
	RootPath string `toml:"root_path"`
}

func init() {
	registry.Register(&plugin.Registration{
		Type:     constants.CubeMetaStorePlugin,
		ID:       constants.MultiMetaID.ID(),
		Config:   defaultConfig(),
		Requires: []plugin.Type{},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			l := &multimeta{}
			l.config = ic.Config.(*cubeboxConfig)
			if l.config.RootPath == "" {
				l.config.RootPath = ic.Properties[plugins.PropertyStateDir]
			}

			err := l.initDb()
			if err != nil {
				return nil, err
			}
			return l, nil
		},
	})
}

func defaultConfig() *cubeboxConfig {
	return &cubeboxConfig{}
}

type MetadataDBAPI interface {
	Get(bucket, key string) (result []byte, err error)
	SetWithTx(bucket, key string, value []byte, callback func() error) (err error)
	DeleteWithTx(bucket, key string, callback func() error) (err error)
	ReadAll(bucket string) (all map[string][]byte, err error)
	GetBs(key string, buckets ...[]byte) (result []byte, err error)
	ReadAllBs(buckets ...[]byte) (all map[string][]byte, err error)
	Close() error
}
type multimeta struct {
	config *cubeboxConfig
	*utils.CubeStore

	multimetadb.UnimplementedMultiMetaDBServerServer
}

var _ MetadataDBAPI = &multimeta{}

var (
	dbDir = "db"
)

func (l *multimeta) initDb() error {
	basePath := filepath.Join(l.config.RootPath, dbDir)
	if err := os.MkdirAll(path.Clean(basePath), os.ModeDir|0755); err != nil {
		return fmt.Errorf("init dir %s failed %s", basePath, err.Error())
	}
	var err error
	if l.CubeStore, err = utils.NewCubeStoreExt(basePath, "meta.db", 10, nil); err != nil {
		return err
	}
	return nil
}

type BucketDefineInternal struct {
	*multimetadb.BucketDefine

	CubeStore MetadataDBAPI
}

var (
	lock      sync.RWMutex
	bucketMap map[string]*BucketDefineInternal = make(map[string]*BucketDefineInternal)
)

func RegisterBucket(config *BucketDefineInternal) {
	lock.Lock()
	defer lock.Unlock()
	key := string(config.Name)
	if config.DbName != "" {
		key = config.DbName + "-" + key
	}

	bucketMap[key] = config
}
