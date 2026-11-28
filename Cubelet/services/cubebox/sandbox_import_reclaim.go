// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

const (
	// abandonedImportReclaimInterval paces the sweep for imports whose Create
	// ended without a sandbox taking them over.
	abandonedImportReclaimInterval = 5 * time.Minute
	// abandonedImportMinAge keeps the sweep off a metadata home a Create may
	// still be filling. It applies only to homes imported before claims were
	// recorded; a claimed import is protected by its in-flight claim.
	abandonedImportMinAge = time.Hour
)

var (
	reclaimAbandonedImportsFn = storage.ReclaimAbandonedSandboxImports
	inspectStorageRecordsFn   = storage.InspectStorageVolumes
	finishImportClaimFn       = (*storage.SandboxImportClaim).Finish
)

// sandboxImportOwned reports whether a sandbox on this node holds the disks
// named after sandboxID: either its runtime record or its storage record,
// since destroy releases them through either.
func (s *service) sandboxImportOwned(ctx context.Context) storage.SandboxImportOwnership {
	return func(sandboxID string) (bool, error) {
		if s.cubeboxMgr != nil && s.cubeboxMgr.cubeboxManger != nil {
			if sb, err := s.cubeboxMgr.cubeboxManger.Get(ctx, sandboxID); err == nil && sb != nil {
				return true, nil
			}
		}
		records, err := inspectStorageRecordsFn()
		if err != nil {
			return false, err
		}
		return records[sandboxID] != nil, nil
	}
}

// finishSandboxImport ends a cross-node Create's claim on what it imported.
// A successful Create leaves the disks to the new sandbox. A failed one may
// still have left a sandbox behind (a failover destroy that did not finish);
// only when none holds the disks are they released here.
func (s *service) finishSandboxImport(ctx context.Context, claim *storage.SandboxImportClaim, rsp *cubebox.RunCubeSandboxResponse) {
	owned := rsp != nil && ret.IsSuccessCode(rsp.GetRet().GetRetCode())
	if !owned {
		var err error
		if owned, err = s.sandboxImportOwned(ctx)(claim.SandboxID); err != nil {
			// Unknown ownership: keep the claim for the periodic reclaim.
			log.G(ctx).Warnf("cross-node restore %s: import ownership unknown, deferring release: %v", claim.SandboxID, err)
			return
		}
	}
	if err := finishImportClaimFn(claim, context.WithoutCancel(ctx), owned); err != nil {
		log.G(ctx).Warnf("cross-node restore %s: release unowned import: %v", claim.SandboxID, err)
		return
	}
	if !owned {
		log.G(ctx).Infof("cross-node restore %s: released import left by the failed create", claim.SandboxID)
	}
}

// reclaimAbandonedImports releases, in the background, imports no Create or
// sandbox owns: claims an earlier process left behind, and metadata disks
// imported before claims existed.
func (s *service) reclaimAbandonedImports(ctx context.Context) {
	ticker := time.NewTicker(abandonedImportReclaimInterval)
	defer ticker.Stop()
	for {
		s.reclaimAbandonedImportsOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *service) reclaimAbandonedImportsOnce(ctx context.Context) {
	reclaimed, err := reclaimAbandonedImportsFn(ctx, s.sandboxImportOwned(ctx), abandonedImportMinAge)
	for _, id := range reclaimed {
		log.G(ctx).Infof("reclaimed cross-node import of %s left without an owner", id)
	}
	if err != nil {
		log.G(ctx).Warnf("reclaim abandoned cross-node imports: %v", err)
	}
}
