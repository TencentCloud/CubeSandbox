// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package service

import (
	"fmt"
	"net"
	"os"
)

// TapFDProvider exposes the original TAP fd owned by network-agent. It also
// returns the tap's kernel ifindex so callers (cubelet) can avoid a separate
// netlink LinkByName lookup on the create hot path.
type TapFDProvider interface {
	GetTapFile(sandboxID, tapName string) (*os.File, int, error)
}

func (s *localService) GetTapFile(sandboxID, tapName string) (*os.File, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, ok := s.states[sandboxID]
	if !ok {
		return nil, 0, fmt.Errorf("sandbox %q not found", sandboxID)
	}
	if tapName != "" && state.TapName != tapName {
		return nil, 0, fmt.Errorf("tap name mismatch: want %q got %q", tapName, state.TapName)
	}

	ifindex := state.TapIfIndex

	// Fast path: the fd is already cached, return it with no syscalls.
	if state.tap != nil && state.tap.File != nil {
		return state.tap.File, indexFallback(ifindex, state.tap), nil
	}

	// Hot path: an in-process managed tap that is fully configured but whose fd
	// was closed while it sat idle in the pool. We only need to re-open the fd;
	// the link is already up with the right MTU, the TC filter is attached and
	// the ARP entry exists, so we skip the full restoreTap (and even its netlink
	// lookup) and just issue the cheap open + TUNSETIFF.
	//
	// We keep this under s.mu on purpose: parallelising these syscalls across
	// concurrent sandbox boots regressed tail latency (they contend on kernel
	// locks anyway), so an orderly short critical section is preferable.
	if state.tap != nil && !state.tap.InUse {
		file, err := openTapFdByNameFunc(state.TapName)
		if err != nil {
			return nil, 0, fmt.Errorf("tap fd unavailable for sandbox %q: %w", sandboxID, err)
		}
		state.tap.File = file
		return state.tap.File, indexFallback(ifindex, state.tap), nil
	}

	// Recovery path: no in-memory tap (e.g. after a restart) or the tap is held
	// by another process. Fall back to the full restore which probes the kernel
	// state and only acquires the fd when the tap is idle.
	baseTap := state.tap
	if baseTap == nil {
		baseTap = &tapDevice{
			Name:         state.TapName,
			IP:           net.ParseIP(state.SandboxIP).To4(),
			PortMappings: append([]PortMapping(nil), state.PortMappings...),
		}
	} else {
		baseTap.PortMappings = append([]PortMapping(nil), state.PortMappings...)
	}
	tap, err := restoreTapFunc(baseTap, s.cfg.MvmMtu, s.cfg.MVMMacAddr, s.cubeDev.Index)
	if err != nil {
		return nil, 0, fmt.Errorf("tap fd unavailable for sandbox %q: %w", sandboxID, err)
	}
	state.tap = tap
	// restoreTap intentionally skips fd acquisition when the tap is held by
	// another process. If we still have no fd at this point, surface a clear
	// error rather than handing back a nil file.
	if state.tap.File == nil {
		return nil, 0, fmt.Errorf("tap fd unavailable for sandbox %q: tap %s is currently held by another process", sandboxID, state.TapName)
	}
	return state.tap.File, indexFallback(ifindex, state.tap), nil
}

// indexFallback prefers the persisted ifindex and falls back to the live tap's
// index when the persisted value is unset (0).
func indexFallback(persisted int, tap *tapDevice) int {
	if persisted != 0 {
		return persisted
	}
	if tap != nil {
		return tap.Index
	}
	return 0
}
