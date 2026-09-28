// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package warehouse

import "testing"

func TestNormalizeVersionRejectsSeparators(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"a/../b", "../../v2", `win\path`, "v1/../v2", "ok/v1"} {
		if _, err := NormalizeVersion(in); err == nil {
			t.Errorf("NormalizeVersion(%q) succeeded, want error", in)
		}
	}
}

func TestObjectMountPath(t *testing.T) {
	if ObjectMountPath != "/internal/warehouse/object" {
		t.Fatalf("ObjectMountPath=%q", ObjectMountPath)
	}
}

func TestObjectKeyUsesWarehousePrefix(t *testing.T) {
	got := ObjectKey(ArchAMD64, ComponentShim, "v0.6.0")
	want := "warehouse/blobs/amd64/cube-shim/v0.6.0/component.tar.gz"
	if got != want {
		t.Fatalf("ObjectKey=%q want %q", got, want)
	}
	if UploadObjectKey("abc") != "warehouse/uploads/abc.tar.gz" {
		t.Fatalf("UploadObjectKey=%q", UploadObjectKey("abc"))
	}
}

func TestNormalizeVersionAcceptsPlainKeys(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"v0.7.0", "sha256-abcdef123456", "v0.7.0-rc2"} {
		got, err := NormalizeVersion(in)
		if err != nil {
			t.Errorf("NormalizeVersion(%q): %v", in, err)
		}
		if got != in {
			t.Errorf("NormalizeVersion(%q)=%q", in, got)
		}
	}
}
