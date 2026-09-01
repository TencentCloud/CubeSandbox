// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package critypes bridges CubeSandbox image types (pkgs/proto/types/v1) to
// k8s.io/cri-api runtime types. The conversion helpers used to live as methods
// on the proto types (Cubelet/api/types/v1/cri.go); since pkgs/proto must stay
// free of a k8s dependency, they are plain functions here.
package critypes

import (
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
	runtimeAlpha "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"

	"github.com/tencentcloud/CubeSandbox/pkgs/proto/types/v1"
)

// ToCRI converts a types.AuthConfig to the CRI runtime representation.
func ToCRI(a *types.AuthConfig) *runtime.AuthConfig {
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

// ToCRIAlpha converts a types.AuthConfig to the CRI alpha runtime representation.
func ToCRIAlpha(a *types.AuthConfig) *runtimeAlpha.AuthConfig {
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

// ImageFilterToCRI converts a types.ImageFilter to the CRI runtime representation.
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

// ImageFilterToCRIAlpha converts a types.ImageFilter to the CRI alpha runtime representation.
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

// ImageSpecToCRI converts a types.ImageSpec to the CRI runtime representation.
func ImageSpecToCRI(x *types.ImageSpec) *runtime.ImageSpec {
	if x == nil {
		return nil
	}
	return &runtime.ImageSpec{
		Image:       x.Image,
		Annotations: x.Annotations,
	}
}

// ImageSpecToCRIAlpha converts a types.ImageSpec to the CRI alpha runtime representation.
func ImageSpecToCRIAlpha(x *types.ImageSpec) *runtimeAlpha.ImageSpec {
	if x == nil {
		return nil
	}
	return &runtimeAlpha.ImageSpec{
		Image:       x.Image,
		Annotations: x.Annotations,
	}
}

// FromCRIImage converts a CRI runtime Image to a types.Image.
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

// FromCRIAlphaImage converts a CRI alpha runtime Image to a types.Image.
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

// FromCRIImageSpec converts a CRI runtime ImageSpec to a types.ImageSpec.
func FromCRIImageSpec(cri *runtime.ImageSpec) *types.ImageSpec {
	if cri == nil {
		return nil
	}
	return &types.ImageSpec{
		Image:       cri.Image,
		Annotations: cri.Annotations,
	}
}

// FromCRIAlphaImageSpec converts a CRI alpha runtime ImageSpec to a types.ImageSpec.
func FromCRIAlphaImageSpec(cri *runtimeAlpha.ImageSpec) *types.ImageSpec {
	if cri == nil {
		return nil
	}
	return &types.ImageSpec{
		Image:       cri.Image,
		Annotations: cri.Annotations,
	}
}

// ImageID returns the image identifier.
func ImageID(i *types.Image) string {
	if i == nil {
		return ""
	}
	return i.Id
}
