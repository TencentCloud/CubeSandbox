// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package cube provides the interface for cube master
package cube

import (
	"net/http"
	"path/filepath"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
)

const (
	cubeURI                        = "/cube"
	SandboxAction                  = "/sandbox"
	SandboxPreviewAction           = "/sandbox/preview"
	ImageAction                    = "/image"
	SandboxListAction              = "/sandbox/list"
	SandboxInfoAction              = "/sandbox/info"
	SandboxExecAction              = "/sandbox/exec"
	SandboxUpdateAction            = "/sandbox/update"
	SandboxTimeoutAction           = "/sandbox/timeout"
	SandboxRefreshAction           = "/sandbox/refresh"
	SandboxCommitAction            = "/sandbox/commit"
	SandboxRollbackAction          = "/sandbox/rollback"
	SnapshotAction                 = "/snapshot"
	SnapshotStorageAction          = "/snapshot/storage"
	OperationAction                = "/operation"
	TemplateAction                 = "/template"
	TemplateCompatAction           = "/template/compat"
	TemplateRedoAction             = "/template/redo"
	TemplateBuildStatusAction      = "/template/build"
	TemplateFromImageAction        = "/template/from-image"
	TemplateArtifactDownloadAction = "/template/artifact/download"
	RootfsArtifactAction           = "/rootfs-artifact"
	CADownloadActionPrefix         = "/ca/"
	ListInventoryAction            = "/listinventory"
	SandboxLogsAction              = "/sandbox/logs"
	VolumeAction                   = "/volume"
)

func CubeURI() string {
	return cubeURI
}

func actionURI(uri string) string {
	return filepath.Clean(filepath.Join(cubeURI, uri))
}

func getCaller(r *http.Request) string {
	if r.Header.Get(constants.Caller) != "" {
		return r.Header.Get(constants.Caller)
	}
	return constants.Caller
}
