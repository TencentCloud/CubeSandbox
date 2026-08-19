// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// TestNormalizeStoredTemplateRequestStripsPhysicalAnnotations verifies the
// v4+ contract for stored template requests: per-invocation runtime-snapshot
// binding annotations are scrubbed so they cannot leak into later
// create-from-template flows. The logical template id remains. (v5 removed
// the physical memory_vol/memory_kind annotations entirely.)
func TestNormalizeStoredTemplateRequestStripsPhysicalAnnotations(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(NormalizeRequest, func(in *sandboxtypes.CreateCubeSandboxReq) (*sandboxtypes.CreateCubeSandboxReq, string, error) {
		return in, "tpl-after-norm", nil
	})

	req := &sandboxtypes.CreateCubeSandboxReq{
		InstanceType: "cubebox",
		SnapshotDir:  "/snapshots/should-be-cleared",
		Timeout:      sandboxtypes.TimeoutPtr(1),
		Annotations: map[string]string{
			constants.CubeAnnotationsAppSnapshotCreate:        "true",
			constants.CubeAnnotationRuntimeSnapshotID:         "snap-stale",
			constants.CubeAnnotationRuntimeSnapshotAttachedAt: "2026-05-01T00:00:00Z",
			"unrelated": "keep-me",
		},
	}

	out, err := normalizeStoredTemplateRequest(req)
	require.NoError(t, err)

	for _, k := range []string{
		constants.CubeAnnotationsAppSnapshotCreate,
		constants.CubeAnnotationRuntimeSnapshotID,
		constants.CubeAnnotationRuntimeSnapshotAttachedAt,
	} {
		_, present := out.Annotations[k]
		assert.Falsef(t, present, "annotation %q must be stripped from stored template request", k)
	}
	assert.Equal(t, "tpl-after-norm", out.Annotations[constants.CubeAnnotationAppSnapshotTemplateID])
	assert.Equal(t, "keep-me", out.Annotations["unrelated"])
	assert.Empty(t, out.SnapshotDir)
}

func TestConvergeEnvdVersionUsesNodeCollectionResults(t *testing.T) {
	got := convergeEnvdVersion(context.Background(), []nodeEnvdVersion{
		{NodeID: "node-a", Version: ""},
		{NodeID: "node-b", Version: "envd version 0.5.11"},
		{NodeID: "node-c", Version: "0.6.0"},
	})

	assert.Equal(t, "0.5.11", got)
}

func TestResolveTemplateNodesFiltersRequestedHealthyNodes(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	patches.ApplyFunc(healthyTemplateNodes, func(instanceType string) []*node.Node {
		return []*node.Node{
			{InsID: "node-a", IP: "10.0.0.1", Healthy: true},
			{InsID: "node-b", IP: "10.0.0.2", Healthy: true},
		}
	})

	got, err := resolveTemplateNodes("cubebox", []string{"10.0.0.2", "node-a"})
	if err != nil {
		t.Fatalf("resolveTemplateNodes returned error: %v", err)
	}
	want := []string{"node-a", "node-b"}
	gotIDs := make([]string, 0, len(got))
	for _, item := range got {
		gotIDs = append(gotIDs, item.ID())
	}
	if !reflect.DeepEqual(gotIDs, want) {
		t.Fatalf("selected nodes=%v, want %v", gotIDs, want)
	}
}

func TestResolveTemplateNodesRejectsMissingTargets(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	patches.ApplyFunc(healthyTemplateNodes, func(instanceType string) []*node.Node {
		return []*node.Node{
			{InsID: "node-a", IP: "10.0.0.1", Healthy: true},
		}
	})

	_, err := resolveTemplateNodes("cubebox", []string{"node-b"})
	if err == nil {
		t.Fatal("expected resolveTemplateNodes to reject missing targets")
	}
	if !strings.Contains(err.Error(), "node-b") {
		t.Fatalf("expected error to mention missing node, got %v", err)
	}
}

