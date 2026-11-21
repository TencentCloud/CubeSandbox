// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//go:build linux

package checkpoint

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// dataExtents lists the allocated ranges of f with SEEK_DATA／SEEK_HOLE so
// holes are neither read nor uploaded. A filesystem without the seek
// extensions reports the whole file as one extent.
func dataExtents(f *os.File, size int64) ([][2]int64, error) {
	var out [][2]int64
	fd := int(f.Fd())
	off := int64(0)
	for off < size {
		start, err := unix.Seek(fd, off, unix.SEEK_DATA)
		if err != nil {
			if errors.Is(err, unix.ENXIO) {
				break
			}
			if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EOPNOTSUPP) {
				return [][2]int64{{0, size}}, nil
			}
			return nil, err
		}
		if start >= size {
			break
		}
		end, err := unix.Seek(fd, start, unix.SEEK_HOLE)
		if err != nil {
			return nil, err
		}
		if end > size {
			end = size
		}
		if end > start {
			out = append(out, [2]int64{start, end})
		}
		off = end
	}
	return out, nil
}

// dropCache tells the kernel the exported range will not be read again, so
// a large export does not push the running sandboxes' pages out.
func dropCache(f *os.File, off, length int64) {
	_ = unix.Fadvise(int(f.Fd()), off, length, unix.FADV_DONTNEED)
}
