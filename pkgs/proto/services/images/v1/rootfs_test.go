package images

import (
	"testing"
)

func TestRootfsMetadataCompatibility(t *testing.T) {
	cases := []struct {
		name, wire string
		want       RootfsMetadata
		fail       bool
	}{
		{"legacy", `{"ext4_path":"/old/a.ext4","ext4_sha256":"abc","ext4_size_bytes":4096}`, RootfsMetadata{"ext4", "/old/a.ext4", "abc", 4096}, false},
		{"canonical", `{"rootfs_format":"ext4","rootfs_path":"/a.ext4","rootfs_sha256":"abc","rootfs_size_bytes":4096}`, RootfsMetadata{"ext4", "/a.ext4", "abc", 4096}, false},
		{"both", `{"rootfs_path":"/a.ext4","rootfs_sha256":"abc","rootfs_size_bytes":4096,"ext4_path":"/a.ext4","ext4_sha256":"abc","ext4_size_bytes":4096}`, RootfsMetadata{"ext4", "/a.ext4", "abc", 4096}, false},
		{"partial canonical is not mixed", `{"rootfs_path":"/a.ext4","ext4_path":"/a.ext4","ext4_sha256":"abc"}`, RootfsMetadata{}, true},
		{"path conflict", `{"rootfs_path":"/new.ext4","ext4_path":"/old.ext4"}`, RootfsMetadata{}, true},
		{"size conflict", `{"rootfs_size_bytes":4096,"ext4_size_bytes":8192}`, RootfsMetadata{}, true},
		{"unknown format", `{"rootfs_format":"future"}`, RootfsMetadata{}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := DecodeRootfsMetadata([]byte(c.wire))
			if (err != nil) != c.fail {
				t.Fatalf("err=%v", err)
			}
			if !c.fail && got != c.want {
				t.Fatalf("got %+v want %+v", got, c.want)
			}
		})
	}
}

func TestRootfsFormatSelection(t *testing.T) {
	for _, format := range []string{"", "ext4"} {
		name, err := RootfsFilename("rfs-1", format)
		if err != nil || name != "rfs-1.ext4" {
			t.Fatalf("format=%q name=%q err=%v", format, name, err)
		}
	}
	for _, format := range []string{"docker", "erofs", "unknown"} {
		if _, err := RootfsFilename("rfs-1", format); err == nil {
			t.Fatalf("accepted %q", format)
		}
	}
	if !IsPmemStorageMedia("ext4") || IsPmemStorageMedia("") || IsPmemStorageMedia("docker") {
		t.Fatal("ordinary images must not become PMEM artifacts")
	}
}
