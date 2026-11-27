// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	cubehost "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubehost/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	protocubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

const writableDiskReclaimAnnotationKeyTest = "cube.master.rootfs.writable_disk_reclaim"

type reclaimDiskEntry struct {
	HostPath         string   `json:"path"`
	Discard          *bool    `json:"discard"`
	VolumeSourceName string   `json:"volume_source"`
	Options          []string `json:"options"`
}

func newReclaimTestSandbox(hostImage *cubehost.HostImage) *cubeboxstore.CubeBox {
	ctr := &cubeboxstore.Container{
		Metadata:    cubeboxstore.Metadata{ID: "ctr-wdr"},
		Snapshotter: "overlayfs",
		HostImage:   hostImage,
	}
	return &cubeboxstore.CubeBox{
		Metadata:           cubeboxstore.Metadata{ID: "sb-wdr"},
		FirstContainerName: "ctr-wdr",
		ContainersMap: &cubeboxstore.ContainersMap{
			ContainerMap: map[string]*cubeboxstore.Container{"ctr-wdr": ctr},
		},
	}
}

func newReclaimTestOpts(annotationValue string, annotationPresent bool, volumes map[string]*storage.BackendFileInfo) *workflow.CreateContext {
	annotations := map[string]string{}
	if annotationPresent {
		annotations[writableDiskReclaimAnnotationKeyTest] = annotationValue
	}
	var storageInfo interface{}
	if volumes != nil {
		storageInfo = &storage.StorageInfo{Volumes: volumes}
	}
	return &workflow.CreateContext{
		ReqInfo:     &protocubebox.RunCubeSandboxRequest{Annotations: annotations},
		StorageInfo: storageInfo,
	}
}

func reclaimTestVolumes(includeRoot bool) map[string]*storage.BackendFileInfo {
	return reclaimTestVolumesWithRootType(includeRoot, "ext4")
}

func reclaimTestVolumesWithRootType(includeRoot bool, rootType string) map[string]*storage.BackendFileInfo {
	volumes := map[string]*storage.BackendFileInfo{
		"data": {Name: "data", FilePath: "/dev/mapper/data", Type: "xfs"},
	}
	if includeRoot {
		volumes["cube_rootfs_rw"] = &storage.BackendFileInfo{
			Name:      "cube_rootfs_rw",
			FilePath:  "/dev/mapper/root",
			Type:      rootType,
			SizeLimit: 20 << 30,
		}
	}
	return volumes
}

func readReclaimDisks(t *testing.T, opts *workflow.CreateContext, sandbox *cubeboxstore.CubeBox) []reclaimDiskEntry {
	t.Helper()
	specOpts, err := (&local{}).genStorageMediumDefaultAnnotationOpt(context.Background(), opts, sandbox)
	if err != nil {
		t.Fatalf("genStorageMediumDefaultAnnotationOpt: %v", err)
	}
	spec := &specs.Spec{}
	for _, opt := range specOpts {
		if err := opt(context.Background(), nil, nil, spec); err != nil {
			t.Fatalf("apply spec opt: %v", err)
		}
	}
	raw, ok := spec.Annotations[constants.AnnotationsMountListKey]
	if !ok || raw == "" {
		return nil
	}
	var disks []reclaimDiskEntry
	if err := json.Unmarshal([]byte(raw), &disks); err != nil {
		t.Fatalf("decode %s=%q: %v", constants.AnnotationsMountListKey, raw, err)
	}
	return disks
}

func reclaimDiskByPath(t *testing.T, disks []reclaimDiskEntry, path string) reclaimDiskEntry {
	t.Helper()
	for _, disk := range disks {
		if disk.HostPath == path {
			return disk
		}
	}
	t.Fatalf("disk %q not found in %+v", path, disks)
	return reclaimDiskEntry{}
}

func TestGenStorageMediumDefaultAnnotationDiscardsOnlyRootWritableVolume(t *testing.T) {
	image := &cubehost.HostImage{ImageDevs: &cubehost.ImageDev{HostDevicePath: "/dev/nbd0"}}
	opts := newReclaimTestOpts("true", true, reclaimTestVolumes(true))
	disks := readReclaimDisks(t, opts, newReclaimTestSandbox(image))

	root := reclaimDiskByPath(t, disks, "/dev/mapper/root")
	if root.Discard == nil || !*root.Discard {
		t.Fatalf("root writable volume discard=%v, want true", root.Discard)
	}
	if root.VolumeSourceName != "cube_rootfs_rw" {
		t.Fatalf("root volume source=%q, want cube_rootfs_rw", root.VolumeSourceName)
	}

	if data := reclaimDiskByPath(t, disks, "/dev/mapper/data"); data.Discard != nil {
		t.Fatalf("data volume must not be discarded, got %v", *data.Discard)
	}
	if imageDisk := reclaimDiskByPath(t, disks, "/dev/nbd0"); imageDisk.Discard != nil {
		t.Fatalf("read-only image disk must not be discarded, got %v", *imageDisk.Discard)
	}
}

