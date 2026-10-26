// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package runtime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

// cleanupAttemptContext bounds one synchronous cleanup attempt. A failed
// attempt leaves durable state for the next maintenance pass.
func (s *NetworkController) cleanupAttemptContext() (context.Context, context.CancelFunc) {
	timeout := s.cfg.CubeEgressPushTimeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	return context.WithTimeout(context.Background(), timeout*2)
}

func (s *NetworkController) cleanupTapOnce(state *managedState, stateKind StateFileKind, reason string) error {
	ctx, cancel := s.cleanupAttemptContext()
	defer cancel()
	err := s.cleanupTapForReuse(ctx, state, stateKind)
	if err == nil {
		return nil
	}
	if state != nil && state.tap != nil {
		state.tap.FailureCount++
		state.tap.LastStage = "cleanup"
		state.tap.LastError = err.Error()
		s.tapPool.RecordFailure(state.tap.Name, err)
	}
	CubeLog.WithContext(context.Background()).Warnf(
		"network runtime cleanup attempt failed; durable state remains for maintenance: sandbox_id=%s reason=%s err=%v",
		managedStateSandboxID(state), reason, err,
	)
	return err
}

func (s *NetworkController) cleanupStateOnlyOnce(record *StateRecord, reason string) error {
	ctx, cancel := s.cleanupAttemptContext()
	defer cancel()
	err := s.cleanupStateOnlyResidue(ctx, record)
	if err != nil {
		CubeLog.WithContext(context.Background()).Warnf(
			"network runtime state-only cleanup attempt failed; durable state remains for maintenance: sandbox_id=%s reason=%s path=%s err=%v",
			stateRecordSandboxID(record), reason, stateRecordPath(record), err,
		)
	}
	return err
}

func managedStateSandboxID(state *managedState) string {
	if state == nil {
		return ""
	}
	return state.SandboxID
}

func stateRecordSandboxID(record *StateRecord) string {
	if record == nil || record.State == nil {
		return ""
	}
	return record.State.SandboxID
}

func stateRecordPath(record *StateRecord) string {
	if record == nil {
		return ""
	}
	return record.Path
}

// closeRuntimeTapOwnership releases the Active/cleanup state's sole
// runtime-owned TAP fd back to the idle pool registry after draining frames
// queued during this lifecycle. Caller-owned handoff duplicates remain the
// shim/hypervisor's responsibility.
func (s *NetworkController) closeRuntimeTapOwnership(state *managedState) {
	if s == nil || state == nil || state.tap == nil {
		return
	}
	file := state.tap.File
	state.tap.File = nil
	if file == nil {
		return
	}
	drainTapFD(file)
	s.poolTapFD(state.tap.Name, file)
}

// requireCleaningForReuse ensures cleanup only prepares TAPs that have already
// been removed from Active ownership and moved into the Cleaning state.
func (s *NetworkController) requireCleaningForReuse(state *managedState) error {
	poolState, owner, ok := s.tapPool.StateByName(state.TapName)
	if !ok {
		return fmt.Errorf("tap %s is not managed by pool", state.TapName)
	}
	if poolState != TapPoolCleaning || owner != state.SandboxID {
		return fmt.Errorf("tap %s cannot be cleaned for reuse from state=%s owner=%s", state.TapName, poolState, owner)
	}
	return nil
}

// cleanupAlreadyFinished is an idempotency guard for a maintenance snapshot
// that became stale before it acquired the sandbox lock.
func (s *NetworkController) cleanupAlreadyFinished(state *managedState, stateKind StateFileKind) bool {
	if s == nil || state == nil || state.SandboxID == "" || state.TapName == "" {
		return false
	}
	if stateKind != "" && s.store.Exists(state.SandboxID, stateKind) {
		return false
	}
	poolState, owner, ok := s.tapPool.StateByName(state.TapName)
	if !ok {
		return true
	}
	return poolState != TapPoolCleaning || owner != state.SandboxID
}

// recordCleanStep is a test hook for cleanup crash-point ordering assertions.
func (s *NetworkController) recordCleanStep(step string) {
	if s.cleanStepHook != nil {
		s.cleanStepHook(step)
	}
}

