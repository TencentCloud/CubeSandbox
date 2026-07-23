// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package image

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/agiledragon/gomonkey/v2"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func testPrepareSourceImage(ctx context.Context, req *types.CreateTemplateFromImageReq, downloadBaseURL string) (*PreparedSource, error) {
	if req == nil {
		return nil, errors.New("request is nil")
	}
	return PrepareSource(ctx, SourceSpec{ImageRef: req.SourceImageRef, RegistryUsername: req.RegistryUsername, RegistryPassword: req.RegistryPassword, DownloadBaseURL: downloadBaseURL})
}

func testPrepareDockerlessSourceImage(ctx context.Context, req *types.CreateTemplateFromImageReq, downloadBaseURL string) (*PreparedSource, error) {
	if req == nil {
		return nil, errors.New("request is nil")
	}
	return prepareDockerlessSource(ctx, SourceSpec{ImageRef: req.SourceImageRef, RegistryUsername: req.RegistryUsername, RegistryPassword: req.RegistryPassword, DownloadBaseURL: downloadBaseURL})
}

func withExecutableLookPath(t *testing.T, fn func(string) (string, error)) {
	t.Helper()
	original := executableLookPath
	executableLookPath = fn
	t.Cleanup(func() {
		executableLookPath = original
	})
}

func installFakeCommand(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("install fake command %s failed: %v", name, err)
	}
}

func disableEngineClient(t *testing.T) {
	t.Helper()
	orig := newEngineClient
	newEngineClient = func() (engineClient, error) {
		return nil, errors.New("engine unavailable")
	}
	t.Cleanup(func() {
		newEngineClient = orig
	})
}

func TestPrepareSourceImageSkipsPullWhenImageExistsLocally(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	disableEngineClient(t)
	withExecutableLookPath(t, func(file string) (string, error) {
		return "", errors.New("not found")
	})

	inspectCalls := 0
	inspectPayload := `[{"RepoDigests":["docker.io/library/nginx@sha256:abcd"],"Config":{"Env":["A=B"],"WorkingDir":"/workspace"}}]`

	patches.ApplyFunc(dockerOutput, func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
		if len(args) == 4 && args[0] == "image" && args[1] == "inspect" && args[2] == "--" && args[3] == "docker.io/library/nginx:latest" {
			inspectCalls++
			return []byte(inspectPayload), nil
		}
		if len(args) == 3 && args[0] == "pull" && args[1] == "--" && args[2] == "docker.io/library/nginx:latest" {
			t.Fatal("expected docker pull to be skipped when image exists locally")
		}
		t.Fatalf("unexpected dockerOutput args=%v", args)
		return nil, nil
	})

	got, err := testPrepareSourceImage(context.Background(), &types.CreateTemplateFromImageReq{
		SourceImageRef: "docker.io/library/nginx:latest",
	}, "http://master.example")
	if err != nil {
		t.Fatalf("testPrepareSourceImage failed: %v", err)
	}
	if inspectCalls != 1 {
		t.Fatalf("expected 1 inspect call, got %d", inspectCalls)
	}
	if got == nil || got.Digest != "sha256:abcd" {
		t.Fatalf("unexpected resolved image: %#v", got)
	}
}

func TestPrepareSourceImagePullsAfterLocalInspectMiss(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	disableEngineClient(t)
	withExecutableLookPath(t, func(file string) (string, error) {
		return "", errors.New("not found")
	})

	inspectCalls := 0
	pullCalled := false
	inspectPayload := `[{"RepoDigests":["docker.io/library/nginx@sha256:abcd"],"Config":{"Cmd":["nginx"]}}]`

	patches.ApplyFunc(dockerOutput, func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
		if len(args) == 4 && args[0] == "image" && args[1] == "inspect" && args[2] == "--" && args[3] == "docker.io/library/nginx:latest" {
			inspectCalls++
			if inspectCalls == 1 {
				return nil, errors.New("No such image")
			}
			return []byte(inspectPayload), nil
		}
		if len(args) == 3 && args[0] == "pull" && args[1] == "--" && args[2] == "docker.io/library/nginx:latest" {
			pullCalled = true
			return nil, nil
		}
		t.Fatalf("unexpected dockerOutput args=%v", args)
		return nil, nil
	})

	got, err := testPrepareSourceImage(context.Background(), &types.CreateTemplateFromImageReq{
		SourceImageRef: "docker.io/library/nginx:latest",
	}, "http://master.example")
	if err != nil {
		t.Fatalf("testPrepareSourceImage failed: %v", err)
	}
	if !pullCalled {
		t.Fatal("expected docker pull to run after local inspect miss")
	}
	if inspectCalls != 2 {
		t.Fatalf("expected 2 inspect calls, got %d", inspectCalls)
	}
	if got == nil || got.Digest != "sha256:abcd" {
		t.Fatalf("unexpected resolved image: %#v", got)
	}
}

func TestPrepareSourceImageReturnsPullErrorAfterInspectMiss(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	disableEngineClient(t)
	withExecutableLookPath(t, func(file string) (string, error) {
		return "", errors.New("not found")
	})

	inspectCalls := 0
	patches.ApplyFunc(dockerOutput, func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
		if len(args) == 4 && args[0] == "image" && args[1] == "inspect" && args[2] == "--" && args[3] == "docker.io/library/nginx:latest" {
			inspectCalls++
			return nil, errors.New("No such image")
		}
		if len(args) == 3 && args[0] == "pull" && args[1] == "--" && args[2] == "docker.io/library/nginx:latest" {
			return nil, errors.New("pull denied")
		}
		t.Fatalf("unexpected dockerOutput args=%v", args)
		return nil, nil
	})

	got, err := testPrepareSourceImage(context.Background(), &types.CreateTemplateFromImageReq{
		SourceImageRef: "docker.io/library/nginx:latest",
	}, "http://master.example")
	if err == nil || !strings.Contains(err.Error(), "docker pull docker.io/library/nginx:latest failed") {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil resolved image on error, got %#v", got)
	}
	if inspectCalls != 1 {
		t.Fatalf("expected 1 inspect call before pull failure, got %d", inspectCalls)
	}
}

