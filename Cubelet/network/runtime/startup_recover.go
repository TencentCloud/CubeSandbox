// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/cilium/ebpf"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
)

// recover reconciles durable state files with live host TAP devices after
// Cubelet restarts. Success states restore active ownership; cleanup-intent and
// state-only records each receive one synchronous cleanup attempt and remain
// durable for maintenance when that attempt fails.
func (s *NetworkController) recover() error {
	records, err := s.store.Scan()
	if err != nil {
		return err
	}
	legacyRecords, err := (legacyStateScanner{LegacyDir: s.legacyStateDir}).Scan()
	if err != nil {
		return err
	}
	records = append(records, legacyRecords...)
	for _, record := range records {
		if record == nil || record.Legacy || record.Kind != StateFileTmp {
			continue
		}
		if _, err := s.store.DeleteRecordIfCurrent(record); err != nil {
			return fmt.Errorf("retire startup tmp state %s: %w", record.Path, err)
		}
	}
	taps, err := s.tapAdapter.List()
	if err != nil {
		return err
	}
	statesByTapName, _, err := s.indexRecoverableStates(records)
	if err != nil {
		return err
	}
	processed := make(map[string]struct{}, len(records))
	for _, tap := range taps {
		record := statesByTapName[tap.Name]
		if record == nil && tap.IP != nil {
			record = statesByTapName[tap.IP.String()]
		}
		if record == nil && tap.Index != 0 {
			record = statesByTapName[fmt.Sprintf("ifindex:%d", tap.Index)]
		}
		if err := s.recoverLiveTap(tap, record, processed); err != nil {
			return err
		}
	}
	for _, record := range records {
		if err := s.recoverStateWithoutTap(record, processed); err != nil {
			return err
		}
	}
	return nil
}

// runStaleNetPolicyMapGC deletes allow_out_v2 / deny_out / dns_allow outer keys
// for ifindexes that are not in the live TAP set / pool. Normal TAP teardown
// removes these keys, so the healthy startup path only scans the three outer
// maps and deletes nothing. Cleanup can be slow when failed/bypassed teardown
// has left thousands of stale inners; that exceptional recovery work runs
// synchronously during controller startup.
//
// startControllerRuntime invokes this after recover has registered every live
// TAP, but before the DNS reaper, background pool warmup, and request handling
// can create or use another TAP lifecycle. The stillPresent/onConflict callbacks
// remain as defensive checks for host-netdev changes outside this controller.
func (s *NetworkController) runStaleNetPolicyMapGC() {
	logger := CubeLog.WithContext(context.Background())
	keep, err := s.buildStaleNetPolicyKeepSet()
	if err != nil {
		logger.Warnf(
			"network runtime stale policy map gc: list taps failed, skip gc: %v",
			err,
		)
		return
	}
	deleted, err := s.cubevsAdapter.GCStaleNetPolicyMaps(
		keep,
		netPolicyIfindexStillPresent,
		s.restoreDefaultDenyAfterStaleGCConflict,
	)
	if err != nil {
		logger.Warnf(
			"network runtime stale policy map gc: keep=%d deleted=%d err=%v",
			len(keep), deleted, err,
		)
		return
	}
	logger.Infof(
		"network runtime stale policy map gc: keep=%d deleted=%d",
		len(keep), deleted,
	)
}

// restoreDefaultDenyAfterStaleGCConflict reinstalls reusable-pool default deny
// when GC deleted outer keys for an ifindex that became live again (create
// raced between stillPresent and outer.Delete).
func (s *NetworkController) restoreDefaultDenyAfterStaleGCConflict(ifindex uint32) {
	if err := s.cubevsAdapter.InstallTAPDefaultDenyPolicy(ifindex); err != nil {
		CubeLog.WithContext(context.Background()).Warnf(
			"network runtime stale policy map gc: restore default deny failed: ifindex=%d err=%v",
			ifindex, err,
		)
		return
	}
	CubeLog.WithContext(context.Background()).Warnf(
		"network runtime stale policy map gc: restored default deny after create race: ifindex=%d",
		ifindex,
	)
}

// buildStaleNetPolicyKeepSet returns ifindexes that must not have their
// HashOfMaps outer policy keys deleted: every live Cube TAP plus every pool
// entry (Ready/Cleaning/Active). Listing live TAPs is required; an incomplete
// keep set would let GC treat live ifindexes as stale.
func (s *NetworkController) buildStaleNetPolicyKeepSet() (map[uint32]struct{}, error) {
	keep := make(map[uint32]struct{})
	taps, err := s.tapAdapter.List()
	if err != nil {
		return nil, err
	}
	for _, tap := range taps {
		if tap != nil && tap.Index > 0 {
			keep[uint32(tap.Index)] = struct{}{}
		}
	}
	if s.tapPool != nil {
		for _, entry := range s.tapPool.Entries() {
			if entry != nil && entry.TapIfIndex > 0 {
				keep[uint32(entry.TapIfIndex)] = struct{}{}
			}
		}
	}
	return keep, nil
}

