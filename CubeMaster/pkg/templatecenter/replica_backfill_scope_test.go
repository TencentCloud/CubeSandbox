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

// backfillPlacementScope starts from the latest CREATE job (highest attempt)
// and adds the nodes of every REDO after it. Redos before that create, a
// create without a scope and a template without a create job leave it
// unscoped (nil).
func TestBackfillPlacementScopeFromJobTable(t *testing.T) {
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
	job := func(id, tpl, op string, attempt int32, requestJSON string) models.TemplateImageJob {
		return models.TemplateImageJob{JobID: id, TemplateID: tpl, Operation: op, AttemptNo: attempt, RequestJSON: requestJSON}
	}
	rows := []models.TemplateImageJob{
		// tpl-1: an older create and the redo before the latest create do
		// not count; the redos after it add their nodes.
		job("t1-create-1", "tpl-1", JobOperationCreate, 1, request("node-old")),
		job("t1-redo-2", "tpl-1", JobOperationRedo, 2, request("node-stale")),
		job("t1-create-3", "tpl-1", JobOperationCreate, 3, request("node-a", "node-b")),
		job("t1-redo-4", "tpl-1", JobOperationRedo, 4, request("node-c", "node-a")),
		job("t1-redo-5", "tpl-1", JobOperationRedo, 5, request("node-d")),
		// tpl-2: the create's only node is gone and a redo moved the
		// template; the redo node must stay in scope.
		job("t2-create-1", "tpl-2", JobOperationCreate, 1, request("node-gone")),
		job("t2-redo-2", "tpl-2", JobOperationRedo, 2, request("node-b")),
		// tpl-3: unscoped create stays unscoped.
		job("t3-create-1", "tpl-3", JobOperationCreate, 1, request()),
		job("t3-redo-2", "tpl-3", JobOperationRedo, 2, request("node-a")),
		// tpl-4: no create on record.
		job("t4-redo-1", "tpl-4", JobOperationRedo, 1, request("node-a")),
		// tpl-5: malformed create request.
		job("t5-create-1", "tpl-5", JobOperationCreate, 1, "{not json"),
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatalf("insert jobs: %v", err)
	}

	ctx := context.Background()
	for _, tc := range []struct {
		tpl  string
		want []string
	}{
		{tpl: "tpl-1", want: []string{"node-a", "node-b", "node-c", "node-d"}},
		{tpl: "tpl-2", want: []string{"node-gone", "node-b"}},
		{tpl: "tpl-3", want: nil},
		{tpl: "tpl-4", want: nil},
	} {
		got, err := backfillPlacementScope(ctx, tc.tpl)
		if err != nil {
			t.Fatalf("%s: %v", tc.tpl, err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s scope = %v, want %v", tc.tpl, got, tc.want)
		}
	}
	if _, err := backfillPlacementScope(ctx, "tpl-5"); err == nil {
		t.Fatal("tpl-5: want a decode error for a malformed create request")
	}
}
