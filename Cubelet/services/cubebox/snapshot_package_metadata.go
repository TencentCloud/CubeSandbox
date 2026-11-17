// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// snapshotPackageMetadataFile is the package descriptor the shim writes next to
// the captured VM config. Cold boot reads it for the guest runtime identity and
// for the writable layer.
const snapshotPackageMetadataFile = "metadata.json"

// snapshotPackageWritableLayerKey is the descriptor field Cubelet adds after a
// filesystem-only capture.
const snapshotPackageWritableLayerKey = "rootfs_writable_layer"

// recordWritableLayer records the overlay upper directory the captured rootfs
// was mounted on, so a cold boot mounts that directory instead of deriving a
// name from a container id. An empty layer means the descriptor the shim wrote
// is already correct.
func recordWritableLayer(specDir, layer string) error {
	layer = strings.TrimSpace(layer)
	if layer == "" {
		return nil
	}
	return mutateSnapshotPackageMetadata(specDir, "snapshot metadata",
		func(fields map[string]json.RawMessage) error {
			encoded, err := json.Marshal(layer)
			if err != nil {
				return fmt.Errorf("encode writable layer: %w", err)
			}
			fields[snapshotPackageWritableLayerKey] = encoded
			return nil
		})
}

// mutateSnapshotPackageMetadata rewrites the package descriptor in place as a
// field map: fields this side does not understand are preserved byte for byte.
// Publishing goes through a temporary file and a rename so a torn write cannot
// leave a package that cannot boot.
func mutateSnapshotPackageMetadata(
	specDir, label string, mutate func(fields map[string]json.RawMessage) error,
) error {
	path := filepath.Join(specDir, snapshotPackageMetadataFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", label, err)
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return fmt.Errorf("parse %s: %w", label, err)
	}
	if fields == nil {
		return fmt.Errorf("parse %s: expected JSON object", label)
	}
	if err := mutate(fields); err != nil {
		return err
	}
	updated, err := json.Marshal(fields)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", label, err)
	}
	// Both failure branches remove the staging file: os.WriteFile can leave a
	// partial file behind (ENOSPC/EDQUOT), and the JFS uploader walks every
	// regular file in the metadata directory, so a stray one would be packaged.
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, updated, 0o644); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write %s: %w", label, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("publish %s: %w", label, err)
	}
	return nil
}
