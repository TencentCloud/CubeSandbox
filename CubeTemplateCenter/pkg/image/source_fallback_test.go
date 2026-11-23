// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package image

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

const localOnlyRef = "localtest/app:v1"

func localEngineImage() *dockerInspectImage {
	return &dockerInspectImage{
		ID:     "sha256:localimage",
		Config: DockerImageConfig{Cmd: []string{"app"}, WorkingDir: "/srv"},
	}
}

// engineWithLocalImage serves inspect for localOnlyRef and fails the test on
// any pull: the fallback must only use an image that is already local.
func engineWithLocalImage(t *testing.T, present bool) *fakeEngineClient {
	t.Helper()
	return &fakeEngineClient{
		inspect: func(_ context.Context, ref string) (*dockerInspectImage, error) {
			if present && ref == localOnlyRef {
				return localEngineImage(), nil
			}
			return nil, errEngineImageNotFound
		},
		pull: func(context.Context, string, string) (io.ReadCloser, error) {
			t.Fatal("local docker fallback must not pull")
			return nil, nil
		},
	}
}

// withDockerExportTools reports the tools the docker export path runs.
func withDockerExportTools(t *testing.T) {
	t.Helper()
	withExecutableLookPath(t, func(file string) (string, error) {
		if file == "docker" || file == "tar" {
			return "/usr/bin/" + file, nil
		}
		return "", errors.New("not found")
	})
}

// configErrImage resolves from the registry but fails to fetch its config.
type configErrImage struct{ v1.Image }

func (configErrImage) ConfigFile() (*v1.ConfigFile, error) {
	return nil, errors.New("config blob fetch failed")
}

func assertLocalDockerSource(t *testing.T, got *PreparedSource, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("expected local docker fallback, got error: %v", err)
	}
	if got.ExportMode != ExportModeDocker {
		t.Fatalf("ExportMode=%q, want docker", got.ExportMode)
	}
	if got.LocalRef != localOnlyRef || got.Digest != "sha256:localimage" || got.Config.WorkingDir != "/srv" {
		t.Fatalf("unexpected local source: %#v", got)
	}
}

func TestPrepareSourceFallsBackToLocalDockerImageWhenNativeResolveFails(t *testing.T) {
	t.Setenv("CUBEMASTER_NATIVE_ROOTFS_EXPORT_ENABLED", "true")
	patches := gomonkey.ApplyFuncReturn(remote.Image, nil, errors.New("UNAUTHORIZED: authentication required"))
	defer patches.Reset()
	withEngineClient(t, engineWithLocalImage(t, true), nil)
	withDockerExportTools(t)

	for name, fn := range map[string]func(context.Context, SourceSpec) (*PreparedSource, error){
		"PrepareSource":      PrepareSource,
		"PrepareLocalSource": PrepareLocalSource,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := fn(context.Background(), SourceSpec{ImageRef: localOnlyRef})
			assertLocalDockerSource(t, got, err)
		})
	}
}

func TestPrepareSourceLocalFallbackKeepsUserImage(t *testing.T) {
	t.Setenv("CUBEMASTER_NATIVE_ROOTFS_EXPORT_ENABLED", "true")
	patches := gomonkey.ApplyFuncReturn(remote.Image, nil, errors.New("not found"))
	defer patches.Reset()
	withEngineClient(t, engineWithLocalImage(t, true), nil)
	withDockerExportTools(t)
	patches.ApplyFunc(dockerRun, func(ctx context.Context, configDir string, args ...string) error {
		t.Fatalf("cleanup must not touch the user's local image, got docker %v", args)
		return nil
	})

	got, err := PrepareSource(context.Background(), SourceSpec{ImageRef: localOnlyRef})
	assertLocalDockerSource(t, got, err)
	got.Cleanup(context.Background())
}

func TestPrepareSourceKeepsNativeErrorWhenImageIsNotLocal(t *testing.T) {
	t.Setenv("CUBEMASTER_NATIVE_ROOTFS_EXPORT_ENABLED", "true")
	patches := gomonkey.ApplyFuncReturn(remote.Image, nil, errors.New("UNAUTHORIZED: authentication required"))
	defer patches.Reset()
	withEngineClient(t, engineWithLocalImage(t, false), nil)
	withDockerExportTools(t)
	patches.ApplyFunc(dockerOutput, func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
		return nil, errors.New("Error: No such image")
	})

	got, err := PrepareSource(context.Background(), SourceSpec{ImageRef: localOnlyRef})
	if err == nil || !strings.Contains(err.Error(), "native export failed to resolve image") || !strings.Contains(err.Error(), "UNAUTHORIZED") {
		t.Fatalf("expected the original native error, got source=%#v err=%v", got, err)
	}
}

func TestPrepareSourcePrefersRegistryWhenResolvable(t *testing.T) {
	t.Setenv("CUBEMASTER_NATIVE_ROOTFS_EXPORT_ENABLED", "true")
	patches := gomonkey.ApplyFuncReturn(remote.Image, empty.Image, nil)
	defer patches.Reset()
	withEngineClient(t, &fakeEngineClient{
		inspect: func(context.Context, string) (*dockerInspectImage, error) {
			t.Fatal("docker must not be consulted when the registry resolves the image")
			return nil, nil
		},
	}, nil)

	got, err := PrepareSource(context.Background(), SourceSpec{ImageRef: localOnlyRef})
	if err != nil {
		t.Fatalf("PrepareSource failed: %v", err)
	}
	if got.ExportMode != ExportModeNative {
		t.Fatalf("ExportMode=%q, want native", got.ExportMode)
	}
}

