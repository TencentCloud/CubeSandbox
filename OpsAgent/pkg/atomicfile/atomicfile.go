// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package atomicfile provides crash-safe file replacement: write to a temp
// file in the same directory, fsync, then rename over the target. Backups are
// rotated alongside so a bad write can always be rolled back by hand.
package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Replace writes content to path atomically (tmp + fsync + rename) while
// keeping the existing file mode. It does NOT back up; call Backup first when
// history matters.
func Replace(path, content string) error {
	base := filepath.Dir(path)
	mode := os.FileMode(0644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode()
	}
	tmp, err := os.CreateTemp(base, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("fsync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename into place: %w", err)
	}
	tmpName = ""
	return nil
}

// Backup copies path to "<path>.bak.<unix-nano>" and prunes older backups
// beyond keep. A missing source file is not an error (first-ever write).
func Backup(path string, keep int) (string, error) {
	if keep <= 0 {
		keep = 5
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read for backup: %w", err)
	}
	bak := fmt.Sprintf("%s.bak.%d", path, time.Now().UnixNano())
	if err := os.WriteFile(bak, data, 0644); err != nil {
		return "", fmt.Errorf("write backup: %w", err)
	}
	pruneBackups(path, keep)
	return bak, nil
}

// ListBackups returns the existing backups of path, oldest first.
func ListBackups(path string) []string {
	matches, _ := filepath.Glob(path + ".bak.*")
	sort.Strings(matches)
	return matches
}

func pruneBackups(path string, keep int) {
	matches, err := filepath.Glob(path + ".bak.*")
	if err != nil || len(matches) <= keep {
		return
	}
	sort.Strings(matches) // unix-nano suffixes sort chronologically
	for _, old := range matches[:len(matches)-keep] {
		_ = os.Remove(old)
	}
}