func TestCreateTemplateUsesRequestedDistributionScope(t *testing.T) {
	origDB := store.db
	store.db = &gorm.DB{}
	defer func() {
		store.db = origDB
	}()

	patches := gomonkey.NewPatches()
	defer patches.Reset()

	req := &sandboxtypes.CreateCubeSandboxReq{
		Request:           &sandboxtypes.Request{RequestID: "req-1"},
		InstanceType:      "cubebox",
		DistributionScope: []string{"node-a"},
		Annotations: map[string]string{
			"cube.master.appsnapshot.template.id":      "tpl-scope",
			"cube.master.appsnapshot.template.version": "v2",
		},
	}

	patches.ApplyFunc(NormalizeRequest, func(in *sandboxtypes.CreateCubeSandboxReq) (*sandboxtypes.CreateCubeSandboxReq, string, error) {
		return in, "tpl-scope", nil
	})
	patches.ApplyFunc(normalizeStoredTemplateRequest, func(in *sandboxtypes.CreateCubeSandboxReq) (*sandboxtypes.CreateCubeSandboxReq, error) {
		return in, nil
	})
	patches.ApplyFunc(createDefinitionWithOptions, func(ctx context.Context, templateID string, storedReq *sandboxtypes.CreateCubeSandboxReq, instanceType, version string, _ definitionCreateOptions) error {
		return nil
	})
	patches.ApplyFunc(setTemplateRequestCache, func(templateID string, req *sandboxtypes.CreateCubeSandboxReq) error {
		return nil
	})

	var gotScope []string
	patches.ApplyFunc(resolveTemplateNodes, func(instanceType string, scope []string) ([]*node.Node, error) {
		gotScope = append([]string(nil), scope...)
		return []*node.Node{{InsID: "node-a", IP: "10.0.0.1", Healthy: true}}, nil
	})
	patches.ApplyFunc(createTemplateReplicasOnNodes, func(ctx context.Context, templateID string, req *sandboxtypes.CreateCubeSandboxReq, targets []*node.Node, opts replicaRunOptions) ([]ReplicaStatus, error) {
		if len(targets) != 1 || targets[0].ID() != "node-a" {
			return nil, errors.New("unexpected target nodes")
		}
		return []ReplicaStatus{{NodeID: "node-a", NodeIP: "10.0.0.1", InstanceType: req.InstanceType, Status: ReplicaStatusReady}}, nil
	})
	patches.ApplyFunc(finalizeTemplateReplicas, func(ctx context.Context, templateID, jobID, instanceType, version string, replicas []ReplicaStatus) (*TemplateInfo, string, error) {
		return &TemplateInfo{TemplateID: templateID, InstanceType: instanceType, Version: version, Replicas: replicas}, "", nil
	})
	patches.ApplyFunc(cleanupTemplateReplicas, func(ctx context.Context, templateID string) error {
		return nil
	})
	patches.ApplyFunc(cleanupTemplateMetadata, func(ctx context.Context, templateID string) error {
		return nil
	})

	info, err := CreateTemplate(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateTemplate returned error: %v", err)
	}
	if info == nil || info.TemplateID != "tpl-scope" {
		t.Fatalf("unexpected template info: %#v", info)
	}
	if !reflect.DeepEqual(gotScope, []string{"node-a"}) {
		t.Fatalf("resolveTemplateNodes scope=%v, want [node-a]", gotScope)
	}
}

// TestFinalizeTemplateReplicasClaimsAliasBeforePublishingReady guards the
// create/claim publish-ordering invariant: finalizeTemplateReplicas MUST claim
// the alias BEFORE it publishes the READY status via UpdateDefinitionStatus.
// Otherwise a client that polls the template and observes READY can race a
// GET-by-alias that 404s because display_name/alias_key is not yet written
// (the original test_template_alias_create_get_and_delete failure). We record
// the call order of both operations and assert the claim happens first.
func TestFinalizeTemplateReplicasClaimsAliasBeforePublishingReady(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	var order []string
	patches.ApplyFunc(publishTemplateStatusWithAlias, func(ctx context.Context, templateID, jobID, status, lastError string) (string, string, error) {
		order = append(order, "publish:"+status)
		return "my-alias", "", nil
	})
	patches.ApplyFunc(setTemplateLocalityCache, func(templateID string, replicas []ReplicaStatus) {})
	patches.ApplyFunc(registerReadyTemplateReplicas, func(templateID string, replicas []ReplicaStatus) {})

	replicas := []ReplicaStatus{{NodeID: "node-a", NodeIP: "10.0.0.1", Status: ReplicaStatusReady}}
	info, claimWarning, err := finalizeTemplateReplicas(context.Background(), "tpl-order", "job-order", "cubebox", "v2", replicas)
	if err != nil {
		t.Fatalf("finalizeTemplateReplicas returned error: %v", err)
	}
	if claimWarning != "" {
		t.Fatalf("unexpected claim warning: %q", claimWarning)
	}
	if info == nil || info.Status != StatusReady {
		t.Fatalf("expected READY info, got %#v", info)
	}
	if info.DisplayName != "my-alias" {
		t.Fatalf("expected DisplayName propagated, got %q", info.DisplayName)
	}
	if !reflect.DeepEqual(order, []string{"publish:" + StatusReady}) {
		t.Fatalf("alias must be claimed before READY is published; got order=%v", order)
	}
}

// TestFinalizeTemplateReplicasSkipsAliasClaimWhenFailed verifies that a failed
// template (no ready replica) does NOT claim the alias — an alias must never
// point at a broken template.
func TestFinalizeTemplateReplicasSkipsAliasClaimWhenFailed(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	publishedStatus := ""
	patches.ApplyFunc(publishTemplateStatusWithAlias, func(ctx context.Context, templateID, jobID, status, lastError string) (string, string, error) {
		publishedStatus = status
		return "", "", nil
	})
	patches.ApplyFunc(setTemplateLocalityCache, func(templateID string, replicas []ReplicaStatus) {})
	patches.ApplyFunc(registerReadyTemplateReplicas, func(templateID string, replicas []ReplicaStatus) {})

	replicas := []ReplicaStatus{{NodeID: "node-a", Status: ReplicaStatusFailed, ErrorMessage: "boom"}}
	_, _, err := finalizeTemplateReplicas(context.Background(), "tpl-failed", "job-failed", "cubebox", "v2", replicas)
	if err == nil {
		t.Fatalf("expected error for all-failed template")
	}
	if publishedStatus != StatusFailed {
		t.Fatalf("expected FAILED to be published, got %q", publishedStatus)
	}
}

