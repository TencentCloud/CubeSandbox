// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package images

import (
	"context"
	"fmt"
	"strings"
	"time"

	"encoding/json"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/images/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/volumefile"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

func (s *service) VolumeUtils(ctx context.Context, req *images.VolumUtilsRequest) (*images.VolumUtilsResponse, error) {
	rt := &CubeLog.RequestTrace{
		Action:       "VolumeUtils",
		RequestID:    req.RequestID,
		Caller:       constants.ImagesServiceID.ID(),
		Callee:       constants.ImagesServiceID.ID(),
		CalleeAction: req.Cmd,
	}

	ctx = CubeLog.WithRequestTrace(ctx, rt)
	log.G(ctx).Errorf("VolumeUtils:%s", utils.InterfaceToString(req))
	switch req.Cmd {
	case "resetVolumeRef":
		return s.doResetVolumeRef(ctx, req)
	case "resetVolumeRefExec":
		return s.doResetVolumeRefExec(ctx, req)
	default:
		return &images.VolumUtilsResponse{
			RequestID: req.RequestID,
			Ret: &errorcode.Ret{
				RetCode: errorcode.ErrorCode_InvalidParamFormat,
				RetMsg:  "not support cmd",
			},
		}, nil
	}
}
func (s *service) doResetVolumeRefExec(ctx context.Context, req *images.VolumUtilsRequest) (*images.VolumUtilsResponse, error) {
	rsp := &images.VolumUtilsResponse{
		RequestID: req.RequestID,
		Ret: &errorcode.Ret{
			RetCode: errorcode.ErrorCode_Success,
			RetMsg:  "success",
		},
	}
	doReset := func(fileType volumefile.FileType) error {
		bucket := getBucketName(fileType)
		all, err := s.volume.lifetime.db.ReadAll(bucket)
		if err != nil {
			return err
		}
		for k, v := range all {
			keyList := strings.SplitN(k, "|", 2)
			if len(keyList) < 2 {
				continue
			}
			m := &meta{}
			err = json.Unmarshal(v, m)
			if err != nil {
				continue
			}

			deltaT := time.Now().Unix() - m.Timestamp
			expiredException := m.Ref > 0 && m.Timestamp > 10000 && deltaT > s.volume.config.ExpiredExceptionInSec
			if !expiredException {
				continue
			}
			m.userID, m.fileSha256, m.fileType = keyList[0], keyList[1], fileType
			filterKey := "cube.code.sha256"
			if fileType == volumefile.FtLayer {
				filterKey = "cube.layer.sha256"
			}
			cnt, err := s.getSandboxBySha256(ctx, filterKey, m.fileSha256)
			if err != nil {
				continue
			}
			if cnt > 0 {
				log.G(ctx).Errorf("doResetVolumeRefExec nothing todo:%s ", m.fileSha256)
				continue
			}
			m.Ref = 0
			m.Timestamp = time.Now().Unix()
			value, err := json.Marshal(m)
			if err != nil {
				return err
			}
			err = s.volume.lifetime.db.Set(bucket, k, value)
			if err != nil {
				log.G(ctx).Errorf("doResetVolumeRefExec set:%s, err:%v", k, err)
				return err
			}
		}
		return nil
	}
	start := time.Now()
	err := doReset(volumefile.FtCode)
	log.G(ctx).Errorf("doReset finish:%v, cost:%v,err:%v", volumefile.FtCode, time.Since(start), err)
	if err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = err.Error()
		return rsp, err
	}
	start = time.Now()
	err = doReset(volumefile.FtLayer)
	log.G(ctx).Errorf("doReset finish:%v, cost:%v,err:%v", volumefile.FtLayer, time.Since(start), err)
	if err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = err.Error()
		return rsp, err
	}
	return rsp, nil
}
func (s *service) doResetVolumeRef(ctx context.Context, req *images.VolumUtilsRequest) (*images.VolumUtilsResponse, error) {
	rsp := &images.VolumUtilsResponse{
		RequestID: req.RequestID,
		Ret: &errorcode.Ret{
			RetCode: errorcode.ErrorCode_Success,
			RetMsg:  "success",
		},
	}

	s.volume.lifetime.syncFlush(ctx)
	log.G(ctx).Errorf("syncFlush finish")

	doReset := func(fileType volumefile.FileType) error {
		bucket := getBucketName(fileType)
		all, err := s.volume.lifetime.db.ReadAll(bucket)
		if err != nil {
			return err
		}
		for k, v := range all {
			keyList := strings.SplitN(k, "|", 2)
			if len(keyList) < 2 {
				continue
			}
			m := &meta{}
			err = json.Unmarshal(v, m)
			if err != nil {
				continue
			}
			if m.Ref > 0 {
				continue
			}

			m.userID, m.fileSha256, m.fileType = keyList[0], keyList[1], fileType
			filterKey := "cube.code.sha256"
			if fileType == volumefile.FtLayer {
				filterKey = "cube.layer.sha256"
			}
			cnt, err := s.getSandboxBySha256(ctx, filterKey, m.fileSha256)
			if err != nil {
				continue
			}
			if m.Ref == cnt || (m.Ref == 0 && cnt == 0) {
				log.G(ctx).Errorf("doResetVolumeRef nothing todo:%s ", m.fileSha256)
				continue
			}
			m.Ref = cnt
			m.Timestamp = time.Now().Unix()
			value, err := json.Marshal(m)
			if err != nil {
				return err
			}
			err = s.volume.lifetime.db.Set(bucket, k, value)
			if err != nil {
				log.G(ctx).Errorf("doResetVolumeRef set:%s, err:%v", k, err)
				return err
			}
		}
		return nil
	}
	start := time.Now()
	err := doReset(volumefile.FtCode)
	log.G(ctx).Errorf("doReset finish:%v, cost:%v,err:%v", volumefile.FtCode, time.Since(start), err)
	if err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = err.Error()
		return rsp, err
	}
	start = time.Now()
	err = doReset(volumefile.FtLayer)
	log.G(ctx).Errorf("doReset finish:%v, cost:%v,err:%v", volumefile.FtLayer, time.Since(start), err)
	if err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = err.Error()
		return rsp, err
	}
	return rsp, nil
}

func (s *service) getSandboxBySha256(ctx context.Context, key, sha256 string) (int, error) {
	req := &cubebox.ListCubeSandboxRequest{
		Filter: &cubebox.CubeSandboxFilter{
			LabelSelector: map[string]string{
				key: sha256,
			},
		},
	}
	resp, err := s.imageGCManager.cubeletClient.CubeBoxService().List(ctx, req)
	if err != nil {
		return 0, fmt.Errorf("list cubebox: %w", err)
	}
	validCnt := 0
	for _, sb := range resp.Items {
		for _, c := range sb.Containers {
			if isContainerInGoodState(c) {
				validCnt++
				break
			}
		}
	}
	return validCnt, nil
}
