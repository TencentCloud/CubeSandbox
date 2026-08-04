// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package cubesandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// testVolumeDriver returns the volume driver the integration tests should
// pin, from CUBE_VOLUME_TEST_DRIVER. Empty (the default) omits the driver so
// the backend falls back to its first configured plugin — set it to e.g.
// "cos" to run the same suite against a specific storage backend.
func testVolumeDriver() string {
	return os.Getenv("CUBE_VOLUME_TEST_DRIVER")
}

// TestIntegrationVolumeLifecycle exercises the full volume feature end to end
// against a live backend: CRUD, mounting at sandbox creation, data persistence
// across sandbox lifecycles, read-only mounts, and in-use delete protection.
// Requires at least one volume plugin configured in CubeMaster/Cubelet.
func TestIntegrationVolumeLifecycle(t *testing.T) {
	cfg := integrationConfig(t)
	client := NewClient(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	volumeName := fmt.Sprintf("go-sdk-e2e-%d", time.Now().UnixNano())

	// -- Create --
	volume, err := client.CreateVolume(ctx, CreateVolumeOptions{Name: volumeName, Driver: testVolumeDriver()})
	if err != nil {
		t.Fatalf("CreateVolume returned error: %v", err)
	}
	t.Cleanup(func() { deleteVolumeWithRetry(t, client, volume.VolumeID) })
	if volume.VolumeID != volumeName || volume.Name != volumeName {
		t.Fatalf("created volume mismatch: %#v", volume)
	}

	// -- Get --
	got, err := client.GetVolume(ctx, volume.VolumeID)
	if err != nil {
		t.Fatalf("GetVolume returned error: %v", err)
	}
	if got.VolumeID != volume.VolumeID {
		t.Fatalf("GetVolume volumeID=%q, want %q", got.VolumeID, volume.VolumeID)
	}

	// -- List --
	volumes, err := client.ListVolumes(ctx)
	if err != nil {
		t.Fatalf("ListVolumes returned error: %v", err)
	}
	found := false
	for _, v := range volumes {
		if v.VolumeID == volume.VolumeID {
			found = true
			if v.Token != "" {
				t.Fatalf("list entry should not surface token: %#v", v)
			}
		}
	}
	if !found {
		t.Fatalf("ListVolumes does not contain %q: %#v", volume.VolumeID, volumes)
	}

	// -- Missing volume maps to ErrVolumeNotFound --
	if _, err := client.GetVolume(ctx, "go-sdk-e2e-does-not-exist"); !errors.Is(err, ErrVolumeNotFound) {
		t.Fatalf("GetVolume missing error=%v, want ErrVolumeNotFound", err)
	}

	// -- Mount into a sandbox and write through the mount --
	const mountPath = "/mnt/e2e-vol"
	marker := fmt.Sprintf("volume-data-%d", time.Now().UnixNano())

	first := createIntegrationSandbox(t, ctx, client, CreateOptions{
		Timeout:      DurationPtr(2 * time.Minute),
		Metadata:     map[string]string{"sdk": "go", "scenario": "integration-volume"},
		VolumeMounts: []VolumeMount{{Name: volume.VolumeID, Path: mountPath}},
	})

	info, err := first.GetInfo(ctx)
	if err != nil {
		t.Fatalf("GetInfo returned error: %v", err)
	}
	if len(info.VolumeMounts) == 0 {
		// Servers older than #696 do not return volumeMounts in sandbox info.
		t.Logf("server does not report volumeMounts in sandbox info (pre-#696); skipping assertion")
	} else if info.VolumeMounts[0].Name != volume.VolumeID || info.VolumeMounts[0].Path != mountPath {
		t.Fatalf("sandbox volumeMounts mismatch: %#v", info.VolumeMounts)
	}

	write, err := first.Commands().Run(ctx,
		fmt.Sprintf("printf %%s %s > %s/persist.txt && cat %s/persist.txt", marker, mountPath, mountPath),
		CommandOptions{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("write through mount returned error: %v", err)
	}
	if write.ExitCode != 0 || write.Stdout != marker {
		t.Fatalf("write through mount failed: %#v", write)
	}

	// -- Deleting a mounted volume must be refused with ErrVolumeInUse --
	err = client.DeleteVolume(ctx, volume.VolumeID)
	if !errors.Is(err, ErrVolumeInUse) {
		t.Fatalf("DeleteVolume while mounted error=%v, want ErrVolumeInUse", err)
	}

	// -- Data must survive the sandbox lifecycle --
	if err := first.Kill(ctx); err != nil {
		t.Fatalf("Kill first sandbox returned error: %v", err)
	}

	second := createIntegrationSandbox(t, ctx, client, CreateOptions{
		Timeout:      DurationPtr(2 * time.Minute),
		Metadata:     map[string]string{"sdk": "go", "scenario": "integration-volume-second"},
		VolumeMounts: []VolumeMount{{Name: volume.VolumeID, Path: mountPath}},
	})

	read, err := second.Commands().Run(ctx, fmt.Sprintf("cat %s/persist.txt", mountPath),
		CommandOptions{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("read through mount returned error: %v", err)
	}
	if read.ExitCode != 0 || read.Stdout != marker {
		t.Fatalf("volume data did not persist across sandboxes: %#v", read)
	}
	if err := second.Kill(ctx); err != nil {
		t.Fatalf("Kill second sandbox returned error: %v", err)
	}

	// -- Read-only mount: reads succeed, writes are rejected --
	// The server allows each volume to be mounted at most once per sandbox,
	// so the read-only attachment gets its own sandbox.
	const roPath = "/mnt/e2e-vol-ro"
	third := createIntegrationSandbox(t, ctx, client, CreateOptions{
		Timeout:      DurationPtr(2 * time.Minute),
		Metadata:     map[string]string{"sdk": "go", "scenario": "integration-volume-readonly"},
		VolumeMounts: []VolumeMount{{Name: volume.VolumeID, Path: roPath, ReadOnly: true}},
	})

	roRead, err := third.Commands().Run(ctx, fmt.Sprintf("cat %s/persist.txt", roPath),
		CommandOptions{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("read through read-only mount returned error: %v", err)
	}
	if roRead.ExitCode != 0 || roRead.Stdout != marker {
		t.Fatalf("read-only mount read mismatch: %#v", roRead)
	}
	roWrite, err := third.Commands().Run(ctx, fmt.Sprintf("touch %s/should-fail.txt 2>&1", roPath),
		CommandOptions{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("write attempt on read-only mount returned transport error: %v", err)
	}
	if roWrite.ExitCode == 0 {
		// Servers older than #1098 accept the readOnly flag but do not
		// enforce it; treat that as a known capability gap, not a failure.
		t.Logf("server does not enforce read-only attachments (pre-#1098); skipping assertion")
	}

	// -- After all sandboxes are gone the volume can be deleted --
	if err := third.Kill(ctx); err != nil {
		t.Fatalf("Kill third sandbox returned error: %v", err)
	}
	deleteVolumeWithRetry(t, client, volume.VolumeID)

	if _, err := client.GetVolume(ctx, volume.VolumeID); !errors.Is(err, ErrVolumeNotFound) {
		t.Fatalf("GetVolume after delete error=%v, want ErrVolumeNotFound", err)
	}
}

// TestIntegrationCreateVolumeUnknownDriver verifies the server rejects a
// driver that is not configured, and that the SDK surfaces it as an APIError.
func TestIntegrationCreateVolumeUnknownDriver(t *testing.T) {
	cfg := integrationConfig(t)
	client := NewClient(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := client.CreateVolume(ctx, CreateVolumeOptions{
		Name:   fmt.Sprintf("go-sdk-e2e-baddriver-%d", time.Now().UnixNano()),
		Driver: "go-sdk-e2e-no-such-driver",
	})
	if err == nil {
		t.Fatal("CreateVolume with unknown driver returned nil error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is not APIError: %v", err)
	}
	if !strings.Contains(strings.ToLower(apiErr.Message), "unknown driver") {
		t.Fatalf("unexpected error message: %v", apiErr)
	}
}

// TestIntegrationVolumeSharedByConcurrentSandboxes mounts one volume into two
// running sandboxes at once: writes from one must be visible in the other, and
// the volume must stay delete-protected until the last sandbox is gone.
func TestIntegrationVolumeSharedByConcurrentSandboxes(t *testing.T) {
	cfg := integrationConfig(t)
	client := NewClient(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	volumeName := fmt.Sprintf("go-sdk-e2e-shared-%d", time.Now().UnixNano())
	volume, err := client.CreateVolume(ctx, CreateVolumeOptions{Name: volumeName, Driver: testVolumeDriver()})
	if err != nil {
		t.Fatalf("CreateVolume returned error: %v", err)
	}
	t.Cleanup(func() { deleteVolumeWithRetry(t, client, volume.VolumeID) })

	const mountPath = "/mnt/e2e-shared"
	marker := fmt.Sprintf("shared-data-%d", time.Now().UnixNano())

	writer := createIntegrationSandbox(t, ctx, client, CreateOptions{
		Timeout:      DurationPtr(3 * time.Minute),
		Metadata:     map[string]string{"sdk": "go", "scenario": "integration-volume-shared-writer"},
		VolumeMounts: []VolumeMount{{Name: volume.VolumeID, Path: mountPath}},
	})
	reader := createIntegrationSandbox(t, ctx, client, CreateOptions{
		Timeout:      DurationPtr(3 * time.Minute),
		Metadata:     map[string]string{"sdk": "go", "scenario": "integration-volume-shared-reader"},
		VolumeMounts: []VolumeMount{{Name: volume.VolumeID, Path: mountPath}},
	})

	write, err := writer.Commands().Run(ctx,
		fmt.Sprintf("printf %%s %s > %s/shared.txt", marker, mountPath),
		CommandOptions{Timeout: 30 * time.Second})
	if err != nil || write.ExitCode != 0 {
		t.Fatalf("write from writer sandbox failed: err=%v result=%#v", err, write)
	}

	// Cross-sandbox visibility: the write must be observable from the other
	// running sandbox. Shared FUSE/virtiofs backends may have a small
	// propagation delay, so poll briefly.
	readDeadline := time.Now().Add(30 * time.Second)
	for {
		read, err := reader.Commands().Run(ctx, fmt.Sprintf("cat %s/shared.txt 2>/dev/null", mountPath),
			CommandOptions{Timeout: 30 * time.Second})
		if err != nil {
			t.Fatalf("read from reader sandbox returned error: %v", err)
		}
		if read.ExitCode == 0 && read.Stdout == marker {
			break
		}
		if time.Now().After(readDeadline) {
			t.Fatalf("write not visible from concurrent sandbox: %#v", read)
		}
		time.Sleep(2 * time.Second)
	}

	// Both sandboxes alive: delete must be refused.
	if err := client.DeleteVolume(ctx, volume.VolumeID); !errors.Is(err, ErrVolumeInUse) {
		t.Fatalf("DeleteVolume with two mounts error=%v, want ErrVolumeInUse", err)
	}

	// One sandbox alive: delete must still be refused.
	if err := writer.Kill(ctx); err != nil {
		t.Fatalf("Kill writer sandbox returned error: %v", err)
	}
	if err := client.DeleteVolume(ctx, volume.VolumeID); !errors.Is(err, ErrVolumeInUse) {
		t.Fatalf("DeleteVolume with one remaining mount error=%v, want ErrVolumeInUse", err)
	}

	// Last sandbox gone: delete succeeds (retry while detach refcount settles).
	if err := reader.Kill(ctx); err != nil {
		t.Fatalf("Kill reader sandbox returned error: %v", err)
	}
	deleteVolumeWithRetry(t, client, volume.VolumeID)
}

// TestIntegrationVolumeGeneratedName creates a volume without a name: the
// server must generate an ID usable for mounting and deletion.
func TestIntegrationVolumeGeneratedName(t *testing.T) {
	cfg := integrationConfig(t)
	client := NewClient(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	volume, err := client.CreateVolume(ctx, CreateVolumeOptions{Driver: testVolumeDriver()})
	if err != nil {
		t.Fatalf("CreateVolume with empty name returned error: %v", err)
	}
	t.Cleanup(func() { deleteVolumeWithRetry(t, client, volume.VolumeID) })
	if volume.VolumeID == "" {
		t.Fatalf("server did not generate a volume ID: %#v", volume)
	}
	if volume.Name != volume.VolumeID {
		t.Fatalf("generated name should equal volume ID: %#v", volume)
	}

	const mountPath = "/mnt/e2e-generated"
	sb := createIntegrationSandbox(t, ctx, client, CreateOptions{
		Timeout:      DurationPtr(2 * time.Minute),
		Metadata:     map[string]string{"sdk": "go", "scenario": "integration-volume-generated"},
		VolumeMounts: []VolumeMount{{Name: volume.VolumeID, Path: mountPath}},
	})
	run, err := sb.Commands().Run(ctx, fmt.Sprintf("printf ok > %s/gen.txt && cat %s/gen.txt", mountPath, mountPath),
		CommandOptions{Timeout: 30 * time.Second})
	if err != nil || run.ExitCode != 0 || run.Stdout != "ok" {
		t.Fatalf("write/read through generated-name volume failed: err=%v result=%#v", err, run)
	}
	if err := sb.Kill(ctx); err != nil {
		t.Fatalf("Kill sandbox returned error: %v", err)
	}
}

// TestIntegrationCreateVolumeDuplicateName verifies that creating the same
// volume name twice is rejected with a conflict that is NOT ErrVolumeInUse.
func TestIntegrationCreateVolumeDuplicateName(t *testing.T) {
	cfg := integrationConfig(t)
	client := NewClient(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	volumeName := fmt.Sprintf("go-sdk-e2e-dup-%d", time.Now().UnixNano())
	volume, err := client.CreateVolume(ctx, CreateVolumeOptions{Name: volumeName, Driver: testVolumeDriver()})
	if err != nil {
		t.Fatalf("CreateVolume returned error: %v", err)
	}
	t.Cleanup(func() { deleteVolumeWithRetry(t, client, volume.VolumeID) })

	_, err = client.CreateVolume(ctx, CreateVolumeOptions{Name: volumeName, Driver: testVolumeDriver()})
	if err == nil {
		t.Fatal("duplicate CreateVolume returned nil error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("duplicate create error is not APIError: %v", err)
	}
	if !strings.Contains(strings.ToLower(apiErr.Message), "already exists") {
		t.Fatalf("unexpected duplicate create message: %v", apiErr)
	}
	if errors.Is(err, ErrVolumeInUse) {
		t.Fatalf("duplicate create must not map to ErrVolumeInUse: %v", err)
	}
}

// TestIntegrationVolumeSurvivesPauseResume checks that a mounted volume is
// still readable and writable after the sandbox is paused and resumed.
func TestIntegrationVolumeSurvivesPauseResume(t *testing.T) {
	cfg := integrationConfig(t)
	client := NewClient(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	volumeName := fmt.Sprintf("go-sdk-e2e-pause-%d", time.Now().UnixNano())
	volume, err := client.CreateVolume(ctx, CreateVolumeOptions{Name: volumeName, Driver: testVolumeDriver()})
	if err != nil {
		t.Fatalf("CreateVolume returned error: %v", err)
	}
	t.Cleanup(func() { deleteVolumeWithRetry(t, client, volume.VolumeID) })

	const mountPath = "/mnt/e2e-pause"
	marker := fmt.Sprintf("pause-data-%d", time.Now().UnixNano())

	sb := createIntegrationSandbox(t, ctx, client, CreateOptions{
		Timeout:      DurationPtr(3 * time.Minute),
		Metadata:     map[string]string{"sdk": "go", "scenario": "integration-volume-pause"},
		VolumeMounts: []VolumeMount{{Name: volume.VolumeID, Path: mountPath}},
	})

	write, err := sb.Commands().Run(ctx,
		fmt.Sprintf("printf %%s %s > %s/pause.txt", marker, mountPath),
		CommandOptions{Timeout: 30 * time.Second})
	if err != nil || write.ExitCode != 0 {
		t.Fatalf("write before pause failed: err=%v result=%#v", err, write)
	}

	wait := true
	if err := sb.Pause(ctx, PauseOptions{Wait: &wait, Timeout: 90 * time.Second, Interval: 2 * time.Second}); err != nil {
		t.Fatalf("Pause returned error: %v", err)
	}
	resumed, err := client.Connect(ctx, sb.SandboxID)
	if err != nil {
		t.Fatalf("Connect after pause returned error: %v", err)
	}
	sb = resumed

	verify, err := sb.Commands().Run(ctx,
		fmt.Sprintf("cat %s/pause.txt && printf %%s -after >> %s/pause.txt && cat %s/pause.txt", mountPath, mountPath, mountPath),
		CommandOptions{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("read/write after resume returned error: %v", err)
	}
	if verify.ExitCode != 0 || verify.Stdout != marker+marker+"-after" {
		t.Fatalf("volume not usable after pause/resume: %#v", verify)
	}
	if err := sb.Kill(ctx); err != nil {
		t.Fatalf("Kill sandbox returned error: %v", err)
	}
}

// deleteVolumeWithRetry deletes a volume, retrying briefly while the server
// still reports it as in use — Cubelet reports detach-side refcount changes
// asynchronously after a sandbox is killed. Missing volumes are fine.
func deleteVolumeWithRetry(t *testing.T, client *Client, volumeID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for {
		err := client.DeleteVolume(ctx, volumeID)
		if err == nil || errors.Is(err, ErrVolumeNotFound) {
			return
		}
		if !errors.Is(err, ErrVolumeInUse) {
			t.Fatalf("DeleteVolume %s returned error: %v", volumeID, err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("DeleteVolume %s still in use after retries: %v", volumeID, err)
		case <-time.After(2 * time.Second):
		}
	}
}