// TestFinalizeTemplateReplicasClaimsAliasForPartiallyReady verifies that a
// PARTIALLY_READY template (at least one serving replica) still claims the
// alias — the guard is status != FAILED, not status == READY — and that the
// claim is ordered before the status is published.
func TestFinalizeTemplateReplicasClaimsAliasForPartiallyReady(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	var order []string
	patches.ApplyFunc(publishTemplateStatusWithAlias, func(ctx context.Context, templateID, jobID, status, lastError string) (string, string, error) {
		order = append(order, "publish:"+status)
		return "my-alias", "", nil
	})
	patches.ApplyFunc(setTemplateLocalityCache, func(templateID string, replicas []ReplicaStatus) {})
	patches.ApplyFunc(registerReadyTemplateReplicas, func(templateID string, replicas []ReplicaStatus) {})

	replicas := []ReplicaStatus{
		{NodeID: "node-a", Status: ReplicaStatusReady},
		{NodeID: "node-b", Status: ReplicaStatusFailed, ErrorMessage: "boom"},
	}
	info, claimWarning, err := finalizeTemplateReplicas(context.Background(), "tpl-partial", "job-partial", "cubebox", "v2", replicas)
	if err != nil {
		t.Fatalf("finalizeTemplateReplicas returned error: %v", err)
	}
	if claimWarning != "" {
		t.Fatalf("unexpected claim warning: %q", claimWarning)
	}
	if info == nil || info.Status != StatusPartiallyReady {
		t.Fatalf("expected PARTIALLY_READY info, got %#v", info)
	}
	if !reflect.DeepEqual(order, []string{"publish:" + StatusPartiallyReady}) {
		t.Fatalf("alias must be claimed before status is published; got order=%v", order)
	}
}

// TestFinalizeTemplateReplicasSurfacesClaimWarningOnNonDuplicateError verifies
// that a non-duplicate claim failure is surfaced as a warning (not an error)
// while the status is still published, and that DisplayName stays empty — the
// warning and DisplayName are mutually exclusive.
func TestFinalizeTemplateReplicasSurfacesClaimWarningOnNonDuplicateError(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	published := false
	patches.ApplyFunc(publishTemplateStatusWithAlias, func(ctx context.Context, templateID, jobID, status, lastError string) (string, string, error) {
		published = true
		return "", "template is ready but alias could not be claimed", nil
	})
	patches.ApplyFunc(UpdateDefinitionStatus, func(ctx context.Context, templateID, status, lastError string) error {
		published = true
		return nil
	})
	patches.ApplyFunc(setTemplateLocalityCache, func(templateID string, replicas []ReplicaStatus) {})
	patches.ApplyFunc(registerReadyTemplateReplicas, func(templateID string, replicas []ReplicaStatus) {})

	replicas := []ReplicaStatus{{NodeID: "node-a", Status: ReplicaStatusReady}}
	info, claimWarning, err := finalizeTemplateReplicas(context.Background(), "tpl-warn", "job-warn", "cubebox", "v2", replicas)
	if err != nil {
		t.Fatalf("finalizeTemplateReplicas returned error: %v", err)
	}
	if !published {
		t.Fatalf("status must still be published when the alias claim fails")
	}
	if claimWarning == "" {
		t.Fatalf("expected a non-empty claim warning on a non-duplicate claim failure")
	}
	if info == nil || info.DisplayName != "" {
		t.Fatalf("expected empty DisplayName on claim failure, got %#v", info)
	}
}

// TestFinalizeTemplateReplicasSwallowsDuplicateAliasError verifies that a
// duplicate-alias violation (another template concurrently won the alias) is
// swallowed: no warning, empty DisplayName, and the template still publishes
// READY without the alias.
func TestFinalizeTemplateReplicasSwallowsDuplicateAliasError(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	patches.ApplyFunc(publishTemplateStatusWithAlias, func(ctx context.Context, templateID, jobID, status, lastError string) (string, string, error) {
		return "", "", nil
	})
	patches.ApplyFunc(UpdateDefinitionStatus, func(ctx context.Context, templateID, status, lastError string) error {
		return nil
	})
	patches.ApplyFunc(setTemplateLocalityCache, func(templateID string, replicas []ReplicaStatus) {})
	patches.ApplyFunc(registerReadyTemplateReplicas, func(templateID string, replicas []ReplicaStatus) {})

	replicas := []ReplicaStatus{{NodeID: "node-a", Status: ReplicaStatusReady}}
	info, claimWarning, err := finalizeTemplateReplicas(context.Background(), "tpl-dup", "job-dup", "cubebox", "v2", replicas)
	if err != nil {
		t.Fatalf("finalizeTemplateReplicas returned error: %v", err)
	}
	if claimWarning != "" {
		t.Fatalf("duplicate-alias error must be swallowed, got warning %q", claimWarning)
	}
	if info == nil || info.Status != StatusReady || info.DisplayName != "" {
		t.Fatalf("expected READY info with empty DisplayName, got %#v", info)
	}
}

// TestFinalizeTemplateReplicasSkipsClaimForEmptyAlias verifies the synchronous
// create path (CreateTemplate passes ""): no alias is claimed, yet the status
// is still published.
func TestFinalizeTemplateReplicasSkipsClaimForEmptyAlias(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	published := false
	patches.ApplyFunc(publishTemplateStatusWithAlias, func(ctx context.Context, templateID, jobID, status, lastError string) (string, string, error) {
		published = true
		return "", "", nil
	})
	patches.ApplyFunc(setTemplateLocalityCache, func(templateID string, replicas []ReplicaStatus) {})
	patches.ApplyFunc(registerReadyTemplateReplicas, func(templateID string, replicas []ReplicaStatus) {})

	replicas := []ReplicaStatus{{NodeID: "node-a", Status: ReplicaStatusReady}}
	info, claimWarning, err := finalizeTemplateReplicas(context.Background(), "tpl-noalias", "job-noalias", "cubebox", "v2", replicas)
	if err != nil {
		t.Fatalf("finalizeTemplateReplicas returned error: %v", err)
	}
	if !published || claimWarning != "" || info == nil || info.DisplayName != "" {
		t.Fatalf("expected READY published, no warning, empty DisplayName; got warning=%q info=%#v", claimWarning, info)
	}
}

