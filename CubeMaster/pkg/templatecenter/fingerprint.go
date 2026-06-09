// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func unmarshalTemplateImageJobRequest(payload string) (*types.CreateTemplateFromImageReq, error) {
	req := &types.CreateTemplateFromImageReq{}
	if err := json.Unmarshal([]byte(payload), req); err != nil {
		return nil, err
	}
	req.Request = &types.Request{RequestID: uuid.NewString()}
	return normalizeTemplateImageRequest(req)
}

func buildTemplateSpecFingerprint(req *types.CreateTemplateFromImageReq, sourceImageDigest string) string {
	type fingerprintPayload struct {
		SourceImageDigest  string                    `json:"source_image_digest"`
		WritableLayerSize  string                    `json:"writable_layer_size"`
		ExposedPorts       []int32                   `json:"exposed_ports,omitempty"`
		InstanceType       string                    `json:"instance_type"`
		NetworkType        string                    `json:"network_type"`
		ContainerOverrides *types.ContainerOverrides `json:"container_overrides,omitempty"`
	}
	payload, _ := json.Marshal(fingerprintPayload{
		SourceImageDigest:  sourceImageDigest,
		WritableLayerSize:  req.WritableLayerSize,
		ExposedPorts:       req.ExposedPorts,
		InstanceType:       req.InstanceType,
		NetworkType:        req.NetworkType,
		ContainerOverrides: req.ContainerOverrides,
	})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func buildArtifactID(fingerprint string) string {
	return "rfs-" + fingerprint[:24]
}

func marshalTemplateImageJobRequest(req *types.CreateTemplateFromImageReq) (string, error) {
	if req == nil {
		return "", errors.New("request is nil")
	}
	cloned := *req
	cloned.RegistryPassword = ""
	cloned.Request = nil
	payload, err := json.Marshal(&cloned)
	if err != nil {
		return "", err
	}
	return string(payload), nil
}

func marshalTemplateCommitJobRequest(req *types.CreateCubeSandboxReq) (string, error) {
	if req == nil {
		return "", errors.New("request is nil")
	}
	cloned, err := cloneCreateRequest(req)
	if err != nil {
		return "", err
	}
	cloned.Request = nil
	payload, err := json.Marshal(cloned)
	if err != nil {
		return "", err
	}
	return string(payload), nil
}

func buildCommitTemplateSpecFingerprintFromSnapshot(requestSnapshot string) string {
	sum := sha256.Sum256([]byte(requestSnapshot))
	return hex.EncodeToString(sum[:])
}

// buildCommitTemplateSpecFingerprint preserves the pre-merge call signature
// used by snapshot_ops.go (it takes the unmarshaled request, hashes the
// canonical JSON form, and returns the same value as the *FromSnapshot
// helper would for the corresponding payload). Keeping a thin wrapper here
// avoids touching every snapshot call site while still routing fingerprint
// generation through a single canonical encoder.
func buildCommitTemplateSpecFingerprint(req *types.CreateCubeSandboxReq) string {
	payload, _ := marshalTemplateCommitJobRequest(req)
	return buildCommitTemplateSpecFingerprintFromSnapshot(payload)
}
