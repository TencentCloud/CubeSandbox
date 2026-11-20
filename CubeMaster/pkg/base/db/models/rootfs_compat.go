package models

import (
	"fmt"

	images "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/images/v1"
	"gorm.io/gorm"
)

func (r *RootfsArtifact) AfterFind(_ *gorm.DB) error {
	metadata, err := images.ResolveLegacyRootfsMetadata(
		images.RootfsMetadata{Format: r.RootfsFormat, Path: r.RootfsPath, SHA256: r.RootfsSHA256, SizeBytes: r.RootfsSizeBytes},
		images.RootfsMetadata{Path: r.LegacyExt4Path, SHA256: r.LegacyExt4SHA256, SizeBytes: r.LegacyExt4SizeBytes})
	if err != nil {
		return fmt.Errorf("resolve rootfs metadata for artifact %q: %w", r.ArtifactID, err)
	}
	r.RootfsFormat, r.RootfsPath, r.RootfsSHA256, r.RootfsSizeBytes = metadata.Format, metadata.Path, metadata.SHA256, metadata.SizeBytes
	return nil
}

func (r *RootfsArtifact) BeforeSave(_ *gorm.DB) error {
	format, err := images.ResolveRootfsFormat(r.RootfsFormat)
	if err != nil {
		return err
	}
	r.RootfsFormat = format
	r.LegacyExt4Path, r.LegacyExt4SHA256, r.LegacyExt4SizeBytes = "", "", 0
	if format == images.ImageStorageMediaType_ext4.String() {
		r.LegacyExt4Path, r.LegacyExt4SHA256, r.LegacyExt4SizeBytes = r.RootfsPath, r.RootfsSHA256, r.RootfsSizeBytes
	}
	return nil
}

// RootfsArtifactUpdate validates and projects metadata for map-based GORM writes.
// Metadata updates must include the artifact's format; an omitted map field
// means unchanged, not the legacy EXT4 default. Status-only updates need no format.
// Legacy columns may only accompany the canonical fields projected below.
func RootfsArtifactUpdate(values map[string]any) error {
	columns := map[string]string{"rootfs_path": "ext4_path", "rootfs_sha256": "ext4_sha256", "rootfs_size_bytes": "ext4_size_bytes"}
	_, updatesMetadata := values["rootfs_format"]
	for canonical, legacy := range columns {
		_, updatesCanonical := values[canonical]
		if _, updatesLegacy := values[legacy]; updatesLegacy && !updatesCanonical {
			return fmt.Errorf("rootfs metadata updates require %q instead of legacy column %q", canonical, legacy)
		}
		if updatesCanonical {
			updatesMetadata = true
		}
	}
	if !updatesMetadata {
		return nil
	}
	format, ok := values["rootfs_format"].(string)
	if !ok {
		return fmt.Errorf("rootfs metadata updates require a string rootfs_format")
	}
	format, err := images.ResolveRootfsFormat(format)
	if err != nil {
		return err
	}
	values["rootfs_format"] = format
	if format != images.ImageStorageMediaType_ext4.String() {
		values["ext4_path"], values["ext4_sha256"], values["ext4_size_bytes"] = "", "", int64(0)
		return nil
	}
	for canonical, legacy := range columns {
		if value, ok := values[canonical]; ok {
			values[legacy] = value
		}
	}
	return nil
}

// NormalizeTemplateImagePhase accepts the persisted pre-rootfs phase token.
func NormalizeTemplateImagePhase(phase string) string {
	if phase == "BUILDING_EXT4" {
		return "BUILDING_ROOTFS"
	}
	return phase
}

func (j *TemplateImageJob) AfterFind(_ *gorm.DB) error {
	j.Phase = NormalizeTemplateImagePhase(j.Phase)
	j.ResumePhase = NormalizeTemplateImagePhase(j.ResumePhase)
	return nil
}
