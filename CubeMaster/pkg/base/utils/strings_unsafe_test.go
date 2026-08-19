// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package utils

import (
	"bytes"
	"hash/crc32"
	"runtime"
	"testing"
)

func TestString2Slice(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  []byte
	}{
		{"empty string", "", []byte{}},
		{"ascii", "hello world", []byte("hello world")},
		{"utf8", "héllo-世界", []byte("héllo-世界")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := String2Slice(tc.input)
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("String2Slice(%q) = %v, want %v", tc.input, got, tc.want)
			}
			if len(got) != len(tc.input) {
				t.Fatalf("len = %d, want %d", len(got), len(tc.input))
			}
		})
	}
}

func TestString2SliceSurvivesGC(t *testing.T) {
	produce := func() []byte {
		return String2Slice("a-string-that-goes-out-of-scope")
	}

	b := produce()
	runtime.GC()
	runtime.GC()

	if string(b) != "a-string-that-goes-out-of-scope" {
		t.Fatalf("slice contents changed after GC: %q", string(b))
	}
	if crc32.ChecksumIEEE(b) != crc32.ChecksumIEEE([]byte("a-string-that-goes-out-of-scope")) {
		t.Fatal("checksum mismatch after GC")
	}
}
