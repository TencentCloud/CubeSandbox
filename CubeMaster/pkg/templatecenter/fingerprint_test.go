// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"encoding/json"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

// TestUnmarshalTemplateImageJobRequestPreservesTemplateID guards against a
// regression where decoding an already-submitted job's RequestJSON snapshot
// silently minted a SECOND, different template_id (via
// normalizeTemplateImageRequest's unconditional regeneration), leaving the
// job's real template_id row (definition + replicas) orphaned while a
// different, freshly generated ID got used downstream instead. See
// remote_build_resume.go's resumeRemoteBuiltTemplateImageJob, which decodes
// the snapshot and must operate on the SAME template_id the job (and its
// replicas) were created under.
func TestUnmarshalTemplateImageJobRequestPreservesTemplateID(t *testing.T) {
	original := &types.CreateTemplateFromImageReq{
		TemplateID:        "tpl-original0000000000000000",
		SourceImageRef:    "docker.io/library/nginx:latest",
		WritableLayerSize: "20Gi",
	}
	payload, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal fail: %v", err)
	}

	got, err := unmarshalTemplateImageJobRequest(string(payload))
	if err != nil {
		t.Fatalf("unmarshalTemplateImageJobRequest failed: %v", err)
	}
	if got.TemplateID != original.TemplateID {
		t.Fatalf("template_id must be preserved across decode: got %q, want %q", got.TemplateID, original.TemplateID)
	}
}

// TestUnmarshalTemplateImageJobRequestPinsLegacyFormatToExt4 is the upgrade-
// hazard regression: a request_json snapshot persisted before the rootfs_format
// field existed decodes with an empty RootfsFormat. Replaying it must NOT take
// the current template.rootfs_format config default -- the build started under
// ext4 (the only format that existed), and re-stamping it erofs would fail the
// rootfs_format mismatch guard in prepareTemplateImageJobAfterRemoteBuild
// (old TC reports ext4) and silently switch redo rebuilds. Here the config
// default is forced to erofs; the replayed legacy snapshot must still come back
// ext4, proving the format is pinned to the legacy value, not the config.
func TestUnmarshalTemplateImageJobRequestPinsLegacyFormatToExt4(t *testing.T) {
	// Force the config default to erofs so a config-driven fill would be
	// observable (without the pin, an empty snapshot would become erofs here).
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(config.GetConfig, func() *config.Config {
		return &config.Config{Template: &config.TemplateConf{RootfsFormat: config.RootfsFormatErofs}}
	})

	// A pre-upgrade snapshot: every field a legacy request had, no rootfs_format.
	legacy := &types.CreateTemplateFromImageReq{
		TemplateID:        "tpl-legacy0000000000000000000",
		SourceImageRef:    "docker.io/library/nginx:latest",
		WritableLayerSize: "20Gi",
	}
	payload, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal fail: %v", err)
	}

	got, err := unmarshalTemplateImageJobRequest(string(payload))
	if err != nil {
		t.Fatalf("unmarshalTemplateImageJobRequest failed: %v", err)
	}
	if got.RootfsFormat != config.RootfsFormatExt4 {
		t.Fatalf("replayed pre-upgrade snapshot must pin to ext4, not the current config default: got %q, want %q",
			got.RootfsFormat, config.RootfsFormatExt4)
	}
}

// TestUnmarshalTemplateImageJobRequestKeepsExplicitFormat verifies a
// post-upgrade snapshot that explicitly carries rootfs_format is preserved
// (not overwritten by the legacy pin or the config default), so a replayed
// erofs job stays erofs.
func TestUnmarshalTemplateImageJobRequestKeepsExplicitFormat(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(config.GetConfig, func() *config.Config {
		return &config.Config{Template: &config.TemplateConf{RootfsFormat: config.RootfsFormatExt4}}
	})

	for _, format := range []string{config.RootfsFormatErofs, config.RootfsFormatExt4} {
		snap := &types.CreateTemplateFromImageReq{
			TemplateID:        "tpl-fmt000000000000000000000",
			SourceImageRef:    "docker.io/library/nginx:latest",
			WritableLayerSize: "20Gi",
			RootfsFormat:      format,
		}
		payload, err := json.Marshal(snap)
		if err != nil {
			t.Fatalf("marshal fail: %v", err)
		}
		got, err := unmarshalTemplateImageJobRequest(string(payload))
		if err != nil {
			t.Fatalf("unmarshalTemplateImageJobRequest failed: %v", err)
		}
		if got.RootfsFormat != format {
			t.Fatalf("explicit rootfs_format %q must be preserved on replay, got %q", format, got.RootfsFormat)
		}
	}
}
