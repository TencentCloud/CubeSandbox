// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"encoding/json"
	"testing"
)

func TestUpdateNodeStatusRequestUnmarshalReportedFlag(t *testing.T) {
	cases := []struct {
		name         string
		raw          string
		wantReported bool
		wantLen      int
	}{
		{"omitted", `{"heartbeat_time":"2026-01-01T00:00:00Z"}`, false, 0},
		{"explicit empty", `{"local_templates":[]}`, true, 0},
		{"present", `{"local_templates":[{"template_id":"tpl-1"}]}`, true, 1},
		{"present null", `{"local_templates":null}`, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var req UpdateNodeStatusRequest
			if err := json.Unmarshal([]byte(tc.raw), &req); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if req.LocalTemplatesReported != tc.wantReported {
				t.Fatalf("LocalTemplatesReported=%v want %v", req.LocalTemplatesReported, tc.wantReported)
			}
			if len(req.LocalTemplates) != tc.wantLen {
				t.Fatalf("LocalTemplates len=%d want %d", len(req.LocalTemplates), tc.wantLen)
			}
		})
	}
}

func TestUpdateNodeStatusRequestUnmarshalMalformed(t *testing.T) {
	var req UpdateNodeStatusRequest
	if err := json.Unmarshal([]byte(`{`), &req); err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
}

func TestSchedulerNodeJSONRoundTrip(t *testing.T) {
	cases := []struct {
		name      string
		reported  bool
		templates []string
	}{
		{name: "unreported"},
		{name: "reported empty", reported: true, templates: []string{}},
		{name: "reported non-empty", reported: true, templates: []string{"tpl-1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := &SchedulerNode{
				InsID:                  "node-1",
				LocalTemplates:         tc.templates,
				LocalTemplatesReported: tc.reported,
			}
			raw, err := json.Marshal(in)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var out SchedulerNode
			if err := json.Unmarshal(raw, &out); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if out.LocalTemplatesReported != tc.reported {
				t.Fatalf("LocalTemplatesReported=%v want %v", out.LocalTemplatesReported, tc.reported)
			}
			if len(out.LocalTemplates) != len(tc.templates) {
				t.Fatalf("LocalTemplates=%v want %v", out.LocalTemplates, tc.templates)
			}
			for i := range tc.templates {
				if out.LocalTemplates[i] != tc.templates[i] {
					t.Fatalf("LocalTemplates=%v want %v", out.LocalTemplates, tc.templates)
				}
			}
		})
	}
}

func TestSchedulerNodeUnmarshalLegacyInventory(t *testing.T) {
	var node SchedulerNode
	if err := json.Unmarshal([]byte(`{"LocalTemplates":["tpl-1"]}`), &node); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !node.LocalTemplatesReported || len(node.LocalTemplates) != 1 || node.LocalTemplates[0] != "tpl-1" {
		t.Fatalf("legacy inventory lost provenance: %+v", node)
	}
}

func TestSchedulerNodeUnmarshalMalformed(t *testing.T) {
	var node SchedulerNode
	if err := json.Unmarshal([]byte(`{`), &node); err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
}
