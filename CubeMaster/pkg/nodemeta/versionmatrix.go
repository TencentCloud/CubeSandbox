// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package nodemeta

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"strings"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/version"
)

var platformVersionSuffixes = []string{"-amd64", "-arm64", "-x86_64", "-aarch64"}

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

// ControlPlaneVersion describes the version of the cubemaster serving this
// request (the cluster's reference / target version).
type ControlPlaneVersion struct {
	Version   string `json:"version"`
	Commit    string `json:"commit,omitempty"`
	BuildTime string `json:"build_time,omitempty"`
}

// ComponentVersionGroup groups the nodes that report a given version of a
// component.
type ComponentVersionGroup struct {
	Version string   `json:"version"`
	Nodes   []string `json:"nodes"`
}

// ComponentMatrixRow is the per-component aggregation across all nodes.
type ComponentMatrixRow struct {
	Component        string                  `json:"component"`
	DeclaredVersion  string                  `json:"declared_version,omitempty"`
	DeclaredVersions []string                `json:"declared_versions,omitempty"`
	Consistent       bool                    `json:"consistent"`
	Versions         []ComponentVersionGroup `json:"versions"`
}

// NodeComponentEntry is a single component version on a single node. Declared
// tells whether the actual version belongs to the release-manifest declaration
// set when such a set exists; it is informational, not a failure verdict.
type NodeComponentEntry struct {
	Component string `json:"component"`
	Version   string `json:"version"`
	Declared  bool   `json:"declared"`
}

// NodeVersionRow is the per-node view of the matrix.
type NodeVersionRow struct {
	NodeID     string               `json:"node_id"`
	Healthy    bool                 `json:"healthy"`
	Components []NodeComponentEntry `json:"components"`
}

// VersionMatrix is the full node x component version matrix returned by
// GET /internal/meta/version-matrix.
type VersionMatrix struct {
	ControlPlane ControlPlaneVersion  `json:"control_plane"`
	Components   []ComponentMatrixRow `json:"components"`
	Nodes        []NodeVersionRow     `json:"nodes"`
}

// GetVersionMatrix aggregates the node-component version table into a matrix.
//
// It reads versions directly from t_cube_node_component_version (rather than
// the per-replica in-memory snapshot) so the matrix is consistent across
// cubemaster replicas. Release manifest data is exposed only as declared
// artifacts; the matrix itself is an inventory/distribution view, not a
// policy engine that decides whether mixed versions are wrong.
func GetVersionMatrix(ctx context.Context) (*VersionMatrix, error) {
	return global.getVersionMatrix(ctx)
}

func (s *service) getVersionMatrix(ctx context.Context) (*VersionMatrix, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	rows := make([]*models.NodeComponentVersion, 0)
	if err := s.db.WithContext(ctx).Model(&models.NodeComponentVersion{}).Find(&rows).Error; err != nil {
		return nil, err
	}

	healthy := s.healthyByNode(ctx)
	return buildVersionMatrix(rows, healthy, s.declaredVersions, s.declaredVersionSets), nil
}

