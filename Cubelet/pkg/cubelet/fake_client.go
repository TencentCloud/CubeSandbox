// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubelet

import (
	"context"

	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/images/v1"
)

func NewFromFakeService(f *FakeService) *Client {
	c, _ := New("", WithServices(
		WithCubeboxClient(f),
		WithImageClient(f),
	))
	return c
}

type FakeService struct {
	CubeBoxes []*cubebox.CubeSandbox
	Images    sets.String
	cubebox.UnimplementedCubeboxMgrServer
	images.UnimplementedImagesServer

	ImageDeleteEvent []string
}

func (f *FakeService) Create(ctx context.Context, request *cubebox.RunCubeSandboxRequest) (*cubebox.RunCubeSandboxResponse, error) {

	panic("implement me")
}

func (f *FakeService) Destroy(ctx context.Context, request *cubebox.DestroyCubeSandboxRequest) (*cubebox.DestroyCubeSandboxResponse, error) {

	panic("implement me")
}

func (f *FakeService) List(ctx context.Context, request *cubebox.ListCubeSandboxRequest) (*cubebox.ListCubeSandboxResponse, error) {
	return &cubebox.ListCubeSandboxResponse{
		Items: f.CubeBoxes,
	}, nil
}

func (f *FakeService) CreateImage(ctx context.Context, request *images.CreateImageRequest) (*images.CreateImageRequestResponse, error) {

	panic("implement me")
}

func (f *FakeService) DestroyImage(ctx context.Context, request *images.DestroyImageRequest) (*images.DestroyImageResponse, error) {
	id := request.GetSpec().GetImage()
	f.Images.Delete(id)
	f.ImageDeleteEvent = append(f.ImageDeleteEvent, id)
	return &images.DestroyImageResponse{
		Ret: &errorcode.Ret{RetCode: errorcode.ErrorCode_Success},
	}, nil
}
