// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func stageEnvdHostBinary(t *testing.T, content []byte) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, envdBinaryName)
	if err := os.WriteFile(path, content, envdInjectionFileMode); err != nil {
		t.Fatalf("stage envd stub: %v", err)
	}
	t.Setenv(envdHostDirEnv, dir)
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func reqWithEnvdAnnotation(value string) *types.CreateTemplateFromImageReq {
	return &types.CreateTemplateFromImageReq{
		ContainerOverrides: &types.ContainerOverrides{
			Annotations: map[string]string{
				constants.CubeAnnotationsInjectEnvd: value,
			},
		},
	}
}

func Test_injectEnvdIntoRootfs_optIn_copiesBinary(t *testing.T) {
	want := []byte("ENVD-STUB-CONTENT")
	wantSHA := stageEnvdHostBinary(t, want)
	rootfs := t.TempDir()

	gotSHA, err := injectEnvdIntoRootfs(context.Background(), rootfs, reqWithEnvdAnnotation("true"))
	assert.NoError(t, err)
	assert.Equal(t, wantSHA, gotSHA)

	dst := filepath.Join(rootfs, constants.CubeEnvdInImagePath)
	got, err := os.ReadFile(dst)
	assert.NoError(t, err)
	assert.Equal(t, want, got)

	st, err := os.Stat(dst)
	assert.NoError(t, err)
	assert.Equal(t, os.FileMode(envdInjectionFileMode), st.Mode().Perm())
}

func Test_injectEnvdIntoRootfs_disabled_leavesRootfsUntouched(t *testing.T) {
	stageEnvdHostBinary(t, []byte("not-used"))

	tests := []struct {
		name string
		req  *types.CreateTemplateFromImageReq
	}{
		{name: "nil_request", req: nil},
		{name: "no_overrides", req: &types.CreateTemplateFromImageReq{}},
		{name: "no_annotations", req: &types.CreateTemplateFromImageReq{ContainerOverrides: &types.ContainerOverrides{}}},
		{name: "annotation_false", req: reqWithEnvdAnnotation("false")},
		{name: "annotation_other", req: reqWithEnvdAnnotation("yes")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rootfs := t.TempDir()
			sha, err := injectEnvdIntoRootfs(context.Background(), rootfs, tt.req)
			assert.NoError(t, err)
			assert.Empty(t, sha)
			_, err = os.Stat(filepath.Join(rootfs, constants.CubeEnvdInImagePath))
			assert.True(t, os.IsNotExist(err))
		})
	}
}

func Test_injectEnvdPayloadIntoRootfs_doesNotRereadHostBinary(t *testing.T) {
	want := []byte("ENVD-PAYLOAD-CONTENT")
	dir := t.TempDir()
	path := filepath.Join(dir, envdBinaryName)
	assert.NoError(t, os.WriteFile(path, want, envdInjectionFileMode))
	t.Setenv(envdHostDirEnv, dir)
	req := reqWithEnvdAnnotation("true")
	payload, err := prepareEnvdInjectionPayload(req)
	assert.NoError(t, err)
	assert.NoError(t, os.Remove(path))

	rootfs := t.TempDir()
	gotSHA, err := injectEnvdPayloadIntoRootfs(context.Background(), rootfs, payload)
	assert.NoError(t, err)
	assert.Equal(t, payload.SHA256, gotSHA)
	got, err := os.ReadFile(filepath.Join(rootfs, constants.CubeEnvdInImagePath))
	assert.NoError(t, err)
	assert.Equal(t, want, got)
}

func Test_injectEnvdIntoRootfs_usesEnvdHostDir(t *testing.T) {
	want := []byte("ENVD-CUSTOM-DIR-CONTENT")
	dir := t.TempDir()
	path := filepath.Join(dir, envdBinaryName)
	assert.NoError(t, os.WriteFile(path, want, envdInjectionFileMode))
	t.Setenv(envdHostDirEnv, dir)
	sum := sha256.Sum256(want)
	wantSHA := hex.EncodeToString(sum[:])
	req := reqWithEnvdAnnotation("true")
	rootfs := t.TempDir()

	gotSHA, err := injectEnvdIntoRootfs(context.Background(), rootfs, req)
	assert.NoError(t, err)
	assert.Equal(t, wantSHA, gotSHA)
	got, err := os.ReadFile(filepath.Join(rootfs, constants.CubeEnvdInImagePath))
	assert.NoError(t, err)
	assert.Equal(t, want, got)
}

func Test_injectEnvdIntoRootfs_missingBinary_failsLoud(t *testing.T) {
	t.Setenv(envdHostDirEnv, "/nonexistent-dir-for-envd-test")
	rootfs := t.TempDir()

	sha, err := injectEnvdIntoRootfs(context.Background(), rootfs, reqWithEnvdAnnotation("true"))
	assert.Error(t, err)
	assert.Empty(t, sha)
	_, statErr := os.Stat(filepath.Join(rootfs, constants.CubeEnvdInImagePath))
	assert.True(t, os.IsNotExist(statErr))
}

func Test_buildTemplateSpecFingerprint_envdInfluence(t *testing.T) {
	stageEnvdHostBinary(t, []byte("envd-v1"))
	envdReq := reqWithEnvdAnnotation("true")
	envdReq.SourceImageRef = "ubuntu:22.04"
	payloadV1, err := prepareEnvdInjectionPayload(envdReq)
	assert.NoError(t, err)
	plainReq := &types.CreateTemplateFromImageReq{SourceImageRef: "ubuntu:22.04"}

	fpEnvdV1 := buildTemplateSpecFingerprintWithEnvdSHA(envdReq, "sha256:src", "", payloadV1.SHA256)
	fpPlain := buildTemplateSpecFingerprint(plainReq, "sha256:src")
	assert.NotEqual(t, fpEnvdV1, fpPlain)

	stageEnvdHostBinary(t, []byte("envd-v2"))
	payloadV2, err := prepareEnvdInjectionPayload(envdReq)
	assert.NoError(t, err)
	fpEnvdV2 := buildTemplateSpecFingerprintWithEnvdSHA(envdReq, "sha256:src", "", payloadV2.SHA256)
	assert.NotEqual(t, fpEnvdV1, fpEnvdV2)

	disableReq := reqWithEnvdAnnotation("false")
	disableReq.SourceImageRef = "ubuntu:22.04"
	fpDisable := buildTemplateSpecFingerprint(disableReq, "sha256:src")
	stageEnvdHostBinary(t, []byte("envd-v3"))
	fpDisableAgain := buildTemplateSpecFingerprint(disableReq, "sha256:src")
	assert.Equal(t, fpDisable, fpDisableAgain)
}
