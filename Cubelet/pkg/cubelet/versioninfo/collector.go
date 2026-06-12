// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package versioninfo collects the real version of every component installed
// on a cubelet node and normalises them into a flat list for reporting to
// cubemaster on register / heartbeat.
//
// Primary data source is the release-manifest.json installed alongside the
// node binaries (machine-readable, complete, release-consistent). Four
// adjustments are layered on top:
//
//  1. the cubelet entry is overridden with the running binary's own
//     pkg/version (the truly-running cubelet, not just what the manifest
//     shipped);
//  2. guest-image is taken from the on-node cube-image/version file (the
//     version actually in effect, which may drift from the manifest), with a
//     lazy mtime-based re-read so an out-of-band guest upgrade is picked up
//     without restarting cubelet;
//  3. kernel is selected from the active cube-kernel-scf/vmlinux symlink so
//     PVM nodes report the PVM guest kernel version instead of the ordinary
//     packaged kernel;
//  4. components are filtered to those actually installed on this node, so a
//     compute node does not report control-plane binaries it never runs.
//
// Collection never blocks register/heartbeat: a missing or malformed manifest
// degrades to "cubelet self version + guest-image file (if present)".
package versioninfo

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/components"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/version"
)

// Source labels for ComponentVersion.Source.
const (
	SourceManifest = "manifest"
	SourceBinary   = "binary"
	SourceFile     = "file"
)

// Canonical component names.
const (
	ComponentCubelet    = "cubelet"
	ComponentCubeAgent  = "cube-agent"
	ComponentGuestImage = "guest-image"
	ComponentKernel     = "kernel"
	ComponentCubeEgress = "cube-egress"
)

const (
	manifestFileName  = "release-manifest.json"
	guestImageVerPath = "cube-image/version"
	kernelVmlinuxPath = "cube-kernel-scf/vmlinux"
	cubeEgressVerPath = "cube-egress/version"
)

// oneClickInstallLayout maps manifest component keys to the concrete one-click
// install paths that prove the component is actually present on this node. The
// collector still supports the original "${baseDir}/<component>/" directory
// shape first; these aliases cover the packaged control-plane layout
// (CubeMaster/CubeAPI/Cubelet/cube-shim, etc.).
var oneClickInstallLayout = map[string][][]string{
	"cubemaster":              {{"CubeMaster", "bin", "cubemaster"}},
	"cubemastercli":           {{"CubeMaster", "bin", "cubemastercli"}},
	"cube-api":                {{"CubeAPI", "bin", "cube-api"}},
	"cubecli":                 {{"Cubelet", "bin", "cubecli"}},
	"network-agent":           {{"network-agent", "bin", "network-agent"}},
	"containerd-shim-cube-rs": {{"cube-shim", "bin", "containerd-shim-cube-rs"}},
	"cube-runtime":            {{"cube-shim", "bin", "cube-runtime"}},
	"cube-egress":             {{"cube-egress", "version"}},
}

// ComponentVersion is a pure-data version record. It mirrors
// masterclient.ComponentVersion (kept independent to avoid a layering
// dependency from this low-level package onto the HTTP client).
type ComponentVersion struct {
	Component string
	Version   string
	Commit    string
	BuildTime string
	Source    string
}

