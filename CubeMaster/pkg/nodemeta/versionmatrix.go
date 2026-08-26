// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package nodemeta

import (
	"encoding/json"
	"os"
	"strings"
)

// defaultReleaseManifestPath is the on-disk location of the release manifest
// installed by the one-click bundle. It can be overridden with the
// CUBE_RELEASE_MANIFEST environment variable (mainly for tests / non-standard
// layouts).
const defaultReleaseManifestPath = "/usr/local/services/cubetoolbox/release-manifest.json"

// Canonical component names for components that follow their own version
// system (must match the names the cubelet collector reports).
const (
	componentGuestImage = "guest-image"
	componentCubeAgent  = "cube-agent"
	componentKernel     = "kernel"
)

// releaseManifest is the subset of release-manifest.json needed to expose
// declared component artifacts.
type releaseManifest struct {
	Components map[string]struct {
		Version string `json:"version"`
	} `json:"components"`
	GuestImage struct {
		Version      string `json:"version"`
		AgentVersion string `json:"agent_version"`
	} `json:"guest_image"`
	Kernel struct {
		Version          string `json:"version"`
		PVMVersion       string `json:"pvm_version"`
		VMLinuxDigest    string `json:"vmlinux_digest_sha256"`
		VMLinuxPVMDigest string `json:"vmlinux_pvm_digest_sha256"`
	} `json:"kernel"`
}

type declaredVersionInfo struct {
	Primary map[string]string
	Sets    map[string]map[string]struct{}
}

// loadDeclaredVersions returns the release-declared version per component.
// Returns an empty map when the manifest is missing/unreadable.
func loadDeclaredVersions() map[string]string {
	path := os.Getenv("CUBE_RELEASE_MANIFEST")
	if path == "" {
		path = defaultReleaseManifestPath
	}
	return loadDeclaredVersionsFromPath(path)
}

func loadDeclaredVersionsFromPath(path string) map[string]string {
	return loadDeclaredVersionInfoFromPath(path).Primary
}

func loadDeclaredVersionInfo() declaredVersionInfo {
	path := os.Getenv("CUBE_RELEASE_MANIFEST")
	if path == "" {
		path = defaultReleaseManifestPath
	}
	return loadDeclaredVersionInfoFromPath(path)
}

func loadDeclaredVersionInfoFromPath(path string) declaredVersionInfo {
	data, err := os.ReadFile(path)
	if err != nil {
		return declaredVersionInfo{Primary: map[string]string{}, Sets: map[string]map[string]struct{}{}}
	}
	var m releaseManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return declaredVersionInfo{Primary: map[string]string{}, Sets: map[string]map[string]struct{}{}}
	}
	declared := make(map[string]string, len(m.Components)+3)
	declaredSets := make(map[string]map[string]struct{}, len(m.Components)+3)
	for name, c := range m.Components {
		addDeclaredVersion(declared, declaredSets, name, c.Version, false)
	}
	// guest-image / cube-agent / kernel follow their own version systems; take
	// them from the dedicated manifest sections (cube-agent overrides any
	// components["cube-agent"] entry to match the agent baked into the guest).
	if m.GuestImage.Version != "" {
		setDeclaredVersion(declared, declaredSets, componentGuestImage, m.GuestImage.Version)
	}
	if m.GuestImage.AgentVersion != "" {
		setDeclaredVersion(declared, declaredSets, componentCubeAgent, m.GuestImage.AgentVersion)
	}
	if identity := kernelArtifactIdentity(m.Kernel.Version, m.Kernel.VMLinuxDigest); identity != "" {
		setDeclaredVersion(declared, declaredSets, componentKernel, identity)
	}
	if identity := kernelArtifactIdentity(m.Kernel.PVMVersion, m.Kernel.VMLinuxPVMDigest); identity != "" {
		addDeclaredVersion(declared, declaredSets, componentKernel, identity, false)
	}
	return declaredVersionInfo{Primary: declared, Sets: declaredSets}
}

func addDeclaredVersion(primary map[string]string, sets map[string]map[string]struct{}, component, version string, forcePrimary bool) {
	if version == "" || version == "unknown" {
		return
	}
	if forcePrimary || primary[component] == "" {
		primary[component] = version
	}
	if sets[component] == nil {
		sets[component] = map[string]struct{}{}
	}
	sets[component][version] = struct{}{}
}

func setDeclaredVersion(primary map[string]string, sets map[string]map[string]struct{}, component, version string) {
	if version == "" || version == "unknown" {
		return
	}
	primary[component] = version
	sets[component] = map[string]struct{}{version: {}}
}

func kernelArtifactIdentity(tag, digest string) string {
	tag = trimKernelIdentityPart(tag)
	digest = trimKernelIdentityPart(digest)
	if digest != "" {
		if tag != "" {
			return tag + "@" + digest
		}
		return digest
	}
	return tag
}

func trimKernelIdentityPart(value string) string {
	value = strings.TrimSpace(value)
	if value == "unknown" {
		return ""
	}
	return value
}
