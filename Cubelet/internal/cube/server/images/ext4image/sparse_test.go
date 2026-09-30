// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package ext4image

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestIsAllZeros(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want bool
	}{
		{"empty", []byte{}, true},
		{"single zero", []byte{0}, true},
		{"seven zeros", make([]byte, 7), true},
		{"eight zeros", make([]byte, 8), true},
		{"nine zeros", make([]byte, 9), true},
		{"16 zeros", make([]byte, 16), true},
		{"1MB zeros", make([]byte, 1024*1024), true},
		{"first byte non-zero", append([]byte{1}, make([]byte, 15)...), false},
		{"last byte non-zero", append(make([]byte, 15), 1), false},
		{"middle byte non-zero", append(append(make([]byte, 7), 0xff), make([]byte, 8)...), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isAllZeros(tc.data)
			if got != tc.want {
				t.Fatalf("isAllZeros() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSparseCopy(t *testing.T) {
	// The first zero run starts 512 KiB into a 1 MiB read buffer. This guards
	// against implementations that only recognize buffer-aligned zero chunks.
	part1 := bytes.Repeat([]byte{0xAB}, 512*1024)
	part2 := make([]byte, 2*1024*1024) // unaligned 2 MiB zero run
	part3 := bytes.Repeat([]byte{0xCD}, 512*1024)
	part4 := make([]byte, 1024*1024) // trailing 1 MiB zero run

	fullPayload := append(part1, part2...)
	fullPayload = append(fullPayload, part3...)
	fullPayload = append(fullPayload, part4...)

	expectedHasher := sha256.New()
	expectedHasher.Write(fullPayload)
	expectedSHA := hex.EncodeToString(expectedHasher.Sum(nil))

	tmpDir := t.TempDir()
	targetPath := filepath.Join(tmpDir, "sparse_test.bin")

	f, err := os.Create(targetPath)
	if err != nil {
		t.Fatalf("os.Create failed: %v", err)
	}
	defer f.Close()

	hasher := sha256.New()
	n, err := sparseCopy(f, hasher, bytes.NewReader(fullPayload), 1024*1024)
	if err != nil {
		t.Fatalf("sparseCopy failed: %v", err)
	}

	if n != int64(len(fullPayload)) {
		t.Fatalf("sparseCopy returned length %d, want %d", n, len(fullPayload))
	}

	gotSHA := hex.EncodeToString(hasher.Sum(nil))
	if gotSHA != expectedSHA {
		t.Fatalf("sha256 mismatch: got %s, want %s", gotSHA, expectedSHA)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("sync sparse file failed: %v", err)
	}

	st, err := os.Stat(targetPath)
	if err != nil {
		t.Fatalf("os.Stat failed: %v", err)
	}
	if st.Size() != int64(len(fullPayload)) {
		t.Fatalf("file apparent size %d != expected %d", st.Size(), len(fullPayload))
	}
	stat, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("unexpected stat type %T", st.Sys())
	}
	allocatedBytes := stat.Blocks * 512
	if allocatedBytes >= st.Size() {
		t.Fatalf("file is not sparse: allocated %d bytes for apparent size %d", allocatedBytes, st.Size())
	}

	// Verify content read-back
	readBack, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("os.ReadFile failed: %v", err)
	}
	if !bytes.Equal(readBack, fullPayload) {
		t.Fatalf("read back content does not match original payload")
	}
}
