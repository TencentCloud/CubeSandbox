package s3store

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/pkgs/blobstore"
)

func TestStoredObjectKeyLifecycle(t *testing.T) {
	root := t.TempDir()
	client, err := NewClient(Config{Driver: "fs", FSRoot: root, ArtifactPrefix: "artifacts"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.store.Close()
	src := filepath.Join(t.TempDir(), "local.ext4")
	data := []byte("rootfs content")
	if err := os.WriteFile(src, data, 0600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	key := "artifacts/custom/build-identity.img"
	got, err := client.Upload(ctx, key, src, "")
	if err != nil || got != key {
		t.Fatalf("upload key=%q err=%v", got, err)
	}
	object, err := client.store.Get(ctx, "custom/build-identity.img", blobstore.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer object.Body.Close()
	dataOnDisk, err := io.ReadAll(object.Body)
	if err != nil || string(dataOnDisk) != string(data) {
		t.Fatalf("stored content=%q err=%v", dataOnDisk, err)
	}

	if ok, err := client.Stat(ctx, key); err != nil || !ok {
		t.Fatalf("stat=%v err=%v", ok, err)
	}
	if locator, err := client.PresignedGetURL(ctx, key); !errors.Is(err, blobstore.ErrUnsupported) {
		t.Fatalf("locator=%q err=%v", locator, err)
	}
	if err := client.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if ok, err := client.Stat(ctx, key); err != nil || ok {
		t.Fatalf("deleted stat=%v err=%v", ok, err)
	}
	const foreignKey = "foreign/path.img"
	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{"upload", func() error { _, err := client.Upload(ctx, foreignKey, src, ""); return err }},
		{"stat", func() error { _, err := client.Stat(ctx, foreignKey); return err }},
		{"presign", func() error { _, err := client.PresignedGetURL(ctx, foreignKey); return err }},
		{"delete", func() error { return client.Delete(ctx, foreignKey) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			if !errors.Is(err, blobstore.ErrInvalidKey) || !strings.Contains(err.Error(), foreignKey) || !strings.Contains(err.Error(), "artifacts") {
				t.Fatalf("expected identifiable foreign-prefix error, got %v", err)
			}
		})
	}
}
