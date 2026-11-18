// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHookErrorsHideInternalDetails(t *testing.T) {
	const detail = "mount.nfs failed: nfs.internal:/exports/private/volume-1 at /var/lib/cubelet/volumes/private"
	for _, op := range []string{"create", "attach", "detach", "destroy", "unknown"} {
		t.Run(op, func(t *testing.T) {
			res := errorResult(op, errors.New(detail))
			if res.Error == "" || res.ErrorKind != "" {
				t.Fatalf("internal failure lost its classification: %+v", res)
			}
			for _, private := range []string{"nfs.internal", "/exports/", "/var/lib/", "volume-1"} {
				if strings.Contains(res.Error, private) {
					t.Fatalf("private detail in response: %+v", res)
				}
			}
		})
	}
}

func TestCreateErrorClassification(t *testing.T) {
	for _, name := range []string{"invalid-id", "missing-volume", "missing-data", "claimed", "missing-root", "invalid-directory"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			id := "volume-1"
			switch name {
			case "invalid-id":
				id = "../private"
			case "missing-data":
				if err := os.Mkdir(filepath.Join(root, id), 0700); err != nil {
					t.Fatal(err)
				}
			case "claimed":
				if err := os.MkdirAll(filepath.Join(root, id, dataDirName), 0700); err != nil {
					t.Fatal(err)
				}
				if _, err := claimVolume(root, id); err != nil {
					t.Fatal(err)
				}
			case "missing-root":
				root = filepath.Join(root, "not-mounted")
			case "invalid-directory":
				if err := os.WriteFile(filepath.Join(root, id), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("CUBE_NFS_CONTROL_ROOT", root)
			_, err := run(context.Background(), options{op: "create", id: id})
			if err == nil {
				t.Fatal("expected registration error")
			}
			res := errorResult("create", err)
			wantKind := "invalid_argument"
			if name == "missing-root" || name == "invalid-directory" {
				wantKind = ""
			}
			if res.ErrorKind != wantKind {
				t.Fatalf("kind = %q, want %q", res.ErrorKind, wantKind)
			}
			encoded, err := json.Marshal(res)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), root) || strings.Contains(res.Error, "../") {
				t.Fatalf("private path in response: %s", encoded)
			}
		})
	}
}
