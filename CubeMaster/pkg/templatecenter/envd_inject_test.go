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

func fakeEnvdBinary(s string) []byte {
	return append([]byte{0x7f, 'E', 'L', 'F'}, []byte(s)...)
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

func TestNewEnvdInjectionPayloadFromBytesComputesSHA(t *testing.T) {
	data := fakeEnvdBinary("uploaded")
	payload, err := NewEnvdInjectionPayloadFromBytes(data)
	assert.NoError(t, err)
	assert.Equal(t, data, payload.Data)
	sum := sha256.Sum256(data)
	assert.Equal(t, hex.EncodeToString(sum[:]), payload.SHA256)
}

func TestNewEnvdInjectionPayloadFromBytesRejectsInvalidPayload(t *testing.T) {
	tests := []struct {
		name    string
		data    []byte
		wantErr string
	}{
		{name: "empty", data: nil, wantErr: "must not be empty"},
		{name: "not_elf", data: []byte("#!/bin/sh"), wantErr: "ELF"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload, err := NewEnvdInjectionPayloadFromBytes(tt.data)
			assert.Error(t, err)
			assert.Nil(t, payload)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestEnvdInjectionPayloadReleaseData(t *testing.T) {
	payload, err := NewEnvdInjectionPayloadFromBytes(fakeEnvdBinary("uploaded"))
	assert.NoError(t, err)
	assert.NotEmpty(t, payload.Data)

	payload.ReleaseData()

	assert.Nil(t, payload.Data)
	assert.NotEmpty(t, payload.SHA256)
}

func TestInjectEnvdPayloadIntoRootfsCopiesUploadedBinary(t *testing.T) {
	want := fakeEnvdBinary("ENVD-PAYLOAD-CONTENT")
	payload, err := NewEnvdInjectionPayloadFromBytes(want)
	assert.NoError(t, err)
	rootfs := t.TempDir()

	gotSHA, err := injectEnvdPayloadIntoRootfs(context.Background(), rootfs, payload)
	assert.NoError(t, err)
	assert.Equal(t, payload.SHA256, gotSHA)

	dst := filepath.Join(rootfs, constants.CubeEnvdInImagePath)
	got, err := os.ReadFile(dst)
	assert.NoError(t, err)
	assert.Equal(t, want, got)

	st, err := os.Stat(dst)
	assert.NoError(t, err)
	assert.Equal(t, os.FileMode(envdInjectionFileMode), st.Mode().Perm())
}

func TestInjectEnvdPayloadIntoRootfsNilPayloadLeavesRootfsUntouched(t *testing.T) {
	rootfs := t.TempDir()
	sha, err := injectEnvdPayloadIntoRootfs(context.Background(), rootfs, nil)
	assert.NoError(t, err)
	assert.Empty(t, sha)
	_, err = os.Stat(filepath.Join(rootfs, constants.CubeEnvdInImagePath))
	assert.True(t, os.IsNotExist(err))
}

func TestBuildTemplateSpecFingerprintEnvdInfluence(t *testing.T) {
	envdReq := reqWithEnvdAnnotation("true")
	envdReq.SourceImageRef = "ubuntu:22.04"
	payloadV1, err := NewEnvdInjectionPayloadFromBytes(fakeEnvdBinary("envd-v1"))
	assert.NoError(t, err)
	plainReq := &types.CreateTemplateFromImageReq{SourceImageRef: "ubuntu:22.04"}

	fpEnvdV1 := buildTemplateSpecFingerprintWithEnvdSHA(envdReq, "sha256:src", "", payloadV1.SHA256)
	fpPlain := buildTemplateSpecFingerprint(plainReq, "sha256:src")
	assert.NotEqual(t, fpEnvdV1, fpPlain)

	payloadV2, err := NewEnvdInjectionPayloadFromBytes(fakeEnvdBinary("envd-v2"))
	assert.NoError(t, err)
	fpEnvdV2 := buildTemplateSpecFingerprintWithEnvdSHA(envdReq, "sha256:src", "", payloadV2.SHA256)
	assert.NotEqual(t, fpEnvdV1, fpEnvdV2)

	disableReq := reqWithEnvdAnnotation("false")
	disableReq.SourceImageRef = "ubuntu:22.04"
	fpDisable := buildTemplateSpecFingerprint(disableReq, "sha256:src")
	fpDisableAgain := buildTemplateSpecFingerprint(disableReq, "sha256:src")
	assert.Equal(t, fpDisable, fpDisableAgain)
}
