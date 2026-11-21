// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//go:build linux

package jfsdiff

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// residency returns, per warmChunk-sized chunk, whether every page of the
// chunk is in the page cache, plus the total resident bytes.
func residency(f *os.File, size int64) ([]bool, int64, error) {
	mem, err := unix.Mmap(int(f.Fd()), 0, int(size), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		return nil, 0, fmt.Errorf("mmap for mincore: %w", err)
	}
	defer unix.Munmap(mem)
	page := int64(os.Getpagesize())
	vec := make([]byte, (size+page-1)/page)
	if _, _, e := unix.Syscall(unix.SYS_MINCORE, uintptr(unsafe.Pointer(&mem[0])), uintptr(len(mem)),
		uintptr(unsafe.Pointer(&vec[0]))); e != 0 {
		return nil, 0, fmt.Errorf("mincore: %w", e)
	}
	pagesPerChunk := warmChunk / page
	chunks := make([]bool, (size+warmChunk-1)/warmChunk)
	var resident int64
	for c := range chunks {
		full := true
		for p := int64(c) * pagesPerChunk; p < int64(c+1)*pagesPerChunk && p < int64(len(vec)); p++ {
			if vec[p]&1 != 0 {
				resident += page
			} else {
				full = false
			}
		}
		chunks[c] = full
	}
	if resident > size {
		resident = size
	}
	return chunks, resident, nil
}

func warmFile(ctx context.Context, path string, workers int, skipFraction float64) (WarmStats, error) {
	start := time.Now()
	var st WarmStats
	f, err := os.Open(path)
	if err != nil {
		return st, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return st, err
	}
	st.SizeBytes = fi.Size()
	if st.SizeBytes == 0 {
		st.Duration = time.Since(start)
		return st, nil
	}
	chunks, resident, err := residency(f, st.SizeBytes)
	if err != nil {
		return st, err
	}
	st.ResidentBefore = resident
	if skipFraction > 0 && float64(resident) >= skipFraction*float64(st.SizeBytes) {
		st.Skipped = true
		st.Duration = time.Since(start)
		return st, nil
	}

	todo := make(chan int64, len(chunks))
	for c, full := range chunks {
		if !full {
			todo <- int64(c) * warmChunk
		}
	}
	close(todo)
	var read atomic.Int64
	var firstErr error
	var errOnce sync.Once
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, warmChunk)
			for off := range todo {
				if ctx.Err() != nil {
					errOnce.Do(func() { firstErr = ctx.Err() })
					return
				}
				n := int64(warmChunk)
				if rem := st.SizeBytes - off; rem < n {
					n = rem
				}
				if err := readFullAt(f, buf[:n], off); err != nil {
					errOnce.Do(func() { firstErr = fmt.Errorf("read %s at %d: %w", path, off, err) })
					return
				}
				read.Add(n)
			}
		}()
	}
	wg.Wait()
	st.ReadBytes = read.Load()
	st.Duration = time.Since(start)
	return st, firstErr
}
