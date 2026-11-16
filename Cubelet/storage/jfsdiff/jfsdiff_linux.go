// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//go:build linux

package jfsdiff

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	fsIocFiemap      = 0xC020660B
	fiemapFlagSync   = 0x1
	fiemapExtentLast = 0x1
	fiemapExtentUnkn = 0x2
	fiemapExtentDela = 0x4
	fiemapExtentUnwr = 0x800
	fiemapBatch      = 512
)

type fiemapHeader struct {
	Start         uint64
	Length        uint64
	Flags         uint32
	MappedExtents uint32
	ExtentCount   uint32
	Reserved      uint32
}

type fiemapExtent struct {
	Logical    uint64
	Physical   uint64
	Length     uint64
	Reserved64 [2]uint64
	Flags      uint32
	Reserved   [3]uint32
}

// dataMap returns the real data extents of path (unwritten extents read as
// zero and are omitted), sorted by logical offset, plus the file size.
// FIEMAP_FLAG_SYNC flushes delayed allocation first so every written block
// has its final physical address.
func dataMap(path string) ([]mapping, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	return dataMapFile(f)
}

// dataMapFile is dataMap on an open file (which may already be unlinked).
func dataMapFile(f *os.File) ([]mapping, int64, error) {
	path := f.Name()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	size := st.Size()
	hdrSize := int(unsafe.Sizeof(fiemapHeader{}))
	extSize := int(unsafe.Sizeof(fiemapExtent{}))
	buf := make([]byte, hdrSize+extSize*fiemapBatch)
	var out []mapping
	start := uint64(0)
	for int64(start) < size {
		for i := range buf {
			buf[i] = 0
		}
		hdr := (*fiemapHeader)(unsafe.Pointer(&buf[0]))
		hdr.Start = start
		hdr.Length = ^uint64(0) - start
		hdr.Flags = fiemapFlagSync
		hdr.ExtentCount = fiemapBatch
		if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), fsIocFiemap, uintptr(unsafe.Pointer(&buf[0]))); e != 0 {
			return nil, 0, fmt.Errorf("FIEMAP %s: %w", path, e)
		}
		n := int(hdr.MappedExtents)
		if n == 0 {
			break
		}
		last := false
		var next uint64
		for i := 0; i < n; i++ {
			ext := (*fiemapExtent)(unsafe.Pointer(&buf[hdrSize+i*extSize]))
			if ext.Flags&(fiemapExtentUnkn|fiemapExtentDela) != 0 {
				return nil, 0, fmt.Errorf("FIEMAP %s: extent at %d has no stable physical address (flags %#x)", path, ext.Logical, ext.Flags)
			}
			next = ext.Logical + ext.Length
			if ext.Flags&fiemapExtentLast != 0 {
				last = true
			}
			if ext.Flags&fiemapExtentUnwr != 0 {
				continue
			}
			end := int64(ext.Logical + ext.Length)
			if end > size {
				end = size
			}
			if int64(ext.Logical) >= end {
				continue
			}
			out = append(out, mapping{start: int64(ext.Logical), end: end, phys: int64(ext.Physical)})
		}
		if last || next <= start {
			break
		}
		start = next
	}
	return out, size, nil
}

func punchHole(f *os.File, off, length int64) error {
	return unix.Fallocate(int(f.Fd()), unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, off, length)
}

// nextData returns the next data segment at or after off.
func nextData(f *os.File, off, size int64) (start, end int64, ok bool, err error) {
	start, err = unix.Seek(int(f.Fd()), off, unix.SEEK_DATA)
	if err != nil {
		if errors.Is(err, unix.ENXIO) {
			return 0, 0, false, nil
		}
		return 0, 0, false, err
	}
	end, err = unix.Seek(int(f.Fd()), start, unix.SEEK_HOLE)
	if err != nil {
		return 0, 0, false, err
	}
	if end > size {
		end = size
	}
	return start, end, true, nil
}

func statFile(path string) (ino uint64, size, mtimeNs int64, err error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return 0, 0, 0, err
	}
	return st.Ino, st.Size, st.Mtim.Nano(), nil
}
