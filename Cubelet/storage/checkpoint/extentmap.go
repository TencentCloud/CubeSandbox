// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package checkpoint

// extent is one written range of a file and where it sits on disk.
type extent struct {
	logical, physical, length int64
}

// extentCursor walks a file's extents window by window, in order.
type extentCursor struct {
	exts []extent
	i    int
}

// window returns the written ranges inside [start, end), clipped and with
// physically contiguous neighbours merged, so two files that share the same
// blocks yield equal slices whatever their extent boundaries.
func (c *extentCursor) window(start, end int64) []extent {
	for c.i < len(c.exts) && c.exts[c.i].logical+c.exts[c.i].length <= start {
		c.i++
	}
	var out []extent
	for j := c.i; j < len(c.exts) && c.exts[j].logical < end; j++ {
		e := c.exts[j]
		lo, hi := max(e.logical, start), min(e.logical+e.length, end)
		if lo >= hi {
			continue
		}
		seg := extent{logical: lo, physical: e.physical + (lo - e.logical), length: hi - lo}
		if n := len(out); n > 0 {
			p := &out[n-1]
			if p.logical+p.length == seg.logical && p.physical+p.length == seg.physical {
				p.length += seg.length
				continue
			}
		}
		out = append(out, seg)
	}
	return out
}

func sameExtents(a, b []extent) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
