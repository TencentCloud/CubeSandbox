// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package versioninfo collects the real version of every component installed
// on a cubelet node and normalises them into a flat list for reporting to
// cubemaster on register / heartbeat.
//
// Primary data source is per-component version.json next to staged artifacts.
// Fallbacks, in order:
//
//  1. cubelet always from the running binary;
//  2. directory version.json (allowlisted keys only);
//  3. single-line markers (cube-image/version, agent-version, egress);
//  4. release-manifest.json for remaining installed binary components / kernel.
//
// Kernel identity prefers bootstrap-state/vmlinux-active, then the artifact
// vmlinux symlink, and reports only the active bm|pvm variant.
//
// Collection never blocks register/heartbeat: missing sources degrade gracefully.
package versioninfo

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/components"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/version"
)

// Source labels for ComponentVersion.Source.
const (
	SourceManifest      = "manifest"
	SourceBinary        = "binary"
	SourceFile          = "file"
	SourceComponentJSON = "component-json"
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
	manifestFileName     = "release-manifest.json"
	componentVersionJSON = "version.json"
	guestImageVerPath    = "cube-image/version"
	guestAgentVerPath    = "cube-image/agent-version"
	kernelVmlinuxPath    = "cube-kernel-scf/vmlinux"
	cubeEgressVerPath    = "cube-egress/version"
	maxVersionJSONBytes  = 64 << 10
)

// oneClickInstallLayout maps manifest component keys to the concrete one-click
// install paths that prove the component is actually present on this node.
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

// pathAllowlist restricts which component keys may be accepted from each
// directory's version.json.
var pathAllowlist = map[string]map[string]struct{}{
	"Cubelet":       {"cubelet": {}, "cubecli": {}},
	"network-agent": {"network-agent": {}},
	"cube-shim":     {"containerd-shim-cube-rs": {}, "cube-runtime": {}},
	"cube-image":    {"guest-image": {}, "cube-agent": {}},
	"cube-egress":   {"cube-egress": {}},
}

// ComponentVersion is a pure-data version record.
type ComponentVersion struct {
	Component string
	Version   string
	Commit    string
	BuildTime string
	Source    string
	Variant   string // kernel: bm|pvm
}

