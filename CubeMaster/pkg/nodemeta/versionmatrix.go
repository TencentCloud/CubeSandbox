// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package nodemeta

import (
	"context"
	"encoding/json"
	"os"
	"sort"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/version"
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
	Component       string                  `json:"component"`
	ExpectedVersion string                  `json:"expected_version,omitempty"`
	Consistent      bool                    `json:"consistent"`
	Versions        []ComponentVersionGroup `json:"versions"`
}

// NodeComponentEntry is a single component version on a single node, with the
// outdated flag pre-computed against the expected version.
type NodeComponentEntry struct {
	Component string `json:"component"`
	Version   string `json:"version"`
	Outdated  bool   `json:"outdated"`
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
// cubemaster replicas. Expected versions come from the control-plane's own
// release manifest, classified per component: release-bound binaries compare
// against the manifest's (== semver) version, while independent-versioning
// components (guest-image / kernel / cube-agent) compare against their own
// manifest entry — never against the semver — so they are not falsely flagged
// as outdated.
func GetVersionMatrix(ctx context.Context) (*VersionMatrix, error) {
	return global.getVersionMatrix(ctx)
}

func (s *service) getVersionMatrix(_ context.Context) (*VersionMatrix, error) {
	rows := make([]*models.NodeComponentVersion, 0)
	if err := s.db.Model(&models.NodeComponentVersion{}).Find(&rows).Error; err != nil {
		return nil, err
	}

	healthy := s.healthyByNode()
	expected := loadExpectedVersions()

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

		exp, hasExpected := expected[r.Component]
		outdated := hasExpected && exp != "" && r.Version != exp
		byNode[r.NodeID] = append(byNode[r.NodeID], NodeComponentEntry{
			Component: r.Component,
			Version:   r.Version,
			Outdated:  outdated,
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
			Component:       c,
			ExpectedVersion: expected[c],
			Consistent:      len(groups) <= 1,
			Versions:        groups,
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
	return matrix, nil
}

// healthyByNode reads node health straight from the status table so the
// matrix reflects the cluster-wide persisted state rather than this replica's
// in-memory snapshot.
func (s *service) healthyByNode() map[string]bool {
	out := make(map[string]bool)
	statuses := make([]*models.NodeStatus, 0)
	if err := s.db.Model(&models.NodeStatus{}).Find(&statuses).Error; err != nil {
		return out
	}
	for _, st := range statuses {
		out[st.NodeID] = st.Healthy
	}
	return out
}

// releaseManifest is the subset of release-manifest.json needed to derive
// expected component versions.
type releaseManifest struct {
	Components map[string]struct {
		Version string `json:"version"`
	} `json:"components"`
	GuestImage struct {
		Version      string `json:"version"`
		AgentVersion string `json:"agent_version"`
	} `json:"guest_image"`
	Kernel struct {
		Version string `json:"version"`
	} `json:"kernel"`
}

// loadExpectedVersions returns the expected version per component, derived
// from the control-plane's own release manifest. Returns an empty map (no
// outdated checks, consistency-only) when the manifest is missing/unreadable.
func loadExpectedVersions() map[string]string {
	path := os.Getenv("CUBE_RELEASE_MANIFEST")
	if path == "" {
		path = defaultReleaseManifestPath
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]string{}
	}
	var m releaseManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return map[string]string{}
	}
	expected := make(map[string]string, len(m.Components)+3)
	for name, c := range m.Components {
		expected[name] = c.Version
	}
	// guest-image / cube-agent / kernel follow their own version systems; take
	// them from the dedicated manifest sections (cube-agent overrides any
	// components["cube-agent"] entry to match the agent baked into the guest).
	if m.GuestImage.Version != "" {
		expected[componentGuestImage] = m.GuestImage.Version
	}
	if m.GuestImage.AgentVersion != "" {
		expected[componentCubeAgent] = m.GuestImage.AgentVersion
	}
	if m.Kernel.Version != "" {
		expected[componentKernel] = m.Kernel.Version
	}
	return expected
}
