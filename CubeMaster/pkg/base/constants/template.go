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
	// shared secret from. The chart, one-click installer and Terraform always
	// generate one; when it is unset both ends fail CLOSED (the callback and
	// TC's internal API refuse all calls) unless the operator explicitly opts
	// into token-less mode via TemplateCallbackInsecureNoTokenEnv.
	TemplateCallbackTokenEnv = "CUBE_TEMPLATE_CALLBACK_TOKEN"
	// TemplateCallbackInsecureNoTokenEnv is the explicit escape hatch for
	// running WITHOUT the callback token: only acceptable in a single-binary
	// local dev setup where the listener never leaves the loopback host. Set
	// to "true" on BOTH CubeMaster and CubeTemplateCenter. Never set it in a
	// deployed environment — a forged BUILT report becomes the rootfs every
	// node boots from.
	TemplateCallbackInsecureNoTokenEnv = "CUBE_TEMPLATE_CALLBACK_INSECURE_NO_TOKEN"
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
