// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package terminalcore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// FileJournal stores one atomically replaced record per session. Per-record
// files avoid a shared append/truncate window and make crash recovery local to
// the affected session.
type FileJournal struct {
	dir string
	mu  sync.Mutex
}

func NewFileJournal(dir string) (*FileJournal, error) {
	if dir == "" {
		return nil, errors.New("terminal journal directory is empty")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create terminal journal: %w", err)
	}
	return &FileJournal{dir: dir}, nil
}

func (j *FileJournal) Put(record JournalRecord) error {
	if err := validateJournalRecord(record); err != nil {
		return err
	}
	body, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal terminal journal record: %w", err)
	}

	j.mu.Lock()
	defer j.mu.Unlock()

	tmp, err := os.CreateTemp(j.dir, ".terminal-*.tmp")
	if err != nil {
		return fmt.Errorf("create terminal journal temp file: %w", err)
	}
	tmpName := tmp.Name()
	removeTmp := true
	defer func() {
		_ = tmp.Close()
		if removeTmp {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod terminal journal temp file: %w", err)
	}
	if _, err := tmp.Write(body); err != nil {
		return fmt.Errorf("write terminal journal temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync terminal journal temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close terminal journal temp file: %w", err)
	}
	if err := os.Rename(tmpName, j.recordPath(record.SessionID)); err != nil {
		return fmt.Errorf("replace terminal journal record: %w", err)
	}
	removeTmp = false
	return syncDirectory(j.dir)
}

func (j *FileJournal) Remove(sessionID string) error {
	if _, err := uuid.Parse(sessionID); err != nil {
		return fmt.Errorf("invalid terminal session id: %w", err)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := os.Remove(j.recordPath(sessionID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove terminal journal record: %w", err)
	}
	return syncDirectory(j.dir)
}

func (j *FileJournal) List() ([]JournalRecord, error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	entries, err := os.ReadDir(j.dir)
	if err != nil {
		return nil, fmt.Errorf("read terminal journal: %w", err)
	}
	records := make([]JournalRecord, 0, len(entries))
	var errs []error
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if strings.HasSuffix(entry.Name(), ".tmp") {
			// A crash between CreateTemp and the atomic rename left an
			// incomplete file. It is never a valid record, so sweep it.
			_ = os.Remove(filepath.Join(j.dir, entry.Name()))
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(j.dir, entry.Name())
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			errs = append(errs, fmt.Errorf("read terminal journal record %s: %w", entry.Name(), readErr))
			continue
		}
		var record JournalRecord
		decoder := json.NewDecoder(strings.NewReader(string(body)))
		decoder.DisallowUnknownFields()
		if decodeErr := decoder.Decode(&record); decodeErr != nil {
			errs = append(errs, fmt.Errorf("decode terminal journal record %s: %w", entry.Name(), decodeErr))
			continue
		}
		if validateErr := validateJournalRecord(record); validateErr != nil {
			errs = append(errs, fmt.Errorf("validate terminal journal record %s: %w", entry.Name(), validateErr))
			continue
		}
		records = append(records, record)
	}
	return records, errors.Join(errs...)
}

func (j *FileJournal) recordPath(sessionID string) string {
	return filepath.Join(j.dir, sessionID+".json")
}

func validateJournalRecord(record JournalRecord) error {
	if _, err := uuid.Parse(record.SessionID); err != nil {
		return fmt.Errorf("invalid terminal session id: %w", err)
	}
	if record.ExecID == "" || record.Target.SandboxID == "" || record.Target.RuntimeContainerID == "" {
		return errors.New("terminal journal record is missing target identity")
	}
	if record.OpenedAt.IsZero() {
		return errors.New("terminal journal record is missing opened_at")
	}
	return nil
}

func syncDirectory(dir string) error {
	if runtime.GOOS == "windows" {
		// Windows does not expose a portable fsync operation for directory
		// handles. The file itself is still fsynced before the atomic rename.
		return nil
	}
	f, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open terminal journal directory: %w", err)
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync terminal journal directory: %w", err)
	}
	return nil
}