// CollectReport is the full collection outcome for one heartbeat.
type CollectReport struct {
	Versions   []ComponentVersion
	Incomplete bool
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

type componentJSONFile struct {
	SchemaVersion int `json:"schema_version"`
	Components    map[string]struct {
		Version   string `json:"version"`
		Commit    string `json:"commit"`
		BuildTime string `json:"build_time"`
	} `json:"components"`
	Variants map[string]struct {
		Version      string `json:"version"`
		Tag          string `json:"tag"`
		DigestSHA256 string `json:"digest_sha256"`
	} `json:"variants"`
}

// Collector assembles the node's component versions. Safe for concurrent use.
type Collector struct {
	baseDir      string
	bootstrapDir string

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
// back to the component manager's default versioned base dir.
func NewCollector(baseDir string) *Collector {
	if baseDir == "" {
		baseDir = components.DefaultConfig().VersionedBaseDir
	}
	bootstrap := strings.TrimSpace(os.Getenv("STATE_DIR"))
	if bootstrap == "" {
		bootstrap = strings.TrimSpace(os.Getenv("CUBE_BOOTSTRAP_STATE"))
	}
	if bootstrap == "" {
		bootstrap = "/var/lib/cube-node-bootstrap"
	}
	return &Collector{baseDir: baseDir, bootstrapDir: bootstrap}
}

// NewCollectorWithDirs is for tests that need an isolated bootstrap state dir.
func NewCollectorWithDirs(baseDir, bootstrapDir string) *Collector {
	c := NewCollector(baseDir)
	if strings.TrimSpace(bootstrapDir) != "" {
		c.bootstrapDir = bootstrapDir
	}
	return c
}

// Collect returns versions only (backward compatible).
func (c *Collector) Collect() []ComponentVersion {
	return c.CollectReport().Versions
}

// CollectReport returns versions plus incompleteness signals.
func (c *Collector) CollectReport() CollectReport {
	c.mu.Lock()
	defer c.mu.Unlock()

	report := CollectReport{Versions: make([]ComponentVersion, 0, 16)}
	seen := map[string]struct{}{}
	// Keys owned by a malformed/unsupported version.json must not be filled
	// from marker/manifest.
	blocked := map[string]struct{}{}
	add := func(v ComponentVersion) {
		if v.Component == "" || v.Version == "" {
			return
		}
		if _, ok := seen[v.Component]; ok {
			return
		}
		if _, ok := blocked[v.Component]; ok {
			return
		}
		seen[v.Component] = struct{}{}
		report.Versions = append(report.Versions, v)
	}
	blockKeys := func(keys map[string]struct{}) {
		for k := range keys {
			blocked[k] = struct{}{}
		}
	}

	add(ComponentVersion{
		Component: ComponentCubelet,
		Version:   version.Version,
		Commit:    version.Commit,
		BuildTime: version.BuildTime,
		Source:    SourceBinary,
	})

	// Mid-stage gap: staging marker or ready-sentinel with missing dir.
	c.detectStageGapsLocked(&report)

	// Directory version.json.
	for dir, allow := range pathAllowlist {
		path := filepath.Join(c.baseDir, dir, componentVersionJSON)
		entries, errMsg, malformed := c.readComponentJSON(path, allow)
		if errMsg != "" {
			report.Incomplete = true
		}
		if malformed {
			blockKeys(allow)
			continue
		}
		for _, e := range entries {
			if e.Component == ComponentCubelet {
				continue // running binary wins
			}
			add(e)
		}
	}

	// Markers (add() skips when already seen or blocked by malformed JSON).
	add(ComponentVersion{Component: ComponentGuestImage, Version: c.guestImageVersionLocked(), Source: SourceFile})
	add(ComponentVersion{
		Component: ComponentCubeAgent,
		Version:   c.readSingleLine(filepath.Join(c.baseDir, guestAgentVerPath)),
		Source:    SourceFile,
	})
	add(ComponentVersion{
		Component: ComponentCubeEgress,
		Version:   c.readSingleLine(filepath.Join(c.baseDir, cubeEgressVerPath)),
		Source:    SourceFile,
	})

	// Kernel: version.json variants + active selection.
	// When version.json exists but is unusable, do not fall back to manifest.
	if k, errMsg := c.kernelFromJSONLocked(); k.Version != "" {
		add(k)
	} else if errMsg != "" {
		report.Incomplete = true
		blocked[ComponentKernel] = struct{}{}
	}

	// Manifest fill: kernel (if not blocked) + remaining installed binaries / agent.
	if man := c.loadManifestLocked(); man != nil {
		add(c.kernelVersionLocked(man))
		for name, mc := range man.Components {
			if name == ComponentCubelet || name == ComponentCubeAgent || name == ComponentCubeEgress || name == ComponentKernel {
				continue
			}
			if !c.componentInstalledLocked(name) {
				continue
			}
			add(ComponentVersion{
				Component: name,
				Version:   mc.Version,
				Commit:    mc.Commit,
				BuildTime: mc.BuildTime,
				Source:    SourceManifest,
			})
		}
		add(ComponentVersion{
			Component: ComponentCubeAgent,
			Version:   man.GuestImage.AgentVersion,
			Source:    SourceManifest,
		})
	}

	return report
}

func (c *Collector) detectStageGapsLocked(report *CollectReport) {
	type staged struct {
		dir      string
		sentinel string
		staging  string
	}
	checks := []staged{
		{"Cubelet", ".staged-cubelet", ".staging-cubelet"},
		{"network-agent", ".staged-network-agent", ".staging-network-agent"},
		{"cube-shim", ".staged-cube-shim", ".staging-cube-shim"},
		{"cube-image", ".staged-cube-guest", ".staging-cube-guest"},
		{"cube-kernel-scf", ".staged-cube-kernel", ".staging-cube-kernel"},
	}
	for _, ch := range checks {
		staging := filepath.Join(c.baseDir, ch.staging)
		if exists(staging) {
			report.Incomplete = true
			continue
		}
		sentinel := filepath.Join(c.baseDir, ch.sentinel)
		if !exists(sentinel) {
			continue
		}
		dir := filepath.Join(c.baseDir, ch.dir)
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			report.Incomplete = true
		}
	}
}

// loadVersionJSON reads and validates a component version.json.
// missing is true when the file is absent (not an error).
// bad is true when the file exists but is unusable (blocks marker/manifest fallback).
func loadVersionJSON(path string) (parsed componentJSONFile, errMsg string, missing, bad bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return componentJSONFile{}, "", true, false
		}
		return componentJSONFile{}, path + ": " + err.Error(), false, true
	}
	if len(data) > maxVersionJSONBytes {
		return componentJSONFile{}, path + ": exceeds size limit", false, true
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return componentJSONFile{}, path + ": malformed json", false, true
	}
	if parsed.SchemaVersion != 0 && parsed.SchemaVersion != 1 {
		return componentJSONFile{}, path + ": unsupported schema_version", false, true
	}
	return parsed, "", false, false
}