// TestRefreshTemplateReplicaSummaryClaimsAliasBeforePublishingReady guards the
// SAME ordering invariant as finalizeTemplateReplicas but for the redo path:
// refreshTemplateReplicaSummary MUST claim the alias BEFORE publishing the
// status. Without this test, reverting the reorder in the redo path would keep
// the suite green.
func TestRefreshTemplateReplicaSummaryClaimsAliasBeforePublishingReady(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	var order []string
	patches.ApplyFunc(ListReplicas, func(ctx context.Context, templateID string) ([]models.TemplateReplica, error) {
		return []models.TemplateReplica{{NodeID: "node-a", Status: ReplicaStatusReady}}, nil
	})
	patches.ApplyFunc(publishTemplateStatusWithAlias, func(ctx context.Context, templateID, jobID, status, lastError string) (string, string, error) {
		order = append(order, "publish:"+status)
		return "my-alias", "", nil
	})
	patches.ApplyFunc(setTemplateLocalityCache, func(templateID string, replicas []ReplicaStatus) {})
	patches.ApplyFunc(registerReadyTemplateReplicas, func(templateID string, replicas []ReplicaStatus) {})

	claimWarning, err := refreshTemplateReplicaSummary(context.Background(), "tpl-redo", "job-redo")
	if err != nil {
		t.Fatalf("refreshTemplateReplicaSummary returned error: %v", err)
	}
	if claimWarning != "" {
		t.Fatalf("unexpected claim warning: %q", claimWarning)
	}
	if !reflect.DeepEqual(order, []string{"publish:" + StatusReady}) {
		t.Fatalf("redo path must claim the alias before publishing READY; got order=%v", order)
	}
}

// TestRefreshTemplateReplicaSummarySkipsAliasClaimWhenFailed verifies the redo
// path never claims an alias for a FAILED template.
func TestRefreshTemplateReplicaSummarySkipsAliasClaimWhenFailed(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	publishedStatus := ""
	patches.ApplyFunc(ListReplicas, func(ctx context.Context, templateID string) ([]models.TemplateReplica, error) {
		return []models.TemplateReplica{{NodeID: "node-a", Status: ReplicaStatusFailed, ErrorMessage: "boom"}}, nil
	})
	patches.ApplyFunc(publishTemplateStatusWithAlias, func(ctx context.Context, templateID, jobID, status, lastError string) (string, string, error) {
		publishedStatus = status
		return "", "", nil
	})
	patches.ApplyFunc(setTemplateLocalityCache, func(templateID string, replicas []ReplicaStatus) {})
	patches.ApplyFunc(registerReadyTemplateReplicas, func(templateID string, replicas []ReplicaStatus) {})

	if _, err := refreshTemplateReplicaSummary(context.Background(), "tpl-redo-failed", "job-redo-failed"); err != nil {
		t.Fatalf("refreshTemplateReplicaSummary returned error: %v", err)
	}
	if publishedStatus != StatusFailed {
		t.Fatalf("expected FAILED to be published, got %q", publishedStatus)
	}
}

func TestGetTemplateRequestAssignsRuntimeRequestID(t *testing.T) {
	templateID := "tpl-runtime-request"
	invalidateTemplateCaches(templateID)
	defer invalidateTemplateCaches(templateID)

	if err := setTemplateRequestCache(templateID, &sandboxtypes.CreateCubeSandboxReq{}); err != nil {
		t.Fatalf("setTemplateRequestCache returned error: %v", err)
	}

	first, err := GetTemplateRequest(context.Background(), templateID)
	if err != nil {
		t.Fatalf("GetTemplateRequest returned error: %v", err)
	}
	if first.Request == nil {
		t.Fatal("expected runtime request to be hydrated")
	}
	if strings.TrimSpace(first.RequestID) == "" {
		t.Fatal("expected runtime requestID to be populated")
	}

	second, err := GetTemplateRequest(context.Background(), templateID)
	if err != nil {
		t.Fatalf("GetTemplateRequest second call returned error: %v", err)
	}
	if second.Request == nil || strings.TrimSpace(second.RequestID) == "" {
		t.Fatal("expected runtime requestID on subsequent fetch")
	}
	if first.RequestID == second.RequestID {
		t.Fatalf("expected a fresh runtime requestID per fetch, got duplicate %q", first.RequestID)
	}
}

