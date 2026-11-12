// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestPlacementToNodeAddressResolution(t *testing.T) {
	var live map[string]string // node id -> ip currently in the view
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(localcache.GetNode, func(id string) (*node.Node, bool) {
		ip, ok := live[id]
		if !ok {
			return nil, false
		}
		return &node.Node{InsID: id, IP: ip}, true
	})

	cases := []struct {
		name   string
		live   map[string]string
		p      *models.ArtifactNodePlacement
		wantIP string // "" means nil result
	}{
		{"re-registered under a new ip uses the live address", map[string]string{"n1": "10.0.0.2"}, &models.ArtifactNodePlacement{NodeID: "n1", NodeIP: "10.0.0.1"}, "10.0.0.2"},
		{"same ip in view", map[string]string{"n1": "10.0.0.1"}, &models.ArtifactNodePlacement{NodeID: "n1", NodeIP: "10.0.0.1"}, "10.0.0.1"},
		{"absent from view falls back to the stored ip", nil, &models.ArtifactNodePlacement{NodeID: "n1", NodeIP: "10.0.0.1"}, "10.0.0.1"},
		{"in view with empty live ip keeps the stored ip", map[string]string{"n1": ""}, &models.ArtifactNodePlacement{NodeID: "n1", NodeIP: "10.0.0.1"}, "10.0.0.1"},
		{"empty stored ip uses the live address", map[string]string{"n1": "10.0.0.2"}, &models.ArtifactNodePlacement{NodeID: "n1"}, "10.0.0.2"},
		{"nothing resolvable", nil, &models.ArtifactNodePlacement{NodeID: "n1"}, ""},
		{"empty node id never consults the view", map[string]string{"": "10.0.0.9"}, &models.ArtifactNodePlacement{NodeIP: "10.0.0.1"}, "10.0.0.1"},
		{"nil placement", nil, nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			live = c.live
			got := placementToNode(c.p)
			if c.wantIP == "" {
				if got != nil {
					t.Fatalf("want nil, got %+v", got)
				}
				return
			}
			if got == nil || got.IP != c.wantIP {
				t.Fatalf("want ip %s, got %+v", c.wantIP, got)
			}
		})
	}
}

func TestCountArtifactReferencesRejectsLikeWildcards(t *testing.T) {
	for _, artifactID := range []string{"rfs-bad%id", "rfs-bad_id"} {
		_, err := countArtifactReferencesTx(context.Background(), nil, artifactID, "")
		if err == nil {
			t.Fatalf("expected wildcard artifact id %q to be rejected", artifactID)
		}
		if !strings.Contains(err.Error(), "SQL wildcard") {
			t.Fatalf("unexpected error for %q: %v", artifactID, err)
		}
	}
}

func TestRootfsArtifactIDFromCreateRequest(t *testing.T) {
	req := &sandboxtypes.CreateCubeSandboxReq{
		Annotations: map[string]string{
			constants.CubeAnnotationRootfsArtifactID: " rfs-top ",
		},
		Containers: []*sandboxtypes.Container{{
			Image: &sandboxtypes.ImageSpec{
				Annotations: map[string]string{
					constants.CubeAnnotationRootfsArtifactID: "rfs-image",
				},
			},
		}},
	}
	if got := rootfsArtifactIDFromCreateRequest(req); got != "rfs-top" {
		t.Fatalf("expected top-level artifact id, got %q", got)
	}

	req.Annotations = nil
	if got := rootfsArtifactIDFromCreateRequest(req); got != "rfs-image" {
		t.Fatalf("expected image artifact id, got %q", got)
	}

	if got := rootfsArtifactIDFromCreateRequest(nil); got != "" {
		t.Fatalf("nil request should have empty artifact id, got %q", got)
	}
}

// requestTemplateCenterArtifactDelete is the ONLY thing allowed to remove an
// artifact's S3 object/local file/row (see CubeTemplateCenter/pkg/build/
// deleter.go). cleanupArtifactFully reaches it via
// notifyTemplateCenterArtifactDelete exactly once per finalized artifact --
// regression test for the S3-leak bug where CubeMaster used to hard-delete the
// row itself without ever notifying TC.
//
// The DB-bound phases of cleanupArtifactFully (1 and 3) are not unit-testable
// without a live database, so this exercises the production notify step
// directly instead of asserting on the stub itself.
func TestNotifyTemplateCenterArtifactDeleteCallsSeamOnce(t *testing.T) {
	orig := requestTemplateCenterArtifactDelete
	defer func() { requestTemplateCenterArtifactDelete = orig }()

	var calledWith []string
	requestTemplateCenterArtifactDelete = func(ctx context.Context, artifactID string) error {
		calledWith = append(calledWith, artifactID)
		return nil
	}

	notifyTemplateCenterArtifactDelete(context.Background(), "rfs-1")
	if len(calledWith) != 1 || calledWith[0] != "rfs-1" {
		t.Fatalf("calledWith = %v, want exactly [rfs-1]", calledWith)
	}
}

// A notify failure must not propagate: the artifact row stays CLEANUP_PENDING
// and TC's reconciler backstop-sweeps it later, so template deletion proceeds.
func TestNotifyTemplateCenterArtifactDeleteSwallowsErrors(t *testing.T) {
	orig := requestTemplateCenterArtifactDelete
	defer func() { requestTemplateCenterArtifactDelete = orig }()

	requestTemplateCenterArtifactDelete = func(ctx context.Context, artifactID string) error {
		return errors.New("tc unreachable")
	}

	// Must not panic; there is no return value to check by design.
	notifyTemplateCenterArtifactDelete(context.Background(), "rfs-2")
}

// When TC's endpoint is not configured, the seam must fail loudly (not
// silently pretend success) so the caller knows to leave the row
// CLEANUP_PENDING rather than assume cleanup happened.
func TestRequestTemplateCenterArtifactDeleteRequiresEndpoint(t *testing.T) {
	t.Setenv("CUBE_TEMPLATE_CENTER_ADDR", "")
	if err := requestTemplateCenterArtifactDelete(context.Background(), "rfs-2"); err == nil {
		t.Fatal("expected error when template center endpoint is not configured")
	}
}
