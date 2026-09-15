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

func TestArtifactExt4Key(t *testing.T) {
	if g, w := ArtifactExt4Key("", "abc"), "abc.ext4"; g != w {
		t.Fatalf("got %q want %q", g, w)
	}
	if g, w := ArtifactExt4Key("template-artifacts/", "abc"), "template-artifacts/abc.ext4"; g != w {
		t.Fatalf("got %q want %q", g, w)
	}
	if g, w := ArtifactExt4Key("template-artifacts", "abc"), "template-artifacts/abc.ext4"; g != w {
		t.Fatalf("got %q want %q", g, w)
	}
	if g, w := ArtifactExt4Key("  ", "rfs-1"), "rfs-1.ext4"; g != w {
		t.Fatalf("got %q want %q", g, w)
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
