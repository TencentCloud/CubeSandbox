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

	// Template control plane. Every /cube/template/* route is proxied
	// straight to CubeTemplateCenter EXCEPT the two POSTs below (from-image
	// create, redo), which CubeMaster must handle itself.
	//
	// Why the split: createTemplateFromImageGinHandler and
	// handleRedoTemplateAction share code with TC (same package) and, after
	// persisting the job, spawn forwardBuildJobToTemplateCenter /
	// forwardRedoBuildJobToTemplateCenter, which read CubeMaster's own
	// CUBE_TEMPLATE_CENTER_ADDR to push the actual build to TC's internal
	// /tc/api/v1/build endpoint. If these two requests were proxied to TC like
	// every other template route, TC would run that same handler code
	// in-process and its forward call would try to read
	// CUBE_TEMPLATE_CENTER_ADDR from TC's OWN process environment — which is
	// never set (only CubeMaster's process env carries it) — and TC would end
	// up trying to forward the job back to itself over HTTP. Keeping
	// create/redo local to CubeMaster avoids this self-forward loop entirely.
	//
	// Every other template route (status/list/compat/artifact download/CRUD
	// GET-DELETE-alias) is a pure DB read/write with no forwarding, so it is
	// safe to run on either process and stays proxied to TC, which owns
	// that part of the template API surface. Routes are enumerated
	// explicitly (mirroring RegisterTemplateRoutes) rather than via a
	// wildcard catch-all, so the two local routes can be carved out; keep
	// this list in sync with RegisterTemplateRoutes below.
	g.POST(TemplateFromImageAction, createTemplateFromImageGinHandler)
	g.POST(TemplateRedoAction, handleRedoTemplateAction)

	g.POST(TemplateAction, proxyToTemplateCenter)
	g.GET(TemplateAction, proxyToTemplateCenter)
	g.DELETE(TemplateAction, proxyToTemplateCenter)
	g.PUT(TemplateAction+"/:template_id/alias", proxyToTemplateCenter)
	g.GET(TemplateCompatAction, proxyToTemplateCenter)
	g.POST(TemplateCompatAction, proxyToTemplateCenter)
	g.GET(TemplateBuildStatusAction+"/:build_id/status", proxyToTemplateCenter)
	g.GET(TemplateFromImageAction, proxyToTemplateCenter)
	g.GET(TemplateArtifactDownloadAction, proxyToTemplateCenter)
	g.HEAD(TemplateArtifactDownloadAction, proxyToTemplateCenter)

	// Artifact / CA download: served locally from the shared artifact disk so
	// Cubelet does not pay a second network hop through TC.
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

// RegisterTemplateRoutes registers ONLY the template-related routes onto g.
// Used by the standalone CubeTemplateCenter process — sandbox / snapshot /
// volume CRUD stay with CubeMaster and are NOT registered here.
//
// Mirrors the Template + Artifact/CA + RootfsArtifact block of
// RegisterCubeRoutes. Keep in sync with that function.
func RegisterTemplateRoutes(g *gin.RouterGroup) {
	// Template CRUD + build status
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
}

// RegisterInternalTemplateRoutes registers internal callbacks used by the
// remote build mode (CubeTemplateCenter -> CubeMaster status reports).
// Mounted on CubeMaster only — see pkg/server/server.go. These routes are
// NOT part of the public /cube API surface.
func RegisterInternalTemplateRoutes(g *gin.RouterGroup) {
	g.POST("/internal/template/jobs/:job_id/status", handleTemplateJobStatusCallback)
}
