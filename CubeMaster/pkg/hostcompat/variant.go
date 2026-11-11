// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package hostcompat defines stable identities for host-fact compatibility
// classes used by JFS template variants.
package hostcompat

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const (
	// VariantPolicyV1 matches restore-compat v1: cpuid_hash and
	// host_kernel_release must both be present and equal.
	VariantPolicyV1  = "hf1"
	hashPreviewBytes = 8
)

// VariantKey returns a stable, path-safe key for one restore-compatibility
// class. Missing required facts fail closed.
func VariantKey(cpuidHash, kernelRelease string) (string, bool) {
	cpuidHash = strings.TrimSpace(cpuidHash)
	kernelRelease = strings.TrimSpace(kernelRelease)
	if cpuidHash == "" || kernelRelease == "" {
		return "", false
	}
	sum := sha256.Sum256([]byte("restore-compat-v1\x00" + cpuidHash + "\x00" + kernelRelease))
	return VariantPolicyV1 + "-" + hex.EncodeToString(sum[:hashPreviewBytes]), true
}

// TemplateVariantID derives the internal package ID for one JFS template and
// host-fact compatibility class.
func TemplateVariantID(templateID, cpuidHash, kernelRelease string) (variantID, key string, ok bool) {
	templateID = strings.TrimSpace(templateID)
	if templateID == "" {
		return "", "", false
	}
	key, ok = VariantKey(cpuidHash, kernelRelease)
	if !ok {
		return "", "", false
	}
	return templateID + "-" + key, key, true
}
