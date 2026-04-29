// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package service

import (
	"fmt"
	"os"
	"time"

	toml "github.com/pelletier/go-toml/v2"
)

const (
	defaultObjectDir = "/usr/local/services/cubetoolbox/cube-vs/network"
	defaultStateDir  = "/usr/local/services/cubetoolbox/network-agent/state"
)

// Config keeps the minimal single-node network-agent settings aligned with Cubelet.
type Config struct {
	EthName             string
	ObjectDir           string
	CIDR                string
	MVMInnerIP          string
	MVMMacAddr          string
	MvmGwDestIP         string
	MvmGwMacAddr        string
	MvmMask             int
	MvmMtu              int
	TapInitNum          int
	DefaultExposedPorts []uint16
	StateDir            string
	TapFDSocketPath     string
	HostProxyBindIP     string
	ConnectTimeout      time.Duration
}

func DefaultConfig() Config {
	return Config{
		EthName:             "",
		ObjectDir:           defaultObjectDir,
		CIDR:                "192.168.0.0/18",
		MVMInnerIP:          "169.254.68.6",
		MVMMacAddr:          "20:90:6f:fc:fc:fc",
		MvmGwDestIP:         "169.254.68.5",
		MvmGwMacAddr:        "20:90:6f:cf:cf:cf",
		MvmMask:             30,
		MvmMtu:              1300,
		TapInitNum:          0,
		DefaultExposedPorts: []uint16{8080, 32000},
		StateDir:            defaultStateDir,
		TapFDSocketPath:     "/tmp/cube/network-agent-tap.sock",
		HostProxyBindIP:     "127.0.0.1",
		ConnectTimeout:      5 * time.Second,
	}
}

type cubeletConfigFile struct {
	Plugins map[string]cubeletNetworkConfig `toml:"plugins"`
}

type cubeletNetworkConfig struct {
	ObjectDir           string   `toml:"object_dir"`
	EthName             string   `toml:"eth_name"`
	TapInitNum          int      `toml:"tap_init_num"`
	CIDR                string   `toml:"cidr"`
	MVMInnerIP          string   `toml:"mvm_inner_ip"`
	MVMMacAddr          string   `toml:"mvm_mac_addr"`
	MvmGwDestIP         string   `toml:"mvm_gw_dest_ip"`
	MvmGwMacAddr        string   `toml:"mvm_gw_mac_addr"`
	MvmMask             int      `toml:"mvm_mask"`
	MvmMtu              int      `toml:"mvm_mtu"`
	DefaultExposedPorts []uint16 `toml:"default_exposed_ports"`
}

const cubeletNetworkPluginKey = "io.cubelet.internal.v1.network"

// LoadConfigFromCubeletTOML overlays network-agent defaults with Cubelet's network plugin settings.
func LoadConfigFromCubeletTOML(base Config, path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return base, fmt.Errorf("read cubelet config %q: %w", path, err)
	}

	var parsed cubeletConfigFile
	if err := toml.Unmarshal(data, &parsed); err != nil {
		return base, fmt.Errorf("decode cubelet config %q: %w", path, err)
	}

	networkCfg, ok := parsed.Plugins[cubeletNetworkPluginKey]
	if !ok {
		return base, fmt.Errorf("cubelet config %q missing plugins.%q", path, cubeletNetworkPluginKey)
	}
	if networkCfg.EthName == "" {
		return base, fmt.Errorf("cubelet config %q missing plugins.%q.eth_name", path, cubeletNetworkPluginKey)
	}

	if networkCfg.ObjectDir != "" {
		base.ObjectDir = networkCfg.ObjectDir
	}
	if networkCfg.EthName != "" {
		base.EthName = networkCfg.EthName
	}
	if networkCfg.CIDR != "" {
		base.CIDR = networkCfg.CIDR
	}
	if networkCfg.MVMInnerIP != "" {
		base.MVMInnerIP = networkCfg.MVMInnerIP
	}
	if networkCfg.MVMMacAddr != "" {
		base.MVMMacAddr = networkCfg.MVMMacAddr
	}
	if networkCfg.MvmGwDestIP != "" {
		base.MvmGwDestIP = networkCfg.MvmGwDestIP
	}
	if networkCfg.MvmGwMacAddr != "" {
		base.MvmGwMacAddr = networkCfg.MvmGwMacAddr
	}
	if networkCfg.MvmMask != 0 {
		base.MvmMask = networkCfg.MvmMask
	}
	if networkCfg.MvmMtu != 0 {
		base.MvmMtu = networkCfg.MvmMtu
	}
	if networkCfg.TapInitNum != 0 {
		base.TapInitNum = networkCfg.TapInitNum
	}
	if len(networkCfg.DefaultExposedPorts) > 0 {
		base.DefaultExposedPorts = append([]uint16(nil), networkCfg.DefaultExposedPorts...)
	}

	return base, nil
}
