// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package handle

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestReadKVFileParsesPairs(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "cpu.stat",
		"usage_usec 1250000\nuser_usec 500000\nsystem_usec 750000\n")

	kv, err := ReadKVFile(dir, "cpu.stat")
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]uint64{
		"usage_usec":  1250000,
		"user_usec":   500000,
		"system_usec": 750000,
	} {
		if kv[key] != want {
			t.Errorf("kv[%q] = %d, want %d", key, kv[key], want)
		}
	}
}

func TestReadKVFileMissingFileIsZeroAndNoError(t *testing.T) {
	kv, err := ReadKVFile(t.TempDir(), "memory.events")
	if err != nil {
		t.Fatalf("missing file must not error: %v", err)
	}
	if kv != nil {
		t.Errorf("missing file must return nil map, got %v", kv)
	}
}

func TestReadKVFileRejectsMalformedLines(t *testing.T) {
	cases := map[string]string{
		"one field":         "usage_usec\n",
		"three fields":      "usage_usec 1 2\n",
		"non-numeric value": "usage_usec abc\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, "cpu.stat", content)
			if _, err := ReadKVFile(dir, "cpu.stat"); err == nil {
				t.Errorf("malformed %q must error", content)
			}
		})
	}
}

func TestReadUintFile(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    uint64
	}{
		{"plain number", "4096\n", 4096},
		{"max is unlimited", "max\n", ^uint64(0)},
		{"negative is zero", "-1\n", 0},
		{"no trailing newline", "123", 123},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, "memory.max", tc.content)
			got, err := ReadUintFile(dir, "memory.max")
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("ReadUintFile = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestReadUintFileMissingFileIsZeroAndNoError(t *testing.T) {
	got, err := ReadUintFile(t.TempDir(), "memory.current")
	if err != nil {
		t.Fatalf("missing file must not error: %v", err)
	}
	if got != 0 {
		t.Errorf("missing file must be 0, got %d", got)
	}
}

func TestReadUintFileRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "memory.max", "banana\n")
	if _, err := ReadUintFile(dir, "memory.max"); err == nil {
		t.Error("malformed content must error")
	}
}

func TestCpuacctTicksToNanoseconds(t *testing.T) {
	// ticks * 1e9 / 100.
	if got := CpuacctTicksToNanoseconds(250); got != 2500000000 {
		t.Errorf("ticks conversion = %d, want 2500000000", got)
	}
	if got := CpuacctTicksToNanoseconds(0); got != 0 {
		t.Errorf("zero ticks = %d, want 0", got)
	}
}
