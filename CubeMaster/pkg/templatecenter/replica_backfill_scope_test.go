// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/glebarez/sqlite"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// A stored scope names the nodes of the moment the job ran. Entries whose node
// has since gone must be skipped, not fail the whole template.
func TestResolveBackfillNodesSkipsGoneScopeEntries(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(healthyTemplateNodes, func(string) []*node.Node {
		return []*node.Node{testNode("node-a", "10.0.0.1"), testNode("node-b", "10.0.0.2"), testNode("node-c", "10.0.0.3")}
	})

	ids := func(nodes []*node.Node) []string {
		out := make([]string, 0, len(nodes))
		for _, n := range nodes {
			out = append(out, n.ID())
		}
		return out
	}
	for _, tc := range []struct {
		name  string
		scope []string
		want  []string
		err   error
	}{
		{name: "unscoped", scope: nil, want: []string{"node-a", "node-b", "node-c"}},
		{name: "blank entries only", scope: []string{" ", ""}, want: []string{"node-a", "node-b", "node-c"}},
		{name: "id and ip, one entry gone", scope: []string{"node-a", "10.0.0.3", "node-gone"}, want: []string{"node-a", "node-c"}},
		{name: "every entry gone", scope: []string{"node-gone", "10.0.0.9"}, err: ErrNoTemplateNodes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveBackfillNodes("cubebox", tc.scope)
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Fatalf("err = %v, want %v", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveBackfillNodes: %v", err)
			}
			if !reflect.DeepEqual(ids(got), tc.want) {
				t.Fatalf("nodes = %v, want %v", ids(got), tc.want)
			}
		})
	}
}

func TestResolveBackfillNodesNoHealthyNodes(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(healthyTemplateNodes, func(string) []*node.Node { return nil })

	if _, err := resolveBackfillNodes("cubebox", []string{"node-a"}); !errors.Is(err, ErrNoTemplateNodes) {
		t.Fatalf("err = %v, want ErrNoTemplateNodes", err)
	}
}

// backfillCreateScope reads the latest CREATE job (highest attempt), ignores
// redo jobs, and treats a template without a create job as unscoped.
func TestBackfillCreateScopeFromJobTable(t *testing.T) {
	dbName := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dbName), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.TemplateImageJob{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	oldDB := store.db
	store.db = db
	t.Cleanup(func() { store.db = oldDB })

	request := func(scope ...string) string {
		raw, err := json.Marshal(&sandboxtypes.CreateTemplateFromImageReq{
			InstanceType: "cubebox", SourceImageRef: "docker.io/library/nginx:latest", WritableLayerSize: "4Gi", DistributionScope: scope,
		})
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		return string(raw)
	}
	rows := []models.TemplateImageJob{
		{JobID: "create-1", TemplateID: "tpl-1", Operation: JobOperationCreate, AttemptNo: 1, RequestJSON: request("node-old")},
		{JobID: "create-2", TemplateID: "tpl-1", Operation: JobOperationCreate, AttemptNo: 2, RequestJSON: request("node-a", "node-b")},
		{JobID: "redo-3", TemplateID: "tpl-1", Operation: JobOperationRedo, AttemptNo: 3, RequestJSON: request("node-a")},
		{JobID: "redo-only", TemplateID: "tpl-2", Operation: JobOperationRedo, AttemptNo: 1, RequestJSON: request("node-a")},
		{JobID: "create-bad", TemplateID: "tpl-3", Operation: JobOperationCreate, AttemptNo: 1, RequestJSON: "{not json"},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatalf("insert jobs: %v", err)
	}

	ctx := context.Background()
	got, err := backfillCreateScope(ctx, "tpl-1")
	if err != nil {
		t.Fatalf("tpl-1: %v", err)
	}
	if want := []string{"node-a", "node-b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("tpl-1 scope = %v, want %v (latest create, not the redo)", got, want)
	}
	if got, err := backfillCreateScope(ctx, "tpl-2"); err != nil || got != nil {
		t.Fatalf("tpl-2 scope = %v, err = %v; want nil, nil (no create job)", got, err)
	}
	if _, err := backfillCreateScope(ctx, "tpl-3"); err == nil {
		t.Fatal("tpl-3: want a decode error for a malformed create request")
	}
}
