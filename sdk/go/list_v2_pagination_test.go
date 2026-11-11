// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// ListV2 promises every sandbox, so it has to follow the pagination cursor
// `GET /v2/sandboxes` returns in `x-next-token`. Dropping that header — which
// is what the method used to do — silently truncates the result for any caller
// with more sandboxes than one page (100 by default).
func TestListV2FollowsNextToken(t *testing.T) {
	const total = 250

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/sandboxes" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		start := 0
		if cursor := r.URL.Query().Get("nextToken"); cursor != "" {
			if _, err := fmt.Sscanf(cursor, "c%d", &start); err != nil {
				t.Errorf("unreadable cursor %q: %v", cursor, err)
			}
		}
		end := start + 100
		if end > total {
			end = total
		}
		page := make([]SandboxInfo, 0, end-start)
		for i := start; i < end; i++ {
			page = append(page, SandboxInfo{SandboxID: fmt.Sprintf("sb-%03d", i)})
		}
		w.Header().Set("Content-Type", "application/json")
		// No header on the last page: that is the end of the list.
		if end < total {
			w.Header().Set("x-next-token", fmt.Sprintf("c%d", end))
		}
		_ = json.NewEncoder(w).Encode(page)
	}))
	defer server.Close()

	client := NewClient(Config{APIURL: server.URL, TemplateID: "tpl-env", Timeout: 30 * time.Second})
	sandboxes, err := client.ListV2(context.Background())
	if err != nil {
		t.Fatalf("ListV2: %v", err)
	}
	if len(sandboxes) != total {
		t.Fatalf("ListV2 returned %d sandboxes, want %d — the x-next-token cursor was not followed", len(sandboxes), total)
	}
	for i, sb := range sandboxes {
		want := fmt.Sprintf("sb-%03d", i)
		if sb.SandboxID != want {
			t.Fatalf("sandbox[%d] = %q, want %q", i, sb.SandboxID, want)
		}
	}
}
