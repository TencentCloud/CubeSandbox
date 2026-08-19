// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
	"golang.org/x/sys/unix"
)

// persistedState is the durable recovery contract for one sandbox network. It
// intentionally contains enough information to clean or restore host resources
// after a process crash without trusting in-memory maps.
type persistedState struct {
	SandboxID         string             `json:"sandboxID"`
	NetworkHandle     string             `json:"networkHandle"`
	TapName           string             `json:"tapName"`
	TapIfIndex        int                `json:"tapIfIndex"`
	SandboxIP         string             `json:"sandboxIP"`
	Interfaces        []Interface        `json:"interfaces"`
	Routes            []Route            `json:"routes"`
	ARPNeighbors      []ARPNeighbor      `json:"arpNeighbors"`
	PortMappings      []PortMapping      `json:"portMappings"`
	CubeNetworkConfig *CubeNetworkConfig `json:"-"`
	PersistMetadata   map[string]string  `json:"persistMetadata"`
}

// persistedStateOnDisk is the JSON compatibility layer. CubeNetworkConfig is
// persisted under the new name and also mirrored to cubevsContext so older tools
// that still read the legacy field continue to understand the file.
type persistedStateOnDisk struct {
	SandboxID           string             `json:"sandboxID"`
	NetworkHandle       string             `json:"networkHandle"`
	TapName             string             `json:"tapName"`
	TapIfIndex          int                `json:"tapIfIndex"`
	SandboxIP           string             `json:"sandboxIP"`
	Interfaces          []Interface        `json:"interfaces"`
	Routes              []Route            `json:"routes"`
	ARPNeighbors        []ARPNeighbor      `json:"arpNeighbors"`
	PortMappings        []PortMapping      `json:"portMappings"`
	CubeNetworkConfig   *CubeNetworkConfig `json:"cubeNetworkConfig,omitempty"`
	LegacyCubeVSContext *CubeNetworkConfig `json:"cubevsContext,omitempty"`
	PersistMetadata     map[string]string  `json:"persistMetadata"`
}

// MarshalJSON writes both the new and legacy policy field names. This keeps the
// state file self-contained for the new runtime while preserving downgrade/read
// compatibility during the transition from network-agent state files.
func (s *persistedState) MarshalJSON() ([]byte, error) {
	disk := persistedStateOnDisk{
		SandboxID:           s.SandboxID,
		NetworkHandle:       s.NetworkHandle,
		TapName:             s.TapName,
		TapIfIndex:          s.TapIfIndex,
		SandboxIP:           s.SandboxIP,
		Interfaces:          s.Interfaces,
		Routes:              s.Routes,
		ARPNeighbors:        s.ARPNeighbors,
		PortMappings:        s.PortMappings,
		CubeNetworkConfig:   s.CubeNetworkConfig,
		LegacyCubeVSContext: s.CubeNetworkConfig,
		PersistMetadata:     s.PersistMetadata,
	}
	return json.Marshal(&disk)
}

// UnmarshalJSON accepts either cubeNetworkConfig or the legacy cubevsContext
// field, preferring the new field when both are present.
func (s *persistedState) UnmarshalJSON(data []byte) error {
	var disk persistedStateOnDisk
	if err := json.Unmarshal(data, &disk); err != nil {
		return err
	}
	s.SandboxID = disk.SandboxID
	s.NetworkHandle = disk.NetworkHandle
	s.TapName = disk.TapName
	s.TapIfIndex = disk.TapIfIndex
	s.SandboxIP = disk.SandboxIP
	s.Interfaces = disk.Interfaces
	s.Routes = disk.Routes
	s.ARPNeighbors = disk.ARPNeighbors
	s.PortMappings = disk.PortMappings
	if disk.CubeNetworkConfig != nil {
		s.CubeNetworkConfig = disk.CubeNetworkConfig
	} else {
		s.CubeNetworkConfig = disk.LegacyCubeVSContext
	}
	s.PersistMetadata = disk.PersistMetadata
	return nil
}

// StateFileKind is the on-disk transaction state encoded in file names.
type StateFileKind string

const (
	// tmp: JSON has been fsynced, but no recoverable host side effects should
	// exist yet.
	StateFileTmp StateFileKind = "tmp"
	// creating: create intent is durable; recovery must clean or finish any
	// partially-applied kernel/CubeVS/CubeEgress side effects.
	StateFileCreating StateFileKind = "creating"
	// success: the sandbox network is active and should be restored on startup.
	StateFileSuccess StateFileKind = "success"
	// deleting: release intent is durable; recovery must keep cleaning until the
	// state file and runtime side effects are gone.
	StateFileDeleting StateFileKind = "deleting"
)

var allStateFileKinds = []StateFileKind{StateFileTmp, StateFileCreating, StateFileSuccess, StateFileDeleting}