// netPolicyIfindexStillPresent reports whether a host netdev still occupies
// ifindex. Transient netlink errors are treated as present so GC prefers a
// temporary leak over wiping deny_out for a live TAP.
func netPolicyIfindexStillPresent(ifindex uint32) bool {
	_, err := netlinkLinkByIndex(int(ifindex))
	if err == nil {
		return true
	}
	if isTapNotFound(err) {
		return false
	}
	return true
}

func (s *NetworkController) indexRecoverableStates(records []*StateRecord) (map[string]*StateRecord, map[string]struct{}, error) {
	statesByTapNameOrIP := make(map[string]*StateRecord, len(records)*2)
	statesBySandboxID := make(map[string]*StateRecord, len(records))
	legacyBySandboxID := make(map[string]*StateRecord)
	identityOwnerByKey := make(map[string]*StateRecord, len(records)*2)
	superseded := make(map[string]struct{})
	ordered := make([]*StateRecord, 0, len(records))
	for _, record := range records {
		if record != nil {
			record.Superseded = false
			record.LegacyCompanion = nil
			if record.Legacy && record.State != nil {
				existing := legacyBySandboxID[record.State.SandboxID]
				if existing != nil && existing != record {
					return nil, nil, fmt.Errorf(
						"multiple legacy states claim sandbox %s: %s and %s",
						record.State.SandboxID, existing.Path, record.Path,
					)
				}
				legacyBySandboxID[record.State.SandboxID] = record
			}
			if record.State != nil && (record.Legacy || record.Kind != StateFileTmp) {
				for _, key := range recoveryRecordKeys(record) {
					if key == "" {
						continue
					}
					existing := identityOwnerByKey[key]
					if existing != nil && existing != record &&
						existing.State.SandboxID != record.State.SandboxID {
						return nil, nil, fmt.Errorf(
							"network identity %q is claimed by different sandboxes: sandbox=%s path=%s and sandbox=%s path=%s",
							key, existing.State.SandboxID, existing.Path, record.State.SandboxID, record.Path,
						)
					}
					if existing == nil {
						identityOwnerByKey[key] = record
					}
				}
			}
			ordered = append(ordered, record)
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		left, right := recoveryRecordPriority(ordered[i]), recoveryRecordPriority(ordered[j])
		if left != right {
			return left > right
		}
		return ordered[i].Path < ordered[j].Path
	})
	for _, record := range ordered {
		if record == nil || record.State == nil {
			continue
		}
		if !record.Legacy && record.Kind == StateFileTmp {
			// tmp is not a recoverable resource owner. Startup/maintenance retire
			// the exact scanned file separately, under the sandbox lock where needed.
			continue
		}
		existing := statesBySandboxID[record.State.SandboxID]
		if existing != nil && existing != record {
			if isNewSuccessRecord(existing) && isNewSuccessRecord(record) {
				return nil, nil, fmt.Errorf("two success states claim sandbox %s: %s and %s", record.State.SandboxID, existing.Path, record.Path)
			}
			if !lifecycleRecordsCanSupersede(existing, record) {
				return nil, nil, fmt.Errorf(
					"conflicting lifecycle states claim sandbox %s with different cleanup or active payloads: kind=%s path=%s and kind=%s path=%s",
					record.State.SandboxID, existing.Kind, existing.Path, record.Kind, record.Path,
				)
			}
			record.Superseded = true
			superseded[stateRecordIdentity(record)] = struct{}{}
			continue
		}
		keys := recoveryRecordKeys(record)
		for _, key := range keys {
			if key == "" {
				continue
			}
			existing := statesByTapNameOrIP[key]
			if existing == nil || existing == record {
				continue
			}
			// Transaction phases for one sandbox may supersede each other, but
			// records owned by different sandboxes are never interchangeable.
			// In particular, silently retiring a stale deleting record here can
			// discard the only evidence of an old CubeEgress policy keyed by the
			// shared IP. Preserve both records and fail closed instead.
			return nil, nil, fmt.Errorf(
				"network identity %q is claimed by different sandboxes: sandbox=%s path=%s and sandbox=%s path=%s",
				key, existing.State.SandboxID, existing.Path, record.State.SandboxID, record.Path,
			)
		}
		statesBySandboxID[record.State.SandboxID] = record
		for _, key := range keys {
			if key != "" {
				statesByTapNameOrIP[key] = record
			}
		}
	}
	// Migration intentionally keeps legacy state until the new lifecycle has
	// crossed its success boundary. Pair an interrupted creating/deleting intent
	// with the matching legacy record so cleanup retires legacy first. Otherwise a
	// crash after deleting only the new intent could restore legacy as Active.
	for _, record := range ordered {
		if record == nil || !record.Legacy || !record.Superseded || record.State == nil {
			continue
		}
		winner := statesBySandboxID[record.State.SandboxID]
		if isLegacyCleanupPair(winner, record) {
			if winner.LegacyCompanion != nil && winner.LegacyCompanion != record {
				return nil, nil, fmt.Errorf(
					"multiple legacy states accompany cleanup intent for sandbox %s: %s and %s",
					record.State.SandboxID, winner.LegacyCompanion.Path, record.Path,
				)
			}
			winner.LegacyCompanion = record
		}
	}
	return statesByTapNameOrIP, superseded, nil
}

