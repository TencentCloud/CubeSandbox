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
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/store/snapshot"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func (c *CubeImageService) ImageFsInfo(ctx context.Context, r *runtime.ImageFsInfoRequest) (*runtime.ImageFsInfoResponse, error) {
	snapshots := c.snapshotStore.List()
	snapshotterFSInfos := map[string]snapshot.Snapshot{}

	for _, sn := range snapshots {
		if info, ok := snapshotterFSInfos[sn.Key.Snapshotter]; ok {

			if sn.Timestamp < info.Timestamp {
				info.Timestamp = sn.Timestamp
			}
			info.Size += sn.Size
			info.Inodes += sn.Inodes
			snapshotterFSInfos[sn.Key.Snapshotter] = info
		} else {
			snapshotterFSInfos[sn.Key.Snapshotter] = snapshot.Snapshot{
				Timestamp: sn.Timestamp,
				Size:      sn.Size,
				Inodes:    sn.Inodes,
			}
		}
	}

	var imageFilesystems []*runtime.FilesystemUsage

	if info, ok := snapshotterFSInfos[c.config.Snapshotter]; ok {
		imageFilesystems = append(imageFilesystems, &runtime.FilesystemUsage{
			Timestamp:  info.Timestamp,
			FsId:       &runtime.FilesystemIdentifier{Mountpoint: c.imageFSPaths[c.config.Snapshotter]},
			UsedBytes:  &runtime.UInt64Value{Value: info.Size},
			InodesUsed: &runtime.UInt64Value{Value: info.Inodes},
		})
		delete(snapshotterFSInfos, c.config.Snapshotter)
	} else {
		imageFilesystems = append(imageFilesystems, &runtime.FilesystemUsage{
			Timestamp:  time.Now().UnixNano(),
			FsId:       &runtime.FilesystemIdentifier{Mountpoint: c.imageFSPaths[c.config.Snapshotter]},
			UsedBytes:  &runtime.UInt64Value{Value: 0},
			InodesUsed: &runtime.UInt64Value{Value: 0},
		})
	}

	for snapshotter, info := range snapshotterFSInfos {
		imageFilesystems = append(imageFilesystems, &runtime.FilesystemUsage{
			Timestamp:  info.Timestamp,
			FsId:       &runtime.FilesystemIdentifier{Mountpoint: c.imageFSPaths[snapshotter]},
			UsedBytes:  &runtime.UInt64Value{Value: info.Size},
			InodesUsed: &runtime.UInt64Value{Value: info.Inodes},
		})
	}

	return &runtime.ImageFsInfoResponse{ImageFilesystems: imageFilesystems}, nil
}