// StateRecord binds a decoded state to the file kind/path it came from.
type StateRecord struct {
	Kind        StateFileKind
	Path        string
	State       *persistedState
	Legacy      bool
	Fingerprint string
	// Superseded means a higher-priority record describes the same lifecycle
	// payload. Cleanup may retire this exact file but must not touch shared
	// runtime state.
	Superseded bool
	// LegacyCompanion is an old network-agent record that must be retired before
	// this creating/deleting cleanup intent. That order prevents a crash between
	// removals from resurrecting the legacy sandbox as Active.
	LegacyCompanion *StateRecord
}

// stateStore owns the state directory and the tmp -> creating -> success ->
// deleting file-name protocol. File renames are used as transaction commits.
// State files live in 256 hash shard subdirs so concurrent sandboxes serialize
// on different directory inodes instead of the state dir root.
type stateStore struct {
	dir string
	// noSync reports that dir is on tmpfs, where fsync of files and dirs has
	// no durability effect: tmpfs survives the process crashes this protocol
	// guards against, and a host crash loses tmpfs contents anyway.
	noSync bool
}

// tmpfsMagic is the statfs f_type for tmpfs (include/linux/magic.h).
const tmpfsMagic = 0x1021994

func isTmpfs(dir string) bool {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return false
	}
	return st.Type == tmpfsMagic
}

// newStateStore ensures the directory exists before any state transaction runs.
func newStateStore(dir string) (*stateStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	s := &stateStore{dir: dir, noSync: isTmpfs(dir)}
	if err := s.normalizeFileModes(); err != nil {
		return nil, err
	}
	return s, nil
}

