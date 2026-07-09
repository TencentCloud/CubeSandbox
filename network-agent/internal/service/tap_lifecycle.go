// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeNet/cubevs"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
)

var (
	restoreTapFunc         = restoreTap
	openTapFdByNameFunc    = openTapFdByName
	newTapFunc             = newTap
	cubevsListTAPDevices   = cubevs.ListTAPDevices
	cubevsListPortMappings = cubevs.ListPortMapping
	maintenanceInterval    = 5 * time.Second
)

const maxAbnormalRecoveryAttempts = 3

const (
	abnormalStageRecycle         = "recycle"
	abnormalStagePreparePool     = "prepare_pool"
	abnormalStageRecoverRestore  = "recover_restore"
	abnormalStageRecoverCleanup  = "recover_cleanup"
	abnormalStageRetryRestore    = "retry_restore"
	abnormalStageLegacyDestroyed = "legacy_destroy_queue"
)

type tapCleanupError struct {
	err error
}

func (e tapCleanupError) Error() string {
	return e.err.Error()
}

func (e tapCleanupError) Unwrap() error {
	return e.err
}

func markTapCleanupError(err error) error {
	if err == nil {
		return nil
	}
	return tapCleanupError{err: err}
}

func isTapCleanupError(err error) bool {
	var cleanupErr tapCleanupError
	return errors.As(err, &cleanupErr)
}

func (s *localService) enqueueTapLocked(tap *tapDevice) {
	if tap == nil {
		return
	}
	if s.quarantinedTaps != nil {
		delete(s.quarantinedTaps, tap.Name)
	}
	tap.InUse = false
	tap.FailureCount = 0
	tap.LastError = ""
	tap.LastStage = ""
	tap.PortMappings = nil
	s.tapPool = append(s.tapPool, tap)
	CubeLog.WithContext(context.Background()).Infof(
		"network-agent tap pooled: name=%s ifindex=%d pool=%d abnormal=%d quarantined=%d",
		tap.Name, tap.Index, len(s.tapPool), len(s.abnormalTapPool), len(s.quarantinedTaps),
	)
}

func (s *localService) dequeueTapLocked() *tapDevice {
	for len(s.tapPool) > 0 {
		tap := s.tapPool[0]
		s.tapPool = s.tapPool[1:]
		if tap != nil {
			CubeLog.WithContext(context.Background()).Infof(
				"network-agent tap dequeued from pool: name=%s ifindex=%d pool=%d abnormal=%d quarantined=%d",
				tap.Name, tap.Index, len(s.tapPool), len(s.abnormalTapPool), len(s.quarantinedTaps),
			)
			return tap
		}
	}
	return nil
}

func (s *localService) enqueueAbnormalLocked(tap *tapDevice, stage string, err error) {
	if tap == nil {
		return
	}
	tap.FailureCount++
	tap.LastStage = stage
	if err != nil {
		tap.LastError = err.Error()
	}
	s.abnormalTapPool = append(s.abnormalTapPool, tap)
	CubeLog.WithContext(context.Background()).Warnf(
		"network-agent tap marked abnormal: name=%s ifindex=%d stage=%s failures=%d err=%v pool=%d abnormal=%d quarantined=%d",
		tap.Name, tap.Index, stage, tap.FailureCount, err, len(s.tapPool), len(s.abnormalTapPool), len(s.quarantinedTaps),
	)
}