func TestPrepareSourceImageUsesSkopeoWhenDockerlessToolsAvailable(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	withExecutableLookPath(t, func(file string) (string, error) {
		if file == "skopeo" || file == "umoci" {
			return "/usr/bin/" + file, nil
		}
		return "", errors.New("not found")
	})

	var calls [][]string
	patches.ApplyFunc(skopeoOutput, func(ctx context.Context, authFile string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		if len(args) == 2 && args[0] == "inspect" {
			return []byte(`{"Name":"docker.io/library/nginx","Digest":"sha256:abcd"}`), nil
		}
		if len(args) == 3 && args[0] == "inspect" && args[1] == "--config" {
			return []byte(`{"config":{"Env":["A=B"],"WorkingDir":"/workspace","Cmd":["nginx"]}}`), nil
		}
		t.Fatalf("unexpected skopeoOutput args=%v", args)
		return nil, nil
	})
	patches.ApplyFunc(dockerOutput, func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
		t.Fatal("dockerOutput should not be called when dockerless tools are available")
		return nil, nil
	})

	got, err := testPrepareSourceImage(context.Background(), &types.CreateTemplateFromImageReq{
		SourceImageRef: "docker.io/library/nginx:1.27",
	}, "http://master.example")
	if err != nil {
		t.Fatalf("testPrepareSourceImage failed: %v", err)
	}
	wantCalls := [][]string{
		{"inspect", "docker://docker.io/library/nginx:1.27"},
		{"inspect", "--config", "docker://docker.io/library/nginx:1.27"},
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("unexpected skopeo calls: got %#v want %#v", calls, wantCalls)
	}
	if got.Digest != "docker.io/library/nginx@sha256:abcd" {
		t.Fatalf("unexpected Digest: %q", got.Digest)
	}
	if got.Config.WorkingDir != "/workspace" || !reflect.DeepEqual(got.Config.Cmd, []string{"nginx"}) {
		t.Fatalf("unexpected image Config: %#v", got.Config)
	}
}

func TestExportImageRootfsUsesDockerlessSkopeoUmociWhenAvailable(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	workDir := t.TempDir()
	rootfsDir := filepath.Join(workDir, "rootfs")
	if err := os.MkdirAll(rootfsDir, 0o755); err != nil {
		t.Fatalf("prepare rootfs dir failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootfsDir, "old"), []byte("old"), 0o644); err != nil {
		t.Fatalf("prepare stale rootfs file failed: %v", err)
	}

	type commandCall struct {
		dir  string
		name string
		args []string
	}
	var calls []commandCall
	patches.ApplyFunc(runCommand, func(ctx context.Context, dir, name string, args ...string) error {
		calls = append(calls, commandCall{
			dir:  dir,
			name: name,
			args: append([]string(nil), args...),
		})
		if name == "umoci" {
			bundleDir := args[len(args)-1]
			unpackedRootfsDir := filepath.Join(bundleDir, "rootfs")
			if err := os.MkdirAll(unpackedRootfsDir, 0o755); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(unpackedRootfsDir, "etc-release"), []byte("ok"), 0o644)
		}
		return nil
	})
	patches.ApplyFunc(dockerOutput, func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
		t.Fatal("dockerOutput should not be called by default rootfs export")
		return nil, nil
	})

	source := &PreparedSource{
		LocalRef:   "cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:v1.2.3",
		ExportMode: ExportModeDockerless,
	}
	if err := exportImageRootfs(context.Background(), source, rootfsDir); err != nil {
		t.Fatalf("exportImageRootfs failed: %v", err)
	}

	if len(calls) != 2 {
		t.Fatalf("unexpected command calls: %#v", calls)
	}
	if calls[0].name != "skopeo" {
		t.Fatalf("first command=%q, want skopeo", calls[0].name)
	}
	if len(calls[0].args) != 3 || calls[0].args[0] != "copy" || calls[0].args[1] != "docker://cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:v1.2.3" {
		t.Fatalf("unexpected skopeo args: %#v", calls[0].args)
	}
	ociImageRef := strings.TrimPrefix(calls[0].args[2], "oci:")
	ociDir := strings.TrimSuffix(ociImageRef, ":v1.2.3")
	if ociImageRef == calls[0].args[2] || ociDir == ociImageRef || filepath.Base(ociDir) != "image" || filepath.Dir(filepath.Dir(ociDir)) != workDir {
		t.Fatalf("unexpected OCI image ref: %q", calls[0].args[2])
	}
	wantUmociArgs := []string{
		"unpack",
	}
	if os.Geteuid() != 0 {
		wantUmociArgs = append(wantUmociArgs, "--rootless")
	}
	wantUmociArgs = append(wantUmociArgs, "--image", ociDir+":v1.2.3", filepath.Join(filepath.Dir(ociDir), "bundle"))
	if calls[1].name != "umoci" || !reflect.DeepEqual(calls[1].args, wantUmociArgs) {
		t.Fatalf("unexpected umoci call: %#v", calls[1])
	}
	if _, err := os.Stat(filepath.Join(rootfsDir, "etc-release")); err != nil {
		t.Fatalf("expected unpacked rootfs file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootfsDir, "old")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected stale rootfs file to be removed, stat err=%v", err)
	}
}

