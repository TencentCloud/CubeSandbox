// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package runtime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"golang.org/x/sys/unix"
)

var maintenanceInterval = 5 * time.Second

// tapPoolEntryFromDevice converts a live/in-memory TAP into the TapPool state
// representation and validates that the TAP has usable identity fields.
func tapPoolEntryFromDevice(tap *tapDevice, owner string, state TapPoolState) (*TapPoolEntry, error) {
	if tap == nil {
		return nil, fmt.Errorf("tap is nil")
	}
	return NewTapPoolEntry(tap.Name, tap.Index, tap.IP, owner, state)
}

// closeRuntimeTapFile closes a runtime-owned TAP fd and clears exactly that
// cached pointer. Caller-owned handoff duplicates are never stored by runtime.
func (s *NetworkController) closeRuntimeTapFile(tap *tapDevice) {
	if tap == nil || tap.File == nil {
		return
	}
	file := tap.File
	tap.File = nil
	if s != nil && s.tapAdapter != nil {
		s.tapAdapter.Close(file)
		return
	}
	closeTapFile(file)
}

// poolTapFD retains a runtime-owned TAP fd for an idle pooled TAP. A retained
// fd stays attached to the one-queue TAP for its whole pool lifetime, so later
// handoffs only duplicate it and never pay a TUNSETIFF on the hot path. A
// displaced stale entry (should not happen in normal flow) is closed.
func (s *NetworkController) poolTapFD(name string, file *os.File) {
	if name == "" || file == nil {
		return
	}
	s.mu.Lock()
	if s.tapFds == nil {
		s.tapFds = make(map[string]*os.File)
	}
	old := s.tapFds[name]
	s.tapFds[name] = file
	s.mu.Unlock()
	if old != nil && old != file {
		s.tapAdapter.Close(old)
	}
}

// takePooledTapFD moves an idle pooled fd into Active ownership. It returns nil
// when the TAP has no retained fd (e.g. recovered after a runtime restart);
// GetTapFile then falls back to a lazy open as before.
func (s *NetworkController) takePooledTapFD(name string) *os.File {
	s.mu.Lock()
	defer s.mu.Unlock()
	file := s.tapFds[name]
	delete(s.tapFds, name)
	return file
}

// hasPooledTapFD reports whether an idle retained fd exists for the TAP.
func (s *NetworkController) hasPooledTapFD(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tapFds[name] != nil
}

// dropPooledTapFD closes and forgets a retained idle fd (TAP destroyed).
func (s *NetworkController) dropPooledTapFD(name string) {
	s.mu.Lock()
	file := s.tapFds[name]
	delete(s.tapFds, name)
	s.mu.Unlock()
	if file != nil {
		s.tapAdapter.Close(file)
	}
}

// drainTapFD discards frames queued while the TAP was idle so the next sandbox
// never reads a previous lifecycle's traffic. Poll gates every read, so this
// never blocks; the iteration cap is a backstop for pathological queues.
func drainTapFD(file *os.File) {
	if file == nil {
		return
	}
	fds := []unix.PollFd{{Fd: int32(file.Fd()), Events: unix.POLLIN}}
	buf := make([]byte, 65536)
	for i := 0; i < 1024; i++ {
		n, err := unix.Poll(fds, 0)
		if n <= 0 || err != nil {
			return
		}
		nr, err := unix.Read(int(file.Fd()), buf)
		if nr <= 0 || err != nil {
			return
		}
	}
}

// tapDeviceFromEntry rebuilds the process-local TAP view from the pool identity.
// Ready and Cleaning entries never retain a runtime fd.
func tapDeviceFromEntry(entry *TapPoolEntry) (*tapDevice, error) {
	if entry == nil {
		return nil, fmt.Errorf("tap pool entry is nil")
	}
	return &tapDevice{Name: entry.TapName, Index: entry.TapIfIndex, IP: append(net.IP(nil), entry.SandboxIP.To4()...)}, nil
}

