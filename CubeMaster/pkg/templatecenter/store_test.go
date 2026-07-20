// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"gorm.io/driver/mysql"
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
	patches.ApplyFunc(finalizeTemplateReplicas, func(ctx context.Context, templateID, instanceType, version string, replicas []ReplicaStatus) (*TemplateInfo, error) {
		return &TemplateInfo{TemplateID: templateID, InstanceType: instanceType, Version: version, Replicas: replicas}, nil
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

	dryRunDB, err := gorm.Open(mysql.New(mysql.Config{
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
