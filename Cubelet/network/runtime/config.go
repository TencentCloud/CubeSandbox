// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package runtime

import (
	"time"
)

const (
	// defaultObjectDir is where cube-vs objects are deployed on production nodes.
	defaultObjectDir = "/usr/local/services/cubetoolbox/cube-vs/network"
	// defaultStateDir keeps runtime state in tmpfs so each host reboot starts from
	// the live kernel inventory plus the state files left by the previous process.
	defaultStateDir = "/dev/shm/cubelet/network-runtime/state"
	// defaultLegacyStateDir is the recovery input source from the old
	// network-agent state directory. Startup recovery is the only production caller
	// that should ever look at the legacy location.
	defaultLegacyStateDir = "/data/cubelet/network-agent/state"
)

// Config keeps the embedded network runtime settings aligned with Cubelet.
type Config struct {
	// Host and sandbox network identity. EthName must be provided by Cubelet; the
	// rest are defaults for the mvm-facing gateway and inner interface.
	EthName        string
	ObjectDir      string
	CIDR           string
	MVMInnerIP     string
	MVMMacAddr     string
	MvmGwDestIP    string
	MvmGwMacAddr   string
	MvmMask        int
	MvmMtu         int
	TapInitNum     int
	StateDir       string
	HostPortBindIP string

	// CubeEgressAdminURL points at the colocated CubeEgress admin
	// listener (loopback, e.g. http://127.0.0.1:9090). Defaults to
	// the canonical loopback address that CubeEgress's nginx.conf
	// hard-codes; override (or set to "") only for setups where
	// CubeEgress lives elsewhere or isn't deployed at all. When
	// empty, the embedded network runtime skips the per-sandbox push and
	// the /v1/policies/dump endpoint still works (it just returns
	// an empty map until sandboxes with rules are created).
	CubeEgressAdminURL string

	// CubeEgressPushTimeout bounds a single PUT/DELETE call to the
	// CubeEgress admin API. Loopback HTTP against an OpenResty
	// shared-dict op should be sub-millisecond; this is generous on
	// purpose so a transient kernel hiccup doesn't fail the push.
	CubeEgressPushTimeout time.Duration

	// Route-aware egress options. When enabled, sandbox egress is redirected to a
	// dedicated dummy device so the host routing table chooses the real uplink.
	CubeRouterEnable  bool
	CubeRouterCIDR    string
	CubeRouterMacAddr string
}

// DefaultConfig returns the runtime's production-safe defaults. The caller is
// still expected to fill EthName from Cubelet config or flags because the host
// uplink cannot be inferred safely on multi-interface nodes.
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
		HostPortBindIP:        "127.0.0.1",
		CubeEgressAdminURL:    "http://127.0.0.1:9090",
		CubeEgressPushTimeout: 2 * time.Second,

		// Route-aware egress options.
		CubeRouterEnable:  false,
		CubeRouterCIDR:    "",
		CubeRouterMacAddr: "22:90:6f:cf:cf:cf",
	}
}