func buildVersionMatrix(
	rows []*models.NodeComponentVersion,
	healthy map[string]bool,
	declared map[string]string,
	declaredSets map[string]map[string]struct{},
) *VersionMatrix {
	if declared == nil {
		declared = map[string]string{}
	}
	if declaredSets == nil {
		declaredSets = map[string]map[string]struct{}{}
	}

	// component -> version -> nodes
	byComponent := make(map[string]map[string][]string)
	// node -> components
	byNode := make(map[string][]NodeComponentEntry)
	nodeSet := make(map[string]struct{})
	for nodeID := range healthy {
		nodeSet[nodeID] = struct{}{}
	}

	for _, r := range rows {
		nodeSet[r.NodeID] = struct{}{}
		if byComponent[r.Component] == nil {
			byComponent[r.Component] = make(map[string][]string)
		}
		byComponent[r.Component][r.Version] = append(byComponent[r.Component][r.Version], r.NodeID)

		byNode[r.NodeID] = append(byNode[r.NodeID], NodeComponentEntry{
			Component: r.Component,
			Version:   r.Version,
			Declared:  versionIsDeclared(r.Component, r.Version, declared, declaredSets),
		})
	}

	matrix := &VersionMatrix{
		ControlPlane: ControlPlaneVersion{
			Version:   version.Version,
			Commit:    version.Commit,
			BuildTime: version.BuildTime,
		},
		Components: make([]ComponentMatrixRow, 0, len(byComponent)),
		Nodes:      make([]NodeVersionRow, 0, len(nodeSet)),
	}

	components := make([]string, 0, len(byComponent))
	for c := range byComponent {
		components = append(components, c)
	}
	sort.Strings(components)
	for _, c := range components {
		versionsMap := byComponent[c]
		groups := make([]ComponentVersionGroup, 0, len(versionsMap))
		for v, nodes := range versionsMap {
			sort.Strings(nodes)
			groups = append(groups, ComponentVersionGroup{Version: v, Nodes: nodes})
		}
		sort.Slice(groups, func(i, j int) bool { return groups[i].Version < groups[j].Version })
		matrix.Components = append(matrix.Components, ComponentMatrixRow{
			Component:        c,
			DeclaredVersion:  declared[c],
			DeclaredVersions: sortedDeclaredVersions(c, declared, declaredSets),
			Consistent:       len(groups) <= 1,
			Versions:         groups,
		})
	}

	nodeIDs := make([]string, 0, len(nodeSet))
	for n := range nodeSet {
		nodeIDs = append(nodeIDs, n)
	}
	sort.Strings(nodeIDs)
	for _, n := range nodeIDs {
		entries := byNode[n]
		sort.Slice(entries, func(i, j int) bool { return entries[i].Component < entries[j].Component })
		matrix.Nodes = append(matrix.Nodes, NodeVersionRow{
			NodeID:     n,
			Healthy:    healthy[n],
			Components: entries,
		})
	}
	return matrix
}

// healthyByNode reads node health straight from the status table so the
// matrix reflects the cluster-wide persisted state rather than this replica's
// in-memory snapshot.
func (s *service) healthyByNode(ctx context.Context) map[string]bool {
	out := make(map[string]bool)
	statuses := make([]*models.NodeStatus, 0)
	if err := s.db.WithContext(ctx).Model(&models.NodeStatus{}).Find(&statuses).Error; err != nil {
		return out
	}
	for _, st := range statuses {
		out[st.NodeID] = st.Healthy
	}
	return out
}

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

func versionIsDeclared(component, actual string, primary map[string]string, sets map[string]map[string]struct{}) bool {
	if actual == "" || actual == "unknown" {
		return false
	}
	if set := sets[component]; len(set) > 0 {
		if _, ok := set[actual]; ok {
			return true
		}
		for declared := range set {
			if versionMatchesDeclared(declared, actual) {
				return true
			}
		}
		return false
	}
	exp := primary[component]
	return exp != "" && exp != "unknown" && versionMatchesDeclared(exp, actual)
}

// versionMatchesDeclared reports whether actual matches declared. Declared
// values from the release manifest stay canonical; only actual is normalized
// by stripping known platform suffixes before comparison.
func versionMatchesDeclared(declared, actual string) bool {
	if declared == actual {
		return true
	}
	return stripPlatformVersionSuffix(actual) == declared
}

func stripPlatformVersionSuffix(value string) string {
	changed := true
	for changed {
		changed = false
		for _, suffix := range platformVersionSuffixes {
			if strings.HasSuffix(value, suffix) {
				value = strings.TrimSuffix(value, suffix)
				changed = true
				break
			}
		}
	}
	return value
}

func sortedDeclaredVersions(component string, primary map[string]string, sets map[string]map[string]struct{}) []string {
	set := sets[component]
	if len(set) == 0 {
		if primary[component] == "" {
			return nil
		}
		return []string{primary[component]}
	}
	out := make([]string, 0, len(set))
	for v := range set {
		if v != "" {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
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
