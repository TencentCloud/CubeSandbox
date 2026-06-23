// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"gopkg.in/yaml.v3"
)

// shippedReqTemplateConfigs are the config files that ship a default
// req_template_conf.cube_box_req_template and are consumed verbatim by the
// one-click deployments:
//   - CubeMaster/conf.yaml          : the in-repo dev / reference config
//   - configs/single-node/cubemaster.yaml : the quickstart one-click and
//     non-Tencent-Cloud install-package config (build-release-bundle.sh copies it
//     to CubeMaster/conf.yaml inside the release bundle)
//
// Paths are relative to the repository root, resolved by findRepoFile.
var shippedReqTemplateConfigs = []string{
	"CubeMaster/conf.yaml",
	"configs/single-node/cubemaster.yaml",
}

// findRepoFile walks up from the test's working directory to the first ancestor
// that contains relPath and returns its absolute path. The shipped configs live
// both inside the CubeMaster module (conf.yaml) and at the repository root
// (configs/...), i.e. outside this package directory, so a fixed relative path
// would be brittle.
func findRepoFile(t *testing.T, relPath string) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		candidate := filepath.Join(dir, relPath)
		if _, statErr := os.Stat(candidate); statErr == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate %s walking up from the test directory", relPath)
		}
		dir = parent
	}
}

// cubeBoxReqTemplateFromConfig parses the YAML config at path and returns the
// raw req_template_conf.cube_box_req_template JSON string.
func cubeBoxReqTemplateFromConfig(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc struct {
		ReqTemplateConf struct {
			CubeBoxReqTemplate string `yaml:"cube_box_req_template"`
		} `yaml:"req_template_conf"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse yaml %s: %v", path, err)
	}
	return doc.ReqTemplateConf.CubeBoxReqTemplate
}

// TestShippedCubeBoxReqTemplateDeserializesCubeNetworkConfig is a regression
// guard for the default egress policy embedded in the shipped cube_box_req_template.
//
// The policy lives under the JSON key "cube_network_config", which is the ONLY
// key CubeMaster deserializes (CreateCubeSandboxReq.CubeNetworkConfig,
// json:"cube_network_config"; see getCubeboxReqTemplate). A stale "cubevs_context"
// key — or any other rename — deserializes to a nil config, so the default
// denyOut egress policy is silently dropped and sandboxes can reach the private
// network the template meant to block. This test deserializes the exact template
// shipped in every config via the production unmarshal path and asserts the
// network config is populated.
//
// Keep this in sync with the TKE deployer's cube_box_req_template in
// deploy/one-click/terraform/tencentcloud/tke-addons.tf (guarded statically by
// deploy/one-click/tests/test_package_layout.sh).
func TestShippedCubeBoxReqTemplateDeserializesCubeNetworkConfig(t *testing.T) {
	// The full RFC1918 / CGNAT blocks that every shipped config denies. The
	// 192.168.x private block is asserted separately by prefix: its mask
	// legitimately differs across configs (the single-node / TKE configs use
	// 192.168.0.0/16, the dev conf.yaml keeps the historical /18), so pinning an
	// exact value here would couple the test to one config's choice.
	stableDenyOut := []string{"10.0.0.0/8", "100.64.0.0/10", "172.16.0.0/12"}

	for _, rel := range shippedReqTemplateConfigs {
		rel := rel
		t.Run(rel, func(t *testing.T) {
			tplStr := cubeBoxReqTemplateFromConfig(t, findRepoFile(t, rel))
			if tplStr == "" {
				t.Fatalf("%s has an empty req_template_conf.cube_box_req_template", rel)
			}

			tpl := &types.CreateCubeSandboxReq{}
			if err := utils.JSONTool.UnmarshalFromString(tplStr, tpl); err != nil {
				t.Fatalf("unmarshal cube_box_req_template from %s: %v", rel, err)
			}

			if !assert.NotNilf(t, tpl.CubeNetworkConfig,
				"%s: cube_box_req_template must populate cube_network_config (wrong/stale JSON key?)", rel) {
				return
			}
			if assert.NotNilf(t, tpl.CubeNetworkConfig.AllowInternetAccess,
				"%s: allowInternetAccess must be set", rel) {
				assert.Truef(t, *tpl.CubeNetworkConfig.AllowInternetAccess,
					"%s: allowInternetAccess should be true", rel)
			}
			assert.Subsetf(t, tpl.CubeNetworkConfig.DenyOut, stableDenyOut,
				"%s: default egress denyOut policy not applied", rel)

			has192 := false
			for _, cidr := range tpl.CubeNetworkConfig.DenyOut {
				if strings.HasPrefix(cidr, "192.168.0.0/") {
					has192 = true
					break
				}
			}
			assert.Truef(t, has192,
				"%s: denyOut must block the 192.168.0.0 private block", rel)
		})
	}
}