func TestExportImageRootfsUsesDockerPathWhenSourceNotDockerless(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	dockerExportCalled := false
	patches.ApplyFunc(dockerlessExportImageRootfs, func(ctx context.Context, source *PreparedSource, destRootfsDir string) error {
		t.Fatal("dockerlessExportImageRootfs should not be called when source was not prepared dockerless")
		return nil
	})
	patches.ApplyFunc(dockerExportImageRootfs, func(ctx context.Context, source *PreparedSource, destRootfsDir string) error {
		dockerExportCalled = true
		return nil
	})

	// ExportMode: ExportModeDocker, so the export must honor the docker path chosen at
	// prepare time even if skopeo/umoci happen to be installed.
	if err := exportImageRootfs(context.Background(), &PreparedSource{LocalRef: "example.com/app:latest", ExportMode: ExportModeDocker}, t.TempDir()); err != nil {
		t.Fatalf("exportImageRootfs failed: %v", err)
	}
	if !dockerExportCalled {
		t.Fatal("expected docker export path to be used")
	}
}

func TestExportImageRootfsRejectsInjectableImageRef(t *testing.T) {
	err := exportImageRootfs(context.Background(), &PreparedSource{LocalRef: "-rm -rf"}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "invalid image reference") {
		t.Fatalf("expected invalid image reference error, got %v", err)
	}
}

func TestExportImageRootfsPassesAuthFileToSkopeoCopy(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	workDir := t.TempDir()
	rootfsDir := filepath.Join(workDir, "rootfs")

	var skopeoArgs []string
	patches.ApplyFunc(runCommand, func(ctx context.Context, dir, name string, args ...string) error {
		if name == "skopeo" {
			skopeoArgs = append([]string(nil), args...)
		}
		if name == "umoci" {
			bundleDir := args[len(args)-1]
			return os.MkdirAll(filepath.Join(bundleDir, "rootfs"), 0o755)
		}
		return nil
	})

	source := &PreparedSource{
		LocalRef:       "example.com/app:latest",
		ExportMode:     ExportModeDockerless,
		SkopeoAuthFile: "/tmp/auth-xyz/auth.json",
	}
	if err := exportImageRootfs(context.Background(), source, rootfsDir); err != nil {
		t.Fatalf("exportImageRootfs failed: %v", err)
	}
	want := []string{"copy", "--authfile", "/tmp/auth-xyz/auth.json", "docker://example.com/app:latest"}
	if len(skopeoArgs) != 5 || !reflect.DeepEqual(skopeoArgs[:4], want) {
		t.Fatalf("expected skopeo copy to receive --authfile, got %#v", skopeoArgs)
	}
}

func TestCreateSkopeoAuthFileWritesScopedCredentials(t *testing.T) {
	authFile, cleanup, err := createSkopeoAuthFile("docker://example.com:5000/ns/app:tag", "alice", "s3cret")
	if err != nil {
		t.Fatalf("createSkopeoAuthFile failed: %v", err)
	}
	defer cleanup()

	info, err := os.Stat(authFile)
	if err != nil {
		t.Fatalf("stat auth file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("auth file mode = %o, want 0600", perm)
	}

	raw, err := os.ReadFile(authFile) // NOCC:Path Traversal()
	if err != nil {
		t.Fatalf("read auth file: %v", err)
	}
	var payload struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal auth file: %v", err)
	}
	entry, ok := payload.Auths["example.com:5000"]
	if !ok {
		t.Fatalf("auth file missing registry host key, got %#v", payload.Auths)
	}
	if entry.Auth != base64.StdEncoding.EncodeToString([]byte("alice:s3cret")) {
		t.Fatalf("unexpected auth value: %q", entry.Auth)
	}

	cleanup()
	if _, err := os.Stat(authFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected auth file removed after cleanup, stat err=%v", err)
	}
}

func TestCreateSkopeoAuthFileNoCredentials(t *testing.T) {
	authFile, cleanup, err := createSkopeoAuthFile("example.com/app:tag", "", "")
	if err != nil {
		t.Fatalf("createSkopeoAuthFile failed: %v", err)
	}
	if authFile != "" {
		t.Fatalf("expected empty auth file path, got %q", authFile)
	}
	if cleanup == nil {
		t.Fatal("expected non-nil cleanup func")
	}
	cleanup() // must be a safe no-op
}

func TestPrepareDockerlessSourceImageCleansAuthFileOnInspectFailure(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	withExecutableLookPath(t, func(file string) (string, error) {
		if file == "skopeo" || file == "umoci" {
			return "/usr/bin/" + file, nil
		}
		return "", errors.New("not found")
	})

	cleanupCalled := false
	patches.ApplyFunc(createSkopeoAuthFile, func(imageRef, username, password string) (string, func(), error) {
		return "/tmp/fake-auth/auth.json", func() { cleanupCalled = true }, nil
	})
	patches.ApplyFunc(skopeoOutput, func(ctx context.Context, authFile string, args ...string) ([]byte, error) {
		return nil, errors.New("inspect boom")
	})

	_, err := testPrepareSourceImage(context.Background(), &types.CreateTemplateFromImageReq{
		SourceImageRef:   "example.com/app:tag",
		RegistryUsername: "alice",
		RegistryPassword: "s3cret",
	}, "http://master.example")
	if err == nil || !strings.Contains(err.Error(), "skopeo inspect") {
		t.Fatalf("expected skopeo inspect error, got %v", err)
	}
	if !cleanupCalled {
		t.Fatal("expected auth file cleanup on inspect failure")
	}
}

