// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package cubesandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// TestIntegrationVolumeDirectContent exercises direct object-storage volume
// content access against a live CubeAPI + CubeMaster + credential-issuing
// plugin (PR #2297+). Set CUBE_API_URL and CUBE_VOLUME_DIRECT_TEST=1 to run;
// optionally set CUBE_VOLUME_TEST_DRIVER (e.g. "cos" or "s3"),
// CUBE_VOLUME_ALLOW_PRIVATE_ENDPOINTS=1, or CUBE_VOLUME_ALLOW_INSECURE_LOOPBACK=1.
func TestIntegrationVolumeDirectContent(t *testing.T) {
	if os.Getenv("CUBE_VOLUME_DIRECT_TEST") != "1" {
		t.Skip("set CUBE_VOLUME_DIRECT_TEST=1 with a live credential-issuing plugin to run direct volume content integration tests")
	}

	cfg := integrationConfig(t)
	client := NewClient(cfg)
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	volumeName := fmt.Sprintf("go-sdk-direct-%d", time.Now().UnixNano())
	volume, err := client.CreateVolume(ctx, CreateVolumeOptions{
		Name:   volumeName,
		Driver: testVolumeDriver(),
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	t.Cleanup(func() { deleteVolumeWithRetry(t, client, volume.VolumeID) })

	var opts []VolumeContentOption
	if os.Getenv("CUBE_VOLUME_ALLOW_PRIVATE_ENDPOINTS") == "1" {
		opts = append(opts, WithVolumeAllowPrivateEndpoints(true))
	}
	if os.Getenv("CUBE_VOLUME_ALLOW_INSECURE_LOOPBACK") == "1" {
		opts = append(opts, WithVolumeAllowInsecureLoopback(true))
	}

	vc, err := client.VolumeContent(volume.VolumeID, opts...)
	if err != nil {
		t.Fatalf("VolumeContent: %v", err)
	}
	defer vc.Close()

	if _, err := vc.MakeDir(ctx, "/nested", VolumeMakeDirOptions{Mode: IntPtr(0o755)}); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}

	payload := fmt.Sprintf("direct-go-sdk-%d", time.Now().UnixNano())
	if _, err := vc.Write(ctx, "/nested/hello.txt", []byte(payload), VolumeWriteOptions{Mode: IntPtr(0o640)}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := vc.Read(ctx, "/nested/hello.txt")
	if err != nil || got != payload {
		t.Fatalf("Read=(%q, %v), want %q", got, err, payload)
	}

	streamPayload := bytes.Repeat([]byte("stream-chunk-"), 1024)
	if _, err := vc.WriteFrom(ctx, "/nested/stream.bin", bytes.NewReader(streamPayload), VolumeWriteOptions{}); err != nil {
		t.Fatalf("WriteFrom: %v", err)
	}
	rc, err := vc.ReadStream(ctx, "/nested/stream.bin")
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}
	streamGot, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || !bytes.Equal(streamGot, streamPayload) {
		t.Fatalf("ReadStream mismatch: len=%d err=%v", len(streamGot), err)
	}

	entries, err := vc.List(ctx, "/nested", VolumeListOptions{Depth: IntPtr(1)})
	if err != nil || len(entries) < 2 {
		t.Fatalf("List(/nested)=(%#v, %v)", entries, err)
	}

	if err := vc.Remove(ctx, "/nested"); !errors.Is(err, ErrVolumeDirNotEmpty) {
		t.Fatalf("Remove non-empty dir err=%v, want ErrVolumeDirNotEmpty", err)
	}

	if err := vc.RemoveAll(ctx, "/nested"); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if exists, err := vc.Exists(ctx, "/nested/hello.txt"); err != nil || exists {
		t.Fatalf("Exists after RemoveAll=(%v, %v)", exists, err)
	}
	if !strings.HasPrefix(volume.VolumeID, "go-sdk-direct-") {
		t.Fatalf("unexpected volumeID %q", volume.VolumeID)
	}
}
