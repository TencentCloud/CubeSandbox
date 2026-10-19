package images

import (
	"encoding/json"
	"fmt"
)

// RootfsMetadata describes the materialized filesystem bytes, independently of
// their source image or delivery mechanism. Path is a producer-local path;
// object keys and node-local paths are separate locators.
type RootfsMetadata struct {
	Format    string `json:"rootfs_format,omitempty"`
	Path      string `json:"rootfs_path,omitempty"`
	SHA256    string `json:"rootfs_sha256,omitempty"`
	SizeBytes int64  `json:"rootfs_size_bytes,omitempty"`
}

// ResolveLegacyRootfsMetadata accepts one complete representation, never a
// field-by-field mixture of two conflicting artifact identities.
func ResolveLegacyRootfsMetadata(current, legacy RootfsMetadata) (RootfsMetadata, error) {
	format, err := ResolveRootfsFormat(current.Format)
	if err != nil {
		return RootfsMetadata{}, err
	}
	current.Format = format
	if format != ImageStorageMediaType_ext4.String() {
		if legacy.Path != "" || legacy.SHA256 != "" || legacy.SizeBytes != 0 {
			return RootfsMetadata{}, fmt.Errorf("legacy ext4_* metadata is invalid for rootfs format %q", format)
		}
		return current, nil
	}
	if current.Path == "" && current.SHA256 == "" && current.SizeBytes == 0 {
		legacy.Format = format
		return legacy, nil
	}
	if (legacy.Path != "" && current.Path != legacy.Path) ||
		(legacy.SHA256 != "" && current.SHA256 != legacy.SHA256) ||
		(legacy.SizeBytes != 0 && current.SizeBytes != legacy.SizeBytes) {
		return RootfsMetadata{}, fmt.Errorf("conflicting rootfs_* and legacy ext4_* artifact metadata")
	}
	return current, nil
}

// DecodeRootfsMetadata is the compatibility adapter for persisted reports and
// old HTTP payloads. Callers only use the canonical fields after this point.
func DecodeRootfsMetadata(data []byte) (RootfsMetadata, error) {
	var wire struct {
		RootfsMetadata
		Path      string `json:"ext4_path"`
		SHA256    string `json:"ext4_sha256"`
		SizeBytes int64  `json:"ext4_size_bytes"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return RootfsMetadata{}, err
	}
	return ResolveLegacyRootfsMetadata(wire.RootfsMetadata, RootfsMetadata{Path: wire.Path, SHA256: wire.SHA256, SizeBytes: wire.SizeBytes})
}

// AddRootfsMetadata writes canonical fields and the historical EXT4 projection
// at wire boundaries. The legacy keys are not used by the implementation.
func AddRootfsMetadata(payload map[string]any, metadata RootfsMetadata) {
	payload["rootfs_format"] = metadata.Format
	payload["rootfs_path"] = metadata.Path
	payload["rootfs_sha256"] = metadata.SHA256
	payload["rootfs_size_bytes"] = metadata.SizeBytes
	if metadata.Format == "" || metadata.Format == ImageStorageMediaType_ext4.String() {
		payload["ext4_path"] = metadata.Path
		payload["ext4_sha256"] = metadata.SHA256
		payload["ext4_size_bytes"] = metadata.SizeBytes
	}
}