func TestSkopeoOutputInsertsAuthFileAfterSubcommand(t *testing.T) {
	binDir := t.TempDir()
	t.Setenv("PATH", binDir)
	installFakeCommand(t, binDir, "skopeo", `printf '%s\n' "$*"`)

	out, err := skopeoOutput(context.Background(), "/tmp/auth.json", "inspect", "docker://example.com/app:latest")
	if err != nil {
		t.Fatalf("skopeoOutput failed: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "inspect --authfile /tmp/auth.json docker://example.com/app:latest" {
		t.Fatalf("unexpected skopeo arg ordering: %q", got)
	}
}

func TestSkopeoOutputOmitsAuthFileWhenEmpty(t *testing.T) {
	binDir := t.TempDir()
	t.Setenv("PATH", binDir)
	installFakeCommand(t, binDir, "skopeo", `printf '%s\n' "$*"`)

	out, err := skopeoOutput(context.Background(), "", "inspect", "docker://example.com/app:latest")
	if err != nil {
		t.Fatalf("skopeoOutput failed: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "inspect docker://example.com/app:latest" {
		t.Fatalf("unexpected skopeo args: %q", got)
	}
}

func TestSkopeoImageDigest(t *testing.T) {
	tests := []struct {
		name     string
		info     skopeoInspectImage
		imageRef string
		want     string
	}{
		{
			name:     "empty digest yields empty",
			info:     skopeoInspectImage{Name: "example.com/app"},
			imageRef: "example.com/app:tag",
			want:     "",
		},
		{
			name:     "prefers name from inspect output",
			info:     skopeoInspectImage{Name: "example.com/app", Digest: "sha256:abcd"},
			imageRef: "ignored:tag",
			want:     "example.com/app@sha256:abcd",
		},
		{
			name:     "falls back to ref name without tag",
			info:     skopeoInspectImage{Digest: "sha256:abcd"},
			imageRef: "example.com:5000/ns/app:tag",
			want:     "example.com:5000/ns/app@sha256:abcd",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := skopeoImageDigest(tt.info, tt.imageRef); got != tt.want {
				t.Fatalf("skopeoImageDigest()=%q, want %q", got, tt.want)
			}
		})
	}
}

func TestSkopeoLayersTotalSize(t *testing.T) {
	info := skopeoInspectImage{
		LayersData: []skopeoInspectLayer{
			{Size: 100},
			{Size: 250},
			{Size: -5}, // ignored
			{Size: 0},  // ignored
		},
	}
	if got := skopeoLayersTotalSize(info); got != 350 {
		t.Fatalf("skopeoLayersTotalSize()=%d, want 350", got)
	}
	if got := skopeoLayersTotalSize(skopeoInspectImage{}); got != 0 {
		t.Fatalf("skopeoLayersTotalSize(empty)=%d, want 0", got)
	}
}

func TestEstimateImageSizeFromInspectDockerlessUsesSkopeoSize(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	patches.ApplyFunc(dockerOutput, func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
		t.Fatal("dockerOutput must not be called for dockerless size estimation")
		return nil, nil
	})

	source := &PreparedSource{
		LocalRef:            "example.com/app:latest",
		ExportMode:          ExportModeDockerless,
		CompressedSizeBytes: 1000,
	}
	got, err := estimateImageSizeFromInspect(context.Background(), source)
	if err != nil {
		t.Fatalf("estimateImageSizeFromInspect failed: %v", err)
	}
	if want := int64(1000 * skopeoInspectSizeMultiplier); got != want {
		t.Fatalf("estimateImageSizeFromInspect()=%d, want %d", got, want)
	}
}

func TestEstimateImageSizeFromInspectDockerlessMissingSize(t *testing.T) {
	source := &PreparedSource{
		LocalRef:   "example.com/app:latest",
		ExportMode: ExportModeDockerless,
	}
	if _, err := estimateImageSizeFromInspect(context.Background(), source); err == nil {
		t.Fatal("expected error when skopeo reports no layer sizes")
	}
}

func TestEstimateImageSizeFromInspectDockerPath(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	patches.ApplyFunc(dockerOutput, func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
		return []byte("500\n"), nil
	})

	source := &PreparedSource{LocalRef: "example.com/app:latest"}
	got, err := estimateImageSizeFromInspect(context.Background(), source)
	if err != nil {
		t.Fatalf("estimateImageSizeFromInspect failed: %v", err)
	}
	if want := int64(500 * dockerInspectSizeMultiplier); got != want {
		t.Fatalf("estimateImageSizeFromInspect()=%d, want %d", got, want)
	}
}

func TestRegistryHostFromImageRefStripsDockerTransport(t *testing.T) {
	if got := registryHostFromImageRef("docker://example.com:5000/ns/app:tag"); got != "example.com:5000" {
		t.Fatalf("registryHostFromImageRef()=%q, want example.com:5000", got)
	}
	if got := registryHostFromImageRef("library/nginx:latest"); got != "docker.io" {
		t.Fatalf("registryHostFromImageRef()=%q, want docker.io", got)
	}
}