// cleanupStateOnlyResidue handles a durable record whose TAP no longer exists.
// The exact file fingerprint is checked before any side effect and again before
// deletion so an old maintenance snapshot cannot act on a replacement lifecycle.
func (s *NetworkController) cleanupStateOnlyResidue(ctx context.Context, record *StateRecord) error {
	if record == nil || record.State == nil {
		return nil
	}
	current, err := s.store.RecordCurrent(record)
	if err != nil {
		return err
	}
	if !current {
		return nil
	}
	if record.Superseded {
		deleted, err := s.store.DeleteRecordIfCurrent(record)
		if err != nil {
			return err
		}
		if !deleted {
			return fmt.Errorf("superseded state record changed before retirement: path=%s", record.Path)
		}
		CubeLog.WithContext(context.Background()).Warnf(
			"network runtime retired superseded state without touching shared runtime resources: sandbox_id=%s path=%s",
			record.State.SandboxID, record.Path,
		)
		return nil
	}

	state := record.State
	tap := &tapDevice{
		Index:        state.TapIfIndex,
		Name:         state.TapName,
		IP:           net.ParseIP(state.SandboxIP).To4(),
		PortMappings: append([]PortMapping(nil), state.PortMappings...),
	}

	if err := errors.Join(
		s.deleteCubeVSPortMappings(state.SandboxID, tap),
		s.cleanupCubeVSTapState(state.TapIfIndex, net.ParseIP(state.SandboxIP).To4()),
	); err != nil {
		return err
	}
	if err := s.cleanupEgressForReuse(ctx, state, record.Legacy || record.LegacyCompanion != nil); err != nil {
		return err
	}

	if record.LegacyCompanion != nil {
		deleted, err := s.store.DeleteRecordIfCurrent(record.LegacyCompanion)
		if err != nil {
			return err
		}
		if !deleted {
			return fmt.Errorf("legacy companion changed before state-only cleanup retirement: path=%s", record.LegacyCompanion.Path)
		}
	}
	deleted, err := s.store.DeleteRecordIfCurrent(record)
	if err != nil {
		return err
	}
	if !deleted {
		return fmt.Errorf("state record changed during cleanup: path=%s", record.Path)
	}
	s.ports.ReleaseOwnership(state.SandboxID)
	if ip := net.ParseIP(state.SandboxIP).To4(); ip != nil {
		s.allocator.Release(ip)
	}
	return nil
}