func (s *localService) requeuePreparePoolFailureLocked(tap *tapDevice, err error) {
	if tap == nil {
		return
	}
	closeTapFile(tap.File)
	tap.File = nil
	tap.InUse = false
	tap.PortMappings = nil
	tap.FailureCount++
	tap.LastStage = abnormalStagePreparePool
	if err != nil {
		tap.LastError = err.Error()
	}
	if tap.FailureCount >= maxAbnormalRecoveryAttempts {
		if s.quarantinedTaps == nil {
			s.quarantinedTaps = make(map[string]*tapDevice)
		}
		s.quarantinedTaps[tap.Name] = tap
		CubeLog.WithContext(context.Background()).Errorf(
			"network-agent quarantined tap after repeated pool preparation failures: name=%s ifindex=%d failures=%d err=%v quarantined=%d",
			tap.Name, tap.Index, tap.FailureCount, err, len(s.quarantinedTaps),
		)
		return
	}
	s.abnormalTapPool = append(s.abnormalTapPool, tap)
	CubeLog.WithContext(context.Background()).Warnf(
		"network-agent tap pool preparation failed, will retry: name=%s ifindex=%d failures=%d err=%v pending=%d",
		tap.Name, tap.Index, tap.FailureCount, err, len(s.abnormalTapPool),
	)
}

func (s *localService) dequeueAbnormalLocked() *tapDevice {
	for len(s.abnormalTapPool) > 0 {
		tap := s.abnormalTapPool[0]
		s.abnormalTapPool = s.abnormalTapPool[1:]
		if tap != nil {
			return tap
		}
	}
	return nil
}

func (s *localService) configurePortMappings(tap *tapDevice, requestedMappings []PortMapping) ([]PortMapping, error) {
	actualMappings := make([]PortMapping, 0, len(requestedMappings))
	for _, mapping := range requestedMappings {
		hostPort := mapping.HostPort
		if hostPort == 0 {
			allocatedPort, err := s.ports.Allocate()
			if err != nil {
				if cleanupErr := s.clearPortMappings(tap); cleanupErr != nil {
					return nil, errors.Join(err, markTapCleanupError(cleanupErr))
				}
				return nil, err
			}
			hostPort = int32(allocatedPort)
		} else {
			s.ports.Assign(uint16(hostPort))
		}
		if err := cubevsAddPortMap(uint32(tap.Index), uint16(mapping.ContainerPort), uint16(hostPort)); err != nil {
			if mapping.HostPort == 0 {
				s.ports.Release(uint16(hostPort))
			}
			if cleanupErr := s.clearPortMappings(tap); cleanupErr != nil {
				return nil, errors.Join(err, markTapCleanupError(cleanupErr))
			}
			return nil, err
		}
		actualMappings = append(actualMappings, PortMapping{
			Protocol:      nonEmpty(mapping.Protocol, "tcp"),
			HostIP:        nonEmpty(mapping.HostIP, s.cfg.HostProxyBindIP),
			HostPort:      int32(hostPort),
			ContainerPort: mapping.ContainerPort,
		})
		tap.PortMappings = append([]PortMapping(nil), actualMappings...)
	}
	return actualMappings, nil
}

func (s *localService) clearPortMappings(tap *tapDevice) error {
	if tap == nil {
		return nil
	}
	remaining := tap.PortMappings[:0]
	var cleanupErr error
	for _, mapping := range tap.PortMappings {
		if err := cubevsDelPortMap(uint32(tap.Index), uint16(mapping.ContainerPort), uint16(mapping.HostPort)); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("delete port mapping %d->%d for tap %s(%d): %w",
				mapping.ContainerPort, mapping.HostPort, tap.Name, tap.Index, err))
			remaining = append(remaining, mapping)
			continue
		}
		s.ports.Release(uint16(mapping.HostPort))
	}
	if cleanupErr != nil {
		tap.PortMappings = remaining
		return cleanupErr
	}
	tap.PortMappings = nil
	return nil
}

func (s *localService) recycleTapLocked(tap *tapDevice) {
	s.stageTapForPoolLocked(tap, "recycle")
}

// createPoolTap provisions a fresh tap and stages it into the free pool. Only
// the IP allocation (self-locked allocator) and the final staging take a lock;
// the heavy newTap syscalls run lock-free so background inventory refills never
// hold s.mu while creating taps.
func (s *localService) createPoolTap() error {
	ip, err := s.allocator.Allocate()
	if err != nil {
		return err
	}
	tap, err := newTapFunc(ip, s.cfg.MVMMacAddr, s.cfg.MvmMtu, s.cubeDev.Index)
	if err != nil {
		s.allocator.Release(ip)
		return err
	}
	if err := s.prepareAndStageTapForPool(context.Background(), tap, "create_pool"); err != nil {
		closeTapFile(tap.File)
		_ = destroyTapFunc(tap.Index)
		s.allocator.Release(ip)
		return err
	}
	return nil
}

