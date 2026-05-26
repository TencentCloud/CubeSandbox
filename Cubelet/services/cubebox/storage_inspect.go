// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"fmt"
	"sort"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
)

func (s *service) InspectStorageVolumes(ctx context.Context, req *cubebox.InspectStorageVolumesRequest) (*cubebox.InspectStorageVolumesResponse, error) {
	rsp := &cubebox.InspectStorageVolumesResponse{
		RequestID: req.GetRequestID(),
		Ret:       &errorcode.Ret{RetCode: errorcode.ErrorCode_Success},
	}
	infos, err := storage.InspectStorageVolumes()
	if err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = fmt.Sprintf("inspect storage volumes failed: %v", err)
		return rsp, nil
	}
	ids := make([]string, 0, len(infos))
	for id := range infos {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		info := infos[id]
		if info == nil {
			continue
		}
		entry := &cubebox.SandboxStorageInfo{
			Namespace: info.Namespace,
			SandboxID: info.SandboxID,
		}
		volNames := make([]string, 0, len(info.Volumes))
		for name := range info.Volumes {
			volNames = append(volNames, name)
		}
		sort.Strings(volNames)
		for _, name := range volNames {
			vol := info.Volumes[name]
			if vol == nil {
				continue
			}
			entry.Volumes = append(entry.Volumes, &cubebox.StorageVolumeInfo{
				Name:       vol.Name,
				FilePath:   vol.FilePath,
				SizeLimit:  uint64(vol.SizeLimit),
				VolumeName: vol.VolumeName,
				Kind:       vol.Kind,
				Gen:        vol.Gen,
			})
		}
		rsp.Sandboxes = append(rsp.Sandboxes, entry)
	}
	return rsp, nil
}

func (s *service) CleanupOrphanStorageFiles(ctx context.Context, req *cubebox.CleanupOrphanStorageFilesRequest) (*cubebox.CleanupOrphanStorageFilesResponse, error) {
	rsp := &cubebox.CleanupOrphanStorageFilesResponse{
		RequestID: req.GetRequestID(),
		Ret:       &errorcode.Ret{RetCode: errorcode.ErrorCode_Success},
	}
	formats := req.GetFormats()
	reports, err := storage.CleanupOrphanStorageFiles(formats, req.GetDryRun())
	if err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = fmt.Sprintf("cleanup orphan storage files failed: %v", err)
		return rsp, nil
	}
	for _, r := range reports {
		entry := &cubebox.StorageOrphanEntry{
			Format:   r.Format,
			FilePath: r.FilePath,
			Removed:  r.Removed,
		}
		if r.Err != nil {
			entry.ErrorMessage = r.Err.Error()
		}
		rsp.Orphans = append(rsp.Orphans, entry)
	}
	return rsp, nil
}
