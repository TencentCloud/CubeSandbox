// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package cube provides the interface for cube master
package cube

import (
	"net/http"
	"path/filepath"
	"strings"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/cubelog"
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
)

func CubeURI() string {
	return cubeURI
}

func actionURI(uri string) string {
	return filepath.Clean(filepath.Join(cubeURI, uri))
}

func HttpHandler(w http.ResponseWriter, r *http.Request) {
	rt := CubeLog.GetTraceInfo(r.Context())
	var rsp interface{}
	switch {
	case r.URL.Path == actionURI(SnapshotStorageAction):
		rsp = handleSnapshotStorageAction(w, r, rt)
	case strings.HasPrefix(r.URL.Path, actionURI(OperationAction)+"/"):
		rsp = handleSnapshotOperationAction(w, r, rt)
	case isSandboxRollbackResourcePath(r.URL.Path):
		rsp = handleSandboxRollbackAction(w, r, rt)
	case strings.HasPrefix(r.URL.Path, actionURI(SnapshotAction)+"/"):
		rsp = handleSnapshotAction(w, r, rt)
	case strings.HasPrefix(r.URL.Path, actionURI(TemplateBuildStatusAction)+"/"):
		rsp = handleTemplateBuildStatusAction(w, r, rt)
	case r.URL.Path == actionURI(SandboxPreviewAction):
		rsp = handleSandboxPreviewAction(w, r, rt)
	case r.URL.Path == actionURI(SandboxAction):
		rsp = handleSandboxAction(w, r, rt)
	case r.URL.Path == actionURI(ImageAction):
		rsp = handleImageAction(w, r, rt)
	case r.URL.Path == actionURI(SandboxListAction):
		rsp = handleListAction(w, r, rt)
	case r.URL.Path == actionURI(SandboxInfoAction):
		rsp = handleInfoAction(w, r, rt)
	case r.URL.Path == actionURI(SandboxExecAction):
		rsp = handleExecAction(w, r, rt)
	case r.URL.Path == actionURI(SandboxLogsAction):
		rsp = handleSandboxLogsAction(w, r, rt)
	case r.URL.Path == actionURI(SandboxUpdateAction):
		rsp = handleUpdateAction(w, r, rt)
	case r.URL.Path == actionURI(SandboxTimeoutAction):
		rsp = handleSandboxTimeoutAction(w, r, rt)
	case r.URL.Path == actionURI(SandboxRefreshAction):
		rsp = handleSandboxRefreshAction(w, r, rt)
	case r.URL.Path == actionURI(SandboxCommitAction):
		rsp = handleSandboxCommitAction(w, r, rt)
	case r.URL.Path == actionURI(SandboxRollbackAction):
		rsp = handleSandboxRollbackAction(w, r, rt)
	case r.URL.Path == actionURI(SnapshotAction):
		rsp = handleSnapshotAction(w, r, rt)
	case r.URL.Path == actionURI(TemplateAction):
		rsp = handleTemplateAction(w, r, rt)
	case r.URL.Path == actionURI(TemplateCompatAction):
		rsp = handleTemplateCompatAction(w, r, rt)
	case r.URL.Path == actionURI(TemplateRedoAction):
		rsp = handleRedoTemplateAction(w, r, rt)
	case r.URL.Path == actionURI(TemplateFromImageAction):
		rsp = handleTemplateFromImageAction(w, r, rt)
	case r.URL.Path == actionURI(TemplateArtifactDownloadAction):
		rsp = handleTemplateArtifactDownloadAction(w, r, rt)
	case strings.HasPrefix(r.URL.Path, actionURI(CADownloadActionPrefix)):
		rsp = handleCADownloadAction(w, r, rt)
	case r.URL.Path == actionURI(RootfsArtifactAction):
		rsp = handleRootfsArtifactAction(w, r, rt)
	case r.URL.Path == actionURI(ListInventoryAction):
		rsp = handleListInventoryAction(w, r, rt)
	default:
		rt.RetCode = -1
		rsp = &types.Res{
			Ret: &types.Ret{
				RetCode: -1,
				RetMsg:  http.StatusText(http.StatusNotFound),
			},
		}
	}
	if r.URL.Path == actionURI(SandboxListAction) {
		common.WriteListResponse(w, http.StatusOK, rsp)
	} else if r.URL.Path == actionURI(TemplateArtifactDownloadAction) {
		return
	} else if strings.HasPrefix(r.URL.Path, actionURI(CADownloadActionPrefix)) && rsp == nil {
		// handleCADownloadAction streamed the file via http.ServeContent;
		// nothing left to write. On error paths it returned a *types.Res
		// and we fall through to WriteResponse below.
		return
	} else {
		common.WriteResponse(w, http.StatusOK, rsp)
	}
}

func isSandboxRollbackResourcePath(path string) bool {
	path = strings.Trim(strings.TrimSpace(path), "/")
	parts := strings.Split(path, "/")
	return len(parts) == 4 && parts[0] == strings.Trim(cubeURI, "/") && parts[1] == strings.Trim(SandboxAction, "/") && parts[3] == "rollback"
}

func getCaller(r *http.Request) string {
	if r.Header.Get(constants.Caller) != "" {
		return r.Header.Get(constants.Caller)
	}
	return constants.Caller
}