// addReadyTap publishes a prepared TAP into the free pool. The creation fd is
// retained in the pool registry rather than closed, so the next owner skips
// the TUNSETIFF reopen entirely.
func (s *NetworkController) addReadyTap(tap *tapDevice) error {
	entry, err := tapPoolEntryFromDevice(tap, "", TapPoolReady)
	if err != nil {
		return err
	}
	if err := s.tapPool.Add(entry); err != nil {
		return err
	}
	file := tap.File
	tap.File = nil
	s.poolTapFD(tap.Name, file)
	return nil
}

// resetTapRuntimeFieldsForPool clears process-local fields that must not leak
// into the next sandbox assigned to this reusable TAP. The runtime fd, when
// present, is returned to the idle pool registry.
func (s *NetworkController) resetTapRuntimeFieldsForPool(tap *tapDevice) {
	if tap == nil {
		return
	}
	tap.InUse = false
	tap.FailureCount = 0
	tap.LastError = ""
	tap.LastStage = ""
	tap.PortMappings = nil
	file := tap.File
	tap.File = nil
	s.poolTapFD(tap.Name, file)
}

// verifyTapReusableFD checks that an idle TAP can be reopened before it is
// returned to Ready. With fd retention the common case needs no probe: a
// retained fd is by definition attached and duplicable. Only a TAP without a
// retained fd (e.g. recovered after restart) is probed, and the successfully
// opened fd is then retained for future handoffs.
func (s *NetworkController) verifyTapReusableFD(tap *tapDevice) error {
	if tap == nil || tap.Name == "" {
		return nil
	}
	if s.hasPooledTapFD(tap.Name) {
		return nil
	}
	file, err := s.tapAdapter.Open(tap.Name)
	if err != nil {
		return fmt.Errorf("verify tap %s reusable fd: %w", tap.Name, err)
	}
	s.poolTapFD(tap.Name, file)
	return nil
}

// markTapReady publishes a cleaned TAP to the free pool. Callers must already
// have cleared sandbox-specific runtime fields and verified fd reusability before
// deleting the durable cleanup state and exposing the TAP here.
func (s *NetworkController) markTapReady(tap *tapDevice) error {
	if tap == nil {
		return nil
	}
	entry, ok := s.tapPool.GetByName(tap.Name)
	if !ok {
		return fmt.Errorf("tap %s is not managed by pool", tap.Name)
	}
	if err := s.tapPool.MarkReady(entry); err != nil {
		return err
	}
	return nil
}

// beginTapCleanup moves a TAP into Cleaning ownership. It also creates a pool
// entry for recovered/interrupted TAPs that were not already tracked in memory.
func (s *NetworkController) beginTapCleanup(tap *tapDevice, owner string) error {
	if tap == nil {
		return nil
	}
	if _, ok := s.tapPool.GetByName(tap.Name); !ok {
		entry, err := tapPoolEntryFromDevice(tap, owner, TapPoolCleaning)
		if err != nil {
			return err
		}
		if err := s.tapPool.Add(entry); err != nil {
			return err
		}
		return nil
	}
	_, err := s.tapPool.BeginCleanupByName(tap.Name, owner)
	return err
}

// reservePortMappings reserves concrete host ports and mirrors them onto the TAP
// so later cleanup can remove exactly the mappings that were attempted.
func (s *NetworkController) reservePortMappings(owner string, tap *tapDevice, requestedMappings []PortMapping) ([]PortMapping, error) {
	actualMappings, err := s.ports.Reserve(owner, requestedMappings, s.cfg.HostPortBindIP)
	if err != nil {
		return nil, err
	}
	if tap != nil {
		tap.PortMappings = append([]PortMapping(nil), actualMappings...)
	}
	return actualMappings, nil
}

// applyPortMappings programs CubeVS port maps. If one map fails, previously
// installed maps are removed immediately and the error tells the caller whether
// cleanup itself also failed.
func (s *NetworkController) applyPortMappings(owner string, tap *tapDevice) error {
	if tap == nil {
		return nil
	}
	tap.PortMappings = append([]PortMapping(nil), tap.PortMappings...)
	for _, mapping := range tap.PortMappings {
		if err := s.cubevsAdapter.AddPortMapping(uint32(tap.Index), uint16(mapping.ContainerPort), uint16(mapping.HostPort)); err != nil {
			if cleanupErr := s.cleanupPortMappings(owner, tap); cleanupErr != nil {
				return errors.Join(err, cleanupErr)
			}
			return err
		}
	}
	return nil
}

