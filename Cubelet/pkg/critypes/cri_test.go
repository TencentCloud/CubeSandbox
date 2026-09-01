// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package critypes

import (
	"reflect"
	"testing"

	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
	runtimeAlpha "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"

	"github.com/tencentcloud/CubeSandbox/pkgs/proto/types/v1"
)

func TestToCRI(t *testing.T) {
	cases := []struct {
		name string
		in   *types.AuthConfig
		want *runtime.AuthConfig
	}{
		{"nil", nil, nil},
		{"empty", &types.AuthConfig{}, &runtime.AuthConfig{}},
		{"full", &types.AuthConfig{
			Username:      "user",
			Password:      "pass",
			Auth:          "auth",
			ServerAddress: "reg.example.com",
			IdentityToken: "id-token",
			RegistryToken: "reg-token",
		}, &runtime.AuthConfig{
			Username:      "user",
			Password:      "pass",
			Auth:          "auth",
			ServerAddress: "reg.example.com",
			IdentityToken: "id-token",
			RegistryToken: "reg-token",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ToCRI(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ToCRI(%+v) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestToCRIAlpha(t *testing.T) {
	cases := []struct {
		name string
		in   *types.AuthConfig
		want *runtimeAlpha.AuthConfig
	}{
		{"nil", nil, nil},
		{"full", &types.AuthConfig{Username: "user", Password: "pass"},
			&runtimeAlpha.AuthConfig{Username: "user", Password: "pass"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ToCRIAlpha(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ToCRIAlpha(%+v) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestImageSpecToCRI(t *testing.T) {
	cases := []struct {
		name string
		in   *types.ImageSpec
		want *runtime.ImageSpec
	}{
		{"nil", nil, nil},
		{"empty", &types.ImageSpec{}, &runtime.ImageSpec{}},
		{"full", &types.ImageSpec{Image: "sha256:abc", Annotations: map[string]string{"k": "v"}},
			&runtime.ImageSpec{Image: "sha256:abc", Annotations: map[string]string{"k": "v"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ImageSpecToCRI(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ImageSpecToCRI(%+v) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestImageFilterToCRI(t *testing.T) {
	spec := &types.ImageSpec{Image: "sha256:abc"}
	cases := []struct {
		name string
		in   *types.ImageFilter
		want *runtime.ImageFilter
	}{
		{"nil", nil, nil},
		{"empty filter", &types.ImageFilter{}, &runtime.ImageFilter{}},
		{"with image", &types.ImageFilter{Image: spec},
			&runtime.ImageFilter{Image: &runtime.ImageSpec{Image: "sha256:abc"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ImageFilterToCRI(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ImageFilterToCRI(%+v) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestFromCRIImage(t *testing.T) {
	cases := []struct {
		name string
		in   *runtime.Image
		want *types.Image
	}{
		{"nil", nil, nil},
		{"full", &runtime.Image{
			Id:          "sha256:abc",
			RepoTags:    []string{"repo:tag"},
			RepoDigests: []string{"repo@sha256:abc"},
			Size_:       1024,
			Username:    "user",
			Spec:        &runtime.ImageSpec{Image: "sha256:abc"},
			Pinned:      true,
			Uid:         &runtime.Int64Value{Value: 1000},
		}, &types.Image{
			Id:          "sha256:abc",
			RepoTags:    []string{"repo:tag"},
			RepoDigests: []string{"repo@sha256:abc"},
			Size:        1024,
			Username:    "user",
			Spec:        &types.ImageSpec{Image: "sha256:abc"},
			Pinned:      true,
			Uid:         &types.Int64Value{Value: 1000},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FromCRIImage(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("FromCRIImage(%+v) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestImageSpecToCRIAlpha(t *testing.T) {
	cases := []struct {
		name string
		in   *types.ImageSpec
		want *runtimeAlpha.ImageSpec
	}{
		{"nil", nil, nil},
		{"full", &types.ImageSpec{Image: "sha256:abc", Annotations: map[string]string{"k": "v"}},
			&runtimeAlpha.ImageSpec{Image: "sha256:abc", Annotations: map[string]string{"k": "v"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ImageSpecToCRIAlpha(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ImageSpecToCRIAlpha(%+v) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestImageFilterToCRIAlpha(t *testing.T) {
	cases := []struct {
		name string
		in   *types.ImageFilter
		want *runtimeAlpha.ImageFilter
	}{
		{"nil", nil, nil},
		{"empty filter", &types.ImageFilter{}, &runtimeAlpha.ImageFilter{}},
		{"with image", &types.ImageFilter{Image: &types.ImageSpec{Image: "sha256:abc"}},
			&runtimeAlpha.ImageFilter{Image: &runtimeAlpha.ImageSpec{Image: "sha256:abc"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ImageFilterToCRIAlpha(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ImageFilterToCRIAlpha(%+v) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestFromCRIAlphaImage(t *testing.T) {
	cases := []struct {
		name string
		in   *runtimeAlpha.Image
		want *types.Image
	}{
		{"nil", nil, nil},
		{"full", &runtimeAlpha.Image{
			Id:       "sha256:abc",
			Size_:    1024,
			Spec:     &runtimeAlpha.ImageSpec{Image: "sha256:abc"},
			Pinned:   true,
			Uid:      &runtimeAlpha.Int64Value{Value: 1000},
			Username: "user",
		}, &types.Image{
			Id:       "sha256:abc",
			Size:     1024,
			Spec:     &types.ImageSpec{Image: "sha256:abc"},
			Pinned:   true,
			Uid:      &types.Int64Value{Value: 1000},
			Username: "user",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FromCRIAlphaImage(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("FromCRIAlphaImage(%+v) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestImageID(t *testing.T) {
	if got := ImageID(nil); got != "" {
		t.Errorf("ImageID(nil) = %q, want empty", got)
	}
	if got := ImageID(&types.Image{Id: "sha256:abc"}); got != "sha256:abc" {
		t.Errorf("ImageID(...) = %q, want sha256:abc", got)
	}
}