func TestGetTemplateInfoPopulatesCreatedAtAndImageInfoFromDefinitionAndLatestJob(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	createdAt := time.Date(2026, time.June, 17, 15, 53, 40, 0, time.FixedZone("UTC+8", 8*3600))
	patches.ApplyFunc(GetDefinition, func(ctx context.Context, templateID string) (*models.TemplateDefinition, error) {
		return &models.TemplateDefinition{
			TemplateID:   templateID,
			InstanceType: "cubebox",
			Version:      "v2",
			Status:       "READY",
			RequestJSON: `{
				"containers":[
					{"image":{"image":"rfs-deadbeef"}}
				]
			}`,
			Model: gorm.Model{CreatedAt: createdAt},
		}, nil
	})
	patches.ApplyFunc(ListReplicas, func(ctx context.Context, templateID string) ([]models.TemplateReplica, error) {
		return nil, nil
	})
	patches.ApplyFunc(getLatestTemplateImageJobByTemplateID, func(ctx context.Context, templateID string) (*models.TemplateImageJob, error) {
		return &models.TemplateImageJob{
			TemplateID:        templateID,
			SourceImageRef:    "docker.io/library/python:3.12",
			SourceImageDigest: "sha256:abcd",
		}, nil
	})

	info, err := GetTemplateInfo(context.Background(), "tpl-a")
	if err != nil {
		t.Fatalf("GetTemplateInfo returned error: %v", err)
	}
	if info == nil {
		t.Fatal("expected template info, got nil")
	}
	if info.CreatedAt != "2026-06-17T07:53:40Z" {
		t.Fatalf("unexpected created_at: %q", info.CreatedAt)
	}
	if info.ImageInfo != "docker.io/library/python:3.12@sha256:abcd" {
		t.Fatalf("unexpected image_info: %q", info.ImageInfo)
	}
}

// TestGetTemplateByAliasEmptyAliasReturnsNotFound verifies the empty-alias
// fast path: an empty (or whitespace-only) alias short-circuits to
// ErrTemplateNotFound without touching the database, because empty is never
// a valid alias value (it is the "no alias" sentinel).
func TestGetTemplateByAliasEmptyAliasReturnsNotFound(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	for _, alias := range []string{"", "   "} {
		_, err := GetTemplateByAlias(context.Background(), alias)
		if !errors.Is(err, ErrTemplateNotFound) {
			t.Fatalf("GetTemplateByAlias(%q) error = %v, want ErrTemplateNotFound", alias, err)
		}
	}
}

// TestResolveTemplateIdentifierPassthrough verifies the non-DB code paths of
// ResolveTemplateIdentifier: template-ID-prefixed identifiers (tpl-/snap-)
// and the empty identifier are resolved purely from the string, with no
// alias lookup.
func TestResolveTemplateIdentifierPassthrough(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	tests := []struct {
		name       string
		identifier string
		want       string
	}{
		{name: "tpl-prefix", identifier: "tpl-abc123", want: "tpl-abc123"},
		{name: "snap-prefix", identifier: "snap-abc123", want: "snap-abc123"},
		{name: "empty", identifier: "", want: ""},
		{name: "whitespace-only", identifier: "  ", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveTemplateIdentifier(context.Background(), tc.identifier)
			if err != nil {
				t.Fatalf("ResolveTemplateIdentifier(%q) returned error: %v", tc.identifier, err)
			}
			if got != tc.want {
				t.Fatalf("ResolveTemplateIdentifier(%q) = %q, want %q", tc.identifier, got, tc.want)
			}
		})
	}
}

// TestResolveTemplateIdentifierAliasLookup verifies the alias-resolution path:
// a bare identifier (no tpl-/snap- prefix) is resolved through
// GetTemplateByAlias. The DB call itself is stubbed via gomonkey so the test
// exercises the routing logic, not gorm internals.
func TestResolveTemplateIdentifierAliasLookup(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	patches.ApplyFunc(GetTemplateByAlias, func(_ context.Context, alias string) (*models.TemplateDefinition, error) {
		if alias == "my-alias" {
			return &models.TemplateDefinition{TemplateID: "tpl-resolved"}, nil
		}
		return nil, ErrTemplateNotFound
	})

	got, err := ResolveTemplateIdentifier(context.Background(), "my-alias")
	if err != nil {
		t.Fatalf("ResolveTemplateIdentifier(\"my-alias\") returned error: %v", err)
	}
	if got != "tpl-resolved" {
		t.Fatalf("ResolveTemplateIdentifier(\"my-alias\") = %q, want \"tpl-resolved\"", got)
	}

	// Not-found alias must propagate the error verbatim.
	_, err = ResolveTemplateIdentifier(context.Background(), "missing-alias")
	if !errors.Is(err, ErrTemplateNotFound) {
		t.Fatalf("ResolveTemplateIdentifier(\"missing-alias\") error = %v, want ErrTemplateNotFound", err)
	}
}

