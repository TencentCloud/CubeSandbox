// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package CubeLog

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewRollFileWriterDefaultsZeroArgs(t *testing.T) {
	dir := t.TempDir()
	w := NewRollFileWriter(dir, "cubemaster-req", 0, 0)
	if w.num != defaultRollFileNum {
		t.Fatalf("num = %d, want %d", w.num, defaultRollFileNum)
	}
	wantSize := int64(defaultRollFileSizeMB) * 1024 * 1024
	if w.size != wantSize {
		t.Fatalf("size = %d, want %d", w.size, wantSize)
	}
}

func TestNewRollFileWriterKeepsExplicitArgs(t *testing.T) {
	dir := t.TempDir()
	w := NewRollFileWriter(dir, "cubemaster-req", 3, 1)
	if w.num != 3 {
		t.Fatalf("num = %d, want 3", w.num)
	}
	if w.size != 1024*1024 {
		t.Fatalf("size = %d, want 1MiB", w.size)
	}
}

func TestRollFileWriterRotatesWhenSizeExceeded(t *testing.T) {
	dir := t.TempDir()
	w := NewRollFileWriter(dir, "cubemaster-req", 3, 1)

	chunk := make([]byte, 512*1024)
	for i := 0; i < 3; i++ {
		if _, err := w.Write(chunk); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	live := filepath.Join(dir, "cubemaster-req.log")
	rotated := filepath.Join(dir, "cubemaster-req.log.1")
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("current log missing: %v", err)
	}
	st, err := os.Stat(rotated)
	if err != nil {
		t.Fatalf("rotated log missing: %v", err)
	}
	if st.Size() == 0 {
		t.Fatal("rotated log is empty")
	}
}

func TestRollFileWriterKeepsNumRotatedFiles(t *testing.T) {
	dir := t.TempDir()
	const rollNum = 2
	w := NewRollFileWriter(dir, "cubemaster-req", rollNum, 1)

	chunk := make([]byte, 512*1024)
	// 2 writes ≈ 1MiB trigger one rotation; 6 writes ≈ 3 rotations.
	for i := 0; i < 6; i++ {
		if _, err := w.Write(chunk); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	live := filepath.Join(dir, "cubemaster-req.log")
	first := filepath.Join(dir, "cubemaster-req.log.1")
	second := filepath.Join(dir, "cubemaster-req.log.2")
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("current log missing: %v", err)
	}
	if _, err := os.Stat(first); err != nil {
		t.Fatalf(".log.1 missing: %v", err)
	}
	if _, err := os.Stat(second); !os.IsNotExist(err) {
		t.Fatalf(".log.2 should not exist when file_num=2 (live + one backup): %v", err)
	}
}

func TestRollFileWriterKeepsSizeWhenLiveRenameFails(t *testing.T) {
	dir := t.TempDir()
	// num=2 shifts `.log` → `.log.1` only. A directory at `.log.1` makes
	// that file-over-dir rename fail.
	w := NewRollFileWriter(dir, "cubemaster-req", 2, 1)
	if err := os.Mkdir(filepath.Join(dir, "cubemaster-req.log.1"), 0755); err != nil {
		t.Fatal(err)
	}

	chunk := make([]byte, 512*1024)
	for i := 0; i < 3; i++ {
		if _, err := w.Write(chunk); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if w.currSize < 1024*1024 {
		t.Fatalf("currSize = %d after failed live rename, want >= 1MiB", w.currSize)
	}
}

func TestRollFileWriterRecoversWhenLiveFileRemoved(t *testing.T) {
	dir := t.TempDir()
	w := NewRollFileWriter(dir, "cubemaster-req", 2, 1)
	chunk := make([]byte, 512*1024)
	for i := 0; i < 2; i++ {
		if _, err := w.Write(chunk); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	live := filepath.Join(dir, "cubemaster-req.log")
	if err := os.Remove(live); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := w.Write(chunk); err != nil {
			t.Fatalf("write after remove %d: %v", i, err)
		}
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("live log not recreated after ENOENT rotate: %v", err)
	}
}

func TestRollFileWriterTruncatesWhenNumIsOne(t *testing.T) {
	dir := t.TempDir()
	w := NewRollFileWriter(dir, "cubemaster-req", 1, 1)
	chunk := make([]byte, 512*1024)
	for i := 0; i < 3; i++ {
		if _, err := w.Write(chunk); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	live := filepath.Join(dir, "cubemaster-req.log")
	st, err := os.Stat(live)
	if err != nil {
		t.Fatalf("live log missing: %v", err)
	}
	if st.Size() >= 1024*1024 {
		t.Fatalf("live size %d after num=1 rotate, want truncated below 1MiB", st.Size())
	}
	if _, err := os.Stat(live + ".1"); !os.IsNotExist(err) {
		t.Fatalf("num=1 must not create .log.1: %v", err)
	}
}

func TestRollFileWriterShiftsLog1BlockerWhenNumAtLeast3(t *testing.T) {
	dir := t.TempDir()
	w := NewRollFileWriter(dir, "cubemaster-req", 3, 1)
	if err := os.Mkdir(filepath.Join(dir, "cubemaster-req.log.1"), 0755); err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 512*1024)
	for i := 0; i < 3; i++ {
		if _, err := w.Write(chunk); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	live := filepath.Join(dir, "cubemaster-req.log")
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("live log missing: %v", err)
	}
	st, err := os.Stat(filepath.Join(dir, "cubemaster-req.log.1"))
	if err != nil {
		t.Fatalf(".log.1 missing after shift: %v", err)
	}
	if st.IsDir() {
		t.Fatal("num>=3 should shift the .log.1 directory aside so live rename succeeds")
	}
}
