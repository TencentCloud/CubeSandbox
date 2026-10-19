// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package blobstore

import "testing"

func TestValidateKey(t *testing.T) {
	ok := []string{
		"a",
		"warehouse/blobs/amd64/cube-shim/v0.6.0/component.tar.gz",
		"abc-123.ext4",
	}
	for _, k := range ok {
		if err := ValidateKey(k); err != nil {
			t.Errorf("ValidateKey(%q) = %v, want nil", k, err)
		}
	}
	bad := []string{
		"",
		"/abs",
		"trailing/",
		"../escape",
		"foo/../bar",
		"foo//bar",
		".hidden",
		"dir/.hidden",
		"foo bar",
		"foo\\bar",
		"..",
	}
	for _, k := range bad {
		if err := ValidateKey(k); err == nil {
			t.Errorf("ValidateKey(%q) = nil, want error", k)
		}
	}
}

func TestValidatePrefixPartial(t *testing.T) {
	if err := ValidatePrefix("warehouse/blobs/amd"); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePrefix(""); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePrefix("../x"); err == nil {
		t.Fatal("expected reject")
	}
}

func TestJoinKey(t *testing.T) {
	if g := JoinKey("a/", "/b"); g != "a/b" {
		t.Fatalf("got %q", g)
	}
}

func TestObjectLocator(t *testing.T) {
	if g, w := ObjectLocator("fs", "template-artifacts/abc.ext4"), "blobstore:fs:template-artifacts/abc.ext4"; g != w {
		t.Fatalf("got %q want %q", g, w)
	}
	if ObjectLocator("", "k") != "" || ObjectLocator("s3", "") != "" {
		t.Fatal("empty inputs must yield empty locator")
	}
	if !IsObjectLocator("blobstore:fs:k") || IsObjectLocator("https://example/k") {
		t.Fatal("IsObjectLocator")
	}
}

func TestRelativeObjectKey(t *testing.T) {
	cases := []struct {
		key, prefix, want string
		fail              bool
	}{
		{"artifacts/nested/custom.img", "artifacts", "nested/custom.img", false},
		{"old.ext4", "artifacts", "old.ext4", false},
		{"custom/file.img", "", "custom/file.img", false},
		{"foreign/file.img", "artifacts", "", true},
		{"artifacts", "artifacts", "", true},
		{"artifacts/../file.img", "artifacts", "", true},
		{"/artifacts/file.img", "artifacts", "", true},
		{"", "artifacts", "", true},
	}
	for _, c := range cases {
		got, err := RelativeObjectKey(c.key, c.prefix)
		if got != c.want || (err != nil) != c.fail {
			t.Fatalf("key=%q prefix=%q got=%q err=%v", c.key, c.prefix, got, err)
		}
	}
}
