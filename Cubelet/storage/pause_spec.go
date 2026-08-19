// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"encoding/json"
	"os"
	"path/filepath"

	cubebox "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
)

const pauseSandboxSpecFileName = "sandbox_spec.json"

// LoadPauseSandboxSpec reads sandbox_spec.json from a pause snapshot directory.
func LoadPauseSandboxSpec(snapshotPath string) (*cubebox.RunCubeSandboxRequest, error) {
	path := filepath.Join(snapshotPath, pauseSandboxSpecFileName)
	body, err := os.ReadFile(path) // NOCC:Path Traversal()
	if err != nil {
		return nil, err
	}
	var runReq cubebox.RunCubeSandboxRequest
	if err := json.Unmarshal(body, &runReq); err != nil {
		return nil, err
	}
	return &runReq, nil
}
