// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package ext4image

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
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
		// Binaries and filesystem metadata contain frequent short zero runs.
		{name: "elf-like", payload: elfLikePayload(size)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			f, err := os.CreateTemp(b.TempDir(), "sparse-bench")
			if err != nil {
				b.Fatal(err)
			}
			defer f.Close()
			dst := &countingSparseFile{File: f}
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
				if _, err := sparseCopy(dst, hasher, bytes.NewReader(tc.payload), 1024*1024, 0); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(dst.writes)/float64(b.N), "writes/op")
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
			n, err := sparseCopy(f, nil, src, 1024*1024, 0)
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("error = %v, want ErrUnexpectedEOF", err)
			}
			if n != int64(tc.size) {
				t.Fatalf("bytes read = %d, want %d", n, tc.size)
			}
		})
	}
}

func assertSparseCopy(t *testing.T, fullPayload []byte, src io.Reader) *countingSparseFile {
	t.Helper()
	dst := copyAndVerify(t, fullPayload, src)
	st, err := dst.Stat()
	if err != nil {
		t.Fatalf("stat failed: %v", err)
	}
	stat, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("unexpected stat type %T", st.Sys())
	}
	allocatedBytes := stat.Blocks * 512
	if allocatedBytes >= st.Size() {
		t.Fatalf("file is not sparse: allocated %d bytes for apparent size %d", allocatedBytes, st.Size())
	}
	return dst
}

// copyAndVerify runs sparseCopy into a temporary file and checks the returned
// length, hash, apparent size and content. It does not require any holes.
func copyAndVerify(t *testing.T, fullPayload []byte, src io.Reader) *countingSparseFile {
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
	t.Cleanup(func() { f.Close() })
	dst := &countingSparseFile{File: f}

	hasher := sha256.New()
	n, err := sparseCopy(dst, hasher, src, 1024*1024, 0)
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

	// Verify content read-back
	readBack, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("os.ReadFile failed: %v", err)
	}
	if !bytes.Equal(readBack, fullPayload) {
		t.Fatalf("read back content does not match original payload")
	}
	return dst
}

// countingSparseFile counts the write and seek calls sparseCopy makes on the
// underlying file. Each one is a syscall on *os.File.
type countingSparseFile struct {
	*os.File
	writes int
	seeks  int
}

func (f *countingSparseFile) Write(p []byte) (int, error) {
	f.writes++
	return f.File.Write(p)
}

func (f *countingSparseFile) Seek(offset int64, whence int) (int64, error) {
	f.seeks++
	return f.File.Seek(offset, whence)
}

// elfLikePayload returns deterministic data resembling executable code and
// ext4 metadata: small little-endian integers and pointers interleaved with
// opcode-like bytes, so short zero runs occur every few bytes.
func elfLikePayload(size int) []byte {
	out := make([]byte, size)
	x := uint32(2463534242)
	for i := 0; i+16 <= size; i += 16 {
		binary.LittleEndian.PutUint32(out[i:], uint32(i/16)%4096)
		binary.LittleEndian.PutUint64(out[i+4:], 0x400000+uint64(i))
		for j := 12; j < 16; j++ {
			x ^= x << 13
			x ^= x >> 17
			x ^= x << 5
			out[i+j] = byte(x)
		}
	}
	return out
}

func TestSparseCopyCoalescesShortZeroRuns(t *testing.T) {
	const bufSize = 1024 * 1024
	elf := elfLikePayload(4 * bufSize)
	short := elf[:bufSize/2+5]

	t.Run("dense", func(t *testing.T) {
		dst := copyAndVerify(t, elf, bytes.NewReader(elf))
		// One write per full buffer. Writing each non-zero run separately
		// would issue hundreds of thousands of writes for this payload.
		if want := len(elf) / bufSize; dst.writes > want {
			t.Fatalf("writes = %d, want at most %d", dst.writes, want)
		}
		if dst.seeks != 0 {
			t.Fatalf("seeks = %d, want 0", dst.seeks)
		}
	})
	t.Run("dense with short reads", func(t *testing.T) {
		src := &sparseTestReader{data: elf, chunkSize: 32*1024 + 7, finalErr: io.EOF}
		dst := copyAndVerify(t, elf, src)
		if want := len(elf)/bufSize + 1; dst.writes > want {
			t.Fatalf("writes = %d, want at most %d", dst.writes, want)
		}
	})
	t.Run("holes still punched", func(t *testing.T) {
		payload := append(bytes.Clone(short), make([]byte, 2*bufSize)...)
		payload = append(payload, short...)
		payload = append(payload, make([]byte, sparseHoleThreshold)...)
		payload = append(payload, short...)
		dst := assertSparseCopy(t, payload, bytes.NewReader(payload))
		if dst.seeks != 2 {
			t.Fatalf("seeks = %d, want 2", dst.seeks)
		}
		// Each data segment fits in one buffer and is flushed before a seek
		// or at the end.
		if dst.writes != 3 {
			t.Fatalf("writes = %d, want 3", dst.writes)
		}
	})
}

func TestSparseCopyEdgeCases(t *testing.T) {
	data := bytes.Repeat([]byte{0x7f, 'E', 'L', 'F'}, 1024)
	withZeros := func(n int64) []byte {
		payload := append(bytes.Clone(data), make([]byte, n)...)
		return append(payload, data...)
	}
	for _, tc := range []struct {
		name      string
		payload   []byte
		chunkSize int // 0 reads through bytes.Reader
		wantSeeks int
	}{
		{name: "empty", payload: []byte{}},
		{name: "all zeros", payload: make([]byte, 3*sparseHoleThreshold+11), wantSeeks: 1},
		{name: "zero run below threshold", payload: withZeros(sparseHoleThreshold - 1)},
		{name: "zero run at threshold", payload: withZeros(sparseHoleThreshold), wantSeeks: 1},
		{name: "zero run above threshold", payload: withZeros(sparseHoleThreshold + 1), wantSeeks: 1},
		{name: "trailing zeros below threshold", payload: append(bytes.Clone(data), make([]byte, sparseHoleThreshold-1)...)},
		{name: "trailing zeros at threshold", payload: append(bytes.Clone(data), make([]byte, sparseHoleThreshold)...), wantSeeks: 1},
		{
			name:      "zero run below threshold across short reads",
			payload:   withZeros(sparseHoleThreshold - 1),
			chunkSize: 4096 + 3,
		},
		{
			name:      "zero run at threshold across short reads",
			payload:   withZeros(sparseHoleThreshold),
			chunkSize: 4096 + 3,
			wantSeeks: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var src io.Reader = bytes.NewReader(tc.payload)
			if tc.chunkSize > 0 {
				src = &sparseTestReader{data: tc.payload, chunkSize: tc.chunkSize, finalErr: io.EOF}
			}
			dst := copyAndVerify(t, tc.payload, src)
			if dst.seeks != tc.wantSeeks {
				t.Fatalf("seeks = %d, want %d", dst.seeks, tc.wantSeeks)
			}
		})
	}
}
