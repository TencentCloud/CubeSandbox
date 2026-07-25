// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package scarceresource implements scarce-resource avoidance (SRA) for
// CubeMaster scheduling: sandboxes that do not explicitly require a scarce
// resource must not be placed on nodes labeled with that resource.
package scarceresource

import (
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/affinity"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
)

var effectiveResourcesFn = defaultEffectiveResources

func defaultEffectiveResources() []config.ScarceResourceDef {
	sconf := config.GetConfig().Scheduler
	if sconf == nil {
		return nil
	}
	return sconf.EffectiveScarceResources()
}

// FilterNodes removes scarce-resource nodes when the sandbox does not request them.
// requestSelector decides whether the sandbox declares scarce-resource demand; when
// nil, selCtx.Affinity.NodeSelector is used (normal filter chain). The backoff
// path must pass selCtx.Affinity.BackoffNodeSelector explicitly.
func FilterNodes(selCtx *selctx.SelectorCtx, in node.NodeList, filterID string, requestSelector affinity.NodeSelector) node.NodeList {
	resources := effectiveResources()
	if len(resources) == 0 {
		return in
	}
	ns := requestSelector
	if ns == nil && selCtx != nil {
		ns = selCtx.Affinity.NodeSelector
	}
	if sandboxRequestsScarceResource(ns, resources) {
		return in
	}

	out := make(node.NodeList, 0, in.Len())
	for i := range in {
		n := in[i]
		if nodeCarriesScarceResource(n, resources) {
			if selCtx != nil && selCtx.Ctx != nil {
				log.G(selCtx.Ctx).Warnf("%s scarce_resource_out node=%s", filterID, n.ID())
			}
			continue
		}
		out.Append(n)
	}
	return out
}

func effectiveResources() []config.ScarceResourceDef {
	return effectiveResourcesFn()
}

// SetEffectiveResourcesFnForTest overrides config lookup in unit tests.
func SetEffectiveResourcesFnForTest(fn func() []config.ScarceResourceDef) (restore func()) {
	prev := effectiveResourcesFn
	effectiveResourcesFn = fn
	return func() { effectiveResourcesFn = prev }
}

func nodeCarriesScarceResource(n *node.Node, resources []config.ScarceResourceDef) bool {
	if n == nil {
		return false
	}
	labels := n.Labels()
	for _, def := range resources {
		if nodeLabelMatchesScarce(labels, def) {
			return true
		}
	}
	return false
}

func nodeLabelMatchesScarce(labels map[string]string, def config.ScarceResourceDef) bool {
	if def.LabelKey == "" {
		return false
	}
	val, ok := labels[def.LabelKey]
	if !ok || val == "" {
		return false
	}
	if len(def.LabelValues) == 0 {
		// GPU-only negation heuristic when values are not configured explicitly.
		if def.LabelKey == "gpu" && (val == "false" || val == "0") {
			return false
		}
		return true
	}
	for _, want := range def.LabelValues {
		if val == want {
			return true
		}
	}
	return false
}

func sandboxRequestsScarceResource(ns affinity.NodeSelector, resources []config.ScarceResourceDef) bool {
	if ns == nil {
		return false
	}
	for _, def := range resources {
		if sandboxRequestsResource(ns, def) {
			return true
		}
	}
	return false
}

func sandboxRequestsResource(ns affinity.NodeSelector, def config.ScarceResourceDef) bool {
	if def.LabelKey == "" {
		return false
	}
	return affinity.RequiresLabelKey(ns, def.LabelKey, def.LabelValues)
}