func TestSkopeoDockerImageRefKeepsExistingTransport(t *testing.T) {
	got := skopeoDockerImageRef("docker://example.com/app:latest")
	if got != "docker://example.com/app:latest" {
		t.Fatalf("skopeoDockerImageRef()=%q", got)
	}
}

func TestValidateImageRef(t *testing.T) {
	valid := []string{
		"docker://registry.example.com/image:latest",
		"registry.example.com:5000/ns/app:1.2.3",
		"nginx",
		"library/nginx",
		"library/nginx@sha256:" + strings.Repeat("a", 64),
		"reg.io/my_app.name/sub-component:tag_1.0",
	}
	for _, ref := range valid {
		if err := ValidateImageRef(ref); err != nil {
			t.Errorf("ValidateImageRef(%q) returned error, want nil: %v", ref, err)
		}
	}

	invalid := []string{
		"",
		"docker://",
		"-rm -rf",
		"--authfile=/etc/shadow",
		"registry.example.com/image --authfile /etc/shadow",
		"registry.example.com/image\t--flag",
		"registry.example.com/image\n--flag",
		"image;rm -rf /",
		"image$(whoami)",
		"image`whoami`",
		"library/nginx:",
		"library/nginx@sha256:not-a-digest",
		"docker://docker://library/nginx",
	}
	for _, ref := range invalid {
		if err := ValidateImageRef(ref); err == nil {
			t.Errorf("ValidateImageRef(%q) returned nil, want error", ref)
		}
	}
}

func TestPrepareDockerSourceRejectsInvalidImageRefBeforeEngineAccess(t *testing.T) {
	original := newEngineClient
	engineCalled := false
	newEngineClient = func() (engineClient, error) {
		engineCalled = true
		return nil, errors.New("must not be called")
	}
	t.Cleanup(func() {
		newEngineClient = original
	})

	_, err := prepareDockerSource(context.Background(), SourceSpec{ImageRef: "docker://--help"})
	if err == nil || !strings.Contains(err.Error(), "invalid image reference") {
		t.Fatalf("prepareDockerSource error=%v, want validation failure", err)
	}
	if engineCalled {
		t.Fatal("engine client was accessed before image reference validation")
	}
}

func TestDockerLoginTerminatesOptionsBeforeRegistry(t *testing.T) {
	tmpDir := t.TempDir()
	argsFile := filepath.Join(tmpDir, "args")
	stdinFile := filepath.Join(tmpDir, "stdin")
	t.Setenv("DOCKER_ARGS_FILE", argsFile)
	t.Setenv("DOCKER_STDIN_FILE", stdinFile)
	t.Setenv("PATH", tmpDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	installFakeCommand(t, tmpDir, "docker", `printf '%s\n' "$@" > "$DOCKER_ARGS_FILE"
cat > "$DOCKER_STDIN_FILE"`)

	err := dockerLogin(
		context.Background(),
		"/tmp/cubemaster-docker-config",
		"registry.example.com/ns/app:latest",
		"-v",
		"s3cret",
	)
	if err != nil {
		t.Fatalf("dockerLogin failed: %v", err)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read docker args: %v", err)
	}
	wantArgs := strings.Join([]string{
		"--config",
		"/tmp/cubemaster-docker-config",
		"login",
		"-u",
		"-v",
		"--password-stdin",
		"--",
		"registry.example.com",
		"",
	}, "\n")
	if string(args) != wantArgs {
		t.Fatalf("docker args=%q, want %q", string(args), wantArgs)
	}
	stdin, err := os.ReadFile(stdinFile)
	if err != nil {
		t.Fatalf("read docker stdin: %v", err)
	}
	if string(stdin) != "s3cret" {
		t.Fatalf("docker stdin=%q, want password only", string(stdin))
	}
}

func TestSplitImageRef(t *testing.T) {
	tests := []struct {
		name     string
		imageRef string
		wantName string
		wantTag  string
	}{
		{
			name:     "no colon",
			imageRef: "library/nginx",
			wantName: "library/nginx",
			wantTag:  "",
		},
		{
			name:     "port but no tag",
			imageRef: "registry:5000/image",
			wantName: "registry:5000/image",
			wantTag:  "",
		},
		{
			name:     "port with tag",
			imageRef: "registry:5000/image:v1",
			wantName: "registry:5000/image",
			wantTag:  "v1",
		},
		{
			name:     "tag and digest after docker prefix",
			imageRef: "docker://example.com/ns/app:stable@sha256:abcd",
			wantName: "example.com/ns/app",
			wantTag:  "stable",
		},
		{
			name:     "digest without tag",
			imageRef: "example.com/ns/app@sha256:abcd",
			wantName: "example.com/ns/app",
			wantTag:  "",
		},
		{
			name:     "bare name with tag",
			imageRef: "nginx:latest",
			wantName: "nginx",
			wantTag:  "latest",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotName, gotTag := splitImageRef(tt.imageRef)
			if gotName != tt.wantName || gotTag != tt.wantTag {
				t.Fatalf("splitImageRef(%q)=(%q,%q), want (%q,%q)",
					tt.imageRef, gotName, gotTag, tt.wantName, tt.wantTag)
			}
			if got := imageTagFromRef(tt.imageRef); got != tt.wantTag {
				t.Fatalf("imageTagFromRef(%q)=%q, want %q", tt.imageRef, got, tt.wantTag)
			}
			if got := imageNameWithoutTagDigest(tt.imageRef); got != tt.wantName {
				t.Fatalf("imageNameWithoutTagDigest(%q)=%q, want %q", tt.imageRef, got, tt.wantName)
			}
		})
	}
}

