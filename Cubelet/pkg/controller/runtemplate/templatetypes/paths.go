// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatetypes

import (
	"path"
	"strings"
)

// DefaultVersionedBaseDir is the component_versions inventory root.
const DefaultVersionedBaseDir = "/data/cubelet/root/component_versions"

// DefaultToolboxRoot is the live install tree updated by installers.
const DefaultToolboxRoot = "/usr/local/services/cubetoolbox"

const (
	// Paths relative to component_versions/<component>/<version>/.
	RelativePathCubeShim    = "bin/containerd-shim-cube-rs"
	RelativePathCubeRuntime = "bin/cube-runtime"
	RelativePathCubeKernel  = "vmlinux"
	RelativePathCubeImage   = "cube-guest-image-cpu.img"
	RelativePathCubeAgent   = "cube-agent.ext4"
)

// DefaultRelativePath returns the artifact path under a versioned component root.
// Empty string means the component has no single default file Path.
func DefaultRelativePath(component CubeComponent) string {
	switch component {
	case CubeComponentCubeShim:
		return RelativePathCubeShim
	case CubeComponentCubeKernel:
		return RelativePathCubeKernel
	case CubeComponentCubeImage:
		return RelativePathCubeImage
	case CubeComponentCubeAgent:
		return RelativePathCubeAgent
	default:
		return ""
	}
}

// VersionedComponentDir joins VersionedBase/<name>/<version>.
func VersionedComponentDir(base, name, version string) string {
	return path.Join(base, name, InventoryVersionKey(version))
}

// VersionedLocalPath joins VersionedBase/<name>/<version>/<relativePath>.
func VersionedLocalPath(base, name, version, relativePath string) string {
	return path.Join(VersionedComponentDir(base, name, version), relativePath)
}

// InventoryVersionKey maps a version string to the inventory directory name.
// Digests ("sha256:<hex>" or "...@sha256:<hex>") become "sha256-<12>";
// other versions are unchanged.
func InventoryVersionKey(version string) string {
	version = path.Base(strings.TrimSpace(version))
	if version == "." || version == "/" || version == "" {
		return ""
	}
	candidate := version
	if i := strings.IndexByte(version, '@'); i >= 0 {
		after := strings.TrimSpace(version[i+1:])
		if short := InventoryKeyFromContentDigest(after); short != "" {
			return short
		}
		candidate = strings.TrimSpace(version[:i])
	}
	if short := InventoryKeyFromContentDigest(candidate); short != "" {
		return short
	}
	if strings.HasPrefix(candidate, "sha256:") {
		return ""
	}
	return candidate
}

// InventoryKeyFromContentDigest maps "sha256:<hex>" to "sha256-<12>".
func InventoryKeyFromContentDigest(version string) string {
	const prefix = "sha256:"
	if !strings.HasPrefix(version, prefix) {
		return ""
	}
	hex := version[len(prefix):]
	if len(hex) < 12 || !isLowerHex(hex) {
		return ""
	}
	return "sha256-" + hex[:12]
}

// ContentAddressedKernelIdentity builds "sha256-<12>@sha256:<hex>" from a digest.
func ContentAddressedKernelIdentity(digest string) string {
	digest = strings.TrimSpace(digest)
	if digest == "" || digest == "unknown" {
		return ""
	}
	short := InventoryKeyFromContentDigest(digest)
	if short == "" {
		if strings.HasPrefix(digest, "sha256:") {
			return digest
		}
		return ""
	}
	return short + "@" + digest
}

func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// IsVersionedInventoryComponent is true for shim / kernel / image / agent.
func IsVersionedInventoryComponent(component CubeComponent) bool {
	switch component {
	case CubeComponentCubeShim, CubeComponentCubeKernel, CubeComponentCubeImage, CubeComponentCubeAgent:
		return true
	default:
		return false
	}
}
