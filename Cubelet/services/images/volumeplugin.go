// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package images

import (
	"fmt"
	"log"
	"os"
	"path"
	"time"

	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"

	"github.com/tencentcloud/CubeSandbox/Cubelet/internal/tomlext"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/volumefile"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

type VolumeConfig struct {
	RootPath string `toml:"root_path"`

	DataPath                       string `toml:"data_path"`
	CosProtocol                    string `toml:"cos_protocol"`
	UserCodeDownloadRetryNum       int    `toml:"user_code_download_retry_num"`
	UserCodeDownloadRetrySleepTime string `toml:"user_code_download_retry_sleeptime"`
	userCodeDownloadRetrySleepTime time.Duration
	CodeSliceSize                  int64 `toml:"code_slice_size"`
	CodeSliceMax                   int   `toml:"code_slice_max"`

	AsyncBufferCap           int              `toml:"async_buffer_cap"`
	FlushIntervalInSecond    int              `toml:"flush_interval_in_second"`
	CleanupIntervalInSecond  int              `toml:"cleanup_interval_in_second"`
	CheckLocalVolumeInterval tomlext.Duration `toml:"check_local_volume_interval"`
	CheckDiffFromDbInterval  tomlext.Duration `toml:"check_diff_from_db_interval"`

	MaxCleanNum int `toml:"max_clean_num"`

	ExpiredInSecond int64 `toml:"expired_in_second"`

	LeastActiveInSecond int64 `toml:"least_active_in_second"`

	ExpiredExceptionInSec          int64 `toml:"expired_exception_in_sec"`
	NotInDbToDeleteExpiredInSecond int64 `toml:"not_in_db_to_delete_expired_in_second"`
	AsyncCleanCap                  int   `toml:"async_clean_cap"`

	DisableClean bool `toml:"disable_clean"`

	FreeBlocksThreshold int32 `toml:"free_blocks_threshold"`

	MaxFreeBlocksThreshold int32 `toml:"max_free_blocks_threshold"`

	FreeInodesThreshold    int32 `toml:"free_inodes_threshold"`
	MaxFreeInodesThreshold int32 `toml:"max_free_inodes_threshold"`
}

var (
	defaultBaseDir = "/data/cubelet/root/volume"
)

func init() {
	registry.Register(&plugin.Registration{
		Type:   constants.InternalPlugin,
		ID:     constants.VolumeSourceID.ID(),
		Config: &VolumeConfig{},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			l := &volumeLocal{}
			l.config = ic.Config.(*VolumeConfig)
			if l.config.RootPath == "" {
				l.config.RootPath = ic.Properties[plugins.PropertyStateDir]
			}

			if l.config.DataPath == "" {
				l.config.DataPath = defaultBaseDir
			}

			if l.config.CosProtocol == "" {
				l.config.CosProtocol = constants.DefaultCosProtocol
			}

			t, err := time.ParseDuration(l.config.UserCodeDownloadRetrySleepTime)
			if err != nil || t == 0 {
				l.config.userCodeDownloadRetrySleepTime = constants.DefaultUserCodeDownloadRetrySleepTime
			} else {
				l.config.userCodeDownloadRetrySleepTime = t
			}

			if l.config.UserCodeDownloadRetryNum == 0 {
				l.config.UserCodeDownloadRetryNum = constants.DefaultUserCodeDownloadRetryNum
			}

			if l.config.CodeSliceSize == 0 {
				l.config.CodeSliceSize = constants.DefaultCodeSliceSize
			}

			if l.config.CodeSliceMax == 0 {
				l.config.CodeSliceMax = constants.DefaultCodeSliceMax
			}

			if l.config.AsyncBufferCap == 0 {
				l.config.AsyncBufferCap = 100
			}

			if l.config.FlushIntervalInSecond == 0 {
				l.config.FlushIntervalInSecond = 10
			}

			if l.config.CleanupIntervalInSecond == 0 {
				l.config.CleanupIntervalInSecond = 200
			}

			if l.config.MaxCleanNum == 0 {
				l.config.MaxCleanNum = 8
			}

			if l.config.CheckLocalVolumeInterval == 0 {
				l.config.CheckLocalVolumeInterval = tomlext.FromStdTime(time.Hour)
			}
			if l.config.CheckDiffFromDbInterval == 0 {
				l.config.CheckDiffFromDbInterval = tomlext.FromStdTime(25 * time.Hour)
			}
			if l.config.ExpiredInSecond == 0 {
				l.config.ExpiredInSecond = 3 * 86400
			}
			if l.config.LeastActiveInSecond == 0 {
				l.config.LeastActiveInSecond = 120
			}

			if l.config.ExpiredExceptionInSec == 0 {
				l.config.ExpiredExceptionInSec = 7 * 86400
			}
			if l.config.ExpiredInSecond > l.config.ExpiredExceptionInSec {
				l.config.ExpiredInSecond = l.config.ExpiredExceptionInSec
			}

			if l.config.NotInDbToDeleteExpiredInSecond == 0 {
				l.config.NotInDbToDeleteExpiredInSecond = l.config.ExpiredInSecond
			}
			if l.config.FreeBlocksThreshold == 0 {
				l.config.FreeBlocksThreshold = 15
			}

			if l.config.MaxFreeBlocksThreshold == 0 {
				l.config.MaxFreeBlocksThreshold = 25
			}

			if l.config.MaxFreeInodesThreshold == 0 {
				l.config.MaxFreeInodesThreshold = 25
			}
			if l.config.FreeInodesThreshold == 0 {
				l.config.FreeInodesThreshold = 15
			}

			if l.config.AsyncCleanCap == 0 {
				l.config.AsyncCleanCap = 20000
			}
			CubeLog.Debugf("%v init config:%+v",
				fmt.Sprintf("%v.%v", constants.InternalPlugin, constants.VolumeSourceID), l.config)

			for _, d := range []string{l.volumeDBDir()} {
				if err := os.MkdirAll(path.Clean(d), os.ModeDir|0755); err != nil {
					return nil, fmt.Errorf("%v  MkdirAll failed:%v", d, err.Error())
				}
			}
			for _, ft := range []volumefile.FileType{volumefile.FtCode, volumefile.FtLayer,
				volumefile.FtLang, volumefile.FtLang, volumefile.FtLangExt4} {
				bvd := l.baseVolumeDownloadDir(ft)
				if err := os.MkdirAll(path.Clean(bvd), os.ModeDir|0755); err != nil {
					return nil, fmt.Errorf("%v  MkdirAll failed:%v", bvd, err.Error())
				}
				vd := l.baseVolumeDir(ft)
				if err := os.MkdirAll(path.Clean(vd), os.ModeDir|0755); err != nil {
					return nil, fmt.Errorf("%v  MkdirAll failed:%v", vd, err.Error())
				}
			}

			if err := l.init(ic.Context); err != nil {
				log.Fatalf("plugin %s init fail:%v", constants.VolumeSourceID, err.Error())
			}
			return l, nil
		},
	})
}
