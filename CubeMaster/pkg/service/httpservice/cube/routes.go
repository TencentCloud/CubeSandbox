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

	// Template control plane. A /cube/template/* route runs locally on
	// CubeMaster when its handler can reach a cubelet through the worker
	// grpc pool; everything else is proxied to CubeTemplateCenter.
	//
	// The worker conn pool (pkg/cubelet/grpcconn) is a process-local
	// singleton that only CubeMaster initializes — TC never does, so any
	// handler that issues a cubelet RPC fails on TC with "worker grpc conn
	// pool is not initialized". The cubelet-touching handlers are:
	//
	//   - create-from-snapshot (POST /cube/template): materializes replicas
	//     on nodes, and its rollback cleans them up.
	//   - delete (DELETE /cube/template): CubeMaster owns the whole
	//     control-plane delete — in-use check, failing in-flight work,
	//     replica cleanup on nodes, artifact node-destroys, and the DB
	//     metadata removal. Only the artifact's physical data (local ext4 /
	//     S3 object) is removed by TC, and even then only because CubeMaster
	//     calls TC's internal /tc/api/v1/artifact/delete endpoint after
	//     marking the row CLEANUP_PENDING (see
	//     templatecenter.requestTemplateCenterArtifactDelete). TC never
	//     drives a template delete itself.
	//   - redo (POST /cube/template/redo): resuming at the DISTRIBUTING
	//     phase re-pushes the artifact to nodes.
	//
	// create-from-image and redo also stay local for a second reason: after
	// persisting the job they spawn forwardBuildJobToTemplateCenter /
	// forwardRedoBuildJobToTemplateCenter, which read CubeMaster's own
	// CUBE_TEMPLATE_CENTER_ADDR to push the actual build to TC's internal
	// /tc/api/v1/build endpoint. On TC that env is never set, so a proxied
	// submit would try to forward the job back to TC itself over HTTP.
	// Keeping them local avoids this self-forward loop entirely.
	//
	// GET info/list and the alias write are also local, but for a different
	// reason: the template query caches (templateInfoCache /
	// templateListCache / templateDefinitionCache) are process-local
	// in-memory maps, so a write on CubeMaster (delete/create/alias)
	// invalidates only CubeMaster's own caches. If TC served these reads it
	// would keep answering from its now-stale cache for up to the 1-minute
	// TTL — e.g. a just-deleted template kept showing READY until the entry
	// expired. Serving reads on the process that writes keeps
	// read-your-writes coherent.
	//
	// The proxied remainder is uncached and safe on TC: the compat matrix
	// and the job/build-status polls are plain DB reads, and the artifact
	// download is file serving (or an S3 redirect). Routes are enumerated
	// explicitly (mirroring RegisterTemplateRoutes) rather than via a
	// wildcard catch-all, so the local routes can be carved out; keep this
	// list in sync with RegisterTemplateRoutes below.
	g.POST(TemplateFromImageAction, createTemplateFromImageGinHandler)
	g.POST(TemplateRedoAction, handleRedoTemplateAction)
	g.POST(TemplateAction, createTemplateGinHandler)
	g.DELETE(TemplateAction, deleteTemplateGinHandler)
	g.GET(TemplateAction, getTemplateGinHandler)
	g.PUT(TemplateAction+"/:template_id/alias", setTemplateAliasGinHandler)

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
// Only uncached pure-DB / file-serving handlers are registered here. TC
// never initializes the worker (cubelet) grpc conn pool, so every handler
// that can issue a cubelet RPC — create-from-snapshot (replica
// materialization and rollback cleanup), delete (node replica/artifact
// cleanup), redo (DISTRIBUTING resume) — is served by CubeMaster instead,
// as are the two build-submit POSTs whose forwarding reads
// CUBE_TEMPLATE_CENTER_ADDR from CubeMaster's own environment (see
// RegisterCubeRoutes). CubeMaster reaches TC's data plane exclusively
// through the internal /tc/api/v1/* endpoints (build submit, artifact
// delete), never by sending these writes here.
//
// GET info/list and PUT alias are NOT here either: they go through the
// process-local template query caches, which are only coherent on the
// process that performs the writes (CubeMaster).
//
// Mirrors the proxied subset of the Template + Artifact/CA + RootfsArtifact
// block of RegisterCubeRoutes. Keep in sync with that function.
func RegisterTemplateRoutes(g *gin.RouterGroup) {
	// Uncached reads + build status + file serving only. The cached reads
	// (info/list) and every write live on CubeMaster — see the doc comment.
	g.GET(TemplateCompatAction, getTemplateCompatGinHandler)
	g.POST(TemplateCompatAction, updateTemplateCompatGinHandler)
	g.GET(TemplateBuildStatusAction+"/:build_id/status", handleTemplateBuildStatusAction)
	g.GET(TemplateFromImageAction, getTemplateFromImageGinHandler)
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
