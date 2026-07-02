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
	defaultStateDir  = "/data/cubelet/network-agent/state"
)

// Config keeps the minimal single-node network-agent settings aligned with Cubelet.
type Config struct {
	EthName         string
	ObjectDir       string
	CIDR            string
	MVMInnerIP      string
	MVMMacAddr      string
	MvmGwDestIP     string
	MvmGwMacAddr    string
	MvmMask         int
	MvmMtu          int
	TapInitNum      int
	StateDir        string
	TapFDSocketPath string
	HostProxyBindIP string
	ConnectTimeout  time.Duration

	// CubeEgressAdminURL points at the colocated CubeEgress admin
	// listener (loopback, e.g. http://127.0.0.1:9090). Defaults to
	// the canonical loopback address that CubeEgress's nginx.conf
	// hard-codes; override (or set to "") only for setups where
	// CubeEgress lives elsewhere or isn't deployed at all. When
	// empty, network-agent silently skips the per-sandbox push and
	// the /v1/policies/dump endpoint still works (it just returns
	// an empty map until sandboxes with rules are created).
	CubeEgressAdminURL string

	// CubeEgressPushTimeout bounds a single PUT/DELETE call to the
	// CubeEgress admin API. Loopback HTTP against an OpenResty
	// shared-dict op should be sub-millisecond; this is generous on
	// purpose so a transient kernel hiccup doesn't fail the push.
	CubeEgressPushTimeout time.Duration

	// Route-aware egress options.
	CubeRouterEnable  bool
	CubeRouterCIDR    string
	CubeRouterMacAddr string
}

func DefaultConfig() Config {
	return Config{
		EthName:               "",
		ObjectDir:             defaultObjectDir,
		CIDR:                  "192.168.0.0/18",
		MVMInnerIP:            "169.254.68.6",
		MVMMacAddr:            "20:90:6f:fc:fc:fc",
		MvmGwDestIP:           "169.254.68.5",
		MvmGwMacAddr:          "20:90:6f:cf:cf:cf",
		MvmMask:               30,
		MvmMtu:                1500,
		TapInitNum:            0,
		StateDir:              defaultStateDir,
		TapFDSocketPath:       "/tmp/cube/network-agent-tap.sock",
		HostProxyBindIP:       "127.0.0.1",
		ConnectTimeout:        5 * time.Second,
		CubeEgressAdminURL:    "http://127.0.0.1:9090",
		CubeEgressPushTimeout: 2 * time.Second,

		// Route-aware egress options.
		CubeRouterEnable:  false,
		CubeRouterCIDR:    "",
		CubeRouterMacAddr: "22:90:6f:cf:cf:cf",
	}
}

type cubeletConfigFile struct {
	Plugins map[string]cubeletNetworkConfig `toml:"plugins"`
}

type cubeletNetworkConfig struct {
	ObjectDir             string `toml:"object_dir"`
	EthName               string `toml:"eth_name"`
	TapInitNum            int    `toml:"tap_init_num"`
	CIDR                  string `toml:"cidr"`
	MVMInnerIP            string `toml:"mvm_inner_ip"`
	MVMMacAddr            string `toml:"mvm_mac_addr"`
	MvmGwDestIP           string `toml:"mvm_gw_dest_ip"`
	MvmGwMacAddr          string `toml:"mvm_gw_mac_addr"`
	MvmMask               int    `toml:"mvm_mask"`
	MvmMtu                int    `toml:"mvm_mtu"`
	CubeEgressAdminURL    string `toml:"cube_egress_admin_url"`
	CubeEgressPushTimeout string `toml:"cube_egress_push_timeout"`

	// Route-aware egress options.
	CubeRouterEnable  bool   `toml:"cube_router_enable"`
	CubeRouterCIDR    string `toml:"cube_router_cidr"`
	CubeRouterMacAddr string `toml:"cube_router_mac_addr"`
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
	base.CubeRouterEnable = networkCfg.CubeRouterEnable
	if networkCfg.CubeRouterCIDR != "" {
		base.CubeRouterCIDR = networkCfg.CubeRouterCIDR
	}
	if networkCfg.CubeRouterMacAddr != "" {
		base.CubeRouterMacAddr = networkCfg.CubeRouterMacAddr
	}
	if networkCfg.TapInitNum != 0 {
		base.TapInitNum = networkCfg.TapInitNum
	}
	if networkCfg.CubeEgressAdminURL != "" {
		base.CubeEgressAdminURL = networkCfg.CubeEgressAdminURL
	}
	if networkCfg.CubeEgressPushTimeout != "" {
		d, perr := time.ParseDuration(networkCfg.CubeEgressPushTimeout)
		if perr != nil {
			return base, fmt.Errorf("cubelet config %q: parse cube_egress_push_timeout %q: %w",
				path, networkCfg.CubeEgressPushTimeout, perr)
		}
		base.CubeEgressPushTimeout = d
	}
	return base, nil
}