// lifecycleRecordsCanSupersede permits only records that describe the same
// lifecycle to collapse by phase priority. New lifecycle phases are atomic
// renames, so their bytes must be identical. Legacy JSON has a different wire
// shape; compare only the host-side contract that recovery relies on.
func lifecycleRecordsCanSupersede(winner, candidate *StateRecord) bool {
	if winner == nil || winner.State == nil || candidate == nil || candidate.State == nil {
		return false
	}
	if winner.State.SandboxID == "" || winner.State.SandboxID != candidate.State.SandboxID {
		return false
	}
	if !winner.Legacy && !candidate.Legacy {
		return winner.Fingerprint != "" && winner.Fingerprint == candidate.Fingerprint
	}
	if winner.Legacy == candidate.Legacy {
		return false
	}

	var runtimeRecord, legacyRecord *StateRecord
	if winner.Legacy {
		legacyRecord, runtimeRecord = winner, candidate
	} else {
		runtimeRecord, legacyRecord = winner, candidate
	}
	if !sameLegacyRuntimeIdentity(runtimeRecord.State, legacyRecord.State) {
		return false
	}
	// Interrupted creating/deleting only needs the same TAP/IP identity. Cleanup
	// will wipe residue and treat CubeEgress as unknown; requiring identical port
	// tuples here blocked real migration pairs (and empty creating intents).
	if runtimeRecord.Kind == StateFileCreating || runtimeRecord.Kind == StateFileDeleting {
		return true
	}
	if runtimeRecord.Kind != StateFileSuccess {
		return false
	}
	// Success restores Active, so port mappings and policy must match before the
	// legacy file can be retired without a cleanup pass.
	return samePortMappingSet(runtimeRecord.State.PortMappings, legacyRecord.State.PortMappings) &&
		reflect.DeepEqual(runtimeRecord.State.CubeNetworkConfig, legacyRecord.State.CubeNetworkConfig)
}

func sameLegacyRuntimeIdentity(runtimeState, legacyState *persistedState) bool {
	if runtimeState == nil || legacyState == nil {
		return false
	}
	if runtimeState.SandboxID == "" || runtimeState.SandboxID != legacyState.SandboxID ||
		runtimeState.TapName == "" || runtimeState.TapName != legacyState.TapName ||
		runtimeState.SandboxIP == "" || runtimeState.SandboxIP != legacyState.SandboxIP {
		return false
	}
	return runtimeState.TapIfIndex == 0 || legacyState.TapIfIndex == 0 ||
		runtimeState.TapIfIndex == legacyState.TapIfIndex
}

func samePortMappingSet(left, right []PortMapping) bool {
	if len(left) != len(right) {
		return false
	}
	counts := make(map[PortMapping]int, len(left))
	for _, mapping := range left {
		counts[normalizePortMapping(mapping)]++
	}
	for _, mapping := range right {
		key := normalizePortMapping(mapping)
		if counts[key] == 0 {
			return false
		}
		counts[key]--
	}
	return true
}

// normalizePortMapping treats empty protocol as tcp so legacy JSON and new
// runtime state that omit the field still compare as the same host contract.
func normalizePortMapping(mapping PortMapping) PortMapping {
	if mapping.Protocol == "" {
		mapping.Protocol = "tcp"
	}
	return mapping
}

func isLegacyCleanupPair(runtimeRecord, legacyRecord *StateRecord) bool {
	if runtimeRecord == nil || runtimeRecord.State == nil || runtimeRecord.Legacy ||
		legacyRecord == nil || legacyRecord.State == nil || !legacyRecord.Legacy {
		return false
	}
	if runtimeRecord.Kind != StateFileCreating && runtimeRecord.Kind != StateFileDeleting {
		return false
	}
	return runtimeRecord.State.SandboxID == legacyRecord.State.SandboxID &&
		runtimeRecord.State.TapName != "" &&
		runtimeRecord.State.TapName == legacyRecord.State.TapName &&
		runtimeRecord.State.SandboxIP != "" &&
		runtimeRecord.State.SandboxIP == legacyRecord.State.SandboxIP
}

