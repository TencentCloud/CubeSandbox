// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package images

import (
	"context"
	"fmt"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/images/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/types/v1"
	imagestore "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/store/image"
	"github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/util"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/recov"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

func (s *service) ListImages(ctx context.Context, req *images.CubeListImageRequest) (*images.CubeListImageResponse, error) {
	rsp := &images.CubeListImageResponse{
		RequestID: req.RequestID,
		Ret:       &errorcode.Ret{RetCode: errorcode.ErrorCode_Success},
	}
	rt := &CubeLog.RequestTrace{
		Action:       "ListImage",
		RequestID:    req.RequestID,
		Caller:       constants.ImagesServiceID.ID(),
		Callee:       constants.ImagesServiceID.ID(),
		CalleeAction: "ListImage",
	}

	ctx = CubeLog.WithRequestTrace(ctx, rt)

	start := time.Now()
	defer func() {
		if !ret.IsSuccessCode(rsp.Ret.RetCode) {
			log.G(ctx).WithFields(map[string]interface{}{
				"RetCode": int64(rsp.Ret.RetCode),
			}).Errorf("ListImage fail:%+v", rsp)
		}
		rt.Cost = time.Since(start)
		rt.RetCode = int64(rsp.Ret.RetCode)
		CubeLog.Trace(rt)
	}()
	defer recov.HandleCrash(func(panicError interface{}) {
		log.G(ctx).Fatalf("ListImage panic info:%s, stack:%s", panicError, string(debug.Stack()))
		rsp.Ret.RetMsg = string(debug.Stack())
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
	})

	nses, err := s.ListNamespaces(ctx)
	if err != nil {
		rsp.Ret.RetMsg = fmt.Sprintf("list namespace error: %v", err)
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		return rsp, nil
	}

	for _, ns := range nses {
		ctx := namespaces.WithNamespace(ctx, ns)
		images, err := s.l.criImage.ListImage(ctx)
		if err != nil {
			rsp.Ret.RetMsg = fmt.Sprintf("list image error: %v", err)
			rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
			return rsp, nil
		}
		for _, criImage := range images {
			ci := toCubeImage(criImage)
			ci.Namespace = ns

			rsp.Images = append(rsp.Images, ci)
		}
	}

	return rsp, nil
}

func toCubeImage(image imagestore.Image) *types.Image {
	repoTags, repoDigests := util.ParseImageReferences(image.References)
	runtimeImage := &types.Image{
		Id:          image.ID,
		RepoTags:    repoTags,
		RepoDigests: repoDigests,
		Size:        uint64(image.Size),
		Pinned:      image.Pinned,
	}
	uid, username := getUserFromImage(image.ImageSpec.Config.User)
	if uid != nil {
		runtimeImage.Uid = &types.Int64Value{Value: *uid}
	}
	runtimeImage.Username = username

	return runtimeImage
}

func getUserFromImage(user string) (*int64, string) {

	if user == "" {
		return nil, ""
	}

	user = strings.Split(user, ":")[0]

	uid, err := strconv.ParseInt(user, 10, 64)
	if err != nil {

		return nil, user
	}

	return &uid, ""
}
