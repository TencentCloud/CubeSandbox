// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package service

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigFromCubeletTOML(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	content := `
[plugins]
  [plugins."io.cubelet.internal.v1.network"]
    object_dir = "/tmp/cubevs"
    eth_name = "eth9"
    tap_init_num = 16
    cidr = "192.168.64.0/20"
    mvm_inner_ip = "169.254.100.6"
    mvm_mac_addr = "02:11:22:33:44:55"
    mvm_gw_dest_ip = "169.254.100.5"
    mvm_gw_mac_addr = "02:aa:bb:cc:dd:ee"
    mvm_mask = 29
    mvm_mtu = 1450
    default_exposed_ports = [81, 8081]
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfigFromCubeletTOML(DefaultConfig(), path)
	if err != nil {
		t.Fatalf("LoadConfigFromCubeletTOML error=%v", err)
	}

	if cfg.ObjectDir != "/tmp/cubevs" {
		t.Fatalf("ObjectDir=%q", cfg.ObjectDir)
	}
	if cfg.EthName != "eth9" {
		t.Fatalf("EthName=%q", cfg.EthName)
	}
	if cfg.TapInitNum != 16 {
		t.Fatalf("TapInitNum=%d", cfg.TapInitNum)
	}
	if cfg.CIDR != "192.168.64.0/20" {
		t.Fatalf("CIDR=%q", cfg.CIDR)
	}
	if cfg.MVMInnerIP != "169.254.100.6" {
		t.Fatalf("MVMInnerIP=%q", cfg.MVMInnerIP)
	}
	if cfg.MVMMacAddr != "02:11:22:33:44:55" {
		t.Fatalf("MVMMacAddr=%q", cfg.MVMMacAddr)
	}
	if cfg.MvmGwDestIP != "169.254.100.5" {
		t.Fatalf("MvmGwDestIP=%q", cfg.MvmGwDestIP)
	}
	if cfg.MvmGwMacAddr != "02:aa:bb:cc:dd:ee" {
		t.Fatalf("MvmGwMacAddr=%q", cfg.MvmGwMacAddr)
	}
	if cfg.MvmMask != 29 {
		t.Fatalf("MvmMask=%d", cfg.MvmMask)
	}
	if cfg.MvmMtu != 1450 {
		t.Fatalf("MvmMtu=%d", cfg.MvmMtu)
	}
	if len(cfg.DefaultExposedPorts) != 2 || cfg.DefaultExposedPorts[0] != 81 || cfg.DefaultExposedPorts[1] != 8081 {
		t.Fatalf("DefaultExposedPorts=%v", cfg.DefaultExposedPorts)
	}
}

func TestLoadConfigFromCubeletTOMLMissingEthName(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	content := `
[plugins]
  [plugins."io.cubelet.internal.v1.network"]
    cidr = "192.168.64.0/20"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if _, err := LoadConfigFromCubeletTOML(DefaultConfig(), path); err == nil {
		t.Fatalf("LoadConfigFromCubeletTOML error=nil, want missing eth_name")
	}
}
