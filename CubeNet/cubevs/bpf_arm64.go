// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 Tencent Inc. All rights reserved.

//go:build arm64

package cubevs

import (
	"bytes"
	_ "embed"
	"fmt"

	"github.com/cilium/ebpf"
)

// loadLocalgw returns the embedded CollectionSpec for localgw.
func loadLocalgw() (*ebpf.CollectionSpec, error) {
	reader := bytes.NewReader(_LocalgwBytes)
	spec, err := ebpf.LoadCollectionSpecFromReader(reader)
	if err != nil {
		return nil, fmt.Errorf("can't load localgw: %w", err)
	}
	return spec, nil
}

// loadMvmtap returns the embedded CollectionSpec for mvmtap.
func loadMvmtap() (*ebpf.CollectionSpec, error) {
	reader := bytes.NewReader(_MvmtapBytes)
	spec, err := ebpf.LoadCollectionSpecFromReader(reader)
	if err != nil {
		return nil, fmt.Errorf("can't load mvmtap: %w", err)
	}
	return spec, nil
}

// loadNodenic returns the embedded CollectionSpec for nodenic.
func loadNodenic() (*ebpf.CollectionSpec, error) {
	reader := bytes.NewReader(_NodenicBytes)
	spec, err := ebpf.LoadCollectionSpecFromReader(reader)
	if err != nil {
		return nil, fmt.Errorf("can't load nodenic: %w", err)
	}
	return spec, nil
}

// Reuse the checked-in little-endian eBPF objects. The original bpf2go wrappers
// are amd64-only, but the objects themselves are eBPF bytecode and are usable on
// arm64 Linux as well.
//
//go:embed localgw_x86_bpfel.o
var _LocalgwBytes []byte

//go:embed mvmtap_x86_bpfel.o
var _MvmtapBytes []byte

//go:embed nodenic_x86_bpfel.o
var _NodenicBytes []byte