func (s *localService) ensureTapInventory() error {
	if s.cfg.TapInitNum <= 0 {
		return nil
	}
	taps, err := listCubeTapsFunc()
	if err != nil {
		return err
	}
	need := s.cfg.TapInitNum - len(taps)
	for i := 0; i < need; i++ {
		if err := s.createPoolTap(); err != nil {
			return err
		}
	}
	return nil
}

// warmupTapPoolBackground runs ensureTapInventory off the startup path
// so NewLocalService can return immediately rather than block on
// O(TapInitNum) tap creations. See
// code-analysis/network/11-network-agent-async-init-plan.md for the
// full rationale.
//
// On failure we log at ERROR with the partial pool size so an operator
// can see how degraded the node is, then exit. We deliberately do NOT
// retry or wake up the maintenance loop to backfill: keeping the pool
// at TapInitNum is currently a passive contract (only the
// abnormal-tap-missing path triggers a refill), and turning it into an
// active one is a separate design change. A degraded pool is
// functionally fine — EnsureNetwork transparently falls back to
// creating taps on demand when the pool is empty (see
// localService.createStateLocked, "fromPool == false" branch).
func (s *localService) warmupTapPoolBackground() {
	err := s.ensureTapInventory()
	if err == nil {
		return
	}
	s.mu.Lock()
	poolSize := len(s.tapPool)
	s.mu.Unlock()
	missing := s.cfg.TapInitNum - poolSize
	CubeLog.WithContext(context.Background()).Errorf(
		"network-agent background tap pool warmup failed at pool_size=%d/target=%d: %v; "+
			"the next %d sandbox creations will create taps on demand (~63ms extra each)",
		poolSize, s.cfg.TapInitNum, err, missing,
	)
}

func (s *localService) startMaintenanceLoop() {
	go func() {
		ticker := time.NewTicker(maintenanceInterval)
		defer ticker.Stop()
		for range ticker.C {
			s.handleAbnormalTaps()
			// Re-fire any per-sandbox CubeEgress pushes that failed
			// transiently in EnsureNetwork. Independent of tap
			// recovery — a bad CubeEgress should not block tap upkeep
			// and vice versa.
			s.retryPendingEgressPushes()
		}
	}()
}

