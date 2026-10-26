// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package integration

import (
	"context"
	"testing"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/images/v1"
)

func TestImagePull(t *testing.T) {
	cntdClient := NewDefaultContainerdClient(t)
	cntCtx := namespaces.WithNamespace(context.Background(), namespaces.Default)

	image := "busybox:musl"

	err := cntdClient.ImageService().Delete(cntCtx, image)
	if err != nil && !errdefs.IsNotFound(err) {
		t.Errorf("delete image %s failed: %v", image, err)
	}

	req := &images.CreateImageRequest{
		Spec: &images.ImageSpec{
			Image: image,
		},
	}
	resp := CreateImage(t, req)
	assert.Equal(t, errorcode.ErrorCode_Success, resp.GetRet().GetRetCode())

	err = cntdClient.ImageService().Delete(cntCtx, image)
	if err != nil && !errdefs.IsNotFound(err) {
		t.Errorf("delete image %s failed: %v", image, err)
	}
}

func TestImageDestroy(t *testing.T) {
	dReq := &images.DestroyImageRequest{
		Spec: &images.ImageSpec{
			Image: "docker.io/library/busybox:latest",
		},
	}
	DestroyImage(t, dReq)
}
