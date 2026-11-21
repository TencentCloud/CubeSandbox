// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//go:build linux

package jfsdiff

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestWarmFile(t *testing.T) {
	dir := t.TempDir()
	if d := os.Getenv("JFSDIFF_REFLINK_DIR"); d != "" {
		dir = d // a real block filesystem where DONTNEED reliably drops pages
	}
	p := filepath.Join(dir, "warm-mem")
	defer os.Remove(p)
	data := make([]byte, 40<<20)
	for i := range data {
		data[i] = byte(i)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(p)
	if err := punchPageCache(f); err != nil {
		t.Skipf("cannot drop page cache here: %v", err)
	}
	_, resident, err := residency(f, int64(len(data)))
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resident > int64(len(data))/2 {
		t.Skipf("page cache not dropped on this filesystem (%d bytes still resident)", resident)
	}

	st, err := WarmFile(context.Background(), p, 4, 0.95)
	if err != nil {
		t.Fatal(err)
	}
	if st.SizeBytes != int64(len(data)) || st.Skipped {
		t.Fatalf("unexpected stats %+v", st)
	}
	// Second call: everything is resident, so it is skipped.
	st2, err := WarmFile(context.Background(), p, 4, 0.95)
	if err != nil {
		t.Fatal(err)
	}
	if !st2.Skipped || st2.ReadBytes != 0 || st2.ResidentBefore < int64(len(data))*95/100 {
		t.Fatalf("expected skip on a resident file, got %+v", st2)
	}
	// Without a skip threshold, resident chunks are still not re-read.
	st3, err := WarmFile(context.Background(), p, 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	if st3.Skipped || st3.ReadBytes > warmChunk {
		t.Fatalf("resident chunks should not be re-read, got %+v", st3)
	}
}

func TestWarmFileEmpty(t *testing.T) {
	p := filepath.Join(t.TempDir(), "empty")
	os.WriteFile(p, nil, 0o644)
	st, err := WarmFile(context.Background(), p, 0, 0.9)
	if err != nil || st.SizeBytes != 0 {
		t.Fatalf("empty file: %+v %v", st, err)
	}
}

func punchPageCache(f *os.File) error {
	if err := f.Sync(); err != nil {
		return err
	}
	return unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED)
}