func (s *localService) handleAbnormalTaps() {
	logger := CubeLog.WithContext(context.Background())
	s.mu.Lock()
	if s.quarantinedTaps == nil {
		s.quarantinedTaps = make(map[string]*tapDevice)
	}
	for name, tap := range s.destroyFailedTaps {
		if tap != nil {
			tap.LastStage = abnormalStageLegacyDestroyed
			s.quarantinedTaps[name] = tap
		}
		delete(s.destroyFailedTaps, name)
	}
	s.mu.Unlock()

	missingReleased := false
	for {
		s.mu.Lock()
		tap := s.dequeueAbnormalLocked()
		s.mu.Unlock()
		if tap == nil {
			break
		}
		if tap.LastStage == abnormalStagePreparePool {
			if err := s.prepareAndStageTapForPool(context.Background(), tap, "async_prepare"); err != nil {
				s.mu.Lock()
				s.requeuePreparePoolFailureLocked(tap, err)
				s.mu.Unlock()
			}
			continue
		}
		restored, err := s.tryRecoverAbnormalTap(tap)
		if err != nil {
			s.mu.Lock()
			tap.FailureCount++
			retryStage := abnormalStageRetryRestore
			if tap.LastStage == abnormalStageRecoverCleanup {
				retryStage = abnormalStageRecoverCleanup
			}
			tap.LastStage = retryStage
			tap.LastError = err.Error()
			if isTapMissingError(err) {
				logger.Warnf("network-agent abnormal tap missing on host, releasing ip: name=%s ifindex=%d ip=%s err=%v",
					tap.Name, tap.Index, tap.IP, err)
				s.allocator.Release(tap.IP)
				missingReleased = true
			} else if tap.FailureCount >= maxAbnormalRecoveryAttempts {
				s.quarantinedTaps[tap.Name] = tap
				logger.Errorf("network-agent quarantined tap after repeated recovery failures: name=%s ifindex=%d failures=%d last_stage=%s err=%s quarantined=%d",
					tap.Name, tap.Index, tap.FailureCount, tap.LastStage, tap.LastError, len(s.quarantinedTaps))
			} else {
				s.enqueueAbnormalLocked(tap, retryStage, err)
			}
			s.mu.Unlock()
			continue
		}
		if err := s.prepareAndStageTapForPool(context.Background(), restored, "abnormal_recovered"); err != nil {
			s.mu.Lock()
			s.requeuePreparePoolFailureLocked(restored, err)
			s.mu.Unlock()
			continue
		}
	}

	if missingReleased {
		if err := s.ensureTapInventory(); err != nil {
			logger.Warnf("network-agent refill tap inventory failed: %v", err)
		}
	}
}

func (s *localService) stageTapForPoolLocked(tap *tapDevice, reason string) {
	if tap == nil {
		return
	}
	closeTapFile(tap.File)
	tap.File = nil
	tap.InUse = false
	tap.PortMappings = nil
	CubeLog.WithContext(context.Background()).Infof(
		"network-agent staging tap for pool: name=%s ifindex=%d reason=%s",
		tap.Name, tap.Index, reason,
	)
	s.enqueueTapLocked(tap)
}

func (s *localService) tryRecoverAbnormalTap(tap *tapDevice) (*tapDevice, error) {
	if tap == nil {
		return nil, fmt.Errorf("tap is nil")
	}

	restored, err := restoreTapFunc(tap, s.cfg.MvmMtu, s.cfg.MVMMacAddr, s.cubeDev.Index)
	if err != nil {
		tap.LastError = err.Error()
		return nil, err
	}
	if tap.LastStage == abnormalStageRecoverCleanup {
		if err := s.clearPortMappings(restored); err != nil {
			tap.LastError = err.Error()
			return nil, err
		}
		if err := s.cleanupCubeVSTap(restored.Index, restored.IP.To4()); err != nil {
			tap.LastError = err.Error()
			return nil, err
		}
	}
	return restored, nil
}

func buildRecoveredState(tap *tapDevice, device *cubevs.TAPDevice, mappings []PortMapping, cfg Config) *managedState {
	sandboxID := tap.Name
	if device != nil && device.ID != "" {
		sandboxID = device.ID
	}
	return &managedState{
		persistedState: persistedState{
			SandboxID:     sandboxID,
			NetworkHandle: sandboxID,
			TapName:       tap.Name,
			TapIfIndex:    tap.Index,
			SandboxIP:     tap.IP.String(),
			Interfaces: []Interface{{
				Name:    tap.Name,
				MAC:     cfg.MVMMacAddr,
				MTU:     int32(cfg.MvmMtu),
				IPs:     []string{fmt.Sprintf("%s/%d", cfg.MVMInnerIP, cfg.MvmMask)},
				Gateway: cfg.MvmGwDestIP,
			}},
			PortMappings: append([]PortMapping(nil), mappings...),
			PersistMetadata: map[string]string{
				"sandbox_ip":    tap.IP.String(),
				"host_tap_name": tap.Name,
				"mvm_inner_ip":  cfg.MVMInnerIP,
				"gateway_ip":    cfg.MvmGwDestIP,
			},
		},
		tap: tap,
	}
}

func closeTapFile(file *os.File) {
	if file != nil {
		_ = file.Close()
	}
}