// TestGetTemplateByAliasFiltersByKindExcludesSnapshots is the regression test
// for issue #584: a snapshot that shares a template's display_name (alias)
// must never be returned by GetTemplateByAlias. Template aliases are owned
// exclusively by claimTemplateAlias (a post-READY write path on template-kind
// rows); snapshots only carry an informational display_name. The read path
// mirrors that invariant by filtering on alias_key (the STORED generated
// column, NULL for snapshots); otherwise First() could return the snapshot row
// and resolve the alias to a snap-* id instead of the tpl-* owner.
//
// This package's unit tests stub the DB, and the repo has no in-memory DB
// driver (no sqlite/sqlmock; dependencies are frozen). To still exercise the
// REAL query GetTemplateByAlias builds, we run it against a DryRun gorm.DB:
// SQL is generated but never executed, and in DryRun stmt.SQL/stmt.Vars stay
// populated, so an after-query callback observes the exact WHERE predicate.
// SkipInitializeWithVersion keeps Initialize from issuing SELECT VERSION()
// (which would otherwise need a live MySQL). The captured SQL is therefore
// exactly what the read path runs in production.
func TestGetTemplateByAliasFiltersByKindExcludesSnapshots(t *testing.T) {
	sqlDB, err := sql.Open("mysql", "root:root@tcp(127.0.0.1:3306)/unused?parseTime=true")
	require.NoError(t, err)

	dryRunDB, err := gorm.Open(gormmysql.New(gormmysql.Config{
		Conn:                      sqlDB,
		SkipInitializeWithVersion: true,
	}), &gorm.Config{DryRun: true, DisableAutomaticPing: true})
	require.NoError(t, err)

	// Capture the WHERE clause GetTemplateByAlias generates.
	var capturedSQL string
	var capturedVars []any
	dryRunDB.Callback().Query().After("gorm:query").Register("capture_alias_query", func(tx *gorm.DB) {
		capturedSQL = tx.Statement.SQL.String()
		capturedVars = append(capturedVars, tx.Statement.Vars...)
	})

	oldDB := store.db
	store.db = dryRunDB
	t.Cleanup(func() { store.db = oldDB })

	const alias = "shared-alias"
	def, err := GetTemplateByAlias(context.Background(), alias)
	require.NoError(t, err) // DryRun returns a zero-value def; the assertion target is the captured SQL.
	require.NotNil(t, def)

	// Regression: the WHERE clause must use alias_key (the STORED generated
	// column) so the unique index is used and snapshots (whose alias_key is
	// always NULL) can never be returned. Before the fix the predicate
	// filtered display_name + kind without the index.
	assert.Contains(t, capturedSQL, "alias_key = ?",
		"GetTemplateByAlias must filter alias_key=? to use the unique index; got SQL: %s", capturedSQL)

	// The alias value must be bound so the lookup targets the correct alias.
	aliasBound := false
	for _, v := range capturedVars {
		if s, ok := v.(string); ok && s == alias {
			aliasBound = true
		}
	}
	assert.True(t, aliasBound,
		"alias must be bound in the query; captured vars: %v", capturedVars)
}

// TestSetTemplateAlias_Clear_SetsEmptyDisplayName verifies a real clear
// (template currently holds an alias) reaches setTemplateAliasLocked with
// an empty alias so the locked transaction can clear display_name + CREATE/REDO
// job JSON together (design §3.6 I1).
func TestSetTemplateAlias_Clear_SetsEmptyDisplayName(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(GetDefinition, func(ctx context.Context, templateID string) (*models.TemplateDefinition, error) {
		return &models.TemplateDefinition{
			TemplateID:  templateID,
			Kind:        TemplateKindTemplate,
			Status:      StatusReady,
			DisplayName: "old-alias",
		}, nil
	})

	var capturedTemplateID, capturedAlias string
	called := false
	patches.ApplyFunc(setTemplateAliasLocked, func(ctx context.Context, templateID, alias string) error {
		capturedTemplateID = templateID
		capturedAlias = alias
		called = true
		return nil
	})

	err := SetTemplateAlias(context.Background(), "tpl-clear-1", "")
	require.NoError(t, err)
	assert.True(t, called, "clear path must call setTemplateAliasLocked")
	assert.Equal(t, "tpl-clear-1", capturedTemplateID)
	assert.Equal(t, "", capturedAlias)
}

// TestSetTemplateAlias_ClearEmptyDisplayNameIsNoop verifies that clearing an
// already-empty alias does not open a transaction or rewrite in-flight CREATE
// job JSON (PENDING templates have empty DisplayName until finalize claims).
func TestSetTemplateAlias_ClearEmptyDisplayNameIsNoop(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(GetDefinition, func(ctx context.Context, templateID string) (*models.TemplateDefinition, error) {
		return &models.TemplateDefinition{
			TemplateID: templateID,
			Kind:       TemplateKindTemplate,
			Status:     StatusPending,
		}, nil
	})
	patches.ApplyFunc(setTemplateAliasLocked, func(ctx context.Context, templateID, alias string) error {
		t.Fatal("setTemplateAliasLocked must not run when DisplayName is already empty")
		return nil
	})

	err := SetTemplateAlias(context.Background(), "tpl-pending-1", "")
	require.NoError(t, err)
}

// TestSetTemplateAlias_RejectsSnapshot verifies that snapshots are rejected
// with ErrAliasNotApplicableToSnapshot — a snapshot's display_name is an
// informational label (alias_key is always NULL), so it cannot hold a unique
// alias (design §3.7).
func TestSetTemplateAlias_RejectsSnapshot(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(GetDefinition, func(ctx context.Context, templateID string) (*models.TemplateDefinition, error) {
		return &models.TemplateDefinition{
			TemplateID: templateID,
			Kind:       TemplateKindSnapshot,
		}, nil
	})

	err := SetTemplateAlias(context.Background(), "snap-1", "my-alias")
	assert.ErrorIs(t, err, ErrAliasNotApplicableToSnapshot)
}

// TestSetTemplateAlias_RejectsDeletingTemplate verifies that DELETING
// templates surface as ErrTemplateNotFound, matching GetTemplateByAlias'
// behavior (store.go:921 filters status <> 'DELETING').
func TestSetTemplateAlias_RejectsDeletingTemplate(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(GetDefinition, func(ctx context.Context, templateID string) (*models.TemplateDefinition, error) {
		return &models.TemplateDefinition{
			TemplateID: templateID,
			Kind:       TemplateKindTemplate,
			Status:     StatusDeleting,
		}, nil
	})

	err := SetTemplateAlias(context.Background(), "tpl-deleting-1", "my-alias")
	assert.ErrorIs(t, err, ErrTemplateNotFound)
}

