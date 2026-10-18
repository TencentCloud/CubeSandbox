// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func unmarshalTemplateImageJobRequest(payload string) (*types.CreateTemplateFromImageReq, error) {
	req := &types.CreateTemplateFromImageReq{}
	if err := json.Unmarshal([]byte(payload), req); err != nil {
		return nil, err
	}
	// Preserve the TemplateID exactly as it was persisted in this job's
	// RequestJSON snapshot. normalizeTemplateImageRequest unconditionally
	// overwrites TemplateID with a FRESH random value (by design, for the
	// create-submission path, where a client-supplied ID must always be
	// ignored) -- but this function decodes an ALREADY-SUBMITTED job's
	// snapshot, whose TemplateID was already generated once at submit time
	// and is the same ID the job row, template definition, and replicas were
	// created under. Letting normalize regenerate it here silently produced a
	// SECOND, different template_id (e.g. remote_build_resume.go's resume
	// path used it to register the definition + replicas), leaving the
	// original job's template_id orphaned with no matching definition row
	// while the real template ended up under the regenerated ID instead.
	originalTemplateID := strings.TrimSpace(req.TemplateID)
	// A request_json snapshot persisted before the rootfs_format field existed
	// (pre-erofs upgrade) decodes with an empty RootfsFormat. normalizeTemplateImageRequest
	// would then fill it from the CURRENT template.rootfs_format config -- but this
	// is a REPLAY of an already-submitted job, not a fresh submission: the build
	// was started under the only format that existed then (ext4), and its format is
	// fixed at submit time. Letting the current config override it is an upgrade
	// hazard: an in-flight (or stuck-BUILT) ext4 job replayed after the operator
	// sets template.rootfs_format=erofs would be re-stamped erofs, then fail the
	// rootfs_format mismatch guard in prepareTemplateImageJobAfterRemoteBuild
	// (old TC reports ext4) even though TC did nothing wrong -- and a redo would
	// silently switch the rebuild to erofs. Pin the legacy format here so an empty
	// snapshot field means ext4, never the current config default.
	//
	// Post-upgrade snapshots never hit this: submissions persist the NORMALIZED
	// request (marshalTemplateImageJobRequest(normalized) at template_image.go),
	// and normalize always stamps RootfsFormat non-empty, so an empty value in a
	// replayed snapshot can only come from a pre-upgrade row. Fresh submissions do
	// not pass through this function at all, so the config-default semantics of
	// normalizeTemplateImageRequest's empty branch are unchanged for new creates.
	if strings.TrimSpace(req.RootfsFormat) == "" {
		req.RootfsFormat = config.RootfsFormatExt4
	}
	req.Request = &types.Request{RequestID: uuid.NewString()}
	normalized, err := normalizeTemplateImageRequest(req)
	if err != nil {
		return nil, err
	}
	if originalTemplateID != "" {
		normalized.TemplateID = originalTemplateID
	}
	return normalized, nil
}

func buildTemplateSpecFingerprint(req *types.CreateTemplateFromImageReq, sourceImageDigest string) string {
	// This fingerprint scopes only the reusable rootfs artifact. Runtime inputs
	// such as the active guest kernel identity must stay out of this payload:
	// template compatibility is governed at the replica layer, and the guest
	// kernel file captured for a template is not part of rootfs artifact reuse.
	return buildTemplateSpecFingerprintWithCA(req, sourceImageDigest, "")
}

// buildTemplateSpecFingerprintWithCA folds the CubeEgress CA fingerprint into
// the template spec fingerprint so that a CA rotation invalidates artifact
// reuse automatically. cubeEgressCAFingerprint is empty when CA baking is
// disabled for this request.
func buildTemplateSpecFingerprintWithCA(req *types.CreateTemplateFromImageReq, sourceImageDigest, cubeEgressCAFingerprint string) string {
	return buildTemplateSpecFingerprintWithEnvdSHA(req, sourceImageDigest, cubeEgressCAFingerprint, "")
}

