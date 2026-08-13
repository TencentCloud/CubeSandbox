// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package recovery

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

const currentPersistVersion = 2

// persistState is the on-disk representation of the recovery manager's
// in-memory state. It is written atomically (temp file + rename) whenever
// the state changes, and loaded on startup so the webhook can recover
// after a restart.
type persistState struct {
	Version          int                        `json:"version,omitempty"`
	Paused           map[string][]PausedSandbox `json:"paused"`
	Isolated         map[string]string          `json:"isolated"`
	DesiredProtected map[string]bool            `json:"desired_protected,omitempty"`
}

// persister wraps an on-disk JSON file with a mutex for safe concurrent
// access. It is best-effort: write failures are logged but do not block
// the recovery pipeline.
type persister struct {
	mu   sync.Mutex
	path string
}

func newPersister(path string) *persister {
	return &persister{path: path}
}

// save atomically writes the state to disk.
func (p *persister) save(state persistState) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(p.path), 0o755); err != nil {
		return err
	}

	data, err := json.Marshal(state)
	if err != nil {
		return err
	}

	tmp := p.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p.path)
}

// load reads the state from disk. Returns an empty state if the file does
// not exist.
func (p *persister) load() (persistState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	data, err := os.ReadFile(p.path)
	if err != nil {
		if os.IsNotExist(err) {
			return persistState{
				Paused:   make(map[string][]PausedSandbox),
				Isolated: make(map[string]string),
			}, nil
		}
		return persistState{}, err
	}

	var state persistState
	if err := json.Unmarshal(data, &state); err != nil {
		return persistState{}, err
	}
	if state.Version > currentPersistVersion {
		return persistState{}, fmt.Errorf("unsupported recovery state version %d", state.Version)
	}
	if state.Paused == nil {
		state.Paused = make(map[string][]PausedSandbox)
	}
	if state.Isolated == nil {
		state.Isolated = make(map[string]string)
	}
	if state.DesiredProtected == nil {
		state.DesiredProtected = make(map[string]bool)
		// Version 1 did not persist intent. Existing non-terminal work must be
		// treated as protected until startup reconciliation observes the node.
		for node := range state.Paused {
			state.DesiredProtected[node] = true
		}
		for node := range state.Isolated {
			state.DesiredProtected[node] = true
		}
	}
	state.Version = currentPersistVersion
	return state, nil
}