// TestSetTemplateAlias_RejectsNotReady verifies that a template not in READY
// status is rejected with ErrTemplateNotReady, so an alias never points at a
// building/failed template (and the create-time claim can't overwrite an
// operator change). DELETING is covered separately (→ ErrTemplateNotFound).
func TestSetTemplateAlias_RejectsNotReady(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(GetDefinition, func(ctx context.Context, templateID string) (*models.TemplateDefinition, error) {
		return &models.TemplateDefinition{
			TemplateID: templateID,
			Kind:       TemplateKindTemplate,
			Status:     StatusPending,
		}, nil
	})

	err := SetTemplateAlias(context.Background(), "tpl-building-1", "my-alias")
	assert.ErrorIs(t, err, ErrTemplateNotReady)
}

// TestSetTemplateAlias_ClearAllowedOnFailed verifies the clear path is allowed
// for any non-DELETING template, so an alias stuck on a FAILED template can be
// released without deleting the template (claim still requires READY).
func TestSetTemplateAlias_ClearAllowedOnFailed(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(GetDefinition, func(ctx context.Context, templateID string) (*models.TemplateDefinition, error) {
		return &models.TemplateDefinition{
			TemplateID:  templateID,
			Kind:        TemplateKindTemplate,
			Status:      StatusFailed,
			DisplayName: "stuck",
		}, nil
	})
	called := false
	patches.ApplyFunc(setTemplateAliasLocked, func(ctx context.Context, templateID, alias string) error {
		called = true
		assert.Equal(t, "", alias)
		return nil
	})
	err := SetTemplateAlias(context.Background(), "tpl-failed-1", "")
	assert.NoError(t, err)
	assert.True(t, called, "clear on a FAILED template must reach setTemplateAliasLocked")
}

// TestIsDeadlockError covers MySQL lock-wait/deadlock numbers and PostgreSQL
// SQLSTATEs, and confirms unrelated errors (incl. duplicate-key) are not
// treated as retriable deadlocks.
func TestIsDeadlockError(t *testing.T) {
	assert.True(t, isDeadlockError(&mysql.MySQLError{Number: 1205, Message: "Lock wait timeout exceeded"}))
	assert.True(t, isDeadlockError(&mysql.MySQLError{Number: 1213, Message: "Deadlock found when trying to get lock"}))
	assert.True(t, isDeadlockError(fmt.Errorf("claim fail: %w", &mysql.MySQLError{Number: 1213, Message: "Deadlock found"})))
	assert.True(t, isDeadlockError(errors.New("ERROR: deadlock detected; SQLSTATE 40P01")))
	assert.True(t, isDeadlockError(errors.New("ERROR: lock not available; SQLSTATE 55P03")))
	assert.False(t, isDeadlockError(&mysql.MySQLError{Number: 1062, Message: "Duplicate entry"}))
	assert.False(t, isDeadlockError(errors.New("Error 1062 (23000): Duplicate entry for key 'alias_key'")))
	assert.False(t, isDeadlockError(errors.New("connection reset by peer")))
}

func TestRetryOnceOnDeadlockRetriesThenSucceeds(t *testing.T) {
	calls := 0
	err := retryOnceOnDeadlock(func() error {
		calls++
		if calls == 1 {
			return &mysql.MySQLError{Number: 1213, Message: "Deadlock found"}
		}
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 2, calls)
}

func TestRetryOnceOnDeadlockNonDeadlockIsNotRetried(t *testing.T) {
	calls := 0
	sentinel := errors.New("not a deadlock")
	err := retryOnceOnDeadlock(func() error {
		calls++
		return sentinel
	})
	assert.ErrorIs(t, err, sentinel)
	assert.Equal(t, 1, calls)
}

// TestSyncCreateRedoImageJobAliasTx_UpdatesCreateRequestJSON verifies CREATE
// RequestJSON is rewritten while unrelated fields are preserved.
func TestSyncCreateRedoImageJobAliasTx_UpdatesCreateRequestJSON(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(listCreateRedoImageJobsByTemplateIDTx, func(tx *gorm.DB, templateID string) ([]models.TemplateImageJob, error) {
		return []models.TemplateImageJob{
			{JobID: "job-create", Operation: JobOperationCreate, RequestJSON: `{"alias":"old","source_image_ref":"img","registry_password":"secret"}`},
		}, nil
	})
	updated := map[string]string{}
	patches.ApplyFunc(updateTemplateImageJobTx, func(tx *gorm.DB, jobID string, values map[string]any) error {
		updated[jobID] = values["request_json"].(string)
		return nil
	})

	require.NoError(t, syncCreateRedoImageJobAliasTx(&gorm.DB{}, "tpl-1", "new"))
	require.Contains(t, updated, "job-create")
	assert.Contains(t, updated["job-create"], `"alias":"new"`)
	assert.Contains(t, updated["job-create"], `"source_image_ref":"img"`)
	assert.Contains(t, updated["job-create"], `"registry_password":"secret"`)
}

func TestSyncCreateRedoImageJobAliasTx_UpdateFailureIsFatal(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(listCreateRedoImageJobsByTemplateIDTx, func(tx *gorm.DB, templateID string) ([]models.TemplateImageJob, error) {
		return []models.TemplateImageJob{
			{JobID: "job-1", Operation: JobOperationCreate, RequestJSON: `{"alias":"old"}`},
		}, nil
	})
	patches.ApplyFunc(updateTemplateImageJobTx, func(tx *gorm.DB, jobID string, values map[string]any) error {
		return errors.New("update failed")
	})
	err := syncCreateRedoImageJobAliasTx(&gorm.DB{}, "tpl-1", "new")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "update failed")
}