// releasePortOwnership frees host ports allocated for a sandbox. Safe before or
// after CubeVS mapping install because deletes are idempotent.
func (s *NetworkController) releasePortOwnership(owner string) {
	s.ports.ReleaseOwnership(owner)
}

// deleteCubeVSPortMappings deletes the exact mappings carried by this cleanup
// attempt. CubeVS DelPortMapping performs the current-value comparison across
// both maps, so the runtime does not need a second ownership inference layer.
func (s *NetworkController) deleteCubeVSPortMappings(_ string, tap *tapDevice) error {
	if tap == nil {
		return nil
	}
	remaining := make([]PortMapping, 0, len(tap.PortMappings))
	var cleanupErr error
	for _, mapping := range tap.PortMappings {
		if mapping.HostPort == 0 {
			continue
		}
		if err := s.cubevsAdapter.DelPortMapping(uint32(tap.Index), uint16(mapping.ContainerPort), uint16(mapping.HostPort)); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("delete port mapping %d->%d for tap %s(%d): %w",
				mapping.ContainerPort, mapping.HostPort, tap.Name, tap.Index, err))
			remaining = append(remaining, mapping)
			continue
		}
	}
	if cleanupErr != nil {
		tap.PortMappings = remaining
		return cleanupErr
	}
	tap.PortMappings = nil
	return nil
}

// reconcilePortMappingsForTap rebuilds one recovered TAP's mappings from its
// durable desired state. ifindex identifies the old TAP lifecycle, so a scoped
// sweep is both sufficient and independent of unrelated split-brain tuples in
// the global maps. CubeVS conditional deletion leaves mappings already reused
// by another ifindex untouched.
func (s *NetworkController) reconcilePortMappingsForTap(tap *tapDevice, desired []PortMapping) error {
	if tap == nil {
		return nil
	}
	if err := s.cubevsAdapter.DeletePortMappingsByIfindex(uint32(tap.Index)); err != nil {
		return fmt.Errorf("delete stale port mappings for recovered tap %s(%d): %w", tap.Name, tap.Index, err)
	}
	for _, mapping := range desired {
		if mapping.HostPort == 0 {
			continue
		}
		if err := s.cubevsAdapter.AddPortMapping(uint32(tap.Index), uint16(mapping.ContainerPort), uint16(mapping.HostPort)); err != nil {
			return fmt.Errorf("restore port mapping %d->%d for tap %s(%d): %w", mapping.ContainerPort, mapping.HostPort, tap.Name, tap.Index, err)
		}
	}
	return nil
}

// cleanupPortMappings removes CubeVS mappings and releases PortBinder ownership.
func (s *NetworkController) cleanupPortMappings(owner string, tap *tapDevice) error {
	if err := s.deleteCubeVSPortMappings(owner, tap); err != nil {
		return err
	}
	s.ports.ReleaseOwnership(owner)
	return nil
}

// resetTapPolicyForPool removes any old CubeVS policy for a TAP and installs
// the default-deny policy required before the TAP can enter Ready.
func (s *NetworkController) resetTapPolicyForPool(tap *tapDevice) error {
	if tap == nil {
		return nil
	}
	if err := s.cubevsAdapter.CleanupTAPPolicy(uint32(tap.Index)); err != nil {
		return err
	}
	return s.installPoolDefaultDeny(tap)
}

// installPoolDefaultDeny installs the reusable-pool policy after a TAP has
// already had sandbox-specific CubeVS state removed.
func (s *NetworkController) installPoolDefaultDeny(tap *tapDevice) error {
	if tap == nil {
		return nil
	}
	return s.cubevsAdapter.InstallTAPDefaultDenyPolicy(uint32(tap.Index))
}

