// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package image

import "testing"

func TestAlignUp(t *testing.T) {
	const twoMiB = int64(2 * 1024 * 1024)

	cases := []struct {
		name      string
		size      int64
		alignment int64
		want      int64
	}{
		{"zero size stays zero", 0, twoMiB, 0},
		{"already 2MiB-aligned is unchanged", twoMiB, twoMiB, twoMiB},
		{"one byte over rounds up to next boundary", twoMiB + 1, twoMiB, 2 * twoMiB},
		{"one byte under rounds up to the boundary", twoMiB - 1, twoMiB, twoMiB},
		// 438304768 B = 418 MiB = 209 * 2 MiB (a real minimized-rootfs size).
		{"realistic fs length rounds up", 438304767, twoMiB, 438304768},
		{"realistic aligned length is unchanged", 438304768, twoMiB, 438304768},
		{"non-positive alignment returns size unchanged", 12345, 0, 12345},
		{"negative alignment returns size unchanged", 12345, -1, 12345},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := alignUp(tc.size, tc.alignment)
			if got != tc.want {
				t.Fatalf("alignUp(%d, %d) = %d, want %d", tc.size, tc.alignment, got, tc.want)
			}
			if tc.alignment > 0 && got%tc.alignment != 0 {
				t.Fatalf("alignUp(%d, %d) = %d is not a multiple of %d", tc.size, tc.alignment, got, tc.alignment)
			}
			if got < tc.size {
				t.Fatalf("alignUp(%d, %d) = %d rounded DOWN below input", tc.size, tc.alignment, got)
			}
		})
	}
}