func TestOciLayoutImageRefUsesExplicitSourceTag(t *testing.T) {
	tests := []struct {
		name     string
		imageRef string
		want     string
	}{
		{
			name:     "tagged image",
			imageRef: "example.com/app:v1.2.3",
			want:     "/tmp/image:v1.2.3",
		},
		{
			name:     "registry port with tag",
			imageRef: "example.com:5000/ns/app:release",
			want:     "/tmp/image:release",
		},
		{
			name:     "digest with tag",
			imageRef: "docker://example.com/ns/app:stable@sha256:abcd",
			want:     "/tmp/image:stable",
		},
		{
			name:     "untagged image",
			imageRef: "example.com/ns/app",
			want:     "/tmp/image",
		},
		{
			name:     "digest without tag",
			imageRef: "example.com/ns/app@sha256:abcd",
			want:     "/tmp/image",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ociLayoutImageRef("/tmp/image", tt.imageRef)
			if got != tt.want {
				t.Fatalf("ociLayoutImageRef()=%q, want %q", got, tt.want)
			}
		})
	}
}

func TestEnsureArtifactBuildPreflightAllowsDockerlessWithoutDockerOrTar(t *testing.T) {
	binDir := t.TempDir()
	t.Setenv("PATH", binDir)
	for _, cmd := range []string{"truncate", "cp", "skopeo", "umoci"} {
		installFakeCommand(t, binDir, cmd, "exit 0")
	}
	installFakeCommand(t, binDir, "mkfs.ext4", "echo 'mkfs.ext4 help supports -d'")

	if err := EnsureArtifactBuildPreflight(context.Background()); err != nil {
		t.Fatalf("EnsureArtifactBuildPreflight failed: %v", err)
	}
}

func TestEnsureArtifactBuildPreflightRequiresTarForDockerFallback(t *testing.T) {
	binDir := t.TempDir()
	t.Setenv("PATH", binDir)
	for _, cmd := range []string{"docker", "mkfs.ext4", "truncate", "cp", "skopeo"} {
		installFakeCommand(t, binDir, cmd, "exit 0")
	}

	err := EnsureArtifactBuildPreflight(context.Background())
	if err == nil || !strings.Contains(err.Error(), `required command "tar"`) {
		t.Fatalf("unexpected preflight error: %v", err)
	}
}

func TestIsLocalFastFSFallsBackToParentForMissingArtifactDir(t *testing.T) {
	storeRoot := t.TempDir()
	existingResult := isLocalFastFS(storeRoot)
	missingArtifactDir := filepath.Join(storeRoot, "artifact-1")

	if got := isLocalFastFS(missingArtifactDir); got != existingResult {
		t.Fatalf("isLocalFastFS missing artifact dir=%v, want parent result %v", got, existingResult)
	}
}

func TestLoopMountExt4EnabledParsesBoolValues(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "unset", value: "", want: false},
		{name: "true lowercase", value: "true", want: true},
		{name: "true uppercase", value: "TRUE", want: true},
		{name: "one", value: "1", want: true},
		{name: "false", value: "false", want: false},
		{name: "invalid", value: "yes", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CUBEMASTER_LOOP_MOUNT_EXT4_ENABLED", tt.value)
			if got := loopMountExt4Enabled(); got != tt.want {
				t.Fatalf("loopMountExt4Enabled()=%v, want %v", got, tt.want)
			}
		})
	}
}

func TestArtifactStoreRootDirDefaultAndEnvOverride(t *testing.T) {
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", "")
	if got := ArtifactStoreRootDir(); got != defaultArtifactStoreDir {
		t.Fatalf("ArtifactStoreRootDir default=%q, want %q", got, defaultArtifactStoreDir)
	}

	customDir := filepath.Join(t.TempDir(), "artifact-store")
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", customDir)
	if got := ArtifactStoreRootDir(); got != customDir {
		t.Fatalf("ArtifactStoreRootDir=%q, want %q", got, customDir)
	}
}

func TestResolveArtifactStoreDirFallsBackWhenDefaultUnavailable(t *testing.T) {
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", "")
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	callCount := 0
	patches.ApplyFunc(os.MkdirAll, func(path string, perm os.FileMode) error {
		callCount++
		if strings.Contains(path, defaultArtifactStoreDir) {
			return errors.New("permission denied")
		}
		return nil
	})

	dir, err := ResolveArtifactStoreDir(context.Background(), "artifact-1")
	if err != nil {
		t.Fatalf("ResolveArtifactStoreDir failed: %v", err)
	}
	want := filepath.Join(ArtifactFallbackStoreRootDir(), "artifact-1")
	if dir != want {
		t.Fatalf("ResolveArtifactStoreDir=%q, want %q", dir, want)
	}
	if callCount < 2 {
		t.Fatalf("expected fallback path preparation, callCount=%d", callCount)
	}
}