// cleanupTapForReuse removes all sandbox-specific side effects from a live TAP
// and returns it to Ready. Every invocation is one attempt; failures leave the
// TAP Cleaning and preserve durable state for maintenance.
func (s *NetworkController) cleanupTapForReuse(ctx context.Context, state *managedState, stateKind StateFileKind) error {
	if state == nil {
		return fmt.Errorf("state is nil")
	}
	deleteRuntimeState := stateKind != ""
	if deleteRuntimeState && s.cleanupAlreadyFinished(state, stateKind) {
		return nil
	}
	if err := s.requireCleaningForReuse(state); err != nil {
		return err
	}
	if state.tap == nil {
		state.tap = &tapDevice{
			Index: state.TapIfIndex,
			Name:  state.TapName,
			IP:    net.ParseIP(state.SandboxIP).To4(),
		}
	}

	// Closing runtime ownership is always the first cleanup side effect.
	s.closeRuntimeTapOwnership(state)
	s.recordCleanStep("fd_close")

	if state.restoreBeforeCleanup {
		restored, err := s.tapAdapter.Restore(state.tap, s.cfg.MvmMtu, s.cfg.MVMMacAddr, s.cubeDev.Index)
		if err != nil {
			return err
		}
		state.tap = restored
		state.TapIfIndex = restored.Index
		state.TapName = restored.Name
		state.SandboxIP = restored.IP.String()
	}

	// Durable records already carry the exact expected tuples. CubeVS performs
	// conditional deletion, so stale tuples are harmless no-ops. Only a no-state
	// TAP needs an ifindex-scoped sweep because it has no target list.
	if stateKind == "" && state.legacyStatePath == "" {
		if err := s.cubevsAdapter.DeletePortMappingsByIfindex(uint32(state.tap.Index)); err != nil {
			return fmt.Errorf("delete port mappings by ifindex for no-state tap %s(%d): %w", state.tap.Name, state.tap.Index, err)
		}
		state.PortMappings = nil
	}
	state.tap.PortMappings = append([]PortMapping(nil), state.PortMappings...)

	if err := errors.Join(
		s.deleteCubeVSPortMappings(state.SandboxID, state.tap),
		s.cleanupCubeVSTapState(state.TapIfIndex, net.ParseIP(state.SandboxIP).To4()),
	); err != nil {
		return err
	}
	if err := s.cleanupEgressForReuse(ctx, &state.persistedState, state.legacyStatePath != "" || stateKind == ""); err != nil {
		return err
	}
	s.recordCleanStep("runtime_cleaned")

	s.resetTapRuntimeFieldsForPool(state.tap)
	if err := s.verifyTapReusableFD(state.tap); err != nil {
		return err
	}
	if err := s.installPoolDefaultDeny(state.tap); err != nil {
		return err
	}
	s.recordCleanStep("default_deny_ready")

	if state.legacyStatePath != "" {
		record := &StateRecord{
			Kind:        stateKind,
			Path:        state.legacyStatePath,
			State:       &state.persistedState,
			Legacy:      true,
			Fingerprint: state.legacyStateFingerprint,
		}
		deleted, err := s.store.DeleteRecordIfCurrent(record)
		if err != nil {
			return err
		}
		if !deleted {
			return fmt.Errorf("legacy state changed during live cleanup: path=%s", state.legacyStatePath)
		}
		s.recordCleanStep("legacy_state_deleted")
		if state.migrationStateRecord != nil {
			deleted, err := s.store.DeleteRecordIfCurrent(state.migrationStateRecord)
			if err != nil {
				return err
			}
			if !deleted {
				return fmt.Errorf("runtime migration state changed during live cleanup: path=%s", state.migrationStateRecord.Path)
			}
			s.recordCleanStep("migration_state_deleted")
		}
	} else if deleteRuntimeState {
		if err := s.store.DeleteStateFile(state.SandboxID, stateKind); err != nil {
			return err
		}
		s.recordCleanStep("state_deleted")
	}
	s.ports.ReleaseOwnership(state.SandboxID)
	if err := s.markTapReady(state.tap); err != nil {
		return err
	}
	s.recordCleanStep("ready")
	return nil
}

// cleanupEgressForReuse skips known no-L7 states, but treats explicit L7 state
// and legacy/no-state records as cleanup barriers. Otherwise an IP could be
// returned to the allocator while CubeEgress still holds the previous
// sandbox's secrets or policy.
func (s *NetworkController) cleanupEgressForReuse(ctx context.Context, state *persistedState, unknownPolicyState bool) error {
	if state == nil {
		return nil
	}
	if s.cubeEgressAdapter == nil || !s.cubeEgressAdapter.Configured() {
		return nil
	}
	if !unknownPolicyState && toEgressInput(state.CubeNetworkConfig) == nil {
		return nil
	}
	return s.deleteEgressForState(ctx, state.SandboxID, state.SandboxIP)
}

// startMaintenanceLoop periodically retries each durable cleanup exactly once.
// The loop itself is the retry scheduler; there are no delayed goroutines.
func (s *NetworkController) startMaintenanceLoop() {
	go func() {
		ticker := time.NewTicker(maintenanceInterval)
		defer ticker.Stop()
		for range ticker.C {
			s.handleCleaningEntries()
		}
	}()
}