func TestGenStorageMediumDefaultAnnotationLeavesDisabledDisksUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name     string
		value    string
		present  bool
		volumes  map[string]*storage.BackendFileInfo
		hasImage bool
	}{
		{name: "annotation false", value: "false", present: true, volumes: reclaimTestVolumes(true), hasImage: true},
		{name: "annotation absent", volumes: reclaimTestVolumes(true), hasImage: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var image *cubehost.HostImage
			if tc.hasImage {
				image = &cubehost.HostImage{ImageDevs: &cubehost.ImageDev{HostDevicePath: "/dev/nbd0"}}
			}
			disks := readReclaimDisks(t, newReclaimTestOpts(tc.value, tc.present, tc.volumes), newReclaimTestSandbox(image))
			if len(disks) == 0 {
				t.Fatal("expected historical disk config to remain present")
			}
			for _, disk := range disks {
				if disk.Discard != nil {
					t.Fatalf("disabled policy must not emit discard for %q, got %v", disk.HostPath, *disk.Discard)
				}
			}
		})
	}
}

func TestGenStorageMediumDefaultAnnotationRejectsInvalidAndIncompleteReclaim(t *testing.T) {
	for _, tc := range []struct {
		name       string
		value      string
		volumes    map[string]*storage.BackendFileInfo
		errContain string
	}{
		{name: "invalid value", value: "yes", volumes: reclaimTestVolumes(true), errContain: "writable"},
		{name: "missing root volume", value: "true", volumes: reclaimTestVolumes(false), errContain: "cube_rootfs_rw"},
		{name: "no storage info", value: "true", volumes: nil, errContain: "cube_rootfs_rw"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := (&local{}).genStorageMediumDefaultAnnotationOpt(
				context.Background(),
				newReclaimTestOpts(tc.value, true, tc.volumes),
				newReclaimTestSandbox(nil),
			)
			if err == nil {
				t.Fatal("expected an error rather than silently ignoring the enabled policy")
			}
			if !strings.Contains(strings.ToLower(err.Error()), tc.errContain) {
				t.Fatalf("error %q must mention %q", err.Error(), tc.errContain)
			}
		})
	}
}

func TestGenStorageMediumDefaultAnnotationRejectsEnabledNonExt4Root(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rootType string
	}{
		{name: "xfs", rootType: "xfs"},
		{name: "empty", rootType: ""},
		{name: "other", rootType: "btrfs"},
		{name: "uppercase ext4", rootType: "EXT4"},
		{name: "padded ext4", rootType: " ext4 "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := (&local{}).genStorageMediumDefaultAnnotationOpt(
				context.Background(),
				newReclaimTestOpts("true", true, reclaimTestVolumesWithRootType(true, tc.rootType)),
				newReclaimTestSandbox(nil),
			)
			if err == nil {
				t.Fatal("expected enabled reclaim to reject a non-ext4 guest root")
			}
			if !strings.Contains(strings.ToLower(err.Error()), "ext4") {
				t.Fatalf("error %q must mention ext4", err.Error())
			}
		})
	}
}

func TestGenStorageMediumDefaultAnnotationDisabledLeavesNonExt4RootUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   string
		present bool
	}{
		{name: "annotation false", value: "false", present: true},
		{name: "annotation absent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			disks := readReclaimDisks(t,
				newReclaimTestOpts(tc.value, tc.present, reclaimTestVolumesWithRootType(true, "xfs")),
				newReclaimTestSandbox(nil))
			if len(disks) == 0 {
				t.Fatal("expected historical disk config to remain present")
			}
			for _, disk := range disks {
				if disk.Discard != nil {
					t.Fatalf("disabled policy must not emit discard for %q, got %v", disk.HostPath, *disk.Discard)
				}
			}
		})
	}
}

func TestGenStorageMediumDefaultAnnotationDisabledWithoutStorageInfoStaysUnchanged(t *testing.T) {
	disks := readReclaimDisks(t, newReclaimTestOpts("", false, nil), newReclaimTestSandbox(nil))
	if len(disks) != 0 {
		t.Fatalf("rootless create with disabled reclaim must stay unchanged, got %+v", disks)
	}
}
