// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package node

import "testing"

func TestHostFactsIsZero(t *testing.T) {
	if !(*HostFacts)(nil).IsZero() {
		t.Errorf("nil HostFacts must be zero")
	}
	if !(&HostFacts{}).IsZero() {
		t.Errorf("empty HostFacts must be zero")
	}
	for name, f := range map[string]*HostFacts{
		"vendor":      {CPUVendor: "x"},
		"model":       {CPUModel: "x"},
		"cpuid":       {CPUIDHash: "x"},
		"release":     {HostKernelRelease: "x"},
		"fingerprint": {HostKernelFingerprint: "x"},
		"kvm":         {KVMAPIVersion: 12},
		"module-fp":   {KVMModuleFingerprint: "x"},
		"taint":       {KVMModuleTaint: "x"},
	} {
		if f.IsZero() {
			t.Errorf("HostFacts with %s set must not be zero", name)
		}
	}
}