// normalizeFileModes enforces private permissions on every state file across
// the shard dirs.
func (s *stateStore) normalizeFileModes() error {
	rootEntries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	for _, entry := range rootEntries {
		if !entry.IsDir() || !isShardName(entry.Name()) {
			continue
		}
		shardDir := filepath.Join(s.dir, entry.Name())
		shardEntries, err := os.ReadDir(shardDir)
		if err != nil {
			return err
		}
		for _, se := range shardEntries {
			info, err := se.Info()
			if err != nil {
				return err
			}
			if info.Mode().IsRegular() {
				if err := os.Chmod(filepath.Join(shardDir, se.Name()), 0o600); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// WriteTmp validates and writes the initial state file. It is the only state
// write that serializes JSON content; later steps are atomic renames.
func (s *stateStore) WriteTmp(state *persistedState) error {
	if state == nil {
		return fmt.Errorf("state is nil")
	}
	if err := validateStateForStore(state); err != nil {
		return err
	}
	if err := s.ensureNoCommittedState(state.SandboxID); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	p, err := s.path(state.SandboxID, StateFileTmp)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	if err := s.writeStateFile(p, data, 0o600); err != nil {
		return err
	}
	return s.maybeSyncDir(filepath.Dir(p))
}

// CommitCreating atomically commits the recoverable create intent. The
// committed-state check already ran in WriteTmp under the caller's sandbox
// lock, so it is not repeated here.
func (s *stateStore) CommitCreating(sandboxID string) error {
	return s.rename(sandboxID, StateFileTmp, StateFileCreating)
}

// CommitSuccess atomically marks the sandbox network active.
func (s *stateStore) CommitSuccess(sandboxID string) error {
	return s.rename(sandboxID, StateFileCreating, StateFileSuccess)
}

// MarkDeleting atomically transfers ownership from active runtime state to the
// cleanup/recovery path.
func (s *stateStore) MarkDeleting(sandboxID string) error {
	return s.rename(sandboxID, StateFileSuccess, StateFileDeleting)
}

// DeleteStateFile removes one state file and fsyncs the directory so cleanup is
// durable across process or host crashes.
func (s *stateStore) DeleteStateFile(sandboxID string, kind StateFileKind) error {
	p, err := s.path(sandboxID, kind)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return s.maybeSyncDir(filepath.Dir(p))
}

// Load reads a state file with a known kind.
func (s *stateStore) Load(sandboxID string, kind StateFileKind) (*persistedState, error) {
	p, err := s.path(sandboxID, kind)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	state := &persistedState{}
	if err := json.Unmarshal(data, state); err != nil {
		return nil, err
	}
	return state, nil
}

// Exists checks whether a particular state file currently exists.
func (s *stateStore) Exists(sandboxID string, kind StateFileKind) bool {
	p, err := s.path(sandboxID, kind)
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

// LoadAny returns the first state file found in transaction order. It is used by
// cleanup/recovery code that only knows the sandbox owner, not the interrupted
// transaction stage.
func (s *stateStore) LoadAny(sandboxID string) (*StateRecord, error) {
	for _, kind := range allStateFileKinds {
		p, err := s.path(sandboxID, kind)
		if err != nil {
			return nil, err
		}
		data, err := os.ReadFile(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		state := &persistedState{}
		if err := json.Unmarshal(data, state); err != nil {
			return nil, err
		}
		if state.SandboxID == "" {
			state.SandboxID = sandboxID
		} else if state.SandboxID != sandboxID {
			return nil, fmt.Errorf("state filename sandboxID %q does not match payload sandboxID %q", sandboxID, state.SandboxID)
		}
		return newStateRecord(kind, p, state, false, data), nil
	}
	return nil, os.ErrNotExist
}

// Scan returns every valid state file under hash shards in deterministic
// shard-name then filename order. Invalid names are ignored so unrelated files
// in the state directory do not break runtime startup. Flat (unsharded) layout
// was never released, so root-level "*.json" state files are not scanned.
func (s *stateStore) Scan() ([]*StateRecord, error) {
	rootEntries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	sort.Slice(rootEntries, func(i, j int) bool {
		return rootEntries[i].Name() < rootEntries[j].Name()
	})
	var records []*StateRecord
	for _, entry := range rootEntries {
		if !entry.IsDir() || !isShardName(entry.Name()) {
			continue
		}
		shardDir := filepath.Join(s.dir, entry.Name())
		shardEntries, err := os.ReadDir(shardDir)
		if err != nil {
			return nil, err
		}
		recs, err := s.scanDir(shardDir, shardEntries)
		if err != nil {
			return nil, err
		}
		records = append(records, recs...)
	}
	return records, nil
}

// scanDir reads valid state files from one shard directory in deterministic
// filename order.
func (s *stateStore) scanDir(dir string, entries []os.DirEntry) ([]*StateRecord, error) {
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})
	records := make([]*StateRecord, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		sandboxID, kind, ok := parseStateFileName(entry.Name())
		if !ok {
			continue
		}
		p := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		state := &persistedState{}
		if err := json.Unmarshal(data, state); err != nil {
			// Scan is also used by the live maintenance loop. Never delete a
			// partially-read tmp file here: Ensure may still be writing it under
			// the sandbox lock. A later valid scan retires committed tmp records,
			// and a future WriteTmp safely replaces abandoned corrupt tmp bytes.
			CubeLog.WithContext(context.Background()).Errorf("network runtime skipping corrupt state file during scan: path=%s kind=%s err=%v", p, kind, err)
			continue
		}
		if state.SandboxID == "" {
			state.SandboxID = sandboxID
		} else if state.SandboxID != sandboxID {
			return nil, fmt.Errorf("state filename sandboxID %q does not match payload sandboxID %q: path=%s", sandboxID, state.SandboxID, p)
		}
		records = append(records, newStateRecord(kind, p, state, false, data))
	}
	return records, nil
}

func newStateRecord(kind StateFileKind, path string, state *persistedState, legacy bool, data []byte) *StateRecord {
	sum := sha256.Sum256(data)
	return &StateRecord{
		Kind:        kind,
		Path:        path,
		State:       state,
		Legacy:      legacy,
		Fingerprint: fmt.Sprintf("%x", sum[:]),
	}
}

func stateRecordIdentity(record *StateRecord) string {
	if record == nil {
		return ""
	}
	return fmt.Sprintf("%t:%s:%s:%s", record.Legacy, record.Kind, record.Path, record.Fingerprint)
}

// RecordCurrent verifies that record.Path still contains the exact bytes that
// produced the recovery snapshot. This is the state-only cleanup generation
// fence: a stale snapshot is a no-op rather than authority over a newer state.
func (s *stateStore) RecordCurrent(record *StateRecord) (bool, error) {
	if record == nil || record.State == nil || record.Path == "" || record.Fingerprint == "" {
		return false, fmt.Errorf("state record is missing path, state, or fingerprint")
	}
	if !record.Legacy {
		expected, err := s.path(record.State.SandboxID, record.Kind)
		if err != nil {
			return false, err
		}
		if filepath.Clean(record.Path) != filepath.Clean(expected) {
			return false, fmt.Errorf("state record path mismatch: got=%s want=%s", record.Path, expected)
		}
	}
	data, err := os.ReadFile(record.Path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	sum := sha256.Sum256(data)
	return record.Fingerprint == fmt.Sprintf("%x", sum[:]), nil
}

// DeleteRecordIfCurrent removes the exact scanned record and fsyncs its parent
// directory. It never reconstructs the path from JSON fields.
func (s *stateStore) DeleteRecordIfCurrent(record *StateRecord) (bool, error) {
	current, err := s.RecordCurrent(record)
	if err != nil || !current {
		return false, err
	}
	if err := os.Remove(record.Path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if record.Legacy {
		// Legacy records live outside this store (possibly on another
		// filesystem); always sync their parent directory.
		if err := syncDir(filepath.Dir(record.Path)); err != nil {
			return false, err
		}
		return true, nil
	}
	if err := s.maybeSyncDir(filepath.Dir(record.Path)); err != nil {
		return false, err
	}
	return true, nil
}

// ensureNoCommittedState prevents a new create transaction from overwriting an
// existing recoverable lifecycle file for the same sandbox.
func (s *stateStore) ensureNoCommittedState(sandboxID string) error {
	for _, kind := range []StateFileKind{StateFileCreating, StateFileSuccess, StateFileDeleting} {
		if s.Exists(sandboxID, kind) {
			return fmt.Errorf("state %s already exists for sandbox %s", kind, sandboxID)
		}
	}
	return nil
}

// rename performs an atomic transition between state-file kinds and fsyncs the
// directory so the rename is durable.
func (s *stateStore) rename(sandboxID string, from, to StateFileKind) error {
	fromPath, err := s.path(sandboxID, from)
	if err != nil {
		return err
	}
	toPath, err := s.path(sandboxID, to)
	if err != nil {
		return err
	}
	if err := os.Rename(fromPath, toPath); err != nil {
		return err
	}
	return s.maybeSyncDir(filepath.Dir(toPath))
}

// path constructs a safe state-file path inside the sandbox's hash shard after
// validating both the sandbox ID and the requested transaction kind.
func (s *stateStore) path(sandboxID string, kind StateFileKind) (string, error) {
	if err := validateSandboxID(sandboxID); err != nil {
		return "", err
	}
	if !isValidStateFileKind(kind) {
		return "", fmt.Errorf("invalid state file kind %q", kind)
	}
	return filepath.Join(s.dir, shardOf(sandboxID), fmt.Sprintf("%s.%s.json", sandboxID, kind)), nil
}

// shardOf maps a sandbox ID to one of 256 two-hex-digit shard directories.
func shardOf(sandboxID string) string {
	sum := sha256.Sum256([]byte(sandboxID))
	return fmt.Sprintf("%02x", sum[0])
}

// isShardName reports whether name looks like a two-hex-digit shard directory.
func isShardName(name string) bool {
	if len(name) != 2 {
		return false
	}
	for i := 0; i < 2; i++ {
		c := name[i]
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// validateStateForStore checks the minimum fields required for cleanup and
// recovery. Optional response details may be empty, but tap identity and IP may
// not.
func validateStateForStore(state *persistedState) error {
	if err := validateSandboxID(state.SandboxID); err != nil {
		return err
	}
	if state.TapName == "" {
		return fmt.Errorf("state %q missing tapName", state.SandboxID)
	}
	if state.SandboxIP == "" {
		return fmt.Errorf("state %q missing sandboxIP", state.SandboxID)
	}
	return nil
}

// validateSandboxID rejects path separators and traversal characters because
// sandbox IDs are embedded directly in state file names.
func validateSandboxID(sandboxID string) error {
	if sandboxID == "" || strings.ContainsAny(sandboxID, `/\.`) {
		return fmt.Errorf("invalid sandboxID %q: contains path separators or traversal characters", sandboxID)
	}
	return nil
}

// isValidStateFileKind reports whether kind participates in the transaction protocol.
func isValidStateFileKind(kind StateFileKind) bool {
	for _, candidate := range allStateFileKinds {
		if kind == candidate {
			return true
		}
	}
	return false
}

// parseStateFileName extracts sandbox ID and transaction kind from
// <sandboxID>.<kind>.json.
func parseStateFileName(name string) (string, StateFileKind, bool) {
	if !strings.HasSuffix(name, ".json") {
		return "", "", false
	}
	base := strings.TrimSuffix(name, ".json")
	idx := strings.LastIndexByte(base, '.')
	if idx <= 0 || idx == len(base)-1 {
		return "", "", false
	}
	sandboxID := base[:idx]
	kind := StateFileKind(base[idx+1:])
	if validateSandboxID(sandboxID) != nil || !isValidStateFileKind(kind) {
		return "", "", false
	}
	return sandboxID, kind, true
}

// writeStateFile writes data and, unless the store is on tmpfs, fsyncs the
// file before returning. Directory durability is handled by the caller after
// the file appears in the shard dir.
func (s *stateStore) writeStateFile(path string, data []byte, perm os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	var syncErr error
	if !s.noSync {
		syncErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

// maybeSyncDir fsyncs directory entry changes unless the store is on tmpfs;
// see the noSync field for why fsync is meaningless there.
func (s *stateStore) maybeSyncDir(dir string) error {
	if s.noSync {
		return nil
	}
	return syncDir(dir)
}

// syncDir fsyncs the directory entry changes created by writes, renames, and removes.
func syncDir(dir string) error {
	file, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}
