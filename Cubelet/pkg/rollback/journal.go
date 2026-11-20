// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package rollback

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type journal struct{ dir string }

func (j *journal) path(id string) string {
	digest := sha256.Sum256([]byte(id))
	return filepath.Join(j.dir, hex.EncodeToString(digest[:])+".json")
}

func openJournal(dir string) (*journal, []Record, error) {
	if dir == "" {
		return nil, nil, fmt.Errorf("rollback journal directory is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, err
	}
	j := &journal{dir: dir}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	var records []Record
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, err
		}
		var r Record
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, nil, fmt.Errorf("read rollback journal %s: %w", path, err)
		}
		if r.Version != 1 || r.ID == "" || path != j.path(r.ID) {
			return nil, nil, fmt.Errorf("invalid rollback journal %s", path)
		}
		for _, id := range r.Evidence.Identities {
			if id.PID <= 1 || id.StartTime == 0 {
				return nil, nil, fmt.Errorf("invalid runtime identity in %s", path)
			}
		}
		for _, c := range r.Evidence.Unresolved {
			if c.PID <= 1 || (c.Identity != nil && c.Identity.PID != c.PID) {
				return nil, nil, fmt.Errorf("invalid unresolved runtime in %s", path)
			}
		}
		records = append(records, r)
	}
	return j, records, nil
}

func (j *journal) save(r Record) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(j.dir, ".rollback-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), j.path(r.ID)); err != nil {
		return err
	}
	return j.syncDir()
}

func (j *journal) remove(id string) error {
	if err := os.Remove(j.path(id)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return j.syncDir()
}

func (j *journal) syncDir() error {
	f, err := os.Open(j.dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
