// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package opsagent

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
)

func TestPushQuotaSendsTokenHeader(t *testing.T) {
	var gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get(TokenHeader)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"applied":true}`))
	}))
	defer srv.Close()

	// Reuse the server's port so the fixed hostIP:port URL hits it.
	_, portStr, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	c := New(0, time.Second, "s3cret")
	c.port, _ = strconv.Atoi(portStr)

	req := model.OpsAgentPushRequest{Spec: &model.QuotaSpec{MCpuLimit: 1}}
	if _, err := c.PushQuota(context.Background(), "127.0.0.1", req); err != nil {
		t.Fatalf("push: %v", err)
	}
	if gotToken != "s3cret" {
		t.Fatalf("token header = %q, want s3cret", gotToken)
	}
}