func TestBuildExt4StreamingSuccessSkipsPhase1(t *testing.T) {
	workRoot := t.TempDir()
	storeRoot := t.TempDir()
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_DIR", workRoot)
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", storeRoot)

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(loopMountExt4Enabled, func() bool { return true })
	patches.ApplyFunc(canUseLoopMount, func() bool { return true })
	patches.ApplyFunc(estimateImageSizeFromInspect, func(ctx context.Context, source *PreparedSource) (int64, error) {
		return 1024, nil
	})
	patches.ApplyFunc(checkDiskSpace, func(ctx context.Context, storeDir string, estimatedSizeBytes int64) error {
		return nil
	})
	streamingCalled := false
	patches.ApplyFunc(createExt4ImageStreaming, func(ctx context.Context, source *PreparedSource, workDir, ext4Path string, estimatedSizeBytes int64, postExport func(context.Context, string) error) error {
		streamingCalled = true
		if err := os.MkdirAll(workDir, 0o755); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(ext4Path), 0o755); err != nil {
			return err
		}
		return os.WriteFile(ext4Path, []byte("streaming"), 0o644)
	})
	patches.ApplyFunc(computeFileSHA256, func(path string) (string, int64, error) {
		return "sha-streaming", 9, nil
	})
	patches.ApplyFunc(exportImageRootfs, func(ctx context.Context, source *PreparedSource, destRootfsDir string) error {
		t.Fatal("phase-1 export should not run after streaming success")
		return nil
	})
	patches.ApplyFunc(createExt4Image, func(ctx context.Context, rootfsDir, ext4Path string) error {
		t.Fatal("phase-1 ext4 creation should not run after streaming success")
		return nil
	})

	result, err := BuildExt4(context.Background(), &PreparedSource{LocalRef: "docker.io/library/nginx:latest", ExportMode: ExportModeNative}, BuildOptions{ArtifactID: "artifact-stream"})
	if err != nil {
		t.Fatalf("BuildExt4 failed: %v", err)
	}
	if !streamingCalled {
		t.Fatal("expected streaming build to run")
	}
	if result.SHA256 != "sha-streaming" || result.SizeBytes != 9 {
		t.Fatalf("unexpected result: %#v", result)
	}
	if _, err := os.Stat(filepath.Join(workRoot, "artifact-stream")); !os.IsNotExist(err) {
		t.Fatalf("workDir should be removed after streaming success, stat err=%v", err)
	}
}

