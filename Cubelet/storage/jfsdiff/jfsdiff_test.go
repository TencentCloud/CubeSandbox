// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package jfsdiff

import (
	"reflect"
	"testing"
)

func TestComputeFromMaps(t *testing.T) {
	const blk = 4096
	base := []mapping{
		{start: 0, end: 4 * blk, phys: 1000 * blk},
		{start: 8 * blk, end: 12 * blk, phys: 2000 * blk},
	}
	tests := []struct {
		name      string
		pkg       []mapping
		pkgSize   int64
		wantData  []Range
		wantHoles []Range
	}{
		{
			name:    "identical clone",
			pkg:     base,
			pkgSize: 16 * blk,
		},
		{
			name: "one block rewritten inside a shared extent",
			pkg: []mapping{
				{start: 0, end: 1 * blk, phys: 1000 * blk},
				{start: 1 * blk, end: 2 * blk, phys: 5000 * blk},
				{start: 2 * blk, end: 4 * blk, phys: 1002 * blk},
				{start: 8 * blk, end: 12 * blk, phys: 2000 * blk},
			},
			pkgSize:  16 * blk,
			wantData: []Range{{1 * blk, 2 * blk}},
		},
		{
			name: "write into a base hole and discard base data",
			pkg: []mapping{
				{start: 0, end: 4 * blk, phys: 1000 * blk},
				{start: 5 * blk, end: 6 * blk, phys: 7000 * blk},
				{start: 10 * blk, end: 12 * blk, phys: 2002 * blk},
			},
			pkgSize:   16 * blk,
			wantData:  []Range{{5 * blk, 6 * blk}},
			wantHoles: []Range{{8 * blk, 10 * blk}},
		},
		{
			name: "growth past the base size",
			pkg: append(append([]mapping(nil), base...),
				mapping{start: 16 * blk, end: 18 * blk, phys: 9000 * blk}),
			pkgSize:  20 * blk,
			wantData: []Range{{16 * blk, 18 * blk}},
		},
		{
			name: "same logical data at a shifted physical address is a change",
			pkg: []mapping{
				{start: 0, end: 4 * blk, phys: 1001 * blk},
				{start: 8 * blk, end: 12 * blk, phys: 2000 * blk},
			},
			pkgSize:  16 * blk,
			wantData: []Range{{0, 4 * blk}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := computeFromMaps(tc.pkg, tc.pkgSize, base, 16*blk)
			if !reflect.DeepEqual(d.Data, tc.wantData) {
				t.Fatalf("data = %v, want %v", d.Data, tc.wantData)
			}
			if !reflect.DeepEqual(d.Holes, tc.wantHoles) {
				t.Fatalf("holes = %v, want %v", d.Holes, tc.wantHoles)
			}
		})
	}
}

func TestComputeFromMapsNoBase(t *testing.T) {
	pkg := []mapping{{start: 0, end: 4096, phys: 1}, {start: 8192, end: 12288, phys: 9}}
	d := computeFromMaps(pkg, 16384, nil, 0)
	want := []Range{{0, 4096}, {8192, 12288}}
	if !reflect.DeepEqual(d.Data, want) || len(d.Holes) != 0 {
		t.Fatalf("data=%v holes=%v, want data=%v", d.Data, d.Holes, want)
	}
}
