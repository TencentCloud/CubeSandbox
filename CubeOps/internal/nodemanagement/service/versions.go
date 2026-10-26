// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"fmt"
	"hash/fnv"
	"sort"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
)

const incompleteVersionsHashTag = "|incomplete"

func VersionsHash(versions []model.ComponentVersion) string {
	if len(versions) == 0 {
		return ""
	}
	sorted := append([]model.ComponentVersion(nil), versions...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Component < sorted[j].Component })
	h := fnv.New64a()
	for _, v := range sorted {
		fmt.Fprintf(h, "%s|%s|%s|%s|%s|%s\n", v.Component, v.Version, v.Commit, v.BuildTime, v.Source, v.Variant)
	}
	return fmt.Sprintf("%x", h.Sum64())
}

func MergeComponentVersions(prev, next []model.ComponentVersion) []model.ComponentVersion {
	byName := make(map[string]model.ComponentVersion, len(prev)+len(next))
	for _, v := range prev {
		if v.Component == "" {
			continue
		}
		byName[v.Component] = v
	}
	for _, v := range next {
		if v.Component == "" {
			continue
		}
		byName[v.Component] = v
	}
	out := make([]model.ComponentVersion, 0, len(byName))
	for _, v := range byName {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Component < out[j].Component })
	return out
}

func CompatRelevantVersions(versions []model.ComponentVersion) map[string]string {
	out := map[string]string{
		"guest-image": "",
		"cube-agent":  "",
	}
	for _, v := range versions {
		switch v.Component {
		case "guest-image", "cube-agent":
			out[v.Component] = v.Version
		}
	}
	return out
}

func CompatVersionsChanged(prev, next map[string]string) bool {
	for _, component := range []string{"guest-image", "cube-agent"} {
		if prev[component] != next[component] {
			return true
		}
	}
	return false
}

func BuildVersionMatrix(declared map[string]string, declaredSets map[string]map[string]struct{}, nodes []*model.NodeSnapshot) *model.VersionMatrix {
	matrix := &model.VersionMatrix{
		ControlPlane: map[string]string{},
		Components:   []model.ComponentMatrixEntry{},
		Nodes:        []model.NodeVersionEntry{},
	}
	for k, v := range declared {
		matrix.ControlPlane[k] = v
	}

	componentNodes := map[string]map[string][]string{}
	componentVersions := map[string]map[string]struct{}{}
	for _, snap := range nodes {
		for _, v := range snap.Versions {
			if v.Component == "" {
				continue
			}
			if componentNodes[v.Component] == nil {
				componentNodes[v.Component] = map[string][]string{}
				componentVersions[v.Component] = map[string]struct{}{}
			}
			componentNodes[v.Component][v.Version] = append(componentNodes[v.Component][v.Version], snap.NodeID)
			componentVersions[v.Component][v.Version] = struct{}{}
		}
	}

	for comp, versions := range componentVersions {
		declaredVer := declared[comp]
		declaredSet := declaredSets[comp]
		entry := model.ComponentMatrixEntry{
			Component:        comp,
			DeclaredVersion:  declaredVer,
			DeclaredVersions: setToSortedSlice(declaredSet),
		}
		consistent := true
		for ver := range versions {
			if declaredSet != nil {
				if _, ok := declaredSet[ver]; !ok {
					consistent = false
				}
			} else if ver != declaredVer {
				consistent = false
			}
			verNodes := componentNodes[comp][ver]
			sort.Strings(verNodes)
			entry.Versions = append(entry.Versions, model.VersionNodeGroup{
				Version: ver,
				Nodes:   verNodes,
			})
		}
		sort.Slice(entry.Versions, func(i, j int) bool { return entry.Versions[i].Version < entry.Versions[j].Version })
		entry.Consistent = consistent
		matrix.Components = append(matrix.Components, entry)
	}
	sort.Slice(matrix.Components, func(i, j int) bool { return matrix.Components[i].Component < matrix.Components[j].Component })

	for _, snap := range nodes {
		nve := model.NodeVersionEntry{
			NodeID:  snap.NodeID,
			Healthy: snap.Healthy,
		}
		for _, v := range snap.Versions {
			isDeclared := false
			if set := declaredSets[v.Component]; set != nil {
				_, isDeclared = set[v.Version]
			} else {
				isDeclared = v.Version == declared[v.Component]
			}
			nve.Components = append(nve.Components, model.NodeComponentVersion{
				Component: v.Component,
				Version:   v.Version,
				Declared:  isDeclared,
			})
		}
		matrix.Nodes = append(matrix.Nodes, nve)
	}
	return matrix
}

func setToSortedSlice(s map[string]struct{}) []string {
	if len(s) == 0 {
		return nil
	}
	out := make([]string, 0, len(s))
	for k := range s {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
