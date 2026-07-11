// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cube

import "github.com/gin-gonic/gin"

// RegisterCubeRoutes registers all /cube routes onto the given gin.RouterGroup.
// The method/path registrations mirror the original gorilla/mux wiring in
// server.go:86-110 exactly — only handlers that were registered there are
// registered here.
func RegisterCubeRoutes(g *gin.RouterGroup) {
	// Sandbox CRUD
	g.POST(SandboxAction, Adapt(handleSandboxAction))
	g.DELETE(SandboxAction, Adapt(handleSandboxAction))
	g.POST(SandboxPreviewAction, Adapt(handleSandboxPreviewAction))
	g.POST(SandboxCommitAction, Adapt(handleSandboxCommitAction))
	g.POST(SandboxRollbackAction, Adapt(handleSandboxRollbackAction))
	g.POST(SandboxAction+"/:sandbox_id/rollback", Adapt(handleSandboxRollbackAction))
	g.POST(SandboxUpdateAction, Adapt(handleUpdateAction))
	g.POST(SandboxTimeoutAction, Adapt(handleSandboxTimeoutAction))
	g.POST(SandboxRefreshAction, Adapt(handleSandboxRefreshAction))
	g.POST(SandboxExecAction, Adapt(handleExecAction))
	g.GET(SandboxInfoAction, Adapt(handleInfoAction))
	g.POST(SandboxInfoAction, Adapt(handleInfoAction))
	g.GET(SandboxListAction, AdaptList(handleListAction))
	g.POST(SandboxListAction, AdaptList(handleListAction))
	g.GET(SandboxLogsAction, Adapt(handleSandboxLogsAction))
	g.POST(SandboxLogsAction, Adapt(handleSandboxLogsAction))

	// Image
	g.POST(ImageAction, Adapt(handleImageAction))
	g.DELETE(ImageAction, Adapt(handleImageAction))

	// Snapshot (NOTE: DELETE /snapshot collection-level is NOT registered —
	// the original mux only registered DELETE /snapshot/{snapshot_id})
	g.POST(SnapshotAction, Adapt(handleSnapshotAction))
	g.GET(SnapshotAction, Adapt(handleSnapshotAction))
	g.GET(SnapshotStorageAction, Adapt(handleSnapshotStorageAction))
	g.GET(SnapshotAction+"/:snapshot_id", Adapt(handleSnapshotAction))
	g.DELETE(SnapshotAction+"/:snapshot_id", Adapt(handleSnapshotAction))
	g.GET(OperationAction+"/:operation_id", Adapt(handleSnapshotOperationAction))

	// Template
	g.POST(TemplateAction, Adapt(handleTemplateAction))
	g.GET(TemplateAction, Adapt(handleTemplateAction))
	g.DELETE(TemplateAction, Adapt(handleTemplateAction))
	g.GET(TemplateCompatAction, Adapt(handleTemplateCompatAction))
	g.POST(TemplateCompatAction, Adapt(handleTemplateCompatAction))
	g.POST(TemplateRedoAction, Adapt(handleRedoTemplateAction))
	g.GET(TemplateBuildStatusAction+"/:build_id/status", Adapt(handleTemplateBuildStatusAction))
	g.GET(TemplateFromImageAction, Adapt(handleTemplateFromImageAction))
	g.POST(TemplateFromImageAction, Adapt(handleTemplateFromImageAction))
	g.GET(TemplateArtifactDownloadAction, Adapt(handleTemplateArtifactDownloadAction))
	g.HEAD(TemplateArtifactDownloadAction, Adapt(handleTemplateArtifactDownloadAction))

	// Artifact / CA download
	g.GET(CADownloadActionPrefix+":filename", Adapt(handleCADownloadAction))
	g.HEAD(CADownloadActionPrefix+":filename", Adapt(handleCADownloadAction))
	g.GET(RootfsArtifactAction, Adapt(handleRootfsArtifactAction))

	// Inventory
	g.POST(ListInventoryAction, Adapt(handleListInventoryAction))
}
