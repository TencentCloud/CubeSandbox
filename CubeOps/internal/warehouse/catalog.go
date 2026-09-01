// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package warehouse

import (
	"sort"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/store"
)

// ComponentSummary is the catalog row for GET /warehouse/components.
type ComponentSummary struct {
	Name         string   `json:"name"`
	VersionCount int      `json:"versionCount"`
	Arches       []string `json:"arches"`
	SizeBytes    int64    `json:"sizeBytes"`
	NodesMissing *int     `json:"nodesMissing,omitempty"`
}

// ArtifactView is one arch copy inside a version group.
type ArtifactView struct {
	Arch           string    `json:"arch"`
	SizeBytes      int64     `json:"sizeBytes"`
	Source         string    `json:"source"`
	SourceRef      string    `json:"sourceRef"`
	Checksum       string    `json:"checksum"`
	CreatedAt      time.Time `json:"createdAt"`
	NodesInstalled []string  `json:"nodesInstalled,omitempty"`
	NodesMissing   []string  `json:"nodesMissing,omitempty"`
}

// VersionGroup is one version and its per-arch artifacts.
type VersionGroup struct {
	Version   string         `json:"version"`
	Artifacts []ArtifactView `json:"artifacts"`
}

// ComponentDetail is GET /warehouse/components/:component.
type ComponentDetail struct {
	Name     string         `json:"name"`
	Versions []VersionGroup `json:"versions"`
}

// SummarizeComponents returns one row per catalog component. Empty warehouse
// still lists all four names. When coverageOK is false, NodesMissing is omitted.
func SummarizeComponents(items []store.WarehouseItem, installs []store.NodeInstall, nodeIDs []string, coverageOK bool) []ComponentSummary {
	byComp := map[string][]store.WarehouseItem{}
	for _, item := range items {
		byComp[item.Component] = append(byComp[item.Component], item)
	}
	installed := installIndex(installs)
	out := make([]ComponentSummary, 0, len(Catalog()))
	for _, info := range Catalog() {
		group := byComp[info.Name]
		sum := ComponentSummary{
			Name:   info.Name,
			Arches: []string{},
		}
		versions := map[string]struct{}{}
		arches := map[string]struct{}{}
		for _, item := range group {
			versions[item.Version] = struct{}{}
			arches[item.Arch] = struct{}{}
			sum.SizeBytes += item.SizeBytes
		}
		sum.VersionCount = len(versions)
		sum.Arches = sortedKeys(arches)
		if coverageOK {
			n := nodesMissingAny(group, installed, nodeIDs)
			sum.NodesMissing = &n
		}
		out = append(out, sum)
	}
	return out
}

// GroupComponent versions a single catalog component. Unknown names should be
// rejected before calling. When coverageOK is false, node lists are omitted.
func GroupComponent(name string, items []store.WarehouseItem, installs []store.NodeInstall, nodeIDs []string, coverageOK bool) ComponentDetail {
	installed := installIndex(installs)
	var order []string
	seen := map[string][]store.WarehouseItem{}
	for _, item := range items {
		if item.Component != name {
			continue
		}
		if _, ok := seen[item.Version]; !ok {
			order = append(order, item.Version)
		}
		seen[item.Version] = append(seen[item.Version], item)
	}
	groups := make([]VersionGroup, 0, len(order))
	for _, ver := range order {
		arts := seen[ver]
		sort.Slice(arts, func(i, j int) bool { return arts[i].Arch < arts[j].Arch })
		views := make([]ArtifactView, 0, len(arts))
		for _, item := range arts {
			view := ArtifactView{
				Arch:      item.Arch,
				SizeBytes: item.SizeBytes,
				Source:    item.Source,
				SourceRef: item.SourceRef,
				Checksum:  item.Checksum,
				CreatedAt: item.CreatedAt,
			}
			if coverageOK {
				view.NodesInstalled, view.NodesMissing = splitCoverage(item, installed, nodeIDs)
			}
			views = append(views, view)
		}
		groups = append(groups, VersionGroup{Version: ver, Artifacts: views})
	}
	return ComponentDetail{Name: name, Versions: groups}
}

type coverageKey struct {
	arch, component, version string
}

func itemKey(item store.WarehouseItem) coverageKey {
	return coverageKey{item.Arch, item.Component, item.Version}
}

func installIndex(installs []store.NodeInstall) map[coverageKey]map[string]struct{} {
	out := map[coverageKey]map[string]struct{}{}
	for _, inst := range installs {
		key := coverageKey{inst.Arch, inst.Component, inst.Version}
		if out[key] == nil {
			out[key] = map[string]struct{}{}
		}
		out[key][inst.NodeID] = struct{}{}
	}
	return out
}

func splitCoverage(item store.WarehouseItem, installed map[coverageKey]map[string]struct{}, nodeIDs []string) (present, missing []string) {
	have := installed[itemKey(item)]
	present, missing = []string{}, []string{}
	seen := map[string]struct{}{}
	for _, n := range nodeIDs {
		seen[n] = struct{}{}
		if _, ok := have[n]; ok {
			present = append(present, n)
		} else {
			missing = append(missing, n)
		}
	}
	for n := range have {
		if _, ok := seen[n]; !ok {
			present = append(present, n)
		}
	}
	return present, missing
}

func nodesMissingAny(items []store.WarehouseItem, installed map[coverageKey]map[string]struct{}, nodeIDs []string) int {
	if len(items) == 0 {
		return 0
	}
	count := 0
	for _, n := range nodeIDs {
		for _, item := range items {
			if _, ok := installed[itemKey(item)][n]; !ok {
				count++
				break
			}
		}
	}
	return count
}

func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
