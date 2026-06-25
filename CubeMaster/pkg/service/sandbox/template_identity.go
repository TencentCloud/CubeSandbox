// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"

// runtimeSnapshotAnnotationKeys lists the annotation keys that may appear on
// sandbox labels and should be promoted back into the annotation map when
// reading historical sandbox data.
//
// v5: physical memory volume labels are no longer carried by master. Only
// the logical snapshot id and attachment timestamp survive on the sandbox
// metadata.
var runtimeSnapshotAnnotationKeys = []string{
	constants.CubeAnnotationRuntimeSnapshotID,
	constants.CubeAnnotationRuntimeSnapshotAttachedAt,
	constants.CubeAnnotationComponentEnvdVersion,
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func buildTemplateAnnotations(templateID string) map[string]string {
	if templateID == "" {
		return nil
	}
	return map[string]string{
		constants.CubeAnnotationAppSnapshotTemplateID: templateID,
	}
}

func templateIDFromLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	return labels[constants.CubeAnnotationAppSnapshotTemplateID]
}

func buildAnnotationsFromLabels(labels map[string]string) map[string]string {
	templateID := templateIDFromLabels(labels)
	out := buildTemplateAnnotations(templateID)
	if out == nil {
		out = map[string]string{}
	}
	for _, key := range runtimeSnapshotAnnotationKeys {
		if value := labels[key]; value != "" {
			out[key] = value
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func sandboxViewLabels(sandboxLabels, containerLabels map[string]string) map[string]string {
	if len(sandboxLabels) != 0 {
		return sandboxLabels
	}
	return cloneStringMap(containerLabels)
}
