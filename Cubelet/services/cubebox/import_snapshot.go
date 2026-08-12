// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	cubebox "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	errorcode "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/pathutil"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
)

// Catalog discovery globs the spec-dir segment, so any value works.
const defaultImportSpecDirName = "imported"

func (s *service) ImportSnapshot(ctx context.Context, req *cubebox.ImportSnapshotRequest) (*cubebox.ImportSnapshotResponse, error) {
	rsp := &cubebox.ImportSnapshotResponse{
		RequestID:  req.GetRequestID(),
		TemplateID: strings.TrimSpace(req.GetTemplateID()),
		Ret:        &errorcode.Ret{RetCode: errorcode.ErrorCode_Success},
	}
	fail := func(code errorcode.ErrorCode, format string, args ...any) (*cubebox.ImportSnapshotResponse, error) {
		rsp.Ret.RetCode = code
		rsp.Ret.RetMsg = fmt.Sprintf(format, args...)
		return rsp, nil
	}

	if rsp.TemplateID == "" {
		return fail(errorcode.ErrorCode_InvalidParamFormat, "templateID is required")
	}
	if err := pathutil.ValidateSafeID(rsp.TemplateID); err != nil {
		return fail(errorcode.ErrorCode_InvalidParamFormat, "invalid templateID: %v", err)
	}
	rootfsSrc := strings.TrimSpace(req.GetRootfsSourcePath())
	if rootfsSrc == "" {
		return fail(errorcode.ErrorCode_InvalidParamFormat, "rootfs_source_path is required")
	}

	specDir := strings.TrimSpace(req.GetSpecDir())
	if specDir == "" {
		specDir = defaultImportSpecDirName
	}
	if err := pathutil.ValidateSafeID(specDir); err != nil {
		return fail(errorcode.ErrorCode_InvalidParamFormat, "invalid spec_dir: %v", err)
	}
	snapshotPath := filepath.Join(DefaultSnapshotDir, "cubebox", rsp.TemplateID, specDir)
	if _, err := pathutil.ValidatePathUnderBase(DefaultSnapshotDir, snapshotPath); err != nil {
		return fail(errorcode.ErrorCode_InvalidParamFormat, "invalid snapshot path: %v", err)
	}

	stepLog := log.G(ctx).WithField("templateID", rsp.TemplateID)

	rootfsObject, wlayerSubdir, err := storage.ImportRootfsArtifact(ctx, rsp.TemplateID, rootfsSrc)
	if err != nil {
		if errors.Is(err, storage.ErrImportSourceInvalid) {
			return fail(errorcode.ErrorCode_InvalidParamFormat, "import rootfs artifact: %v", err)
		}
		if errors.Is(err, storage.ErrCowObjectAlreadyExists) {
			return fail(errorcode.ErrorCode_PreConditionFailed, "import rootfs artifact: %v", err)
		}
		return fail(errorcode.ErrorCode_CreateStorageFailed, "import rootfs artifact: %v", err)
	}
	rsp.RootfsVol = rootfsObject.Name
	rsp.RootfsKind = rootfsObject.Kind
	rsp.RootfsSizeBytes = rootfsObject.SizeBytes

	rsp.SnapshotPath = snapshotPath
	if err := storage.WriteSnapshotCatalog(&storage.SnapshotCatalogEntry{
		SnapshotID:   rsp.TemplateID,
		InstanceType: "cubebox",
		SpecDir:      specDir,
		SnapshotPath: snapshotPath,
		MetaDir:      snapshotPath,
		RootfsVol:    rsp.RootfsVol,
		RootfsKind:   rsp.RootfsKind,
		WlayerSubdir: wlayerSubdir,
		DiskOnly:     true,
		// Recorded even though the vehicle is normally dropped during ingest:
		// if that best-effort delete failed, this keeps the orphan reachable
		// from the template cleanup refs.
		BuildRootfsVol:  storage.TemplateBuildRootfsName(rsp.TemplateID),
		BuildRootfsKind: storage.CowKindVolume,
		RootfsSizeBytes: rsp.RootfsSizeBytes,
		Kind:            storage.CatalogKindTemplate,
	}); err != nil {
		storage.CleanupImportedSnapshot(ctx, rsp.TemplateID)
		return fail(errorcode.ErrorCode_CreateStorageFailed, "persist snapshot catalog: %v", err)
	}

	if err := writeSnapshotFlag(stepLog); err != nil {
		stepLog.Warnf("failed to write snapshot flag: %v", err)
	}

	stepLog.Infof("imported snapshot: rootfs=%s size=%d", rsp.RootfsVol, rsp.RootfsSizeBytes)
	return rsp, nil
}
