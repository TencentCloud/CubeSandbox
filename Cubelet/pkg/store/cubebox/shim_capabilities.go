// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/capability"
)

// ShimCapabilities is the persisted record of what a sandbox's shim declared it can do.
// A nil/empty set means nothing is known.
type ShimCapabilitiesRecord struct {
	// Capabilities are the shim's own declarations (`update.*`, `errors.*`).
	Capabilities map[string]uint32 `json:"capabilities,omitempty"`
	// Runtime is what the live guest agent reported (guest.*); 0 means refused.
	Runtime map[string]uint32 `json:"runtime,omitempty"`
	// Source is runtime / artifact / legacy-table / none.
	Source string `json:"source,omitempty"`
	// Version is the version recorded in the document that was read, when any.
	Version string `json:"version,omitempty"`
}

// Empty reports that nothing was resolved.
func (r *ShimCapabilitiesRecord) Empty() bool {
	return r == nil || r.Source == "" || r.Source == string(capability.SourceNone) ||
		len(r.Capabilities) == 0
}

// DeepCopy returns an independent copy, or nil.
func (r *ShimCapabilitiesRecord) DeepCopy() *ShimCapabilitiesRecord {
	if r == nil {
		return nil
	}
	copied := &ShimCapabilitiesRecord{Source: r.Source, Version: r.Version}
	if len(r.Capabilities) > 0 {
		copied.Capabilities = make(map[string]uint32, len(r.Capabilities))
		for k, v := range r.Capabilities {
			copied.Capabilities[k] = v
		}
	}
	if len(r.Runtime) > 0 {
		copied.Runtime = make(map[string]uint32, len(r.Runtime))
		for k, v := range r.Runtime {
			copied.Runtime[k] = v
		}
	}
	return copied
}

// ShimCapabilitiesCopy returns a snapshot of the recorded capabilities.
func (cb *CubeBox) ShimCapabilitiesCopy() *ShimCapabilitiesRecord {
	if cb == nil {
		return nil
	}
	cb.shimCapabilitiesLock.RLock()
	defer cb.shimCapabilitiesLock.RUnlock()
	return cb.ShimCapabilities.DeepCopy()
}

// SetShimCapabilities replaces the recorded capabilities with a whole record.
func (cb *CubeBox) SetShimCapabilities(record *ShimCapabilitiesRecord) {
	if cb == nil {
		return
	}
	cb.shimCapabilitiesLock.Lock()
	defer cb.shimCapabilitiesLock.Unlock()
	cb.ShimCapabilities = record.DeepCopy()
}

// ResolvedShimCapabilities converts the persisted record into a capability.Resolved.
func (cb *CubeBox) ResolvedShimCapabilities() capability.Resolved {
	record := cb.ShimCapabilitiesCopy()
	if record == nil {
		return capability.Resolved{Source: capability.SourceNone}
	}
	return capability.Resolved{
		Capabilities: capability.Set(record.Capabilities).Clone(),
		Runtime:      capability.Set(record.Runtime).Clone(),
		Source:       capability.Source(record.Source),
		Version:      record.Version,
	}
}

// RecordResolvedShimCapabilities stores a resolution result on the sandbox.
func (cb *CubeBox) RecordResolvedShimCapabilities(resolved capability.Resolved) {
	if cb == nil {
		return
	}
	source := resolved.Source
	if source == "" {
		source = capability.SourceNone
	}
	cb.SetShimCapabilities(&ShimCapabilitiesRecord{
		Capabilities: map[string]uint32(resolved.Capabilities.Clone()),
		Runtime:      map[string]uint32(resolved.Runtime.Clone()),
		Source:       string(source),
		Version:      resolved.Version,
	})
}