func recoveryRecordPriority(record *StateRecord) int {
	if record == nil {
		return -1
	}
	if record.Legacy {
		return 0
	}
	switch record.Kind {
	case StateFileSuccess:
		return 30
	case StateFileDeleting:
		return 20
	case StateFileCreating:
		return 10
	default:
		return -1
	}
}

func isNewSuccessRecord(record *StateRecord) bool {
	return record != nil && !record.Legacy && record.Kind == StateFileSuccess
}

func recoveryRecordKeys(record *StateRecord) []string {
	if record == nil || record.State == nil {
		return nil
	}
	keys := []string{record.State.TapName, record.State.SandboxIP}
	if record.State.TapIfIndex != 0 {
		keys = append(keys, fmt.Sprintf("ifindex:%d", record.State.TapIfIndex))
	}
	return keys
}

func (s *NetworkController) recoverLiveTap(tap *tapDevice, record *StateRecord, processed map[string]struct{}) error {
	s.allocator.Assign(tap.IP)
	if record == nil {
		return s.recoverReadyTapWithoutState(tap)
	}
	processed[stateRecordIdentity(record)] = struct{}{}
	if record.Legacy {
		return s.recoverLegacyLiveTap(tap, record)
	}
	switch record.Kind {
	case StateFileSuccess:
		return s.recoverActiveTap(tap, record)
	case StateFileCreating, StateFileDeleting:
		return s.recoverTapCleanup(tap, record)
	default:
		return nil
	}
}

func (s *NetworkController) recoverReadyTapWithoutState(tap *tapDevice) error {
	// During smooth upgrades a no-state TAP may still carry old CubeVS port maps,
	// TAP policy, metadata, or CubeEgress rules. Hold it in Cleaning and let the
	// normal TapCleaner retry path prove it is clean before publishing Ready.
	owner := "nostate-" + strings.NewReplacer(".", "-", "/", "-", "\\", "-").Replace(tap.Name)
	cleaningTap := &tapDevice{
		Index: tap.Index,
		Name:  tap.Name,
		IP:    append(net.IP(nil), tap.IP.To4()...),
		InUse: tap.InUse,
		File:  tap.File,
	}
	managed := &managedState{persistedState: persistedState{
		SandboxID:     owner,
		NetworkHandle: owner,
		TapName:       cleaningTap.Name,
		TapIfIndex:    cleaningTap.Index,
		SandboxIP:     cleaningTap.IP.String(),
	}, tap: cleaningTap, restoreBeforeCleanup: true}
	if err := s.beginTapCleanup(cleaningTap, owner); err != nil {
		return err
	}
	s.closeRuntimeTapOwnership(managed)
	if err := s.cleanupTapOnce(managed, "", "recover_no_state_tap"); err != nil {
		CubeLog.WithContext(context.Background()).Warnf("network runtime no-state TAP remains Cleaning after startup cleanup attempt: tap=%s err=%v", tap.Name, err)
	}
	return nil
}

func (s *NetworkController) recoverLegacyLiveTap(tap *tapDevice, record *StateRecord) error {
	if err := validateLegacyTapMatch(tap, record.State); err != nil {
		CubeLog.WithContext(context.Background()).Warnf("network runtime legacy state cannot restore active; cleaning synchronously: sandbox_id=%s tap=%s err=%v", record.State.SandboxID, tap.Name, err)
		return s.recoverLegacyTapCleanup(tap, record, nil, "recover_legacy_mismatch")
	}
	runtimeCleanupRecord, cleanupSafe, err := s.restoreLegacyActiveTap(tap, record)
	if err != nil {
		if !cleanupSafe {
			// A new runtime success may already be durable. Abort startup and let
			// the next recovery select that higher-priority record; never clean a
			// TAP after crossing the new success commit boundary.
			CubeLog.WithContext(context.Background()).Errorf("network runtime legacy migration failed after new success commit boundary; refusing TAP cleanup: sandbox_id=%s tap=%s err=%v", record.State.SandboxID, tap.Name, err)
			return err
		}
		CubeLog.WithContext(context.Background()).Warnf("network runtime legacy state replay failed before commit; cleaning synchronously: sandbox_id=%s tap=%s err=%v", record.State.SandboxID, tap.Name, err)
		return s.recoverLegacyTapCleanup(tap, record, runtimeCleanupRecord, "recover_legacy_replay_failed")
	}
	return nil
}