func TestBuildExt4StreamingWithPostExport(t *testing.T) {
	workRoot := t.TempDir()
	storeRoot := t.TempDir()
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_DIR", workRoot)
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", storeRoot)

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFuncReturn(loopMountExt4Enabled, true)
	patches.ApplyFuncReturn(canUseLoopMount, true)
	patches.ApplyFuncReturn(estimateImageSizeFromInspect, int64(1024), nil)
	patches.ApplyFuncReturn(checkDiskSpace, nil)
	postExportCalled := false
	streamingCalled := false
	patches.ApplyFunc(createExt4ImageStreaming, func(ctx context.Context, source *PreparedSource, workDir, ext4Path string, estimatedSizeBytes int64, postExport func(context.Context, string) error) error {
		streamingCalled = true
		if postExport != nil {
			_ = postExport(ctx, workDir)
		}
		if err := os.MkdirAll(workDir, 0o755); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(ext4Path), 0o755); err != nil {
			return err
		}
		return os.WriteFile(ext4Path, []byte("streaming-post"), 0o644)
	})
	patches.ApplyFunc(computeFileSHA256, func(path string) (string, int64, error) {
		return "sha-streaming-post", 14, nil
	})
	patches.ApplyFunc(exportImageRootfs, func(ctx context.Context, source *PreparedSource, destRootfsDir string) error {
		t.Fatal("phase-1 export should not run after streaming success")
		return nil
	})
	patches.ApplyFunc(createExt4Image, func(ctx context.Context, rootfsDir, ext4Path string) error {
		t.Fatal("phase-1 ext4 creation should not run after streaming success")
		return nil
	})

	postHook := func(ctx context.Context, mountPoint string) error {
		postExportCalled = true
		return nil
	}

	result, err := BuildExt4(context.Background(), &PreparedSource{LocalRef: "docker.io/library/nginx:latest", ExportMode: ExportModeDocker}, BuildOptions{ArtifactID: "artifact-post", PostRootfsExport: postHook})
	if err != nil {
		t.Fatalf("BuildExt4 failed: %v", err)
	}
	if !streamingCalled {
		t.Fatal("expected streaming build to run")
	}
	if !postExportCalled {
		t.Fatal("expected postExport hook to be called")
	}
	if result.SHA256 != "sha-streaming-post" || result.SizeBytes != 14 {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestBuildExt4StreamingFailureFallsBackToPhase1(t *testing.T) {
	workRoot := t.TempDir()
	storeRoot := t.TempDir()
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_DIR", workRoot)
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", storeRoot)

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(loopMountExt4Enabled, func() bool { return true })
	patches.ApplyFunc(canUseLoopMount, func() bool { return true })
	patches.ApplyFunc(estimateImageSizeFromInspect, func(ctx context.Context, source *PreparedSource) (int64, error) {
		return 1024, nil
	})
	patches.ApplyFunc(checkDiskSpace, func(ctx context.Context, storeDir string, estimatedSizeBytes int64) error {
		return nil
	})
	patches.ApplyFunc(isLocalFastFS, func(path string) bool { return true })
	patches.ApplyFunc(createExt4ImageStreaming, func(ctx context.Context, source *PreparedSource, workDir, ext4Path string, estimatedSizeBytes int64, postExport func(context.Context, string) error) error {
		if err := os.MkdirAll(workDir, 0o755); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(ext4Path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(ext4Path, []byte("partial"), 0o644); err != nil {
			return err
		}
		return errors.New("streaming failed")
	})
	patches.ApplyFunc(exportImageRootfs, func(ctx context.Context, source *PreparedSource, destRootfsDir string) error {
		return os.MkdirAll(destRootfsDir, 0o755)
	})
	patches.ApplyFunc(createExt4Image, func(ctx context.Context, rootfsDir, ext4Path string) error {
		if _, err := os.Stat(ext4Path); !os.IsNotExist(err) {
			t.Fatalf("partial streaming ext4 should be removed before phase-1, stat err=%v", err)
		}
		return os.WriteFile(ext4Path, []byte("phase-1"), 0o644)
	})
	patches.ApplyFunc(computeFileSHA256, func(path string) (string, int64, error) {
		return "sha-phase-1", 7, nil
	})

	result, err := BuildExt4(context.Background(), &PreparedSource{LocalRef: "docker.io/library/nginx:latest"}, BuildOptions{ArtifactID: "artifact-fallback"})
	if err != nil {
		t.Fatalf("BuildExt4 failed: %v", err)
	}
	if result.SHA256 != "sha-phase-1" || result.SizeBytes != 7 {
		t.Fatalf("unexpected result: %#v", result)
	}
	if _, err := os.Stat(filepath.Join(storeRoot, "artifact-fallback", "rootfs")); !os.IsNotExist(err) {
		t.Fatalf("rootfs dir should be removed after successful phase-1 build, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(storeRoot, "artifact-fallback", "artifact-fallback.ext4")); err != nil {
		t.Fatalf("ext4 should remain after successful phase-1 build: %v", err)
	}
}

func TestBuildExt4Phase1FailureCleansStoreDir(t *testing.T) {
	workRoot := t.TempDir()
	storeRoot := t.TempDir()
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_DIR", workRoot)
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", storeRoot)

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(loopMountExt4Enabled, func() bool { return false })
	patches.ApplyFunc(estimateImageSizeFromInspect, func(ctx context.Context, source *PreparedSource) (int64, error) {
		return 1024, nil
	})
	patches.ApplyFunc(checkDiskSpace, func(ctx context.Context, storeDir string, estimatedSizeBytes int64) error {
		return nil
	})
	patches.ApplyFunc(isLocalFastFS, func(path string) bool { return true })
	patches.ApplyFunc(exportImageRootfs, func(ctx context.Context, source *PreparedSource, destRootfsDir string) error {
		if err := os.MkdirAll(destRootfsDir, 0o755); err != nil {
			return err
		}
		return errors.New("export failed")
	})

	_, err := BuildExt4(context.Background(), &PreparedSource{LocalRef: "docker.io/library/nginx:latest"}, BuildOptions{ArtifactID: "artifact-fail"})
	if err == nil || !strings.Contains(err.Error(), "export failed") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(storeRoot, "artifact-fail")); !os.IsNotExist(statErr) {
		t.Fatalf("storeDir should be removed on phase-1 failure, stat err=%v", statErr)
	}
}

func TestPrepareLocalSourceUsesDockerlessWhenAvailable(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	withExecutableLookPath(t, func(file string) (string, error) {
		if file == "skopeo" || file == "umoci" {
			return "/usr/bin/" + file, nil
		}
		return "", errors.New("not found")
	})
	patches.ApplyFunc(skopeoOutput, func(ctx context.Context, authFile string, args ...string) ([]byte, error) {
		if len(args) == 2 && args[0] == "inspect" {
			return []byte(`{"Name":"docker.io/library/nginx","Digest":"sha256:abcd","LayersData":[{"Size":10}]}`), nil
		}
		if len(args) == 3 && args[0] == "inspect" && args[1] == "--config" {
			return []byte(`{"config":{"Cmd":["nginx"]}}`), nil
		}
		t.Fatalf("unexpected skopeo args=%v", args)
		return nil, nil
	})
	patches.ApplyFunc(dockerOutput, func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
		t.Fatal("dockerOutput should not be called for dockerless redo source")
		return nil, nil
	})

	got, err := PrepareLocalSource(context.Background(), SourceSpec{ImageRef: "docker.io/library/nginx:latest", DownloadBaseURL: "http://master.example"})
	if err != nil {
		t.Fatalf("PrepareLocalSource failed: %v", err)
	}
	if got.ExportMode != ExportModeDockerless {
		t.Fatalf("expected dockerless source, got %#v", got)
	}
	if got.CompressedSizeBytes != 10 {
		t.Fatalf("CompressedSizeBytes=%d, want 10", got.CompressedSizeBytes)
	}
}

func TestPrepareLocalSourceRequiresLocalDockerImage(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	withExecutableLookPath(t, func(file string) (string, error) {
		return "", errors.New("not found")
	})
	patches.ApplyFunc(dockerOutput, func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
		return nil, errors.New("No such image")
	})

	_, err := PrepareLocalSource(context.Background(), SourceSpec{ImageRef: "private.example/app:latest"})
	if err == nil || !strings.Contains(err.Error(), "redo requires source image private.example/app:latest to still exist locally") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPrepareLocalSourceRejectsInvalidImageRef(t *testing.T) {
	invalidRefs := []string{
		"",
		"-rm -rf",
		"image;rm -rf /",
		"image$(whoami)",
		"image\n--flag",
	}
	for _, ref := range invalidRefs {
		_, err := PrepareLocalSource(context.Background(), SourceSpec{ImageRef: ref})
		if err == nil {
			t.Errorf("PrepareLocalSource(%q) returned nil error, want validation failure", ref)
			continue
		}
		// ValidateImageRef returns "empty image reference" for blank refs
		// and "invalid image reference" for refs with forbidden characters.
		if !strings.Contains(err.Error(), "invalid image reference") && !strings.Contains(err.Error(), "empty image reference") {
			t.Errorf("PrepareLocalSource(%q) error=%v, want validation failure", ref, err)
		}
	}
}
