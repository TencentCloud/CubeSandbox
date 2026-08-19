// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package runtime

import (
	"context"
	"net"
	"sync/atomic"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeNet/cubevs"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
)

// CubeVSAdapter is the controller's boundary to CubeNet/cubevs. Keeping it as
// an interface lets unit tests model eBPF map failures without loading programs
// or requiring privileged kernel state.
type CubeVSAdapter interface {
	AddTAPDevice(ifindex uint32, ip net.IP, sandboxID string, version uint32, opts cubevs.MVMOptions) error
	UpsertTAPDevice(ifindex uint32, ip net.IP, sandboxID string, version uint32, opts cubevs.MVMOptions) error
	UpsertTAPDeviceMetadata(ifindex uint32, ip net.IP, sandboxID string, version uint32) error
	GetTAPDevice(ifindex uint32) (*cubevs.TAPDevice, error)
	CleanupTAPPolicy(ifindex uint32) error
	DeleteTAPDeviceMetadata(ifindex uint32, ip net.IP) error
	AttachFilter(ifindex uint32) error
	InstallTAPDefaultDenyPolicy(ifindex uint32) error
	GCStaleNetPolicyMaps(keep map[uint32]struct{}, stillPresent func(uint32) bool, onConflict func(uint32)) (int, error)
	AddPortMapping(ifindex uint32, containerPort, hostPort uint16) error
	DelPortMapping(ifindex uint32, containerPort, hostPort uint16) error
	DeletePortMappingsByIfindex(ifindex uint32) error
}

// realCubeVSAdapter forwards calls to the production cubevs package.
type realCubeVSAdapter struct{}

// newRealCubeVSAdapter returns the production CubeVS adapter.
func newRealCubeVSAdapter() CubeVSAdapter {
	return realCubeVSAdapter{}
}

func (realCubeVSAdapter) AddTAPDevice(ifindex uint32, ip net.IP, sandboxID string, version uint32, opts cubevs.MVMOptions) error {
	return cubevs.AddTAPDevice(ifindex, ip, sandboxID, version, opts)
}

func (realCubeVSAdapter) UpsertTAPDevice(ifindex uint32, ip net.IP, sandboxID string, version uint32, opts cubevs.MVMOptions) error {
	return cubevs.UpsertTAPDevice(ifindex, ip, sandboxID, version, opts)
}

func (realCubeVSAdapter) UpsertTAPDeviceMetadata(ifindex uint32, ip net.IP, sandboxID string, version uint32) error {
	return cubevs.UpsertTAPDeviceMetadata(ifindex, ip, sandboxID, version)
}

func (realCubeVSAdapter) GetTAPDevice(ifindex uint32) (*cubevs.TAPDevice, error) {
	return cubevs.GetTAPDevice(ifindex)
}

func (realCubeVSAdapter) CleanupTAPPolicy(ifindex uint32) error {
	return cubevs.CleanupTAPDevicePolicy(ifindex)
}

func (realCubeVSAdapter) DeleteTAPDeviceMetadata(ifindex uint32, ip net.IP) error {
	return cubevs.DeleteTAPDeviceMetadata(ifindex, ip)
}

func (realCubeVSAdapter) AttachFilter(ifindex uint32) error {
	return cubevs.AttachFilter(ifindex)
}

func (realCubeVSAdapter) InstallTAPDefaultDenyPolicy(ifindex uint32) error {
	return cubevs.InstallTAPDefaultDenyPolicy(ifindex)
}

func (realCubeVSAdapter) GCStaleNetPolicyMaps(keep map[uint32]struct{}, stillPresent func(uint32) bool, onConflict func(uint32)) (int, error) {
	return cubevs.GCStaleNetPolicyMaps(keep, stillPresent, onConflict)
}

func (realCubeVSAdapter) AddPortMapping(ifindex uint32, containerPort, hostPort uint16) error {
	return cubevs.AddPortMapping(ifindex, containerPort, hostPort)
}

func (realCubeVSAdapter) DelPortMapping(ifindex uint32, containerPort, hostPort uint16) error {
	return cubevs.DelPortMapping(ifindex, containerPort, hostPort)
}

func (realCubeVSAdapter) DeletePortMappingsByIfindex(ifindex uint32) error {
	return cubevs.DeletePortMappingsByIfindex(ifindex)
}

// registerCubeVSTap writes the complete TAP metadata and policy options into
// CubeVS for a newly-created sandbox.
func (s *NetworkController) registerCubeVSTap(ifindex int, ip net.IP, sandboxID string, cfg *CubeNetworkConfig) (err error) {
	opts, err := cubeVSTapRegistration(cfg)
	if err != nil {
		return err
	}
	CubeLog.WithContext(context.Background()).Infof(
		"network runtime register cubevs tap: sandbox_id=%s ifindex=%d sandbox_ip=%s cube_network_config=%s allow_internet_access=%v allow_out=%v l7_allow_out=%v deny_out=%v",
		sandboxID,
		ifindex,
		ip.String(),
		formatCubeNetworkConfig(cfg),
		opts.AllowInternetAccess,
		opts.AllowOut,
		opts.L7AllowOut,
		opts.DenyOut,
	)
	start := time.Now()
	defer func() {
		if total := time.Since(start); err != nil || total >= slowStageLogThreshold {
			CubeLog.WithContext(context.Background()).Warnf(
				"network runtime register cubevs tap: sandbox_id=%s total=%s success=%t",
				sandboxID, total, err == nil,
			)
		}
	}()
	err = s.cubevsAdapter.AddTAPDevice(uint32(ifindex), ip, sandboxID, atomic.AddUint32(&s.version, 1), opts)
	return err
}

// replaceCubeVSTap rewrites TAP metadata and the complete desired CubeVS policy.
// Legacy recovery uses this to avoid carrying old allow/deny/DNS residue into
// the recovered Active sandbox.
func (s *NetworkController) replaceCubeVSTap(ifindex int, ip net.IP, sandboxID string, cfg *CubeNetworkConfig) error {
	opts, err := cubeVSTapRegistration(cfg)
	if err != nil {
		return err
	}
	CubeLog.WithContext(context.Background()).Infof(
		"network runtime replace cubevs tap: sandbox_id=%s ifindex=%d sandbox_ip=%s cube_network_config=%s allow_internet_access=%v allow_out=%v l7_allow_out=%v deny_out=%v",
		sandboxID,
		ifindex,
		ip.String(),
		formatCubeNetworkConfig(cfg),
		opts.AllowInternetAccess,
		opts.AllowOut,
		opts.L7AllowOut,
		opts.DenyOut,
	)
	return s.cubevsAdapter.UpsertTAPDevice(uint32(ifindex), ip, sandboxID, atomic.AddUint32(&s.version, 1), opts)
}

// upsertCubeVSTapMeta restores only the TAP identity metadata during recovery
// when the policy entry is missing. Unknown desired policy is not replayed.
func (s *NetworkController) upsertCubeVSTapMeta(ifindex int, ip net.IP, sandboxID string) error {
	CubeLog.WithContext(context.Background()).Infof(
		"network runtime upsert cubevs tap metadata only: sandbox_id=%s ifindex=%d sandbox_ip=%s",
		sandboxID,
		ifindex,
		ip.String(),
	)
	return s.cubevsAdapter.UpsertTAPDeviceMetadata(uint32(ifindex), ip, sandboxID, atomic.AddUint32(&s.version, 1))
}
