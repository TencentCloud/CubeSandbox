// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package build

import (
	"encoding/json"
	"fmt"
)

// Keep build metadata in the existing persisted image_config_json. Preserve
// both bare and wrapped registry configs, including unknown fields.
func imageConfigWithCABundle(payload, bundle string) (string, error) {
	if bundle == "" {
		return payload, nil
	}
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal([]byte(payload), &cfg); err != nil || cfg == nil {
		return "", fmt.Errorf("record runtime CA bundle: image config must be an object")
	}
	path, err := json.Marshal(bundle)
	if err != nil {
		return "", err
	}
	cfg["_cube_egress_ca_bundle_path"] = path
	encoded, err := json.Marshal(cfg)
	return string(encoded), err
}
