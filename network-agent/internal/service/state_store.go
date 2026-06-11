// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package service

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

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

// persistedStateOnDisk is the wire layout used by stateStore.
//
// Compatibility window for the cubevs_context → cube_network_config rename:
//   - on read, both keys are accepted; cubeNetworkConfig wins when both
//     are present (cubevsContext is treated as a legacy fallback).
//   - on write, both keys are emitted with the same payload, so a rollback
//     to a binary that only knows the old key keeps working.
//
// Drop the legacy field one release after this lands.
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
	LegacyCubeVSContext *CubeNetworkConfig `json:"cubevsContext,omitempty"` // TODO: remove after one release
	PersistMetadata     map[string]string  `json:"persistMetadata"`
}

func (s *persistedState) MarshalJSON() ([]byte, error) {
	// Dual-write: emit both keys so a rollback keeps reading the state file.
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
	// New key wins; legacy key is the fallback for state files written by
	// pre-rename binaries.
	if disk.CubeNetworkConfig != nil {
		s.CubeNetworkConfig = disk.CubeNetworkConfig
	} else {
		s.CubeNetworkConfig = disk.LegacyCubeVSContext
	}
	s.PersistMetadata = disk.PersistMetadata
	return nil
}

type stateStore struct {
	dir string
}

func newStateStore(dir string) (*stateStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &stateStore{dir: dir}, nil
}

func (s *stateStore) Save(state *persistedState) error {
	if state == nil {
		return fmt.Errorf("state is nil")
	}
	p, err := s.path(state.SandboxID)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644) // NOCC:Path Traversal()
}

func (s *stateStore) Delete(sandboxID string) error {
	p, err := s.path(sandboxID)
	if err != nil {
		return err
	}
	err = os.Remove(p)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *stateStore) LoadAll() ([]*persistedState, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})
	var states []*persistedState
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		state := &persistedState{}
		if err := json.Unmarshal(data, state); err != nil {
			return nil, err
		}
		states = append(states, state)
	}
	return states, nil
}

func (s *stateStore) path(sandboxID string) (string, error) {
	if strings.ContainsAny(sandboxID, `/\.`) || sandboxID == "" {
		return "", fmt.Errorf("invalid sandboxID %q: contains path separators or traversal characters", sandboxID)
	}
	return filepath.Join(s.dir, sandboxID+".json"), nil
}