func validateLegacyTapMatch(tap *tapDevice, state *persistedState) error {
	if tap == nil || state == nil {
		return fmt.Errorf("tap or state is nil")
	}
	if state.SandboxID == "" || state.TapName == "" || state.SandboxIP == "" {
		return fmt.Errorf("legacy state missing required identity")
	}
	if tap.Name != state.TapName {
		return fmt.Errorf("tap name mismatch: live=%s state=%s", tap.Name, state.TapName)
	}
	if tap.IP == nil || tap.IP.String() != state.SandboxIP {
		return fmt.Errorf("tap ip mismatch: live=%s state=%s", tap.IP.String(), state.SandboxIP)
	}
	return nil
}

// restoreLegacyActiveTap commits a new-runtime creating intent before replaying
// any side effect. It returns that exact record when a pre-success failure may be
// cleaned together with legacy. cleanupSafe becomes false whenever the durable
// phase cannot be proved or a new runtime success may already exist.
func (s *NetworkController) restoreLegacyActiveTap(tap *tapDevice, record *StateRecord) (*StateRecord, bool, error) {
	state := clonePersistedState(record.State)
	state.TapIfIndex = tap.Index
	state.TapName = tap.Name
	state.SandboxIP = tap.IP.String()
	state.NetworkHandle = nonEmpty(state.NetworkHandle, state.SandboxID)
	if state.PersistMetadata == nil {
		state.PersistMetadata = s.persistMetadata(nil, state.TapName, state.SandboxIP)
	}
	managed := &managedState{persistedState: *state, tap: tap}
	if err := s.store.WriteTmp(&managed.persistedState); err != nil {
		return nil, true, err
	}
	if err := s.store.CommitCreating(managed.SandboxID); err != nil {
		current, currentErr := s.store.LoadAny(managed.SandboxID)
		if currentErr != nil {
			if errors.Is(currentErr, os.ErrNotExist) {
				return nil, true, err
			}
			return nil, false, errors.Join(err, fmt.Errorf("inspect legacy migration state after creating commit failure: %w", currentErr))
		}
		switch current.Kind {
		case StateFileCreating:
			return current, true, err
		case StateFileSuccess, StateFileDeleting:
			return nil, false, err
		case StateFileTmp:
			deleted, deleteErr := s.store.DeleteRecordIfCurrent(current)
			if deleteErr != nil || !deleted {
				return nil, false, errors.Join(err, deleteErr, fmt.Errorf("could not retire failed legacy migration tmp state: path=%s", current.Path))
			}
			return nil, true, err
		default:
			return nil, false, err
		}
	}
	creatingRecord, err := s.store.LoadAny(managed.SandboxID)
	if err != nil {
		return nil, false, fmt.Errorf("load committed legacy migration creating state: %w", err)
	}
	if creatingRecord.Kind != StateFileCreating {
		return nil, false, fmt.Errorf("legacy migration expected creating state, found %s at %s", creatingRecord.Kind, creatingRecord.Path)
	}
	if err := s.restoreRecoveredTap(managed); err != nil {
		return creatingRecord, true, err
	}
	if err := s.replaceCubeVSTap(managed.TapIfIndex, net.ParseIP(managed.SandboxIP).To4(), managed.SandboxID, managed.CubeNetworkConfig); err != nil {
		return creatingRecord, true, err
	}
	if ip := net.ParseIP(managed.SandboxIP).To4(); ip != nil {
		s.allocator.Assign(ip)
	}
	if err := s.ports.AssignOwner(managed.SandboxID, managed.PortMappings); err != nil {
		return creatingRecord, true, err
	}
	managed.tap.PortMappings = append([]PortMapping(nil), managed.PortMappings...)
	if err := s.reconcilePortMappingsForTap(managed.tap, managed.PortMappings); err != nil {
		return creatingRecord, true, err
	}
	if err := s.store.CommitSuccess(managed.SandboxID); err != nil {
		// Rename may have committed even when the directory fsync failed. Inspect
		// the exact current phase before deciding whether cleanup is still legal.
		current, currentErr := s.store.LoadAny(managed.SandboxID)
		if currentErr != nil {
			if errors.Is(currentErr, os.ErrNotExist) {
				return nil, true, err
			}
			return nil, false, errors.Join(err, fmt.Errorf("inspect legacy migration state after success commit failure: %w", currentErr))
		}
		if current.Kind == StateFileCreating {
			return current, true, err
		}
		return nil, false, err
	}
	entry, err := tapPoolEntryFromDevice(managed.tap, managed.SandboxID, TapPoolActive)
	if err != nil {
		return nil, false, err
	}
	if err := s.tapPool.Add(entry); err != nil {
		return nil, false, err
	}
	s.states[managed.SandboxID] = managed
	if err := deleteLegacyStateFile(record.Path); err != nil {
		CubeLog.WithContext(context.Background()).Warnf(
			"network runtime legacy migration committed new success but old state retirement is pending: sandbox_id=%s path=%s err=%v",
			managed.SandboxID, record.Path, err,
		)
	}
	return nil, false, nil
}