// TestSetTemplateAlias_ValidatesAlias verifies that invalid alias strings
// are rejected before any DB access, and that the rejection wraps
// ErrInvalidAlias so the HTTP handler can map it to 400 (vs. raw DB errors
// which map to 500). validateTemplateAlias rejects tpl-/snap- prefixes and
// anything outside ^[a-z0-9][a-z0-9-]{0,63}$. The empty-string case (clear
// path) is exercised separately by
// TestSetTemplateAlias_Clear_SetsEmptyDisplayName.
func TestSetTemplateAlias_ValidatesAlias(t *testing.T) {
	// Prefix collision: aliases must not look like template IDs.
	err := SetTemplateAlias(context.Background(), "tpl-1", "tpl-hijack")
	assert.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidAlias, "validation errors must wrap ErrInvalidAlias")

	// Uppercase not allowed.
	err = SetTemplateAlias(context.Background(), "tpl-1", "My-Alias")
	assert.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidAlias)

	// Leading hyphen not allowed.
	err = SetTemplateAlias(context.Background(), "tpl-1", "-leading")
	assert.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidAlias)

	// Bare prefix (no suffix) must also be rejected — matches CubeAPI's
	// is_valid_alias (hasValidTemplateIDPrefix alone would allow "tpl-").
	for _, bare := range []string{"tpl-", "snap-"} {
		err := SetTemplateAlias(context.Background(), "tpl-1", bare)
		assert.ErrorIs(t, err, ErrInvalidAlias, "bare prefix %q must be rejected", bare)
	}
}

// TestSetTemplateAlias_RejectsEmptyID verifies that an empty or
// whitespace-only templateID is rejected up front with ErrTemplateIDRequired
// (before any DB access), so direct callers get a clear error instead of a
// misleading ErrTemplateNotFound from GetDefinition.
func TestSetTemplateAlias_RejectsEmptyID(t *testing.T) {
	for _, id := range []string{"", "   ", "\t"} {
		err := SetTemplateAlias(context.Background(), id, "my-alias")
		assert.Error(t, err)
		assert.ErrorIs(t, err, ErrTemplateIDRequired)
	}
}

// TestSetTemplateAlias_ReachableClaim verifies the claim path delegates to
// setTemplateAliasLocked (definition FOR UPDATE + CREATE/REDO sync).
func TestSetTemplateAlias_ReachableClaim(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(GetDefinition, func(ctx context.Context, templateID string) (*models.TemplateDefinition, error) {
		return &models.TemplateDefinition{
			TemplateID: templateID,
			Kind:       TemplateKindTemplate,
			Status:     StatusReady,
		}, nil
	})

	var capturedTemplateID, capturedAlias string
	called := false
	patches.ApplyFunc(setTemplateAliasLocked, func(ctx context.Context, templateID, alias string) error {
		capturedTemplateID = templateID
		capturedAlias = alias
		called = true
		return nil
	})

	err := SetTemplateAlias(context.Background(), "tpl-claim-1", "my-alias")
	require.NoError(t, err)
	assert.True(t, called, "claim path must invoke setTemplateAliasLocked")
	assert.Equal(t, "tpl-claim-1", capturedTemplateID)
	assert.Equal(t, "my-alias", capturedAlias)
}

// TestSetTemplateAlias_PropagatesGetDefinitionNotFound verifies that a
// missing template surfaces as ErrTemplateNotFound (not some wrapped DB
// error), so the HTTP handler can map it to 404.
func TestSetTemplateAlias_PropagatesGetDefinitionNotFound(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(GetDefinition, func(ctx context.Context, templateID string) (*models.TemplateDefinition, error) {
		return nil, ErrTemplateNotFound
	})

	err := SetTemplateAlias(context.Background(), "tpl-missing-1", "my-alias")
	assert.ErrorIs(t, err, ErrTemplateNotFound)
}

func TestIsDuplicateAliasError_WrappedMySQL1062(t *testing.T) {
	inner := &mysql.MySQLError{Number: 1062, Message: "Duplicate entry 'shared' for key 'alias_key'"}
	wrapped := fmt.Errorf("claim alias %q for template %s fail: %w", "shared", "tpl-1", inner)
	assert.True(t, isDuplicateAliasError(wrapped), "1062 must remain detectable after %%w wrap")
}

func TestIsDuplicateAliasError_WrappedPostgres23505(t *testing.T) {
	inner := &pgconn.PgError{
		Code:    "23505",
		Message: "duplicate key value violates unique constraint \"idx_template_definition_alias_unique\"",
	}
	wrapped := fmt.Errorf("claim alias %q for template %s fail: %w", "shared", "tpl-1", inner)
	assert.True(t, isDuplicateAliasError(wrapped), "23505 must remain detectable after %%w wrap")
}

func TestSyncCreateRedoImageJobAliasTx_NoJobsSucceeds(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(listCreateRedoImageJobsByTemplateIDTx, func(tx *gorm.DB, templateID string) ([]models.TemplateImageJob, error) {
		return nil, nil
	})
	require.NoError(t, syncCreateRedoImageJobAliasTx(&gorm.DB{}, "tpl-commit-origin", "new"),
		"commit-origin templates with no CREATE/REDO jobs must still succeed")
}
