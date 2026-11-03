// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
)

func testNode(id, ip string) *node.Node { return &node.Node{InsID: id, IP: ip} }

// The selection must skip nodes with a READY clean replica (matched by node
// id OR ip), and re-target nodes whose replica is failed or still
// cleanup_required (half-cleaned by an interrupted delete).
func TestNodesWithoutReadyReplica(t *testing.T) {
	replicas := []models.TemplateReplica{
		{NodeID: "node-a", NodeIP: "10.0.0.1", Status: ReplicaStatusReady},
		{NodeID: "node-b", NodeIP: "10.0.0.2", Status: ReplicaStatusFailed},
		{NodeID: "node-c", NodeIP: "10.0.0.3", Status: ReplicaStatusReady, CleanupRequired: true},
	}
	targets := []*node.Node{
		testNode("node-a", "10.0.0.1"),
		testNode("node-b", "10.0.0.2"),
		testNode("node-c", "10.0.0.3"),
		testNode("node-new", "10.0.0.4"),
		nil, // tolerated
	}
	missing := nodesWithoutReadyReplica(replicas, targets)
	got := make([]string, 0, len(missing))
	for _, n := range missing {
		got = append(got, n.ID())
	}
	want := []string{"node-b", "node-c", "node-new"}
	if len(got) != len(want) {
		t.Fatalf("missing = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("missing = %v, want %v", got, want)
		}
	}
}

// A replica row keyed by IP only must still count as covering the node.
func TestNodesWithoutReadyReplicaIPOnlyRow(t *testing.T) {
	replicas := []models.TemplateReplica{{NodeIP: "10.0.0.1", Status: ReplicaStatusReady}}
	targets := []*node.Node{testNode("node-a", "10.0.0.1")}
	if missing := nodesWithoutReadyReplica(replicas, targets); len(missing) != 0 {
		t.Fatalf("missing = %v, want none (IP-only replica covers the node)", missing)
	}
}

func TestDecodeBackfillJobRequestRebuildsRequestEnvelope(t *testing.T) {
	// marshalTemplateImageJobRequest persists Request=nil. A bare decode left
	// the embedded *Request nil and generateTemplateCreateRequest panicked on
	// RequestID; this test locks in the rebuilt envelope and the artifact-side
	// writable-layer fallback.
	payload := `{"source_image_ref":"docker.io/library/nginx:latest","template_id":"tpl-backfill","instance_type":"cubebox","network_type":"tap"}`
	req, err := decodeBackfillJobRequest(payload, "1Gi")
	if err != nil {
		t.Fatalf("decodeBackfillJobRequest: %v", err)
	}
	if req.Request == nil || req.Request.RequestID == "" {
		t.Fatalf("Request envelope was not rebuilt: %#v", req.Request)
	}
	if req.TemplateID != "tpl-backfill" {
		t.Fatalf("template id = %q", req.TemplateID)
	}
	if req.WritableLayerSize != "1Gi" {
		t.Fatalf("writable layer size = %q", req.WritableLayerSize)
	}

	artifact := &models.RootfsArtifact{
		ArtifactID:              "rfs-backfill",
		TemplateSpecFingerprint: "fp-backfill",
		Ext4SHA256:              "sha-backfill",
		Ext4SizeBytes:           1024,
		DownloadToken:           "token-backfill",
	}
	if _, err := generateTemplateCreateRequest(context.Background(), req, artifact, DockerImageConfig{}, ""); err != nil {
		t.Fatalf("generateTemplateCreateRequest after backfill decode: %v", err)
	}
}