// createPoolTap provisions a fresh tap and stages it into the free pool. Only
// the IP allocation (self-locked allocator) and the final staging take a lock;
// the heavy newTap syscalls run lock-free so background inventory refills never
// hold s.mu while creating taps.
func (s *NetworkController) createPoolTap() error {
	ip, err := s.allocator.Allocate()
	if err != nil {
		return err
	}
	tap, err := s.tapAdapter.Create(ip, s.cfg.MVMMacAddr, s.cfg.MvmMtu, s.cubeDev.Index)
	if err != nil {
		s.allocator.Release(ip)
		return err
	}
	if err := s.resetTapPolicyForPool(tap); err != nil {
		s.tapAdapter.Close(tap.File)
		_ = s.tapAdapter.Destroy(tap.Index)
		s.allocator.Release(ip)
		return err
	}
	if err := s.addReadyTap(tap); err != nil {
		s.tapAdapter.Close(tap.File)
		_ = s.tapAdapter.Destroy(tap.Index)
		s.allocator.Release(ip)
		return err
	}
	CubeLog.WithContext(context.Background()).Infof("network runtime tap staged as Ready: name=%s ifindex=%d reason=%s", tap.Name, tap.Index, "create_pool")
	return nil
}

// closeTapFile closes a TAP fd when one exists.
func closeTapFile(file *os.File) {
	if file != nil {
		_ = file.Close()
	}
}

// duplicateTapFile returns a caller-owned duplicate of a runtime-owned TAP fd.
func duplicateTapFile(file *os.File) (*os.File, error) {
	if file == nil {
		return nil, fmt.Errorf("tap fd file is nil")
	}
	fd, err := unix.FcntlInt(file.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("duplicate tap fd: %w", err)
	}
	return os.NewFile(uintptr(fd), file.Name()), nil
}

// ensureTapInventory tops the pool up to TapInitNum total entries.
func (s *NetworkController) ensureTapInventory() error {
	if s.cfg.TapInitNum <= 0 {
		return nil
	}
	need := s.cfg.TapInitNum - len(s.tapPool.Entries())
	for i := 0; i < need; i++ {
		if err := s.createPoolTap(); err != nil {
			return err
		}
	}
	return nil
}

// warmupTapPoolBackground runs ensureTapInventory off the startup path
// so NewNetworkController can return immediately rather than block on
// O(TapInitNum) tap creations. Warmup targets the total TapPool entry count:
// Ready is only the allocatable subset, while Active/Cleaning entries still
// consume pool capacity and must not trigger over-creation.
//
// On failure we log at ERROR with the partial pool size so an operator
// can see how degraded the node is, then exit. A degraded pool is
// functionally fine — EnsureNetwork transparently falls back to
// creating taps on demand when the pool is empty (see
// NetworkController.acquireTap).
func (s *NetworkController) warmupTapPoolBackground() {
	err := s.ensureTapInventory()
	if err == nil {
		return
	}
	poolSize := len(s.tapPool.Entries())
	missing := s.cfg.TapInitNum - poolSize
	CubeLog.WithContext(context.Background()).Errorf(
		"network runtime background tap pool warmup failed at pool_size=%d/target=%d: %v; "+
			"the next %d sandbox creations will create taps on demand (~63ms extra each)",
		poolSize, s.cfg.TapInitNum, err, missing,
	)
}

// tapHandoffTimings captures per-stage durations for GetTapFile. Fields are
// cumulative across EBADF retry attempts so the deferred log reflects total
// work, not just the final attempt. Zero-valued fields mean the stage did not
// run (e.g. open_tap=0 indicates the cached fd was reused on every attempt).
type tapHandoffTimings struct {
	lockWait time.Duration
	snapshot time.Duration
	openTap  time.Duration
	dupFd    time.Duration
}