func (s *NetworkController) recoverLegacyTapCleanup(tap *tapDevice, record, runtimeCleanupRecord *StateRecord, reason string) error {
	cleaningTap := &tapDevice{
		Index: tap.Index,
		Name:  tap.Name,
		IP:    append(net.IP(nil), tap.IP.To4()...),
		InUse: tap.InUse,
		File:  tap.File,
	}
	state := clonePersistedState(record.State)
	managed := &managedState{
		persistedState:         *state,
		tap:                    cleaningTap,
		legacyStatePath:        record.Path,
		legacyStateFingerprint: record.Fingerprint,
		migrationStateRecord:   runtimeCleanupRecord,
	}
	cleaningTap.PortMappings = append([]PortMapping(nil), state.PortMappings...)
	managed.TapIfIndex = cleaningTap.Index
	managed.TapName = cleaningTap.Name
	managed.SandboxIP = cleaningTap.IP.String()
	if err := s.beginTapCleanup(cleaningTap, managed.SandboxID); err != nil {
		return err
	}
	s.closeRuntimeTapOwnership(managed)
	if err := s.cleanupTapOnce(managed, record.Kind, reason); err != nil {
		CubeLog.WithContext(context.Background()).Warnf("network runtime legacy TAP remains Cleaning after startup cleanup attempt: sandbox_id=%s tap=%s err=%v", managed.SandboxID, managed.TapName, err)
	}
	return nil
}

func (s *NetworkController) recoverActiveTap(tap *tapDevice, record *StateRecord) error {
	// success records are active networks. Rebuild process-local ownership and
	// datapath state before exposing the network again.
	tap.PortMappings = append([]PortMapping(nil), record.State.PortMappings...)
	managed := &managedState{persistedState: *record.State, tap: tap}
	if err := s.restoreRecoveredTap(managed); err != nil {
		return fmt.Errorf("restore active TAP for sandbox %s: %w", managed.SandboxID, err)
	}
	if err := s.restoreRecoveredCubeVSMetadata(managed); err != nil {
		return fmt.Errorf("restore CubeVS metadata for active sandbox %s: %w", managed.SandboxID, err)
	}
	if err := s.claimRecoveredSuccessResources(managed); err != nil {
		return fmt.Errorf("claim resources for active sandbox %s: %w", managed.SandboxID, err)
	}
	if managed.PersistMetadata == nil {
		managed.PersistMetadata = s.persistMetadata(nil, managed.TapName, managed.SandboxIP)
	}
	entry, err := tapPoolEntryFromDevice(managed.tap, managed.SandboxID, TapPoolActive)
	if err != nil {
		return err
	}
	if err := s.tapPool.Add(entry); err != nil {
		return err
	}
	s.states[managed.SandboxID] = managed
	return nil
}

func (s *NetworkController) recoverTapCleanup(tap *tapDevice, record *StateRecord) error {
	// creating/deleting records represent interrupted transactions. Recovery
	// moves the TAP to Cleaning and performs one bounded cleanup attempt.
	cleaningTap := &tapDevice{
		Index: tap.Index,
		Name:  tap.Name,
		IP:    append(net.IP(nil), tap.IP.To4()...),
		InUse: tap.InUse,
		File:  tap.File,
	}
	state := clonePersistedState(record.State)
	state.TapIfIndex = cleaningTap.Index
	state.TapName = cleaningTap.Name
	state.SandboxIP = cleaningTap.IP.String()
	managed := &managedState{persistedState: *state, tap: cleaningTap}
	if record.LegacyCompanion != nil {
		managed.legacyStatePath = record.LegacyCompanion.Path
		managed.legacyStateFingerprint = record.LegacyCompanion.Fingerprint
		managed.migrationStateRecord = record
	}
	cleaningTap.PortMappings = append([]PortMapping(nil), state.PortMappings...)
	if err := s.beginTapCleanup(cleaningTap, managed.SandboxID); err != nil {
		return err
	}
	s.closeRuntimeTapOwnership(managed)
	if err := s.cleanupTapOnce(managed, record.Kind, "recover_"+string(record.Kind)); err != nil {
		CubeLog.WithContext(context.Background()).Warnf("network runtime interrupted TAP remains Cleaning after startup cleanup attempt: sandbox_id=%s tap=%s kind=%s err=%v", managed.SandboxID, managed.TapName, record.Kind, err)
	}
	return nil
}

