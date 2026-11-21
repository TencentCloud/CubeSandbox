// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/pathutil"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

// isCrossNodeRestore reports Master's verdict that it placed this restore on
// a node holding no replica of the package. Only Master can know this, so
// Cubelet takes it as fact rather than inferring it from a catalog miss.
func isCrossNodeRestore(ann map[string]string) bool {
	return strings.EqualFold(strings.TrimSpace(ann[constants.MasterAnnotationSnapshotCrossNode]), "true")
}

// prepareCrossNodeRestore settles the sandbox id and puts the sandbox's
// metadata disk in place, before anything downstream tries to read the
// package this create restores from.
//
// A package is the static set of snapshot objects belonging to the node that
// built it, and none of it exists here. What the restore actually needs off
// it is a description — sandbox_spec.json for a Resume, the kernel／image
// config for both paths — and that description travels on the metadata disk,
// which is imported under the sandbox's own name and lives and dies with the
// sandbox. Nothing package-shaped is created on this node, because no owner
// on this node would ever delete it.
//
// This runs at the entry rather than inside the create workflow because the
// workflow assigns sandbox ids in parallel with the step that first needs the
// description. Minting the id here removes that race: every later step reads
// the same id off the request. Resume already carries one, since it restores
// a sandbox that keeps its identity.
//
// The rootfs and memory are left to the storage plugin, which imports them
// under the same sandbox-private naming once the create is under way, so a
// create that dies halfway leaves only disks destroy already knows about.
func prepareCrossNodeRestore(ctx context.Context, req *cubebox.RunCubeSandboxRequest) (context.Context, error) {
	if req == nil {
		return ctx, nil
	}
	ann := req.GetAnnotations()
	if !isCrossNodeRestore(ann) {
		return ctx, nil
	}
	if cow.IsXFSS3Backend(ann[constants.MasterAnnotationStorageBackend]) {
		return ctx, materializeXFSS3Restore(ctx, ann)
	}
	// Everything below is rejection rather than fallback: Master already
	// said the package is not on this node, so anything missing here makes
	// the restore unservable. Saying so now beats failing several steps
	// later on what would look like a corrupt catalog.
	backend, err := storageBackendFromAnnotations(ann)
	if err != nil {
		return ctx, fmt.Errorf("cross-node restore backend: %w", err)
	}
	if !storage.CrossNodeImportBackend(backend) {
		return ctx, fmt.Errorf("cross-node restore needs the s3 backend, got %s", backend)
	}
	if cow.ParseRemoteUUIDs(ann[constants.MasterAnnotationSnapshotRemoteUUIDs]).Empty() {
		return ctx, fmt.Errorf("cross-node restore carries no remote_uuids")
	}
	sandboxID, err := ensureCrossNodeSandboxID(req)
	if err != nil {
		return ctx, err
	}
	imp := storage.CrossNodeSandboxImport(req.GetAnnotations())
	if imp == nil {
		return ctx, fmt.Errorf("cross-node restore of %s is incomplete", sandboxID)
	}
	dir, err := imp.EnsureMetadata(ctx)
	if err != nil {
		return ctx, fmt.Errorf("import metadata for cross-node restore of %s: %w", sandboxID, err)
	}
	log.G(ctx).Infof("cross-node restore %s: imported metadata at %s", sandboxID, dir)
	return ctx, nil
}

// xfss3MaterializeTimeout bounds one full download. It is detached from the
// request: a caller that gives up still leaves a usable local package behind
// for its retry.
const xfss3MaterializeTimeout = 30 * time.Minute

var materializeXFSS3Pause = storage.MaterializeXFSS3Pause

