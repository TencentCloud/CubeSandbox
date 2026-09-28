// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"encoding/json"
	"testing"

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
