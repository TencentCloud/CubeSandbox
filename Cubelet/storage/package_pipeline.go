// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import "context"

// PackageFinalizeResult reports non-fatal post-seal cleanup issues. A non-nil
// CleanupErr means warn but keep the package.
type PackageFinalizeResult struct {
	CleanupErr error
}

// ValidateStagedPackage checks staged metadata before publish (no-op on XFS/JFS).
func ValidateStagedPackage(backend, metaDir string, requireMemory bool) error {
	return ValidateS3PackageMetadataFor(backend, metaDir, requireMemory)
}

// FinalizePublishedPackage performs post-publish sealing (no-op on XFS/JFS).
func FinalizePublishedPackage(ctx context.Context, backend, snapshotID string) (PackageFinalizeResult, error) {
	return FinalizeS3PackageSnapshots(ctx, backend, snapshotID)
}

// DiscardSealedPackage removes a failed package's sealed/work objects (no-op on XFS/JFS).
func DiscardSealedPackage(ctx context.Context, backend, snapshotID string) error {
	return DiscardS3SealedPackage(ctx, backend, snapshotID)
}

// ReleasePackageMetadata releases metadata storage for snapshotID (no-op on XFS/JFS).
func ReleasePackageMetadata(ctx context.Context, backend, snapshotID string) error {
	return ReleaseS3MetadataVolume(ctx, backend, snapshotID)
}

func packageMetadataCatalogRef(backend, snapshotID string) (string, string) {
	return S3MetadataCatalogVol(backend, snapshotID), S3MetadataCatalogKind(backend)
}
