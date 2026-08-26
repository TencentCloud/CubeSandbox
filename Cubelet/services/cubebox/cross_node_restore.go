// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"fmt"
	"strings"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/pathutil"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
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
func prepareCrossNodeRestore(ctx context.Context, req *cubebox.RunCubeSandboxRequest) error {
	if req == nil {
		return nil
	}
	ann := req.GetAnnotations()
	if !isCrossNodeRestore(ann) {
		return nil
	}
	// Everything below is rejection rather than fallback: Master already
	// said the package is not on this node, so anything missing here makes
	// the restore unservable. Saying so now beats failing several steps
	// later on what would look like a corrupt catalog.
	backend, err := storageBackendFromAnnotations(ann)
	if err != nil {
		return fmt.Errorf("cross-node restore backend: %w", err)
	}
	if backend != cow.BackendS3 {
		return fmt.Errorf("cross-node restore needs the s3 backend, got %s", backend)
	}
	if cow.ParseRemoteUUIDs(ann[constants.MasterAnnotationSnapshotRemoteUUIDs]).Empty() {
		return fmt.Errorf("cross-node restore carries no remote_uuids")
	}
	sandboxID, err := ensureCrossNodeSandboxID(req)
	if err != nil {
		return err
	}
	imp := storage.CrossNodeSandboxImport(req.GetAnnotations())
	if imp == nil {
		return fmt.Errorf("cross-node restore of %s is incomplete", sandboxID)
	}
	dir, err := imp.EnsureMetadata(ctx)
	if err != nil {
		return fmt.Errorf("import metadata for cross-node restore of %s: %w", sandboxID, err)
	}
	log.G(ctx).Infof("cross-node restore %s: imported metadata at %s", sandboxID, dir)
	return nil
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
