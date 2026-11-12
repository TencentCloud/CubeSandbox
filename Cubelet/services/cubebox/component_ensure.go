// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/components"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
)

var (
	componentManagerOnce sync.Once
	componentManagerInst *components.ComponentManager
)

func getComponentManager() *components.ComponentManager {
	if componentManagerInst != nil {
		return componentManagerInst
	}
	componentManagerOnce.Do(func() {
		if componentManagerInst == nil {
			componentManagerInst = components.NewComponentManager(nil)
		}
	})
	return componentManagerInst
}

// SetComponentManagerForTest overrides the ComponentManager used by Ensure helpers.
func SetComponentManagerForTest(cm *components.ComponentManager) {
	componentManagerInst = cm
}

// EnsureTemplateComponents resolves inventory LocalPaths into Componts.
// Entries without a version are skipped.
func EnsureTemplateComponents(ctx context.Context, local *templatetypes.LocalRunTemplate, versions map[string]string) error {
	if local == nil {
		return nil
	}
	merged := mergeComponentVersionMaps(versions, versionsFromLocalTemplate(local))
	if len(merged) == 0 {
		return nil
	}
	if local.Componts == nil {
		local.Componts = make(map[string]templatetypes.LocalComponent)
	}
	cm := getComponentManager()
	for _, name := range []string{
		templatetypes.CubeComponentCubeShim,
		templatetypes.CubeComponentCubeKernel,
		templatetypes.CubeComponentCubeImage,
		templatetypes.CubeComponentCubeAgent,
	} {
		ver := strings.TrimSpace(merged[name])
		if ver == "" {
			continue
		}
		relativePath := ""
		if existing, ok := local.Componts[name]; ok {
			relativePath = strings.TrimSpace(existing.Component.Path)
			// Absolute Path from a prior Ensure → use default relative path.
			if strings.HasPrefix(relativePath, "/") {
				relativePath = ""
			}
		}
		localPath, err := cm.Ensure(ctx, name, ver, relativePath)
		if err != nil {
			return err
		}
		key := templatetypes.InventoryVersionKey(ver)
		lc := local.Componts[name]
		lc.Component.Name = name
		lc.Component.Version = key
		lc.Component.Path = localPath
		local.Componts[name] = lc
	}
	return nil
}

// EnsureCubeBoxComponents Ensures from CubeBox.ComponentVersions and Componts.
func EnsureCubeBoxComponents(ctx context.Context, cb *cubeboxstore.CubeBox) error {
	if cb == nil {
		return nil
	}
	hasVersions := len(cb.ComponentVersions) > 0 || len(versionsFromLocalTemplate(cb.LocalRunTemplate)) > 0
	if !hasVersions {
		return nil
	}
	if cb.LocalRunTemplate == nil {
		cb.LocalRunTemplate = &templatetypes.LocalRunTemplate{
			Componts: map[string]templatetypes.LocalComponent{},
		}
	}
	templatetypes.ApplyVersionMap(cb.LocalRunTemplate, cb.ComponentVersions)
	return EnsureTemplateComponents(ctx, cb.LocalRunTemplate, cb.ComponentVersions)
}

// seedCubeBoxComponentVersionsFromRequest fills ComponentVersions before Ensure.
func seedCubeBoxComponentVersionsFromRequest(cb *cubeboxstore.CubeBox, flowOpts *workflow.CreateContext) error {
	if cb == nil {
		return nil
	}
	out := make(map[string]string)
	for k, v := range cb.ComponentVersions {
		if key := templatetypes.InventoryVersionKey(v); key != "" {
			out[k] = key
		}
	}
	seedVersionsFromTemplate(out, cb.LocalRunTemplate)
	keepShim := flowOpts != nil && flowOpts.IsRestart && out[templatetypes.CubeComponentCubeShim] != ""
	if !keepShim && flowOpts != nil && flowOpts.ReqInfo != nil {
		raw := strings.TrimSpace(flowOpts.ReqInfo.GetAnnotations()[constants.MasterAnnotationComponentCubeShimVersion])
		if raw != "" && templatetypes.InventoryVersionKey(raw) == "" {
			return fmt.Errorf("invalid explicit shim version %q", raw)
		}
	}
	if flowOpts != nil && flowOpts.ReqInfo != nil {
		for name, ver := range componentVersionsFromAnnotations(flowOpts.ReqInfo.GetAnnotations()) {
			// The master's selected replica can explicitly migrate the shim
			// without rewriting a sealed snapshot's catalog. Keep guest, agent
			// and kernel pins immutable; only the host runtime binding changes.
			if name == templatetypes.CubeComponentCubeShim {
				// Restart the existing instance with its persisted host binding.
				if keepShim {
					continue
				}
				out[name] = ver
				continue
			}
			if _, exists := out[name]; exists {
				continue
			}
			out[name] = ver
		}
	}
	if len(out) == 0 {
		return nil
	}
	cb.ComponentVersions = out
	if cb.LocalRunTemplate == nil {
		cb.LocalRunTemplate = &templatetypes.LocalRunTemplate{
			Componts: map[string]templatetypes.LocalComponent{},
		}
	}
	templatetypes.ApplyVersionMap(cb.LocalRunTemplate, out)
	return nil
}

func componentVersionsFromAnnotations(annotations map[string]string) map[string]string {
	if len(annotations) == 0 {
		return nil
	}
	keys := map[string]string{
		constants.MasterAnnotationComponentCubeShimVersion:   templatetypes.CubeComponentCubeShim,
		constants.MasterAnnotationComponentCubeKernelVersion: templatetypes.CubeComponentCubeKernel,
		constants.MasterAnnotationComponentCubeImageVersion:  templatetypes.CubeComponentCubeImage,
		constants.MasterAnnotationComponentCubeAgentVersion:  templatetypes.CubeComponentCubeAgent,
	}
	out := make(map[string]string)
	for anno, name := range keys {
		ver := strings.TrimSpace(annotations[anno])
		if ver == "" {
			continue
		}
		out[name] = templatetypes.InventoryVersionKey(ver)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func versionsFromLocalTemplate(local *templatetypes.LocalRunTemplate) map[string]string {
	raw := templatetypes.VersionMapFromComponts(local)
	if len(raw) == 0 {
		return nil
	}
	out := make(map[string]string, len(raw))
	for name, ver := range raw {
		if key := templatetypes.InventoryVersionKey(ver); key != "" {
			out[name] = key
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func mergeComponentVersionMaps(primary, secondary map[string]string) map[string]string {
	if len(primary) == 0 && len(secondary) == 0 {
		return nil
	}
	out := make(map[string]string, len(primary)+len(secondary))
	for k, v := range secondary {
		if strings.TrimSpace(v) == "" {
			continue
		}
		out[k] = strings.TrimSpace(v)
	}
	for k, v := range primary {
		if strings.TrimSpace(v) == "" {
			continue
		}
		out[k] = strings.TrimSpace(v)
	}
	return out
}