func buildTemplateSpecFingerprintWithEnvdSHA(req *types.CreateTemplateFromImageReq, sourceImageDigest, cubeEgressCAFingerprint, envdSHA string) string {
	type fingerprintPayload struct {
		SourceImageDigest string  `json:"source_image_digest"`
		WritableLayerSize string  `json:"writable_layer_size"`
		ExposedPorts      []int32 `json:"exposed_ports,omitempty"`
		InstanceType      string  `json:"instance_type"`
		NetworkType       string  `json:"network_type"`
		// RootfsFormat is part of the artifact identity so an ext4 and an
		// erofs build of the same image+spec produce different fingerprints
		// and never reuse (and thus mislabel) each other's artifact.
		//
		// Legacy compatibility: only erofs carries a value here. ext4 (and
		// empty/unknown, which normalize to ext4) leave the field empty so it
		// omitempty's out of the JSON — making the ext4 fingerprint byte-identical
		// to the pre-RootfsFormat fingerprint. Without this, every existing ext4
		// artifact would change fingerprint on upgrade and force a one-time full
		// rebuild of every template. This mirrors the CubeEgressCAFingerprint
		// field below (see TestBuildTemplateSpecFingerprintEmptyCAMatchesLegacy):
		// the legacy/default value omits, only a non-default value (erofs / a
		// real CA) enters the payload and yields a distinct fingerprint.
		RootfsFormat            string                    `json:"rootfs_format,omitempty"`
		ContainerOverrides      *types.ContainerOverrides `json:"container_overrides,omitempty"`
		CubeEgressCAFingerprint string                    `json:"cube_egress_ca_fingerprint,omitempty"`
		EnvdBinarySHA256        string                    `json:"envd_binary_sha256,omitempty"`
	}
	// Only erofs contributes to the fingerprint; ext4/empty/unknown normalize to
	// ext4 and are left blank so the key omits and the fingerprint matches the
	// pre-RootfsFormat legacy value. NormalizeRootfsFormat is the shared
	// canonicalizer (trim + lower, ext4 default) so "EROFS" / " erofs " still
	// produce the erofs fingerprint rather than silently matching legacy ext4.
	rootfsFormatForFingerprint := ""
	if NormalizeRootfsFormat(req.RootfsFormat) == config.RootfsFormatErofs {
		rootfsFormatForFingerprint = config.RootfsFormatErofs
	}
	payload, _ := json.Marshal(fingerprintPayload{
		SourceImageDigest:       sourceImageDigest,
		WritableLayerSize:       req.WritableLayerSize,
		ExposedPorts:            req.ExposedPorts,
		InstanceType:            req.InstanceType,
		NetworkType:             req.NetworkType,
		RootfsFormat:            rootfsFormatForFingerprint,
		ContainerOverrides:      req.ContainerOverrides,
		CubeEgressCAFingerprint: cubeEgressCAFingerprint,
		EnvdBinarySHA256:        envdSHA,
	})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// buildArtifactID generates a unique artifact ID for a given spec fingerprint.
//
// The ID includes a UUID suffix to avoid collisions when the same template
// spec is rebuilt after a previous artifact was deleted. Without the UUID,
// rebuilding the same spec would produce the same artifact ID, causing the
// new ext4 file to overwrite (or be confused with) the old one on shared
// storage or on nodes that still have the old file cached.
//
// Deduplication across concurrent builds of the same spec is handled at the
// Master layer (submitTemplateFromImage checks for existing READY artifacts
// by fingerprint before creating a job) and at the TC layer (build.go checks
// the DB for a READY artifact with the same fingerprint before building), NOT
// by artifact ID equality. This allows artifact IDs to be unique per build
// while still preventing redundant builds of the same spec.
func buildArtifactID(fingerprint string) string {
	return fmt.Sprintf("rfs-%s-%s", fingerprint[:24], uuid.New().String()[:8])
}

// BuildTemplateSpecFingerprintWithEnvdSHA exports the fingerprint builder so
// the standalone CubeTemplateCenter process computes the exact same value
// during remote builds (keeping artifact dedup compatible with local mode).
func BuildTemplateSpecFingerprintWithEnvdSHA(req *types.CreateTemplateFromImageReq, sourceImageDigest, cubeEgressCAFingerprint, envdSHA string) string {
	return buildTemplateSpecFingerprintWithEnvdSHA(req, sourceImageDigest, cubeEgressCAFingerprint, envdSHA)
}

// BuildArtifactID exports buildArtifactID for CubeTemplateCenter (remote
// build mode), so artifact IDs are derived identically in both modes.
func BuildArtifactID(fingerprint string) string {
	return buildArtifactID(fingerprint)
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

// MarshalTemplateImageJobRequestCanonical exports marshalTemplateImageJobRequest
// so the standalone CubeTemplateCenter process can compare a submitted build
// payload against CubeMaster's persisted request_json snapshot byte-for-byte:
// both sides zero the credential and the transport-only Request envelope
// before marshaling, and Go's struct-based json.Marshal is deterministic, so
// equal requests produce identical bytes.
func MarshalTemplateImageJobRequestCanonical(req *types.CreateTemplateFromImageReq) (string, error) {
	return marshalTemplateImageJobRequest(req)
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
