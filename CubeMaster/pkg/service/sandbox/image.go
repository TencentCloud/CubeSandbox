// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"fmt"

	"github.com/hashicorp/go-multierror"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/task"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/images/v1"
)

func CreateImage(ctx context.Context, req *types.CreateImageReq) (rsp *types.Res) {
	rsp = &types.Res{
		RequestID: req.RequestID,
		Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_Success),
			RetMsg:  errorcode.ErrorCode_Success.String(),
		},
	}
	defer func() {
		logger := log.G(ctx).WithFields(map[string]interface{}{
			"RequestId": req.RequestID,
			"RetCode":   int64(rsp.Ret.RetCode),
		})
		logger.Infof("async CreateImage:%+v", utils.InterfaceToString(dealSecurityImage(req)))
		if rsp.Ret.RetCode != int(errorcode.ErrorCode_Success) {
			logger.Errorf("async CreateImage fail:%+v", utils.InterfaceToString(rsp))
		}
	}()

	cubeletReq := &images.CreateImageRequest{
		RequestID: req.RequestID,
		Spec: &images.ImageSpec{
			Image:        req.Image,
			StorageMedia: req.StorageMedia,
			Annotations:  req.Annotations,
		},
	}

	if req.Username != "" {
		if cubeletReq.Spec.Annotations == nil {
			cubeletReq.Spec.Annotations = make(map[string]string)
		}
		cubeletReq.Spec.Annotations[constants.CubeAnnotationsImageName] = req.Username
		cubeletReq.Spec.Annotations[constants.CubeAnnotationsImageToken] = req.Token
	}
	if cubeletReq.Spec.Annotations == nil {
		cubeletReq.Spec.Annotations = map[string]string{}
	}
	if req.WritableLayerSize != "" {
		cubeletReq.Spec.Annotations[constants.CubeAnnotationWritableLayerSize] = req.WritableLayerSize
	}
	var (
		result *multierror.Error
	)
	nodes := localcache.GetHealthyNodesByInstanceType(-1, req.InstanceType)
	for _, node := range nodes {
		if isFilteOut(node, config.GetConfig().Common.DisableCreateImageCluster) {
			continue
		}
		t := &task.Task{
			RequestId: req.RequestID,
			Request:   cubeletReq,
			CallEp:    cubelet.GetCubeletAddr(node.HostIP()),
			TaskType:  task.CreateImage,
		}
		t.Ctx = log.WithLogger(ctx, log.G(ctx).WithFields(map[string]interface{}{
			"RequestId":      req.RequestID,
			"CalleeEndpoint": t.CallEp,
		}))
		if err := task.AddAsyncTask(t); err != nil {
			result = multierror.Append(result, fmt.Errorf("async tasks is full: %+v", t))
		}
	}

	if result.ErrorOrNil() != nil {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterInternalError)
		rsp.Ret.RetMsg = result.ErrorOrNil().Error()
	}
	return
}

func DeleteImage(ctx context.Context, req *types.DeleteImageReq) (rsp *types.Res) {
	rsp = &types.Res{
		RequestID: req.RequestID,
		Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_Success),
			RetMsg:  errorcode.ErrorCode_Success.String(),
		},
	}
	defer func() {
		logger := log.G(ctx).WithFields(map[string]interface{}{
			"RequestId": req.RequestID,
			"RetCode":   int64(rsp.Ret.RetCode),
		})
		logger.Infof("DeleteImage:%+v", utils.InterfaceToString(req))
		if rsp.Ret.RetCode != int(errorcode.ErrorCode_Success) {
			logger.Errorf("DeleteImage fail:%+v", utils.InterfaceToString(rsp))
		}
	}()
	cubeletReq := &images.DestroyImageRequest{
		RequestID: req.RequestID,
		Spec: &images.ImageSpec{
			Image: req.Image,
		},
	}
	var (
		result *multierror.Error
	)
	nodes := localcache.GetHealthyNodesByInstanceType(-1, req.InstanceType)
	for _, node := range nodes {
		t := &task.Task{
			RequestId: req.RequestID,
			Request:   cubeletReq,
			CallEp:    cubelet.GetCubeletAddr(node.HostIP()),
			TaskType:  task.DeleteImage,
		}
		t.Ctx = log.WithLogger(ctx, log.G(ctx).WithFields(map[string]interface{}{
			"RequestId":      req.RequestID,
			"CalleeEndpoint": t.CallEp,
		}))
		if err := task.AddAsyncTask(t); err != nil {
			result = multierror.Append(result, fmt.Errorf("async tasks is full: %+v", t))
		}
	}
	if result.ErrorOrNil() != nil {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterInternalError)
		rsp.Ret.RetMsg = result.ErrorOrNil().Error()
	}
	return
}

func dealSecurityImage(req *types.CreateImageReq) *types.CreateImageReq {
	tmpReq := &types.CreateImageReq{}
	tmpData, _ := types.FastestJsoniter.Marshal(req)
	types.FastestJsoniter.Unmarshal(tmpData, tmpReq)
	if tmpReq.Token != "" {
		tmpReq.Token = "*"
	}
	return tmpReq
}
