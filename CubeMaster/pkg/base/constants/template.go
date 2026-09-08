// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package constants

const (
	// TemplateCallbackTokenHeader carries the shared secret on
	// CubeTemplateCenter -> CubeMaster build-status callbacks
	// (POST /internal/template/jobs/:job_id/status). The callback payload is
	// trusted wholesale by the resume pipeline (artifact id/sha become the
	// rootfs nodes boot from), so the endpoint must not stay anonymous on the
	// public HTTP port.
	TemplateCallbackTokenHeader = "X-Cube-Template-Callback-Token"
	// TemplateCallbackTokenEnv is the environment variable both sides read the
	// shared secret from. When unset on CubeMaster the callback stays open
	// (with a warning) so an older TC keeps working during a rolling upgrade;
	// the chart and one-click installers always generate one.
	TemplateCallbackTokenEnv = "CUBE_TEMPLATE_CALLBACK_TOKEN"
)

func GetAppSnapshotVersion(annotations map[string]string) string {
	if annotations == nil {
		return ""
	}
	if v := annotations[CubeAnnotationAppSnapshotVersion]; v != "" {
		return v
	}
	return annotations[CubeAnnotationAppSnapshotTemplateVersion]
}

func HasAppSnapshotTemplateVersion(annotations map[string]string) bool {
	return GetAppSnapshotVersion(annotations) != ""
}

func SetAppSnapshotVersion(annotations map[string]string, version string) {
	if annotations == nil || version == "" {
		return
	}
	annotations[CubeAnnotationAppSnapshotVersion] = version
	annotations[CubeAnnotationAppSnapshotTemplateVersion] = version
}

func NormalizeAppSnapshotAnnotations(annotations map[string]string) {
	SetAppSnapshotVersion(annotations, GetAppSnapshotVersion(annotations))
}