// handleCleaningEntries directly retries live Cleaning TAPs and orphan durable
// records. Sandbox locks serialize it with Ensure/Release and prevent overlapping
// attempts for the same lifecycle.
func (s *NetworkController) handleCleaningEntries() {
	logger := CubeLog.WithContext(context.Background())
	records, err := s.store.Scan()
	if err != nil {
		logger.Warnf("network runtime maintenance failed to scan state dir: err=%v", err)
		return
	}
	legacyRecords, err := (legacyStateScanner{LegacyDir: s.legacyStateDir}).Scan()
	if err != nil {
		logger.Warnf("network runtime maintenance failed to scan legacy state dir: err=%v", err)
		return
	}
	records = append(records, legacyRecords...)
	_, superseded, err := s.indexRecoverableStates(records)
	if err != nil {
		logger.Errorf("network runtime maintenance found conflicting durable states; destructive cleanup skipped: err=%v", err)
		return
	}

	liveTaps, err := s.tapAdapter.List()
	if err != nil {
		logger.Warnf("network runtime maintenance failed to list taps: err=%v", err)
		return
	}

	activeOwners := make(map[string]struct{})
	processedRecords := make(map[string]struct{})
	for _, entry := range s.tapPool.Entries() {
		if entry == nil || entry.OwnerSandboxID == "" {
			continue
		}
		switch entry.State {
		case TapPoolActive:
			activeOwners[entry.OwnerSandboxID] = struct{}{}
		case TapPoolCleaning:
			record := cleanupRecordForEntry(records, entry)
			if record != nil {
				processedRecords[stateRecordIdentity(record)] = struct{}{}
			}
			s.withSandboxCleanupLock(entry.OwnerSandboxID, func() {
				if record != nil {
					if _, isSuperseded := superseded[stateRecordIdentity(record)]; isSuperseded {
						_ = s.cleanupStateOnlyOnce(record, "maintenance_superseded_cleaning_record")
						return
					}
					if record.Kind == StateFileSuccess && !record.Legacy {
						logger.Warnf("network runtime maintenance found success state on Cleaning TAP; preserving committed Active lifecycle for explicit release or restart recovery: sandbox_id=%s tap=%s", record.State.SandboxID, entry.TapName)
						return
					}
					current, currentErr := s.store.RecordCurrent(record)
					if currentErr != nil || !current {
						if currentErr != nil {
							logger.Warnf("network runtime maintenance could not validate cleanup record: path=%s err=%v", record.Path, currentErr)
						}
						return
					}
				}
				state := managedStateForCleaningEntry(entry)
				if record != nil {
					copyCleanupRecordIntoManagedState(state, record)
				} else {
					state.restoreBeforeCleanup = true
				}
				_ = s.cleanupTapOnce(state, stateKindOf(record), "maintenance_cleaning_retry")
			})
		}
	}

	for _, record := range records {
		if record == nil || record.State == nil {
			continue
		}
		if record.Kind == StateFileTmp {
			s.withSandboxCleanupLock(record.State.SandboxID, func() {
				if _, deleteErr := s.store.DeleteRecordIfCurrent(record); deleteErr != nil {
					logger.Warnf("network runtime maintenance could not retire tmp state: path=%s err=%v", record.Path, deleteErr)
				}
			})
			continue
		}
		recordID := stateRecordIdentity(record)
		if _, done := processedRecords[recordID]; done {
			continue
		}
		sandboxID := record.State.SandboxID
		if _, isSuperseded := superseded[recordID]; isSuperseded {
			s.withSandboxCleanupLock(sandboxID, func() {
				_ = s.cleanupStateOnlyOnce(record, "maintenance_superseded_state")
			})
			continue
		}
		if _, ok := activeOwners[sandboxID]; ok {
			continue
		}
		s.withSandboxCleanupLock(sandboxID, func() {
			if _, active := s.activeStateBySandboxID(sandboxID); active {
				return
			}
			tap := liveTapForState(record.State, liveTaps)
			if tap == nil {
				_ = s.cleanupStateOnlyOnce(record, "maintenance_state_only_retry")
				return
			}
			if record.Kind == StateFileSuccess && !record.Legacy {
				logger.Warnf("network runtime maintenance found unowned success state with live TAP; leaving it intact for explicit recovery: sandbox_id=%s tap=%s", sandboxID, tap.Name)
				return
			}
			managed := managedStateForCleanupRecord(record, tap)
			if err := s.beginTapCleanup(tap, sandboxID); err != nil {
				logger.Warnf("network runtime maintenance could not begin TAP cleanup: sandbox_id=%s tap=%s err=%v", sandboxID, tap.Name, err)
				return
			}
			s.closeRuntimeTapOwnership(managed)
			_ = s.cleanupTapOnce(managed, record.Kind, "maintenance_orphan_tap_retry")
		})
	}
}

func (s *NetworkController) withSandboxCleanupLock(sandboxID string, fn func()) {
	unlock := func() {}
	if s.locks != nil {
		unlock = s.locks.Lock(sandboxID)
	}
	defer unlock()
	fn()
}

func cleanupRecordForEntry(records []*StateRecord, entry *TapPoolEntry) *StateRecord {
	for _, record := range records {
		if record == nil || record.State == nil || record.Kind == StateFileTmp {
			continue
		}
		if record.State.SandboxID != entry.OwnerSandboxID {
			continue
		}
		if record.State.TapName == entry.TapName || record.State.SandboxIP == entry.SandboxIP.String() {
			return record
		}
	}
	return nil
}

