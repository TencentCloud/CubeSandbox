// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package grpcsrv

import (
	"context"
	"testing"

	"github.com/tencentcloud/CubeSandbox/examples/volume/cos/rpc/internal/config"
	"github.com/tencentcloud/CubeSandbox/examples/volume/cos/rpc/internal/cosapi"
	vpb "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/volumeplugin/v1"
)

// TestIssueCredentials_PropagatesHookErrorWithoutPanicking exercises the
// VolumeContentService wiring end-to-end against a Client whose APPID cannot
// be resolved (no STS network call is made; cosapi.IssueContentCredentials
// fails fast). It proves IssueCredentials maps a Hook error to a gRPC error
// instead of panicking or returning a half-filled response — the STS
// success path's field mapping is covered by cosapi's own unit tests.
func TestIssueCredentials_PropagatesHookErrorWithoutPanicking(t *testing.T) {
	client, err := cosapi.New(&config.Config{
		SecretID: "id", SecretKey: "key", Bucket: "my-bucket-prod", Region: "ap-guangzhou",
	})
	if err != nil {
		t.Fatalf("cosapi.New: %v", err)
	}
	srv := &Server{cos: client}

	resp, err := srv.IssueCredentials(context.Background(), &vpb.IssueVolumeCredentialsRequest{VolumeId: "vol-1"})
	if err == nil {
		t.Fatal("expected an error when APPID cannot be resolved")
	}
	if resp != nil {
		t.Fatalf("expected nil response on error, got %+v", resp)
	}
}
