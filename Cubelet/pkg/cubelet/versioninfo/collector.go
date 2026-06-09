// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package versioninfo collects the real version of every component installed
// on a cubelet node and normalises them into a flat list for reporting to
// cubemaster on register / heartbeat.
//
// Primary data source is the release-manifest.json installed alongside the
// node binaries (machine-readable, complete, release-consistent). Three
// adjustments are layered on top:
//
//  1. the cubelet entry is overridden with the running binary's own
//     pkg/version (the truly-running cubelet, not just what the manifest
//     shipped);
//  2. guest-image is taken from the on-node cube-image/version file (the
//     version actually in effect, which may drift from the manifest), with a
//     lazy mtime-based re-read so an out-of-band guest upgrade is picked up
//     without restarting cubelet;
//  3. components are filtered to those actually installed on this node, so a
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
)

const (
	manifestFileName  = "release-manifest.json"
	guestImageVerPath = "cube-image/version"
)

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
		Version string `json:"version"`
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
			if name == ComponentCubelet || name == ComponentCubeAgent {
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
		// (4) kernel.
		if man.Kernel.Version != "" {
			out = append(out, ComponentVersion{
				Component: ComponentKernel,
				Version:   man.Kernel.Version,
				Source:    SourceManifest,
			})
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

	return out
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
// the component (${baseDir}/<component>), i.e. it is actually deployed here.
func (c *Collector) componentInstalledLocked(name string) bool {
	info, err := os.Stat(filepath.Join(c.baseDir, name))
	return err == nil && info.IsDir()
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

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\v' || b == '\f'
}