func (c *Collector) readComponentJSON(path string, allow map[string]struct{}) ([]ComponentVersion, string, bool) {
	parsed, errMsg, missing, bad := loadVersionJSON(path)
	if missing {
		return nil, "", false
	}
	if bad {
		return nil, errMsg, true
	}
	out := make([]ComponentVersion, 0, len(parsed.Components))
	for name, mc := range parsed.Components {
		if _, ok := allow[name]; !ok {
			continue // drop unauthorized keys
		}
		ver := strings.TrimSpace(mc.Version)
		if ver == "" {
			continue
		}
		out = append(out, ComponentVersion{
			Component: name,
			Version:   ver,
			Commit:    mc.Commit,
			BuildTime: mc.BuildTime,
			Source:    SourceComponentJSON,
		})
	}
	return out, "", false
}

func (c *Collector) kernelFromJSONLocked() (ComponentVersion, string) {
	path := filepath.Join(c.baseDir, "cube-kernel-scf", componentVersionJSON)
	parsed, errMsg, missing, bad := loadVersionJSON(path)
	if missing {
		return ComponentVersion{}, ""
	}
	if bad {
		return ComponentVersion{}, errMsg
	}
	if len(parsed.Variants) == 0 {
		return ComponentVersion{}, path + ": unusable kernel version.json"
	}
	variant := c.activeKernelVariantLocked()
	if variant == "" {
		return ComponentVersion{}, "kernel: cannot resolve active variant"
	}
	entry, ok := parsed.Variants[variant]
	if !ok {
		return ComponentVersion{}, "kernel: missing variant " + variant
	}
	identity := strings.TrimSpace(entry.Version)
	if identity == "" {
		identity = kernelArtifactIdentity(entry.Tag, entry.DigestSHA256)
	}
	if identity == "" {
		return ComponentVersion{}, "kernel: empty identity for " + variant
	}
	return ComponentVersion{
		Component: ComponentKernel,
		Version:   identity,
		Source:    SourceComponentJSON,
		Variant:   variant,
	}, ""
}

func kernelVariantFromVmlinuxBase(base string) string {
	switch base {
	case "vmlinux-pvm":
		return "pvm"
	case "vmlinux-bm":
		return "bm"
	}
	return ""
}

func (c *Collector) activeKernelVariantLocked() string {
	// Prefer bootstrap-state/vmlinux-active.
	active := filepath.Join(c.bootstrapDir, "vmlinux-active")
	if target, err := os.Readlink(active); err == nil {
		if v := kernelVariantFromVmlinuxBase(filepath.Base(target)); v != "" {
			return v
		}
	}
	if target, ok := c.kernelLinkTargetLocked(); ok {
		return kernelVariantFromVmlinuxBase(filepath.Base(target))
	}
	return ""
}

func (c *Collector) kernelVersionLocked(man *releaseManifest) ComponentVersion {
	variant := c.activeKernelVariantLocked()
	switch variant {
	case "pvm":
		return ComponentVersion{
			Component: ComponentKernel,
			Version:   kernelArtifactIdentity(man.Kernel.PVMVersion, man.Kernel.VMLinuxPVMDigest),
			Source:    SourceFile,
			Variant:   "pvm",
		}
	case "bm":
		return ComponentVersion{
			Component: ComponentKernel,
			Version:   kernelArtifactIdentity(man.Kernel.Version, man.Kernel.VMLinuxDigest),
			Source:    SourceFile,
			Variant:   "bm",
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
		Variant:   "bm",
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

func (c *Collector) componentInstalledLocked(name string) bool {
	if name == "" || strings.Contains(name, "..") || strings.ContainsAny(name, `/\`) {
		return false
	}
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

func (c *Collector) readSingleLine(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return firstLine(data)
}

func firstLine(data []byte) string {
	start := 0
	for start < len(data) && isSpace(data[start]) {
		start++
	}
	end := start
	for end < len(data) && data[end] != '\n' && data[end] != '\r' {
		end++
	}
	line := data[start:end]
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