func (s *NetworkController) recoverStateWithoutTap(record *StateRecord, processed map[string]struct{}) error {
	if record.Kind == StateFileTmp {
		return nil
	}
	if _, ok := processed[stateRecordIdentity(record)]; ok {
		return nil
	}
	if record.Superseded {
		if err := s.cleanupStateOnlyOnce(record, "recover_superseded_state"); err != nil {
			CubeLog.WithContext(context.Background()).Warnf("network runtime superseded state remains pending: sandbox_id=%s path=%s err=%v", record.State.SandboxID, record.Path, err)
		}
		return nil
	}
	if ip := net.ParseIP(record.State.SandboxIP).To4(); ip != nil {
		s.allocator.Assign(ip)
	}
	if err := s.cleanupStateOnlyOnce(record, "recover_state_only"); err != nil {
		CubeLog.WithContext(context.Background()).Warnf(
			"network runtime state-only record remains pending after startup cleanup attempt: sandbox_id=%s path=%s err=%v",
			record.State.SandboxID, record.Path, err,
		)
	}
	return nil
}

func (s *NetworkController) restoreRecoveredTap(state *managedState) error {
	if state == nil {
		return fmt.Errorf("state is nil")
	}
	if err := s.ensureHostRoute(); err != nil {
		return err
	}
	baseTap := state.tap
	if baseTap == nil {
		baseTap = &tapDevice{
			Name: state.TapName,
			IP:   net.ParseIP(state.SandboxIP).To4(),
		}
	}
	baseTap.PortMappings = append([]PortMapping(nil), state.PortMappings...)
	tap, err := s.tapAdapter.Restore(baseTap, s.cfg.MvmMtu, s.cfg.MVMMacAddr, s.cubeDev.Index)
	if err != nil {
		return err
	}
	state.tap = tap
	state.TapIfIndex = tap.Index
	state.TapName = tap.Name
	state.SandboxIP = tap.IP.String()
	return nil
}

func (s *NetworkController) restoreRecoveredCubeVSMetadata(state *managedState) error {
	if state == nil {
		return nil
	}
	device, err := s.cubevsAdapter.GetTAPDevice(uint32(state.TapIfIndex))
	if err != nil {
		if errors.Is(err, ebpf.ErrKeyNotExist) {
			CubeLog.WithContext(context.Background()).Warnf("network runtime recover cubevs tap metadata missing, restoring metadata only: sandbox_id=%s tap=%s ifindex=%d", state.SandboxID, state.TapName, state.TapIfIndex)
			return s.upsertCubeVSTapMeta(state.TapIfIndex, net.ParseIP(state.SandboxIP).To4(), state.SandboxID)
		}
		return fmt.Errorf("lookup cubevs TAP metadata for %s(%d): %w", state.TapName, state.TapIfIndex, err)
	}
	if device == nil || device.ID != state.SandboxID || device.Ifindex != state.TapIfIndex || !device.IP.Equal(net.ParseIP(state.SandboxIP)) {
		return fmt.Errorf("cubevs TAP metadata identity mismatch for sandbox %s: got=%#v want_ifindex=%d want_ip=%s", state.SandboxID, device, state.TapIfIndex, state.SandboxIP)
	}
	return nil
}

func (s *NetworkController) claimRecoveredSuccessResources(state *managedState) error {
	if state == nil {
		return nil
	}
	if ip := net.ParseIP(state.SandboxIP).To4(); ip != nil {
		s.allocator.Assign(ip)
	}
	if err := s.ports.AssignOwner(state.SandboxID, state.PortMappings); err != nil {
		return err
	}
	state.tap.PortMappings = append([]PortMapping(nil), state.PortMappings...)
	return s.reconcilePortMappingsForTap(state.tap, state.PortMappings)
}

// cleanupConflictingTap destroys a stale host tap that collides with a freshly
// allocated IP. It must be called without holding s.mu: the netlink lookup and
// the destroy syscall run lock-free, while the membership checks against the
// in-memory collections are performed under s.mu. The IP is already exclusively
// owned by the caller (handed out by the allocator) and tap names derive
// uniquely from the IP, so no other goroutine can re-reference this tap between
// the check and the destroy.
//
// Lookup is by deterministic tap name (RTM_GETLINK), not LinkList. A full dump
// of every host interface on every pool-miss create races with concurrent TAP
// churn and surfaces as netlink ErrDumpInterrupted under density load.
func (s *NetworkController) cleanupConflictingTap(ip net.IP) error {
	tap, err := s.tapAdapter.GetByName(tapName(ip.String()))
	if err != nil {
		if isTapNotFound(err) {
			return nil
		}
		return err
	}
	if tap == nil {
		return nil
	}
	if err := s.checkTapConflict(tap, ip); err != nil {
		return err
	}
	if err := s.tapAdapter.Destroy(tap.Index); err != nil {
		return fmt.Errorf("destroy stale tap %s(%d): %w", tap.Name, tap.Index, err)
	}
	s.dropPooledTapFD(tap.Name)
	return nil
}

