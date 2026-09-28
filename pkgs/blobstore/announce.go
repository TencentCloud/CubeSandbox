// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package blobstore

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const selectedBackendFile = "selected_backend"

// AnnounceIfFS records the driver on a shared volume only when it actually
// stores objects there. S3 keeps durable bytes in the bucket, so a process
// that selected s3 must not write or compare this marker.
func AnnounceIfFS(root, driver string) error {
	if strings.ToLower(strings.TrimSpace(driver)) != "fs" {
		return nil
	}
	return AnnounceBackend(root, driver)
}

// AnnounceBackend records the selected driver on a shared volume so two
// processes (typically CubeMaster and CubeTemplateCenter) cannot silently
// pick different backends for the same artifact directory.
//
// root may be empty, in which case this is a no-op. A pre-existing marker
// with a different driver is a hard error, except s3→fs: an s3 marker does
// not mean the volume holds durable objects, so fs may overwrite it.
func AnnounceBackend(root, driver string) error {
	root = strings.TrimSpace(root)
	driver = strings.ToLower(strings.TrimSpace(driver))
	if root == "" || driver == "" {
		return nil
	}
	path := backendMarkerPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("blobstore: mkdir backend marker: %w", err)
	}
	existing, err := os.ReadFile(path)
	if err == nil {
		got := strings.ToLower(strings.TrimSpace(string(existing)))
		if got == "" || got == driver {
			return nil
		}
		if got != "s3" || driver != "fs" {
			return fmt.Errorf("blobstore: artifact store backend mismatch: volume has %q, this process selected %q", got, driver)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("blobstore: read backend marker: %w", err)
	}
	if err := os.WriteFile(path, []byte(driver+"\n"), 0o644); err != nil {
		return fmt.Errorf("blobstore: write backend marker: %w", err)
	}
	return nil
}

func backendMarkerPath(root string) string {
	return filepath.Join(root, "_state", selectedBackendFile)
}
