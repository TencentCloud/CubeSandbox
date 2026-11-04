// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

// recoverAttachFilterConcurrency bounds background AttachFilter during recover.
// Each missing inner map pays synchronize_rcu; extra workers do not shorten
// that grace period.
const recoverAttachFilterConcurrency = 8

// markFilterAttached records a successful AttachFilter for this process.
// newTap attaches before Create returns; the controller records that here so
// a later acquire does not attach again.
func (s *NetworkController) markFilterAttached(ifindex int) {
	if s == nil || ifindex <= 0 {
		return
	}
	s.attachedFilters.Store(ifindex, struct{}{})
}

func (s *NetworkController) clearFilterAttached(ifindex int) {
	if s == nil || ifindex <= 0 {
		return
	}
	s.attachedFilters.Delete(ifindex)
}

func (s *NetworkController) filterAttached(ifindex int) bool {
	if s == nil || ifindex <= 0 {
		return false
	}
	_, ok := s.attachedFilters.Load(ifindex)
	return ok
}

// ensureAttachFilter attaches the TC filter unless this process already
// recorded success for ifindex. A second call is safe; the record only skips
// the inner-map lookup and synchronize_rcu on the allocation path.
func (s *NetworkController) ensureAttachFilter(ifindex int) error {
	if ifindex <= 0 || s.filterAttached(ifindex) {
		return nil
	}
	if err := s.cubevsAdapter.AttachFilter(uint32(ifindex)); err != nil {
		return fmt.Errorf("attach filter ifindex %d: %w", ifindex, err)
	}
	s.markFilterAttached(ifindex)
	return nil
}

// destroyTapIndex destroys the host TAP and drops its attach record only after
// the netdev is gone. A failed destroy leaves the record in place.
func (s *NetworkController) destroyTapIndex(ifindex int) error {
	if err := s.tapAdapter.Destroy(ifindex); err != nil {
		return err
	}
	s.clearFilterAttached(ifindex)
	return nil
}

// rollbackUnattachedAcquire returns a reserved TAP to the free pool when the
// compensatory AttachFilter fails. The device stays; the next acquire retries.
func (s *NetworkController) rollbackUnattachedAcquire(owner string, tap *tapDevice, entry *TapPoolEntry, cause error) error {
	if tap != nil && tap.File != nil {
		file := tap.File
		tap.File = nil
		s.poolTapFD(tap.Name, file)
	}
	if entry != nil {
		if err := s.tapPool.ReleaseReservation(entry, owner); err != nil {
			return fmt.Errorf("attach filter for %s: %w", tapNameOf(tap), errors.Join(cause, err))
		}
	}
	return fmt.Errorf("attach filter for %s: %w", tapNameOf(tap), cause)
}

func tapNameOf(tap *tapDevice) string {
	if tap == nil {
		return ""
	}
	return tap.Name
}

// startAsyncAttachFilters snapshots pool ifindexes and attaches them off the
// startup path. Create records its own ifindex, so taps born in warmup are
// not part of this snapshot.
func (s *NetworkController) startAsyncAttachFilters() {
	ifindexes := s.recoveredFilterIfindexes()
	if len(ifindexes) == 0 {
		return
	}
	go s.attachRecoveredFilters(ifindexes)
}

func (s *NetworkController) recoveredFilterIfindexes() []int {
	if s == nil || s.tapPool == nil {
		return nil
	}
	seen := make(map[int]struct{})
	ifindexes := make([]int, 0)
	for _, entry := range s.tapPool.Entries() {
		if entry == nil || entry.TapIfIndex <= 0 {
			continue
		}
		if _, ok := seen[entry.TapIfIndex]; ok {
			continue
		}
		seen[entry.TapIfIndex] = struct{}{}
		ifindexes = append(ifindexes, entry.TapIfIndex)
	}
	return ifindexes
}

func (s *NetworkController) attachRecoveredFilters(ifindexes []int) {
	logger := CubeLog.WithContext(context.Background())
	logger.Infof("network runtime async AttachFilter start: count=%d concurrency=%d",
		len(ifindexes), recoverAttachFilterConcurrency)
	start := time.Now()
	s.attachFiltersWithLimit(ifindexes)
	logger.Infof("network runtime async AttachFilter done: count=%d elapsed=%s",
		len(ifindexes), time.Since(start))
}

func (s *NetworkController) attachFiltersWithLimit(ifindexes []int) {
	sem := make(chan struct{}, recoverAttachFilterConcurrency)
	var wg sync.WaitGroup
	for _, ifindex := range ifindexes {
		sem <- struct{}{}
		wg.Add(1)
		go func(ifindex int) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := s.ensureAttachFilter(ifindex); err != nil {
				CubeLog.WithContext(context.Background()).Warnf(
					"network runtime async AttachFilter failed: ifindex=%d err=%v",
					ifindex, err,
				)
			}
		}(ifindex)
	}
	wg.Wait()
}