func TestPrepareSourceFallsBackToLocalDockerImageWhenSkopeoFails(t *testing.T) {
	disableNativeRootfsExport(t)
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	withExecutableLookPath(t, func(file string) (string, error) {
		if file == "skopeo" || file == "umoci" || file == "docker" || file == "tar" {
			return "/usr/bin/" + file, nil
		}
		return "", errors.New("not found")
	})
	patches.ApplyFunc(skopeoOutput, func(ctx context.Context, authFile string, args ...string) ([]byte, error) {
		return nil, errors.New("requested access to the resource is denied")
	})
	disableEngineClient(t)
	patches.ApplyFunc(dockerOutput, func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
		if len(args) == 4 && args[0] == "image" && args[1] == "inspect" && args[3] == localOnlyRef {
			return []byte(`[{"Id":"sha256:localimage","Config":{"Cmd":["app"],"WorkingDir":"/srv"}}]`), nil
		}
		t.Fatalf("unexpected dockerOutput args=%v", args)
		return nil, nil
	})

	got, err := PrepareSource(context.Background(), SourceSpec{ImageRef: localOnlyRef})
	assertLocalDockerSource(t, got, err)
}

func TestPrepareSourceDoesNotFallBackWithoutTar(t *testing.T) {
	t.Setenv("CUBEMASTER_NATIVE_ROOTFS_EXPORT_ENABLED", "true")
	patches := gomonkey.ApplyFuncReturn(remote.Image, nil, errors.New("UNAUTHORIZED: authentication required"))
	defer patches.Reset()
	withEngineClient(t, engineWithLocalImage(t, true), nil)
	withExecutableLookPath(t, func(file string) (string, error) {
		return "", errors.New("not found")
	})

	_, err := PrepareSource(context.Background(), SourceSpec{ImageRef: localOnlyRef})
	if err == nil || !strings.Contains(err.Error(), "native export failed to resolve image") {
		t.Fatalf("expected the original native error when tar is unavailable, got %v", err)
	}
}

// The engine API can find the image while the docker CLI the export path runs
// is absent, e.g. TemplateCenter in a container with the socket bind-mounted.
func TestPrepareSourceDoesNotFallBackWithoutDockerCLI(t *testing.T) {
	t.Setenv("CUBEMASTER_NATIVE_ROOTFS_EXPORT_ENABLED", "true")
	patches := gomonkey.ApplyFuncReturn(remote.Image, nil, errors.New("UNAUTHORIZED: authentication required"))
	defer patches.Reset()
	withEngineClient(t, engineWithLocalImage(t, true), nil)
	withExecutableLookPath(t, func(file string) (string, error) {
		if file == "tar" {
			return "/usr/bin/tar", nil
		}
		return "", errors.New("not found")
	})

	_, err := PrepareSource(context.Background(), SourceSpec{ImageRef: localOnlyRef})
	if err == nil || !strings.Contains(err.Error(), "native export failed to resolve image") {
		t.Fatalf("expected the original native error when the docker CLI is unavailable, got %v", err)
	}
}

// A failure after the registry resolved the image must not switch to a
// same-tag local image, which may be stale.
func TestPrepareSourceDoesNotFallBackAfterNativeResolve(t *testing.T) {
	t.Setenv("CUBEMASTER_NATIVE_ROOTFS_EXPORT_ENABLED", "true")
	patches := gomonkey.ApplyFuncReturn(remote.Image, configErrImage{empty.Image}, nil)
	defer patches.Reset()
	withEngineClient(t, &fakeEngineClient{
		inspect: func(context.Context, string) (*dockerInspectImage, error) {
			t.Fatal("docker must not be consulted after the registry resolved the image")
			return nil, nil
		},
	}, nil)
	withDockerExportTools(t)

	_, err := PrepareSource(context.Background(), SourceSpec{ImageRef: localOnlyRef})
	if err == nil || !strings.Contains(err.Error(), "config blob fetch failed") {
		t.Fatalf("expected the config fetch error, got %v", err)
	}
}

func TestPrepareSourceDoesNotFallBackAfterSkopeoResolve(t *testing.T) {
	disableNativeRootfsExport(t)
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	withExecutableLookPath(t, func(file string) (string, error) {
		if file == "skopeo" || file == "umoci" || file == "docker" || file == "tar" {
			return "/usr/bin/" + file, nil
		}
		return "", errors.New("not found")
	})
	patches.ApplyFunc(skopeoOutput, func(ctx context.Context, authFile string, args ...string) ([]byte, error) {
		if len(args) > 1 && args[1] == "--config" {
			return nil, errors.New("connection reset by peer")
		}
		return []byte(`{"Digest":"sha256:remote"}`), nil
	})
	disableEngineClient(t)
	patches.ApplyFunc(dockerOutput, func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
		t.Fatalf("docker must not be consulted after skopeo resolved the image, got %v", args)
		return nil, nil
	})

	_, err := PrepareSource(context.Background(), SourceSpec{ImageRef: localOnlyRef})
	if err == nil || !strings.Contains(err.Error(), "skopeo inspect --config") {
		t.Fatalf("expected the skopeo --config error, got %v", err)
	}
}

func TestPrepareSourceDoesNotFallBackForInvalidImageRef(t *testing.T) {
	t.Setenv("CUBEMASTER_NATIVE_ROOTFS_EXPORT_ENABLED", "true")
	withDockerExportTools(t)
	withEngineClient(t, &fakeEngineClient{
		inspect: func(context.Context, string) (*dockerInspectImage, error) {
			t.Fatal("an invalid image ref must not reach docker")
			return nil, nil
		},
	}, nil)

	if _, err := PrepareSource(context.Background(), SourceSpec{ImageRef: "--help"}); err == nil {
		t.Fatal("expected invalid image ref to be rejected")
	}
}
