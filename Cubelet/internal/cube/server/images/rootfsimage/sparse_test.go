// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package rootfsimage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
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

	for _, tc := range []struct {
		name string
		tail []byte
	}{
		{name: "whole buffers"},
		{name: "short final data", tail: bytes.Repeat([]byte{0xEF}, 37)},
		{name: "short final hole", tail: make([]byte, 37)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := append(append([]byte(nil), fullPayload...), tc.tail...)
			assertSparseCopy(t, payload, bytes.NewReader(payload))
		})
	}
	t.Run("short reads ending with data and EOF", func(t *testing.T) {
		assertSparseCopy(t, fullPayload, &sparseTestReader{
			data: fullPayload, chunkSize: 64*1024 + 3, finalErr: io.EOF,
		})
	})
}

func TestSparseCopyZeroRunsAtByteOffsets(t *testing.T) {
	for zeroOffset := 0; zeroOffset <= 16; zeroOffset++ {
		t.Run(strconv.Itoa(zeroOffset), func(t *testing.T) {
			// Exercise each byte within two words, including high-bit data, so
			// a word containing both data and zeros cannot hide the zero run.
			payload := bytes.Repeat([]byte{0x80, 0x01, 0xff}, 6)[:zeroOffset]
			payload = append(payload, make([]byte, sparseHoleThreshold+7)...)
			payload = append(payload, bytes.Repeat([]byte{0xff, 0x80, 0x01}, 7)...)
			assertSparseCopy(t, payload, bytes.NewReader(payload))
		})
	}
}

func BenchmarkSparseCopy(b *testing.B) {
	const size = 4 * 1024 * 1024
	dense := bytes.Repeat([]byte{0xAB}, size)
	mixed := bytes.Clone(dense)
	clear(mixed[512*1024+3 : 3*1024*1024+7])
	for _, tc := range []struct {
		name    string
		payload []byte
	}{
		{name: "dense", payload: dense},
		{name: "mixed", payload: mixed},
		{name: "zeros", payload: make([]byte, size)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			f, err := os.CreateTemp(b.TempDir(), "sparse-bench")
			if err != nil {
				b.Fatal(err)
			}
			defer f.Close()
			hasher := sha256.New()
			b.SetBytes(int64(len(tc.payload)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := f.Truncate(0); err != nil {
					b.Fatal(err)
				}
				if _, err := f.Seek(0, io.SeekStart); err != nil {
					b.Fatal(err)
				}
				hasher.Reset()
				if _, err := sparseCopy(f, hasher, bytes.NewReader(tc.payload), 1024*1024); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// sparseTestReader returns short reads and delivers the terminal error with the
// last data, including when that data exactly fills the caller's buffer.
type sparseTestReader struct {
	data      []byte
	chunkSize int
	finalErr  error
}

func (r *sparseTestReader) Read(p []byte) (int, error) {
	n := copy(p, r.data[:min(len(r.data), r.chunkSize)])
	r.data = r.data[n:]
	if len(r.data) == 0 {
		return n, r.finalErr
	}
	return n, nil
}

func TestSparseCopyPropagatesUnexpectedEOF(t *testing.T) {
	for _, tc := range []struct {
		name string
		size int
	}{
		{name: "without data"},
		{name: "with short data", size: 17},
		{name: "with full buffer", size: 1024 * 1024},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := bytes.Repeat([]byte{0xAB}, tc.size)
			f, err := os.CreateTemp(t.TempDir(), "truncated")
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			src := &sparseTestReader{data: payload, chunkSize: 1024 * 1024, finalErr: io.ErrUnexpectedEOF}
			n, err := sparseCopy(f, nil, src, 1024*1024)
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("error = %v, want ErrUnexpectedEOF", err)
			}
			if n != int64(tc.size) {
				t.Fatalf("bytes read = %d, want %d", n, tc.size)
			}
		})
	}
}

func assertSparseCopy(t *testing.T, fullPayload []byte, src io.Reader) {
	t.Helper()
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
	n, err := sparseCopy(f, hasher, src, 1024*1024)
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
