// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package statusclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestReportSendsBody(t *testing.T) {
	var got batchRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != reportPath {
			t.Errorf("path %s", r.URL.Path)
		}
		if got := r.Header.Get("X-Cube-Sandbox-Status-Token"); got != "" {
			t.Errorf("unexpected token header %q", got)
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := &Client{base: srv.URL, http: srv.Client()}
	when := time.Date(2026, 9, 24, 1, 2, 3, 0, time.UTC)
	err := c.Report(context.Background(), "host-a", []Item{{
		SandboxID: "sb-1", HostID: "host-a", RestartState: "BackOff",
		RestartCount: 2, StatusSeq: 4, NextRestartAt: &when,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got.HostID != "host-a" || len(got.Items) != 1 || got.Items[0].StatusSeq != 4 {
		t.Fatalf("body %+v", got)
	}
	if got.Items[0].NextRestartAt == nil || !got.Items[0].NextRestartAt.Equal(when) {
		t.Fatalf("next restart %+v", got.Items[0].NextRestartAt)
	}
}

func TestReportEmptyIsNoop(t *testing.T) {
	c := &Client{base: "http://127.0.0.1:1", http: &http.Client{Timeout: time.Second}}
	if err := c.Report(context.Background(), "host", nil); err != nil {
		t.Fatal(err)
	}
}