type manifestComponent struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"build_time"`
}

type releaseManifest struct {
	Components map[string]manifestComponent `json:"components"`
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

// Collector assembles the node's component versions. Safe for concurrent use.
type Collector struct {
	baseDir string

	mu sync.Mutex
	// manifest is parsed once and cached (nil when missing/unreadable).
	manifest       *releaseManifest
	manifestParsed bool
	// guest-image version file, re-read lazily on mtime change.
	guestImageMTime int64
	guestImageVer   string
	guestImageRead  bool
	// active kernel symlink target, re-read lazily on lstat mtime change.
	kernelLinkMTime  int64
	kernelLinkTarget string
	kernelLinkRead   bool
}

// NewCollector builds a collector rooted at baseDir. An empty baseDir falls
// back to the component manager's default versioned base dir (single source
// of truth for the install layout).
func NewCollector(baseDir string) *Collector {
	if baseDir == "" {
		baseDir = components.DefaultConfig().VersionedBaseDir
	}
	return &Collector{baseDir: baseDir}
}

// Collect returns the current component versions for this node. It never
// returns an error: collection failures degrade to a minimal set so the
// heartbeat is never blocked.
func (c *Collector) Collect() []ComponentVersion {
	c.mu.Lock()
	defer c.mu.Unlock()

	man := c.loadManifestLocked()
	out := make([]ComponentVersion, 0, 12)

	// (1) cubelet always reported from the running binary.
	out = append(out, ComponentVersion{
		Component: ComponentCubelet,
		Version:   version.Version,
		Commit:    version.Commit,
		BuildTime: version.BuildTime,
		Source:    SourceBinary,
	})

	if man != nil {
		// (2) binary components from the manifest, filtered to those actually
		// installed on this node. cubelet handled above; cube-agent handled
		// from guest_image.agent_version below.
		for name, mc := range man.Components {
			if name == ComponentCubelet || name == ComponentCubeAgent || name == ComponentCubeEgress {
				continue
			}
			if !c.componentInstalledLocked(name) {
				continue
			}
			out = append(out, ComponentVersion{
				Component: name,
				Version:   mc.Version,
				Commit:    mc.Commit,
				BuildTime: mc.BuildTime,
				Source:    SourceManifest,
			})
		}
		// (3) cube-agent: take the guest's baked-in agent version, de-duped.
		if man.GuestImage.AgentVersion != "" {
			out = append(out, ComponentVersion{
				Component: ComponentCubeAgent,
				Version:   man.GuestImage.AgentVersion,
				Source:    SourceManifest,
			})
		}
		// (4) kernel: report the active guest kernel variant, not the host
		// kernel and not just the static ordinary manifest value.
		if kernel := c.kernelVersionLocked(man); kernel.Version != "" {
			out = append(out, kernel)
		}
	}

	// (5) guest-image: the version actually in effect on this node.
	if ver := c.guestImageVersionLocked(); ver != "" {
		out = append(out, ComponentVersion{
			Component: ComponentGuestImage,
			Version:   ver,
			Source:    SourceFile,
		})
	}

	// (6) cube-egress: version marker written by the deploy system.
	// The marker file cube-egress/version is present only when the
	// CubeEgress container is deployed on this node; when absent the
	// component is silently omitted (graceful degradation).
	if ver := c.cubeEgressVersionLocked(); ver != "" {
		out = append(out, ComponentVersion{
			Component: ComponentCubeEgress,
			Version:   ver,
			Source:    SourceFile,
		})
	}

	return out
}

// kernelVersionLocked returns the active guest kernel version. The active
// variant is represented by cube-kernel-scf/vmlinux: a symlink to vmlinux-bm
// for ordinary nodes, or vmlinux-pvm for PVM nodes. If the symlink is missing
// or from an older install layout, fall back to the ordinary manifest identity.
func (c *Collector) kernelVersionLocked(man *releaseManifest) ComponentVersion {
	target, ok := c.kernelLinkTargetLocked()
	if ok {
		switch filepath.Base(target) {
		case "vmlinux-pvm":
			return ComponentVersion{
				Component: ComponentKernel,
				Version:   kernelArtifactIdentity(man.Kernel.PVMVersion, man.Kernel.VMLinuxPVMDigest),
				Source:    SourceFile,
			}
		case "vmlinux-bm":
			return ComponentVersion{
				Component: ComponentKernel,
				Version:   kernelArtifactIdentity(man.Kernel.Version, man.Kernel.VMLinuxDigest),
				Source:    SourceFile,
			}
		}
	}
	identity := kernelArtifactIdentity(man.Kernel.Version, man.Kernel.VMLinuxDigest)
	if identity == "" {
		return ComponentVersion{}
	}
	return ComponentVersion{
		Component: ComponentKernel,
		Version:   identity,
		Source:    SourceManifest,
	}
}

func (c *Collector) kernelLinkTargetLocked() (string, bool) {
	path := filepath.Join(c.baseDir, kernelVmlinuxPath)
	info, err := os.Lstat(path)
	if err != nil {
		c.kernelLinkRead = true
		c.kernelLinkTarget = ""
		return "", false
	}
	if info.Mode()&os.ModeSymlink == 0 {
		c.kernelLinkRead = true
		c.kernelLinkMTime = info.ModTime().UnixNano()
		c.kernelLinkTarget = ""
		return "", false
	}
	mtime := info.ModTime().UnixNano()
	if c.kernelLinkRead && mtime == c.kernelLinkMTime {
		return c.kernelLinkTarget, c.kernelLinkTarget != ""
	}
	c.kernelLinkRead = true
	c.kernelLinkMTime = mtime
	c.kernelLinkTarget = ""
	target, err := os.Readlink(path)
	if err != nil {
		return "", false
	}
	c.kernelLinkTarget = target
	return target, true
}

// loadManifestLocked parses the manifest once and caches the result.
func (c *Collector) loadManifestLocked() *releaseManifest {
	if c.manifestParsed {
		return c.manifest
	}
	c.manifestParsed = true
	data, err := os.ReadFile(filepath.Join(c.baseDir, manifestFileName))
	if err != nil {
		return nil
	}
	var m releaseManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil
	}
	c.manifest = &m
	return c.manifest
}

// componentInstalledLocked reports whether a versioned directory exists for
// the component (${baseDir}/<component>), or whether the one-click packaged
// install layout carries the matching binary/config path, i.e. it is actually
// deployed here.
func (c *Collector) componentInstalledLocked(name string) bool {
	if exists(filepath.Join(c.baseDir, name)) {
		return true
	}
	for _, rel := range oneClickInstallLayout[name] {
		parts := append([]string{c.baseDir}, rel...)
		if exists(filepath.Join(parts...)) {
			return true
		}
	}
	return false
}

// guestImageVersionLocked returns the single-line guest image version, using
// an mtime cache so an out-of-band guest upgrade is reflected without
// restarting cubelet.
func (c *Collector) guestImageVersionLocked() string {
	path := filepath.Join(c.baseDir, guestImageVerPath)
	info, err := os.Stat(path)
	if err != nil {
		c.guestImageRead = true
		c.guestImageVer = ""
		return ""
	}
	mtime := info.ModTime().UnixNano()
	if c.guestImageRead && mtime == c.guestImageMTime {
		return c.guestImageVer
	}
	c.guestImageRead = true
	c.guestImageMTime = mtime
	c.guestImageVer = ""
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	c.guestImageVer = firstLine(data)
	return c.guestImageVer
}

// cubeEgressVersionLocked returns the single-line cube-egress version from the
// host-side marker file written by the deploy system at install time. The file
// is static between deployments, so we read it directly without mtime caching.
func (c *Collector) cubeEgressVersionLocked() string {
	path := filepath.Join(c.baseDir, cubeEgressVerPath)
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return firstLine(data)
}

// firstLine returns the first line of data, trimmed of surrounding
// whitespace. Matches CubeShim::get_image_version's strict single-line read.
func firstLine(data []byte) string {
	start := 0
	// skip leading whitespace
	for start < len(data) && isSpace(data[start]) {
		start++
	}
	end := start
	for end < len(data) && data[end] != '\n' && data[end] != '\r' {
		end++
	}
	line := data[start:end]
	// trim trailing whitespace
	j := len(line)
	for j > 0 && isSpace(line[j-1]) {
		j--
	}
	return string(line[:j])
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
	value = firstLine([]byte(value))
	if value == "unknown" {
		return ""
	}
	return value
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\v' || b == '\f'
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