func stateKindOf(record *StateRecord) StateFileKind {
	if record == nil {
		return ""
	}
	return record.Kind
}

func copyCleanupRecordIntoManagedState(state *managedState, record *StateRecord) {
	if state == nil || record == nil || record.State == nil {
		return
	}
	state.NetworkHandle = record.State.NetworkHandle
	state.Interfaces = append([]Interface(nil), record.State.Interfaces...)
	state.Routes = append([]Route(nil), record.State.Routes...)
	state.ARPNeighbors = append([]ARPNeighbor(nil), record.State.ARPNeighbors...)
	state.CubeNetworkConfig = cloneCubeNetworkConfig(record.State.CubeNetworkConfig)
	state.PersistMetadata = cloneStringMap(record.State.PersistMetadata)
	state.legacyStatePath = ""
	state.legacyStateFingerprint = ""
	state.migrationStateRecord = nil
	if record.Legacy {
		state.legacyStatePath = record.Path
		state.legacyStateFingerprint = record.Fingerprint
	} else if record.LegacyCompanion != nil {
		state.legacyStatePath = record.LegacyCompanion.Path
		state.legacyStateFingerprint = record.LegacyCompanion.Fingerprint
		state.migrationStateRecord = record
	}
	// TAP identity remains sourced from the live pool. Mapping tuples come from
	// the durable record and are conditionally deleted by CubeVS.
	state.PortMappings = append([]PortMapping(nil), record.State.PortMappings...)
	state.tap.PortMappings = append([]PortMapping(nil), record.State.PortMappings...)
}

func managedStateForCleanupRecord(record *StateRecord, tap *tapDevice) *managedState {
	state := clonePersistedState(record.State)
	state.TapName = tap.Name
	state.TapIfIndex = tap.Index
	state.SandboxIP = tap.IP.String()
	managed := &managedState{persistedState: *state, tap: cloneTapForCleanup(tap, nil)}
	managed.tap.PortMappings = append([]PortMapping(nil), state.PortMappings...)
	if record.Legacy {
		managed.legacyStatePath = record.Path
		managed.legacyStateFingerprint = record.Fingerprint
	} else if record.LegacyCompanion != nil {
		managed.legacyStatePath = record.LegacyCompanion.Path
		managed.legacyStateFingerprint = record.LegacyCompanion.Fingerprint
		managed.migrationStateRecord = record
	}
	return managed
}

func managedStateForCleaningEntry(entry *TapPoolEntry) *managedState {
	ip := append(net.IP(nil), entry.SandboxIP.To4()...)
	return &managedState{persistedState: persistedState{
		SandboxID:  entry.OwnerSandboxID,
		TapName:    entry.TapName,
		TapIfIndex: entry.TapIfIndex,
		SandboxIP:  ip.String(),
	}, tap: &tapDevice{
		Name:  entry.TapName,
		Index: entry.TapIfIndex,
		IP:    ip,
	}}
}

func (s *NetworkController) activeStateBySandboxID(sandboxID string) (*managedState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.states[sandboxID]
	return state, ok
}

func liveTapForState(state *persistedState, liveTaps map[string]*tapDevice) *tapDevice {
	if state == nil || liveTaps == nil {
		return nil
	}
	if state.SandboxIP != "" {
		if tap := liveTaps[state.SandboxIP]; tap != nil {
			return cloneTapForCleanup(tap, nil)
		}
	}
	for _, tap := range liveTaps {
		if tap == nil {
			continue
		}
		if state.TapName != "" && tap.Name == state.TapName {
			return cloneTapForCleanup(tap, nil)
		}
		if state.TapIfIndex != 0 && tap.Index == state.TapIfIndex {
			return cloneTapForCleanup(tap, nil)
		}
	}
	return nil
}

func cloneTapForCleanup(tap *tapDevice, _ *persistedState) *tapDevice {
	if tap == nil {
		return nil
	}
	ip := append(net.IP(nil), tap.IP.To4()...)
	return &tapDevice{
		Name:  tap.Name,
		Index: tap.Index,
		IP:    ip,
		InUse: tap.InUse,
		File:  tap.File,
	}
}
