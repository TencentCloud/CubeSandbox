// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	cubebox "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
)

const pauseSandboxSpecFileName = "sandbox_spec.json"

// LoadPauseSandboxSpec reads sandbox_spec.json from a pause snapshot directory.
// For S3 packages the file lives under metadata/; callers may pass either the
// metadata dir or the snapshot home.
func LoadPauseSandboxSpec(snapshotPath string) (*cubebox.RunCubeSandboxRequest, error) {
	snapshotPath = filepath.Clean(strings.TrimSpace(snapshotPath))
	candidates := []string{filepath.Join(snapshotPath, pauseSandboxSpecFileName)}
	// Only append home/metadata/ when the caller passed the package home.
	if filepath.Base(snapshotPath) != SnapshotMetadataDir {
		candidates = append(candidates, filepath.Join(snapshotPath, SnapshotMetadataDir, pauseSandboxSpecFileName))
	}
	var (
		body []byte
		err  error
	)
	for _, path := range candidates {
		body, err = os.ReadFile(path) // NOCC:Path Traversal()
		if err == nil {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	var runReq cubebox.RunCubeSandboxRequest
	if err := json.Unmarshal(body, &runReq); err != nil {
		return nil, err
	}
	return &runReq, nil
}