// GetTapFile is the hot fd handoff path used after EnsureNetwork succeeds. It
// validates durable success + Active ownership, lazily opens the runtime-owned
// fd when needed, and returns a caller-owned duplicate. The managed state's
// tap.File is the single runtime cache; runtime repair belongs to startup
// recovery and maintenance rather than this handoff path.
func (s *NetworkController) GetTapFile(sandboxID, tapName string) (handoff *os.File, err error) {
	totalStart := time.Now()
	stageStart := totalStart
	var t tapHandoffTimings
	attempts := 0
	defer func() {
		if total := time.Since(totalStart); err != nil || total >= slowStageLogThreshold {
			CubeLog.WithContext(context.Background()).Warnf(
				"network runtime tap fd handoff: sandbox_id=%s total=%s success=%t attempts=%d lock_wait=%s snapshot=%s open_tap=%s dup_fd=%s",
				sandboxID, total, err == nil, attempts,
				t.lockWait, t.snapshot, t.openTap, t.dupFd,
			)
		}
	}()
	unlock := func() {}
	if s.locks != nil {
		unlock = s.locks.Lock(sandboxID)
	}
	defer unlock()
	t.lockWait = time.Since(stageStart)
	stageStart = time.Now()

	for attempt := 0; attempt < 2; attempt++ {
		attempts++
		state, cachedFile, err := s.tapFileHandoffSnapshot(sandboxID, tapName)
		if err != nil {
			return nil, err
		}
		t.snapshot += time.Since(stageStart)
		stageStart = time.Now()

		file := cachedFile
		opened := false
		if file == nil {
			file, err = s.tapAdapter.Open(state.TapName)
			if err != nil {
				return nil, fmt.Errorf("tap fd unavailable for sandbox %q: reopen tap %s: %w", sandboxID, state.TapName, err)
			}
			opened = true
			state, err = s.publishTapFileForHandoff(sandboxID, state.TapName, file)
			if err != nil {
				s.clearStaleTapFile(sandboxID, file)
				s.tapAdapter.Close(file)
				return nil, err
			}
		}
		t.openTap += time.Since(stageStart)
		stageStart = time.Now()
		handoff, err = duplicateTapFile(file)
		t.dupFd += time.Since(stageStart)
		stageStart = time.Now()
		if err == nil {
			return handoff, nil
		}
		if errors.Is(err, unix.EBADF) {
			s.clearStaleTapFile(sandboxID, file)
			if opened {
				s.tapAdapter.Close(file)
			}
			CubeLog.WithContext(context.Background()).Warnf("network runtime cached tap fd is stale; reopening for handoff: sandbox_id=%s tap=%s err=%v", sandboxID, state.TapName, err)
			continue
		}
		if opened {
			s.clearStaleTapFile(sandboxID, file)
			s.tapAdapter.Close(file)
		}
		return nil, fmt.Errorf("tap fd unavailable for sandbox %q: %w", sandboxID, err)
	}
	return nil, fmt.Errorf("tap fd unavailable for sandbox %q after stale fd retry", sandboxID)
}

func (s *NetworkController) tapFileHandoffSnapshot(sandboxID, tapName string) (*managedState, *os.File, error) {
	s.mu.Lock()
	state, ok := s.states[sandboxID]
	if !ok {
		s.mu.Unlock()
		return nil, nil, fmt.Errorf("sandbox %q not found", sandboxID)
	}
	if tapName != "" && state.TapName != tapName {
		s.mu.Unlock()
		return nil, nil, fmt.Errorf("tap name mismatch: want %q got %q", tapName, state.TapName)
	}
	if state.tap == nil {
		s.mu.Unlock()
		return nil, nil, fmt.Errorf("tap fd unavailable for sandbox %q: tap metadata missing", sandboxID)
	}
	expectedTap := state.TapName
	s.mu.Unlock()

	if !s.store.Exists(sandboxID, StateFileSuccess) {
		return nil, nil, fmt.Errorf("tap fd unavailable for sandbox %q: success state missing", sandboxID)
	}
	poolState, owner, ok := s.tapPool.StateByName(expectedTap)
	if !ok || poolState != TapPoolActive || owner != sandboxID {
		return nil, nil, fmt.Errorf("tap fd unavailable for sandbox %q: tap %s state=%s owner=%s", sandboxID, expectedTap, poolState, owner)
	}
	return state, state.tap.File, nil
}

func (s *NetworkController) publishTapFileForHandoff(sandboxID, tapName string, file *os.File) (*managedState, error) {
	s.mu.Lock()
	state, ok := s.states[sandboxID]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("sandbox %q not found", sandboxID)
	}
	if state.TapName != tapName || state.tap == nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("tap fd unavailable for sandbox %q: tap metadata changed", sandboxID)
	}
	state.tap.File = file
	s.mu.Unlock()
	return state, nil
}

func (s *NetworkController) clearStaleTapFile(sandboxID string, file *os.File) {
	s.mu.Lock()
	if state := s.states[sandboxID]; state != nil && state.tap != nil && state.tap.File == file {
		state.tap.File = nil
	}
	s.mu.Unlock()
}
