// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package runtime

import (
	"time"

	"github.com/vishvananda/netlink"
)

// EffectiveMTU returns the MTU the runtime should use when no per-sandbox
// override is supplied. The resolution order is:
//
//  1. Per-sandbox override (see NetRequest.MTU → EnsureNetworkRequest).
//  2. MvmMtu from the runtime Config (operator-configured, defaults to 1500).
//  3. MvmMtu=0 plus MTUInterface set → live MTU of that host link
//     (e.g. the Kubernetes parent bridge) read once at startup.
//  4. MTUInterface missing or unreadable → fall back to DefaultGuestMTU.
//
// This is the fix for issue #1673: with a hardcoded 1500 the guest kernel
// sends packets larger than the underlying overlay's transport MTU and they
// are silently dropped (or trigger ICMP fragmentation-needed the guest has
// no way to honour). Now the operator can either configure MvmMtu to the
// right value, or set MTUInterface = "cbr0" and let the runtime mirror the
// bridge MTU automatically.
func (c Config) EffectiveMTU() int {
	if c.MvmMtu > 0 {
		return c.MvmMtu
	}
	if c.MTUInterface != "" {
		if mtu, err := lookupLinkMTU(c.MTUInterface); err == nil && mtu > 0 {
			return mtu
		}
	}
	return DefaultGuestMTU
}

// DefaultGuestMTU is the IPv4-safe Ethernet MTU. Used as a last-resort
// fallback when no explicit value or interface lookup is available. Matches
// the historical default so existing deployments do not see a surprise.
const DefaultGuestMTU = 1500

// lookupLinkMTU returns the MTU of the named host link via netlink. It is
// only consulted when the runtime is being constructed on a Linux host (the
// tap_device.go path already assumes netlink), so a transient lookup error
// here simply falls through to DefaultGuestMTU. We intentionally swallow
// the error: misconfiguration should not prevent the runtime from starting,
// only from picking the best MTU.
func lookupLinkMTU(name string) (int, error) {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return 0, err
	}
	return link.Attrs().MTU, nil
}

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
	// MTUInterface, when set to the name of a host link (e.g. the parent bridge
	// "cbr0" used by Kubernetes, or the egress uplink), causes the runtime to
	// read that link's MTU at startup and use it as the effective guest MTU
	// when MvmMtu is left at zero. Per-sandbox overrides (see NetRequest.MTU)
	// always win over both. Issue #1673: a hardcoded 1500 breaks overlay
	// networks whose transport MTU is below the Ethernet default.
	MTUInterface string
	TapInitNum     int
	StateDir       string
	HostPortBindIP string

	// CubeEgressAdminURL points at the colocated CubeEgress admin
	// listener (loopback, e.g. http://127.0.0.1:9091). Defaults to
	// the canonical loopback address that CubeEgress's nginx.conf
	// uses (CUBE_EGRESS_ADMIN_PORT, default 9091); override (or set
	// to "") only for setups where CubeEgress lives elsewhere or
	// isn't deployed at all. When empty, the embedded network runtime
	// skips the per-sandbox push and the /v1/policies/dump endpoint
	// still works (it just returns an empty map until sandboxes with
	// rules are created).
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
		CubeEgressAdminURL:    "http://127.0.0.1:9091",
		CubeEgressPushTimeout: 2 * time.Second,

		// Route-aware egress options.
		CubeRouterEnable:  false,
		CubeRouterCIDR:    "",
		CubeRouterMacAddr: "22:90:6f:cf:cf:cf",
	}
}
