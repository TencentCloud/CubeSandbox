// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
)

// DefaultEmptyDirFormats is the fallback list of format roots used by
// CleanupOrphanStorageFiles when callers do not supply their own.
var DefaultEmptyDirFormats = []string{"512Mi", "others"}

// InspectStorageVolumes returns every sandbox storage record cubelet currently
// owns, with cubecow device paths re-resolved through the live engine. It is
// the canonical data source for `cubecli storage ls` and any other tool that
// needs an authoritative cross-sandbox view.
func InspectStorageVolumes() (map[string]*StorageInfo, error) {
	if localStorage == nil {
		return nil, errors.New("storage plugin is not initialized")
	}
	return localStorage.readAllStorageInfo()
}

// CleanupOrphanReport describes the outcome of a single orphan file cleanup
// attempt produced by CleanupOrphanStorageFiles.
type CleanupOrphanReport struct {
	Format   string
	FilePath string
	Removed  bool
	Err      error
}

// CleanupOrphanStorageFiles scans the configured emptydir format roots and
// drops any file that has no live sandbox owner.  When dryRun is true cubelet
// only reports the files it would delete.  formats may be nil, in which case
// DefaultEmptyDirFormats is used.
func CleanupOrphanStorageFiles(formats []string, dryRun bool) ([]CleanupOrphanReport, error) {
	if localStorage == nil {
		return nil, errors.New("storage plugin is not initialized")
	}
	if len(formats) == 0 {
		formats = append([]string(nil), DefaultEmptyDirFormats...)
	}
	owned, err := localStorage.readAllStorageInfo()
	if err != nil {
		return nil, fmt.Errorf("read storage info: %w", err)
	}
	live := make(map[string]struct{})
	for _, info := range owned {
		if info == nil {
			continue
		}
		for _, vol := range info.Volumes {
			if vol == nil || vol.VolumeName != "" || vol.FilePath == "" {
				continue
			}
			live[vol.FilePath] = struct{}{}
		}
	}

	dataPath := localStorage.config.DataPath
	storageRoot := filepath.Join(dataPath, "io.cubelet.internal.v1.storage", "emptydir")

	reports := make([]CleanupOrphanReport, 0)
	for _, format := range formats {
		baseFormatPath := filepath.Join(storageRoot, format)
		for filePath := range scanEmptyDirFormat(baseFormatPath) {
			if _, exists := live[filePath]; exists {
				continue
			}
			rep := CleanupOrphanReport{Format: format, FilePath: filePath}
			if !dryRun {
				if err := os.RemoveAll(filePath); err != nil {
					rep.Err = err
				} else {
					rep.Removed = true
				}
			}
			reports = append(reports, rep)
		}
	}
	return reports, nil
}

func scanEmptyDirFormat(baseFormatPath string) map[string]struct{} {
	all := map[string]struct{}{}
	denList, err := os.ReadDir(baseFormatPath)
	if err != nil {
		return all
	}
	for _, den := range denList {
		entryPath := path.Join(baseFormatPath, den.Name())
		if den.IsDir() {
			subList, err := os.ReadDir(path.Clean(entryPath))
			if err != nil {
				continue
			}
			for _, sub := range subList {
				if sub.IsDir() {
					continue
				}
				all[path.Join(path.Clean(entryPath), sub.Name())] = struct{}{}
			}
			continue
		}
		all[entryPath] = struct{}{}
	}
	return all
}
