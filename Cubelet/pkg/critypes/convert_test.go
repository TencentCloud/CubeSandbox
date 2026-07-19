// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package critypes

import (
	"testing"

	types "github.com/tencentcloud/CubeSandbox/proto/types/v1"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
	runtimeAlpha "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
)

func TestAuthConfigToCRI(t *testing.T) {
	tests := []struct {
		name  string
		input *types.AuthConfig
	}{
		{
			name:  "fully populated",
			input: &types.AuthConfig{Username: "u", Password: "p", Auth: "a", ServerAddress: "s", IdentityToken: "it", RegistryToken: "rt"},
		},
		{
			name:  "empty fields",
			input: &types.AuthConfig{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := AuthConfigToCRI(tc.input)
			assertAuthFieldsMatch(t, tc.input, got.Username, got.Password, got.Auth, got.ServerAddress, got.IdentityToken, got.RegistryToken)
		})
	}
}

func TestAuthConfigToCRI_Nil(t *testing.T) {
	if got := AuthConfigToCRI(nil); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestAuthConfigToCRIAlpha(t *testing.T) {
	tests := []struct {
		name  string
		input *types.AuthConfig
	}{
		{
			name:  "fully populated",
			input: &types.AuthConfig{Username: "u", Password: "p", Auth: "a", ServerAddress: "s", IdentityToken: "it", RegistryToken: "rt"},
		},
		{
			name:  "empty fields",
			input: &types.AuthConfig{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := AuthConfigToCRIAlpha(tc.input)
			assertAuthFieldsMatch(t, tc.input, got.Username, got.Password, got.Auth, got.ServerAddress, got.IdentityToken, got.RegistryToken)
		})
	}
}

func TestAuthConfigToCRIAlpha_Nil(t *testing.T) {
	if got := AuthConfigToCRIAlpha(nil); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestImageFilterToCRI(t *testing.T) {
	t.Run("with image spec", func(t *testing.T) {
		f := &types.ImageFilter{Image: &types.ImageSpec{Image: "img:v1", Annotations: map[string]string{"k": "v"}}}
		got := ImageFilterToCRI(f)
		if got.Image == nil {
			t.Fatal("expected non-nil Image")
		}
		if got.Image.Image != "img:v1" {
			t.Errorf("Image = %q, want %q", got.Image.Image, "img:v1")
		}
	})
	t.Run("nil image spec", func(t *testing.T) {
		f := &types.ImageFilter{}
		got := ImageFilterToCRI(f)
		if got.Image != nil {
			t.Errorf("expected nil Image, got %v", got.Image)
		}
	})
}

func TestImageFilterToCRI_Nil(t *testing.T) {
	if got := ImageFilterToCRI(nil); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestImageFilterToCRIAlpha(t *testing.T) {
	t.Run("with image spec", func(t *testing.T) {
		f := &types.ImageFilter{Image: &types.ImageSpec{Image: "img:v2", Annotations: map[string]string{"k": "v"}}}
		got := ImageFilterToCRIAlpha(f)
		if got.Image == nil {
			t.Fatal("expected non-nil Image")
		}
		if got.Image.Image != "img:v2" {
			t.Errorf("Image = %q, want %q", got.Image.Image, "img:v2")
		}
	})
	t.Run("nil image spec", func(t *testing.T) {
		f := &types.ImageFilter{}
		got := ImageFilterToCRIAlpha(f)
		if got.Image != nil {
			t.Errorf("expected nil Image, got %v", got.Image)
		}
	})
}

func TestImageFilterToCRIAlpha_Nil(t *testing.T) {
	if got := ImageFilterToCRIAlpha(nil); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestImageSpecToCRI(t *testing.T) {
	spec := &types.ImageSpec{Image: "img:latest", Annotations: map[string]string{"a": "b"}}
	got := ImageSpecToCRI(spec)
	if got.Image != "img:latest" {
		t.Errorf("Image = %q, want %q", got.Image, "img:latest")
	}
	if got.Annotations["a"] != "b" {
		t.Errorf("Annotations = %v, want a=b", got.Annotations)
	}
}

func TestImageSpecToCRI_Nil(t *testing.T) {
	if got := ImageSpecToCRI(nil); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestImageSpecToCRIAlpha(t *testing.T) {
	spec := &types.ImageSpec{Image: "img:alpha", Annotations: map[string]string{"x": "y"}}
	got := ImageSpecToCRIAlpha(spec)
	if got.Image != "img:alpha" || got.Annotations["x"] != "y" {
		t.Errorf("unexpected result: %+v", got)
	}
}

func TestImageSpecToCRIAlpha_Nil(t *testing.T) {
	if got := ImageSpecToCRIAlpha(nil); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestFromCRIImage(t *testing.T) {
	t.Run("fully populated with uid", func(t *testing.T) {
		cri := &runtime.Image{
			Id:          "sha256:abc",
			RepoTags:    []string{"repo:tag"},
			RepoDigests: []string{"repo@sha256:abc"},
			Size_:       1024,
			Username:    "user",
			Spec:        &runtime.ImageSpec{Image: "img"},
			Pinned:      true,
			Uid:         &runtime.Int64Value{Value: 1000},
		}
		got := FromCRIImage(cri)
		if got.Id != "sha256:abc" || got.Size != 1024 || got.Username != "user" || !got.Pinned {
			t.Errorf("basic fields mismatch: %+v", got)
		}
		if got.Uid == nil || got.Uid.Value != 1000 {
			t.Errorf("Uid = %v, want 1000", got.Uid)
		}
		if got.Spec == nil || got.Spec.Image != "img" {
			t.Errorf("Spec mismatch: %+v", got.Spec)
		}
	})
	t.Run("nil uid", func(t *testing.T) {
		cri := &runtime.Image{Id: "id1"}
		got := FromCRIImage(cri)
		if got.Uid != nil {
			t.Errorf("expected nil Uid, got %v", got.Uid)
		}
	})
}

func TestFromCRIImage_Nil(t *testing.T) {
	if got := FromCRIImage(nil); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestFromCRIAlphaImage(t *testing.T) {
	t.Run("fully populated with uid", func(t *testing.T) {
		cri := &runtimeAlpha.Image{
			Id:          "id-alpha",
			RepoTags:    []string{"alpha:tag"},
			RepoDigests: []string{"alpha@sha256:def"},
			Size_:       2048,
			Username:    "alpha-user",
			Uid:         &runtimeAlpha.Int64Value{Value: 500},
			Spec:        &runtimeAlpha.ImageSpec{Image: "alpha-img"},
			Pinned:      true,
		}
		got := FromCRIAlphaImage(cri)
		if got.Id != "id-alpha" || got.Size != 2048 || got.Username != "alpha-user" || !got.Pinned {
			t.Errorf("basic fields mismatch: %+v", got)
		}
		if got.Uid == nil || got.Uid.Value != 500 {
			t.Errorf("Uid = %v, want 500", got.Uid)
		}
		if got.Spec == nil || got.Spec.Image != "alpha-img" {
			t.Errorf("Spec mismatch: %+v", got.Spec)
		}
	})
	t.Run("nil uid", func(t *testing.T) {
		cri := &runtimeAlpha.Image{Id: "id-alpha-no-uid"}
		got := FromCRIAlphaImage(cri)
		if got.Uid != nil {
			t.Errorf("expected nil Uid, got %v", got.Uid)
		}
	})
}

func TestFromCRIAlphaImage_Nil(t *testing.T) {
	if got := FromCRIAlphaImage(nil); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestFromCRIImageSpec(t *testing.T) {
	t.Run("nil input", func(t *testing.T) {
		if got := FromCRIImageSpec(nil); got != nil {
			t.Errorf("expected nil, got %v", got)
		}
	})
	t.Run("valid input", func(t *testing.T) {
		cri := &runtime.ImageSpec{Image: "spec-img", Annotations: map[string]string{"k": "v"}}
		got := FromCRIImageSpec(cri)
		if got.Image != "spec-img" || got.Annotations["k"] != "v" {
			t.Errorf("unexpected: %+v", got)
		}
	})
}

func TestFromCRIAlphaImageSpec(t *testing.T) {
	t.Run("nil input", func(t *testing.T) {
		if got := FromCRIAlphaImageSpec(nil); got != nil {
			t.Errorf("expected nil, got %v", got)
		}
	})
	t.Run("valid input", func(t *testing.T) {
		cri := &runtimeAlpha.ImageSpec{Image: "alpha-spec"}
		got := FromCRIAlphaImageSpec(cri)
		if got.Image != "alpha-spec" {
			t.Errorf("Image = %q, want %q", got.Image, "alpha-spec")
		}
	})
}

func assertAuthFieldsMatch(t *testing.T, src *types.AuthConfig, username, password, auth, server, identity, registry string) {
	t.Helper()
	if username != src.Username {
		t.Errorf("Username = %q, want %q", username, src.Username)
	}
	if password != src.Password {
		t.Errorf("Password = %q, want %q", password, src.Password)
	}
	if auth != src.Auth {
		t.Errorf("Auth = %q, want %q", auth, src.Auth)
	}
	if server != src.ServerAddress {
		t.Errorf("ServerAddress = %q, want %q", server, src.ServerAddress)
	}
	if identity != src.IdentityToken {
		t.Errorf("IdentityToken = %q, want %q", identity, src.IdentityToken)
	}
	if registry != src.RegistryToken {
		t.Errorf("RegistryToken = %q, want %q", registry, src.RegistryToken)
	}
}
