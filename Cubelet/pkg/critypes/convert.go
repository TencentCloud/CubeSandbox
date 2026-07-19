// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package critypes

import (
	types "github.com/tencentcloud/CubeSandbox/proto/types/v1"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
	runtimeAlpha "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
)

func AuthConfigToCRI(a *types.AuthConfig) *runtime.AuthConfig {
	if a == nil {
		return nil
	}
	return &runtime.AuthConfig{
		Username:      a.Username,
		Password:      a.Password,
		Auth:          a.Auth,
		ServerAddress: a.ServerAddress,
		IdentityToken: a.IdentityToken,
		RegistryToken: a.RegistryToken,
	}
}

func AuthConfigToCRIAlpha(a *types.AuthConfig) *runtimeAlpha.AuthConfig {
	if a == nil {
		return nil
	}
	return &runtimeAlpha.AuthConfig{
		Username:      a.Username,
		Password:      a.Password,
		Auth:          a.Auth,
		ServerAddress: a.ServerAddress,
		IdentityToken: a.IdentityToken,
		RegistryToken: a.RegistryToken,
	}
}

func ImageFilterToCRI(x *types.ImageFilter) *runtime.ImageFilter {
	if x == nil {
		return nil
	}
	ifer := &runtime.ImageFilter{}
	if x.Image != nil {
		ifer.Image = ImageSpecToCRI(x.Image)
	}
	return ifer
}

func ImageFilterToCRIAlpha(x *types.ImageFilter) *runtimeAlpha.ImageFilter {
	if x == nil {
		return nil
	}
	ifer := &runtimeAlpha.ImageFilter{}
	if x.Image != nil {
		ifer.Image = ImageSpecToCRIAlpha(x.Image)
	}
	return ifer
}

func ImageSpecToCRI(x *types.ImageSpec) *runtime.ImageSpec {
	if x == nil {
		return nil
	}
	return &runtime.ImageSpec{
		Image:       x.Image,
		Annotations: x.Annotations,
	}
}

func ImageSpecToCRIAlpha(x *types.ImageSpec) *runtimeAlpha.ImageSpec {
	if x == nil {
		return nil
	}
	return &runtimeAlpha.ImageSpec{
		Image:       x.Image,
		Annotations: x.Annotations,
	}
}

func FromCRIImage(cri *runtime.Image) *types.Image {
	if cri == nil {
		return nil
	}
	img := &types.Image{
		Id:          cri.Id,
		RepoTags:    cri.RepoTags,
		RepoDigests: cri.RepoDigests,
		Size:        cri.Size_,
		Username:    cri.Username,
		Spec:        FromCRIImageSpec(cri.Spec),
		Pinned:      cri.Pinned,
	}
	if cri.Uid != nil {
		img.Uid = &types.Int64Value{
			Value: cri.Uid.Value,
		}
	}
	return img
}

func FromCRIAlphaImage(cri *runtimeAlpha.Image) *types.Image {
	if cri == nil {
		return nil
	}
	img := &types.Image{
		Id:          cri.Id,
		RepoTags:    cri.RepoTags,
		RepoDigests: cri.RepoDigests,
		Size:        cri.Size_,
		Username:    cri.Username,
		Spec:        FromCRIAlphaImageSpec(cri.Spec),
		Pinned:      cri.Pinned,
	}
	if cri.Uid != nil {
		img.Uid = &types.Int64Value{
			Value: cri.Uid.Value,
		}
	}
	return img
}

func FromCRIImageSpec(cri *runtime.ImageSpec) *types.ImageSpec {
	if cri == nil {
		return nil
	}
	return &types.ImageSpec{
		Image:       cri.Image,
		Annotations: cri.Annotations,
	}
}

func FromCRIAlphaImageSpec(cri *runtimeAlpha.ImageSpec) *types.ImageSpec {
	if cri == nil {
		return nil
	}
	return &types.ImageSpec{
		Image:       cri.Image,
		Annotations: cri.Annotations,
	}
}
