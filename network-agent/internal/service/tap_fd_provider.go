// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package service

import (
	"fmt"
	"net"
	"os"
)

// TapFDProvider exposes the original TAP fd owned by network-agent.
type TapFDProvider interface {
	GetTapFile(sandboxID, tapName string) (*os.File, error)
}

func (s *localService) GetTapFile(sandboxID, tapName string) (*os.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, ok := s.states[sandboxID]
	if !ok {
		return nil, fmt.Errorf("sandbox %q not found", sandboxID)
	}
	if tapName != "" && state.TapName != tapName {
		return nil, fmt.Errorf("tap name mismatch: want %q got %q", tapName, state.TapName)
	}
	if state.tap == nil || state.tap.File == nil {
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
			return nil, fmt.Errorf("tap fd unavailable for sandbox %q: %w", sandboxID, err)
		}
		state.tap = tap
	}
	// restoreTap intentionally skips fd acquisition when the tap is held by
	// another process. If we still have no fd at this point, surface a clear
	// error rather than handing back a nil file.
	if state.tap.File == nil {
		return nil, fmt.Errorf("tap fd unavailable for sandbox %q: tap %s is currently held by another process", sandboxID, state.TapName)
	}
	return state.tap.File, nil
}
