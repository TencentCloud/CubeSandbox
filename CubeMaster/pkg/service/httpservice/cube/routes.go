// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cube

import "github.com/gin-gonic/gin"

// RegisterCubeRoutes registers all /cube routes onto the given gin.RouterGroup.
// The method/path registrations preserve parity with the previous observable
// behavior of the gorilla/mux wiring in server.go. One intentional delta from
// a literal mux mirror:
//
//   - GET /cube/snapshot/storage is registered as an explicit static route
//     (ahead of /cube/snapshot/:snapshot_id), so the storage listing is never
//     shadowed by the param + method switch — safer than the mux approach of
//     parsing the path inside a single handler.
//
// (mux's POST /internal/fake_create is registered in the inner router, not here
// — see the inner package.)
func RegisterCubeRoutes(g *gin.RouterGroup) {
	// Sandbox CRUD
	g.POST(SandboxAction, createSandboxGinHandler)
	g.DELETE(SandboxAction, deleteSandboxGinHandler)
	g.POST(SandboxPreviewAction, handleSandboxPreviewAction)
	g.POST(SandboxCommitAction, handleSandboxCommitAction)
	g.POST(SandboxRollbackAction, handleSandboxRollbackAction)
	g.POST(SandboxAction+"/:sandbox_id/rollback", handleSandboxRollbackAction)
	g.POST(SandboxUpdateAction, handleUpdateAction)
	g.POST(SandboxNetworkAction, handleSandboxNetworkAction)
	g.POST(SandboxTimeoutAction, handleSandboxTimeoutAction)
	g.POST(SandboxRefreshAction, handleSandboxRefreshAction)
	g.POST(SandboxExecAction, handleExecAction)
	g.GET(SandboxInfoAction, handleInfoAction)
	g.POST(SandboxInfoAction, handleInfoAction)
	g.GET(SandboxListAction, handleListAction)
	g.POST(SandboxListAction, handleListAction)
	g.GET(SandboxInventoryAction, handleInventoryAction)
	g.GET(SandboxLogsAction, handleSandboxLogsAction)
	g.POST(SandboxLogsAction, handleSandboxLogsAction)

	// Image
	g.POST(ImageAction, createImageGinHandler)
	g.DELETE(ImageAction, deleteImageGinHandler)

	// Snapshot (NOTE: DELETE /snapshot collection-level is NOT registered —
	// the original mux only registered DELETE /snapshot/{snapshot_id})
	g.POST(SnapshotAction, createSnapshotGinHandler)
	g.GET(SnapshotAction, getSnapshotGinHandler)
	g.GET(SnapshotStorageAction, handleSnapshotStorageAction)
	// Bare-factor compatible-nodes: an explicit static route (ahead of the
	// /snapshot/:snapshot_id param route) for snapshot-less diagnosis, so callers
	// don't have to pass a dummy snapshot_id segment.
	g.GET(SnapshotCompatibleNodesByFactorsAction, compatibleNodesByFactorsGinHandler)
	g.GET(SnapshotAction+"/:snapshot_id", getSnapshotGinHandler)
	g.GET(SnapshotAction+"/:snapshot_id/restore-compat", restoreCompatGinHandler)
	g.GET(SnapshotAction+"/:snapshot_id/compatible-nodes", compatibleNodesGinHandler)
	g.DELETE(SnapshotAction+"/:snapshot_id", deleteSnapshotGinHandler)
	g.GET(OperationAction+"/:operation_id", handleSnapshotOperationAction)

	// Template
	g.POST(TemplateAction, createTemplateGinHandler)
	g.GET(TemplateAction, getTemplateGinHandler)
	g.DELETE(TemplateAction, deleteTemplateGinHandler)
	g.PUT(TemplateAction+"/:template_id/alias", setTemplateAliasGinHandler)
	g.GET(TemplateCompatAction, getTemplateCompatGinHandler)
	g.POST(TemplateCompatAction, updateTemplateCompatGinHandler)
	g.POST(TemplateRedoAction, handleRedoTemplateAction)
	g.GET(TemplateBuildStatusAction+"/:build_id/status", handleTemplateBuildStatusAction)
	g.GET(TemplateFromImageAction, getTemplateFromImageGinHandler)
	g.POST(TemplateFromImageAction, createTemplateFromImageGinHandler)
	g.GET(TemplateArtifactDownloadAction, downloadTemplateArtifactGinHandler)
	g.HEAD(TemplateArtifactDownloadAction, headTemplateArtifactGinHandler)

	// Artifact / CA download
	g.GET(CADownloadActionPrefix+":filename", downloadCAGinHandler)
	g.HEAD(CADownloadActionPrefix+":filename", headCAGinHandler)
	g.GET(RootfsArtifactAction, handleRootfsArtifactAction)

	// Inventory
	g.POST(ListInventoryAction, handleListInventoryAction)

	// Volume plugin CRUD
	g.GET(VolumeAction, handleListVolumes)
	g.POST(VolumeAction, handleCreateVolume)
	g.GET(VolumeAction+"/:volume_id", handleGetVolume)
	g.DELETE(VolumeAction+"/:volume_id", handleDeleteVolume)
}
