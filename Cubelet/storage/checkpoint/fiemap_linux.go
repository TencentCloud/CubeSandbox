// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//go:build linux

package checkpoint

import (
	"encoding/binary"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	fsIocFiemap        = 0xC020660B
	fiemapFlagSync     = 0x1
	fiemapExtentLast   = 0x1
	fiemapExtentUnwrit = 0x800
	fiemapHeaderLen    = 32
	fiemapExtentLen    = 56
	fiemapBatch        = 1024
)

// physicalExtents maps every written range of f to its blocks on disk with
// FIEMAP. Unwritten (preallocated) ranges read as zero and are left out,
// like holes.
func physicalExtents(f *os.File) ([]extent, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	var out []extent
	buf := make([]byte, fiemapHeaderLen+fiemapExtentLen*fiemapBatch)
	for start := int64(0); start < size; {
		for i := range buf {
			buf[i] = 0
		}
		le := binary.LittleEndian
		le.PutUint64(buf[0:], uint64(start))
		le.PutUint64(buf[8:], uint64(size-start))
		le.PutUint32(buf[16:], fiemapFlagSync)
		le.PutUint32(buf[24:], fiemapBatch)
		if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), fsIocFiemap, uintptr(unsafe.Pointer(&buf[0]))); errno != 0 {
			return nil, errno
		}
		mapped := int(le.Uint32(buf[20:]))
		if mapped == 0 {
			break
		}
		last := false
		var end int64
		for i := 0; i < mapped; i++ {
			e := buf[fiemapHeaderLen+fiemapExtentLen*i:]
			logical := int64(le.Uint64(e[0:]))
			physical := int64(le.Uint64(e[8:]))
			length := int64(le.Uint64(e[16:]))
			flags := le.Uint32(e[40:])
			end = logical + length
			last = flags&fiemapExtentLast != 0
			if flags&fiemapExtentUnwrit == 0 {
				out = append(out, extent{logical: logical, physical: physical, length: length})
			}
		}
		if last {
			break
		}
		start = end
	}
	return out, nil
}