// checkTapConflict reports an error if the given tap is still referenced by any
// in-memory collection (active states or any of the pools). It acquires s.mu
// itself for the duration of the scan, so callers must NOT already hold it
// (hence no "Locked" suffix, which would imply the opposite).
func (s *NetworkController) checkTapConflict(tap *tapDevice, ip net.IP) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, state := range s.states {
		if state.TapName == tap.Name || state.SandboxIP == ip.String() {
			return fmt.Errorf("tap %s(%d) is still allocated to sandbox %s", tap.Name, tap.Index, state.SandboxID)
		}
	}
	for _, entry := range s.tapPool.Entries() {
		if entry != nil && (entry.TapName == tap.Name || entry.SandboxIP.Equal(ip)) {
			return fmt.Errorf("tap %s(%d) is already managed by TapPool state=%s", tap.Name, tap.Index, entry.State)
		}
	}
	return nil
}

// legacyStateScanner reads old network-agent state files as recovery inputs.
// It never imports them as new success files up front because legacy JSON has no
// creating/deleting/success transaction state.
type legacyStateScanner struct {
	LegacyDir string
}

func (s legacyStateScanner) Scan() ([]*StateRecord, error) {
	if s.LegacyDir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(s.LegacyDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	records := make([]*StateRecord, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		legacyPath := filepath.Join(s.LegacyDir, entry.Name())
		data, err := os.ReadFile(legacyPath)
		if err != nil {
			CubeLog.WithContext(context.Background()).Errorf("network runtime failed to read legacy state file during scan: path=%s err=%v", legacyPath, err)
			continue
		}
		state := &persistedState{}
		if unmarshalErr := json.Unmarshal(data, state); unmarshalErr != nil {
			CubeLog.WithContext(context.Background()).Errorf(
				"network runtime skipping corrupt legacy state file during scan: path=%s err=%v",
				legacyPath, unmarshalErr,
			)
			continue
		}
		if state.SandboxID == "" {
			state.SandboxID = strings.TrimSuffix(entry.Name(), ".json")
		}
		if state.NetworkHandle == "" {
			state.NetworkHandle = state.SandboxID
		}
		records = append(records, newStateRecord(StateFileSuccess, legacyPath, state, true, data))
	}
	return records, nil
}

func (s *NetworkController) hasPendingLegacyState(sandboxID string) (bool, error) {
	if s == nil || s.legacyStateDir == "" || sandboxID == "" {
		return false, nil
	}
	if validateSandboxID(sandboxID) == nil {
		exactPath := filepath.Join(s.legacyStateDir, sandboxID+".json")
		if _, err := os.Stat(exactPath); err == nil {
			return true, nil
		} else if !os.IsNotExist(err) {
			return false, err
		}
	}
	records, err := (legacyStateScanner{LegacyDir: s.legacyStateDir}).Scan()
	if err != nil {
		return false, err
	}
	for _, record := range records {
		if record != nil && record.State != nil && record.State.SandboxID == sandboxID {
			return true, nil
		}
	}
	return false, nil
}

// readLegacyState decodes the old network-agent JSON shape. persistedState's
// custom UnmarshalJSON handles both cubeNetworkConfig and the legacy
// cubevsContext field.
func readLegacyState(path string) (*persistedState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	state := &persistedState{}
	if err := json.Unmarshal(data, state); err != nil {
		return nil, fmt.Errorf("decode legacy state %s: %w", path, err)
	}
	return state, nil
}

func deleteLegacyStateFile(path string) error {
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func clonePersistedState(in *persistedState) *persistedState {
	if in == nil {
		return nil
	}
	out := *in
	out.Interfaces = append([]Interface(nil), in.Interfaces...)
	out.Routes = append([]Route(nil), in.Routes...)
	out.ARPNeighbors = append([]ARPNeighbor(nil), in.ARPNeighbors...)
	out.PortMappings = append([]PortMapping(nil), in.PortMappings...)
	out.CubeNetworkConfig = cloneCubeNetworkConfig(in.CubeNetworkConfig)
	out.PersistMetadata = cloneStringMap(in.PersistMetadata)
	return &out
}
