// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestPluginVolumeWireVolume_hasNoDefaultEmptyDir(t *testing.T) {
	t.Parallel()

	v := pluginVolumeWireVolume("max-data")
	if v.GetVolumeSource() == nil {
		t.Fatal("VolumeSource must be non-nil so the volume is still declared")
	}
	if v.GetVolumeSource().GetEmptyDir() != nil {
		t.Fatalf("plugin volumes must not use EmptyDir placeholders; old Cubelets "+
			"derive rootfs for StorageMediumDefault EmptyDir and collide with "+
			"cube_rootfs_rw: got %+v", v.GetVolumeSource().GetEmptyDir())
	}
}

func TestAppendPluginVolumeSourceAnnotation_includesPrivateData(t *testing.T) {
	t.Parallel()

	req := &types.CreateCubeSandboxReq{}
	if err := appendPluginVolumeSourceAnnotation(req, "vol-a", "cos", "volumes/vol-a/"); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := appendPluginVolumeSourceAnnotation(req, "vol-b", "cos-rpc", ""); err != nil {
		t.Fatalf("append empty private_data: %v", err)
	}

	raw := req.Annotations["plugin-volume-sources"]
	if raw == "" {
		t.Fatal("missing plugin-volume-sources annotation")
	}

	var entries []map[string]string
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		t.Fatalf("unmarshal annotation: %v\nraw=%s", err, raw)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d (%s)", len(entries), raw)
	}
	if entries[0]["name"] != "vol-a" || entries[0]["driver"] != "cos" || entries[0]["private_data"] != "volumes/vol-a/" {
		t.Fatalf("entry0=%v", entries[0])
	}
	if entries[1]["name"] != "vol-b" || entries[1]["driver"] != "cos-rpc" {
		t.Fatalf("entry1=%v", entries[1])
	}
	if _, ok := entries[1]["private_data"]; ok {
		t.Fatalf("empty private_data must be omitted from JSON: %s", raw)
	}
}

func TestAppendPluginVolumeSourceAnnotation_malformedExisting(t *testing.T) {
	t.Parallel()

	req := &types.CreateCubeSandboxReq{
		Annotations: map[string]string{
			"plugin-volume-sources": "{not-json",
		},
	}
	if err := appendPluginVolumeSourceAnnotation(req, "vol-a", "cos", "x"); err == nil {
		t.Fatal("expected error for malformed existing annotation")
	}
}

func TestAppendPluginVolumeSourceAnnotationReplacesStaleEntry(t *testing.T) {
	t.Parallel()

	req := &types.CreateCubeSandboxReq{Annotations: map[string]string{
		AnnotationPluginVolumeSources: `[
			{"name":"vol-a","driver":"old","private_data":"stale"},
			{"name":"vol-a","driver":"duplicate","private_data":"duplicate"},
			{"name":"vol-b","driver":"cos"}
		]`,
	}}
	require.NoError(t, appendPluginVolumeSourceAnnotation(req, "vol-a", "s3", "fresh"))

	var entries []map[string]string
	require.NoError(t, json.Unmarshal([]byte(req.Annotations[AnnotationPluginVolumeSources]), &entries))
	require.Len(t, entries, 2)
	assert.Equal(t, map[string]string{
		"name": "vol-a", "driver": "s3", "private_data": "fresh",
	}, entries[0])
	assert.Equal(t, "vol-b", entries[1]["name"])
}
