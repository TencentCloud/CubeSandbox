// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package store persists EvictionEvent records to a local NDJSON file.
// Each record is appended as a single JSON line so the file remains readable
// even if the process is killed mid-write.
package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/tencentcloud/CubeSandbox/eviction-webhook/pkg/types"
)

// Store appends EvictionEvent records to a NDJSON file.
type Store struct {
	mu   sync.Mutex
	file *os.File
}

// New opens (or creates) the NDJSON audit file at path. Missing parent directories are created automatically.
func New(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &Store{file: f}, nil
}

// Save appends the event as a single JSON line. It is safe for concurrent use.
func (s *Store) Save(event *types.EvictionEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	data = append(data, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.file.Write(data)
	return err
}

// Close closes the underlying file.
func (s *Store) Close() error {
	return s.file.Close()
}
