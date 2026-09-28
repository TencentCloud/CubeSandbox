// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReplacePositionalArg(t *testing.T) {
	tests := []struct {
		name string
		args []string
		old  string
		new  string
		want []string
	}{
		{
			name: "short id replaced with full id",
			args: []string{"logs", "aabbccdd"},
			old:  "aabbccdd",
			new:  "aabbccddeeff00112233445566778899",
			want: []string{"logs", "aabbccddeeff00112233445566778899"},
		},
		{
			name: "flags before positional arg",
			args: []string{"logs", "--stderr", "aabbccdd"},
			old:  "aabbccdd",
			new:  "aabbccddeeff00112233445566778899",
			want: []string{"logs", "--stderr", "aabbccddeeff00112233445566778899"},
		},
		{
			name: "flags after positional arg",
			args: []string{"logs", "aabbccdd", "--all"},
			old:  "aabbccdd",
			new:  "aabbccddeeff00112233445566778899",
			want: []string{"logs", "aabbccddeeff00112233445566778899", "--all"},
		},
		{
			name: "multiple flags around positional arg",
			args: []string{"logs", "--stderr", "-t", "50", "aabbccdd"},
			old:  "aabbccdd",
			new:  "aabbccddeeff00112233445566778899",
			want: []string{"logs", "--stderr", "-t", "50", "aabbccddeeff00112233445566778899"},
		},
		{
			name: "no replacement when old equals new",
			args: []string{"logs", "aabbccddeeff00112233445566778899"},
			old:  "aabbccddeeff00112233445566778899",
			new:  "aabbccddeeff00112233445566778899",
			want: []string{"logs", "aabbccddeeff00112233445566778899"},
		},
		{
			name: "no match leaves args unchanged",
			args: []string{"logs", "something"},
			old:  "notfound",
			new:  "aabbccddeeff00112233445566778899",
			want: []string{"logs", "something"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := replacePositionalArg(tt.args, tt.old, tt.new)
			if len(got) != len(tt.want) {
				t.Fatalf("len(got)=%d want=%d", len(got), len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("args[%d]=%q want=%q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestReplacePositionalArgDoesNotModifyOriginal(t *testing.T) {
	original := []string{"logs", "--stderr", "aabbccdd"}
	_ = replacePositionalArg(original, "aabbccdd", "aabbccddeeff00112233445566778899")
	if original[2] != "aabbccdd" {
		t.Fatalf("original slice was modified: got %q want %q", original[2], "aabbccdd")
	}
}

func TestOpenSandboxLogFromPrefersNewPath(t *testing.T) {
	newBase := t.TempDir()
	oldBase := t.TempDir()
	id := "aabbccddeeff00112233445566778899"
	if err := os.MkdirAll(filepath.Join(newBase, id), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(oldBase, id), 0755); err != nil {
		t.Fatal(err)
	}
	newFile := filepath.Join(newBase, id, "stdout")
	oldFile := filepath.Join(oldBase, id, "stdout")
	if err := os.WriteFile(newFile, []byte("new\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldFile, []byte("old\n"), 0644); err != nil {
		t.Fatal(err)
	}

	f, path, err := openSandboxLogFrom(id, "stdout", newBase, oldBase)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if path != newFile {
		t.Fatalf("path=%q want %q", path, newFile)
	}
}

func TestOpenSandboxLogFromFallsBackToBundle(t *testing.T) {
	newBase := t.TempDir()
	oldBase := t.TempDir()
	id := "aabbccddeeff00112233445566778899"
	if err := os.MkdirAll(filepath.Join(oldBase, id), 0755); err != nil {
		t.Fatal(err)
	}
	oldFile := filepath.Join(oldBase, id, "stdout")
	if err := os.WriteFile(oldFile, []byte("old\n"), 0644); err != nil {
		t.Fatal(err)
	}

	f, path, err := openSandboxLogFrom(id, "stdout", newBase, oldBase)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if path != oldFile {
		t.Fatalf("path=%q want %q", path, oldFile)
	}
}

func TestOpenSandboxLogFromMissingBoth(t *testing.T) {
	_, _, err := openSandboxLogFrom("aabbccddeeff00112233445566778899", "stdout", t.TempDir(), t.TempDir())
	if err == nil {
		t.Fatal("expected missing log error")
	}
	if !strings.Contains(err.Error(), "log file not found") {
		t.Fatalf("unexpected error: %v", err)
	}
}