// materializeXFSS3Restore turns a cross-node xfs-s3 Resume into a same-node
// one: the verified checkpoint is downloaded in full and becomes this node's
// local pause package before anything reads the package. Unlike the s3
// backend nothing is imported under the sandbox's name; the package is
// owned like any pause package and goes with the next Pause or Destroy.
func materializeXFSS3Restore(ctx context.Context, ann map[string]string) error {
	snapID := strings.TrimSpace(ann[constants.MasterAnnotationPauseSnapshotID])
	if snapID == "" {
		return fmt.Errorf("cross-node xfs-s3 restore carries no pause snapshot id")
	}
	if err := pathutil.ValidateSafeID(snapID); err != nil {
		return fmt.Errorf("invalid pause snapshot id: %w", err)
	}
	locator := strings.TrimSpace(ann[constants.MasterAnnotationSnapshotRemoteUUIDs])
	if locator == "" {
		return fmt.Errorf("cross-node xfs-s3 restore of %s carries no checkpoint locator", snapID)
	}
	dlCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), xfss3MaterializeTimeout)
	defer cancel()
	start := time.Now()
	if err := materializeXFSS3Pause(dlCtx, snapID, locator); err != nil {
		return fmt.Errorf("materialize xfs-s3 checkpoint %s: %w", snapID, err)
	}
	log.G(ctx).Infof("cross-node xfs-s3 restore: package %s is local after %s", snapID, time.Since(start))
	return nil
}

// isXFSS3Request reports whether Master named the xfs-s3 backend.
func isXFSS3Request(req *cubebox.RunCubeSandboxRequest) bool {
	return cow.IsXFSS3Backend(req.GetAnnotations()[constants.MasterAnnotationStorageBackend])
}

// xfss3ResumeRetry says what to do with an xfs-s3 Resume of a sandbox this
// node already holds.
type xfss3ResumeRetry int

const (
	// xfss3ResumeFresh: no record of the sandbox here, or only its PAUSED
	// tombstone (a same-node Resume replaces that as usual).
	xfss3ResumeFresh xfss3ResumeRetry = iota
	// xfss3ResumeReplay: the sandbox runs here; an earlier attempt whose
	// reply was lost started it. Answer with it instead of starting another.
	xfss3ResumeReplay
	// xfss3ResumeBusy: the sandbox is here in some other state (creating,
	// stopped, being restarted). Neither a new start nor a release of the
	// pin is safe, so the attempt reports a lifecycle conflict.
	xfss3ResumeBusy
)

// existingXFSS3Sandbox decides how an xfs-s3 Resume treats a sandbox of the
// same id that this node already holds. Master pins a Resume whose reply it
// lost to its node and sends it again, so whatever runs here under that id
// is this sandbox, whatever its restore lineage says: a restart policy or an
// opaque restore clears the lineage of the very instance the lost attempt
// started. Other backends keep their Create semantics.
func existingXFSS3Sandbox(boxes []*cubeboxstore.CubeBox, req *cubebox.RunCubeSandboxRequest, sandboxID string) (*cubeboxstore.CubeBox, xfss3ResumeRetry) {
	if !isXFSS3Request(req) || sandboxID == "" {
		return nil, xfss3ResumeFresh
	}
	for _, sb := range boxes {
		if sb == nil || sb.ID != sandboxID {
			continue
		}
		state := cubebox.ContainerState_CONTAINER_UNKNOWN
		if st := sb.GetStatus(); st != nil {
			state = st.Get().State()
		}
		switch state {
		case cubebox.ContainerState_CONTAINER_PAUSED:
			return nil, xfss3ResumeFresh
		case cubebox.ContainerState_CONTAINER_RUNNING:
			return sb, xfss3ResumeReplay
		default:
			return sb, xfss3ResumeBusy
		}
	}
	return nil, xfss3ResumeFresh
}

// ensureCrossNodeSandboxID pins the id this restore will run under, minting
// one when the request does not name it. A Resume always does, because it
// keeps the paused sandbox's identity; a create from a template or snapshot
// would otherwise be given a fresh id inside the workflow, too late for the
// steps that have to name this sandbox's disks.
func ensureCrossNodeSandboxID(req *cubebox.RunCubeSandboxRequest) (string, error) {
	if req.Annotations == nil {
		req.Annotations = map[string]string{}
	}
	sandboxID := strings.TrimSpace(req.Annotations[constants.MasterAnnotationDesiredSandboxID])
	if sandboxID == "" {
		sandboxID = utils.GenerateID()
		req.Annotations[constants.MasterAnnotationDesiredSandboxID] = sandboxID
		return sandboxID, nil
	}
	// Master-supplied, and about to become a path component.
	if err := pathutil.ValidateSafeID(sandboxID); err != nil {
		return "", fmt.Errorf("invalid desired sandbox id: %w", err)
	}
	return sandboxID, nil
}
