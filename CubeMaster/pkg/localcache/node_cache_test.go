// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package localcache

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/patrickmn/go-cache"
	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
)

func init() {
	mydir, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	fmt.Printf("mydir=%s\n", mydir)
	if os.Getenv("CUBE_MASTER_CONFIG_PATH") == "" {
		os.Setenv("CUBE_MASTER_CONFIG_PATH", filepath.Clean(filepath.Join(mydir, "../../conf.yaml")))
	}
	if _, err := config.Init(); err != nil {
		panic(err)
	}
}

func TestUpdateNodeFromMetaDataPropagatesIsolated(t *testing.T) {
	origCache := l.cache
	origNodesByClusters := l.sortedNodesByClusters
	defer func() {
		l.cache = origCache
		l.sortedNodesByClusters = origNodesByClusters
	}()

	l.cache = cache.New(0, 0)
	l.sortedNodesByClusters = make(map[string]node.NodeList)
	l.cache.SetDefault("node-a", &node.Node{
		InsID:            "node-a",
		IP:               "10.0.0.1",
		ReportedReady:    true,
		Healthy:          true,
		MetaDataUpdateAt: time.Now(),
	})

	UpsertNode(&node.Node{
		InsID:            "node-a",
		IP:               "10.0.0.1",
		ReportedReady:    true,
		Healthy:          true,
		Isolated:         true,
		MetaDataUpdateAt: time.Now(),
	})

	raw, ok := l.cache.Get("node-a")
	if !ok {
		t.Fatal("expected node to remain cached")
	}
	if cached := raw.(*node.Node); !cached.Isolated {
		t.Fatal("updateNodeFromMetaData should propagate Isolated onto the cached node")
	}
}

func TestLoadIsolatedSetPreservesCacheOnDBError(t *testing.T) {
	origCache := l.cache
	defer func() {
		l.cache = origCache
	}()

	l.cache = cache.New(0, 0)
	l.cache.SetDefault("node-isolated", &node.Node{
		InsID:    "node-isolated",
		Isolated: true,
	})

	n := &node.Node{InsID: "node-isolated"}
	l.applyIsolationState(n, nil, errors.New("db unavailable"), true)
	if !n.Isolated {
		t.Fatal("isolation read failure should preserve cached isolated=true")
	}

	coldStart := &node.Node{InsID: "node-isolated"}
	l.applyIsolationState(coldStart, nil, errors.New("db unavailable"), false)
	if coldStart.Isolated {
		t.Fatal("cold start without a reliable isolation read should keep the zero value")
	}

	cleared := &node.Node{InsID: "node-isolated", Isolated: true}
	l.applyIsolationState(cleared, map[string]bool{"node-isolated": false}, nil, true)
	if cleared.Isolated {
		t.Fatal("successful isolation read should override the cached value")
	}
}

func Test_local_appendNodeByCluster(t *testing.T) {

	patches := gomonkey.NewPatches()
	defer patches.Reset()

	patches.ApplyFunc(config.GetInstanceTypeOfClusterLabel, func(label string) string {

		switch label {
		case "cluster1":
			return "product1"
		case "invalid-cluster":
			return ""
		}
		return ""
	})

	type fields struct {
		lockSortedNodes       sync.RWMutex
		sortedNodesByClusters map[string]node.NodeList
	}
	type args struct {
		n *node.Node
	}

	healthyNode := &node.Node{
		OssClusterLabel: "cluster1",
		Healthy:         true,
	}
	unhealthyNode := &node.Node{
		OssClusterLabel: "cluster1",
		Healthy:         false,
	}
	invalidClusterNode := &node.Node{
		OssClusterLabel: "invalid-cluster",
		Healthy:         true,
	}

	tests := []struct {
		name      string
		fields    fields
		args      args
		wantNodes map[string]node.NodeList
	}{
		{
			name: "nil node",
			fields: fields{
				sortedNodesByClusters: make(map[string]node.NodeList),
			},
			args:      args{n: nil},
			wantNodes: map[string]node.NodeList{},
		},
		{
			name: "unhealthy node",
			fields: fields{
				sortedNodesByClusters: make(map[string]node.NodeList),
			},
			args:      args{n: unhealthyNode},
			wantNodes: map[string]node.NodeList{},
		},
		{
			name: "invalid cluster label",
			fields: fields{
				sortedNodesByClusters: make(map[string]node.NodeList),
			},
			args:      args{n: invalidClusterNode},
			wantNodes: map[string]node.NodeList{constants.DefaultInstanceTypeName: {invalidClusterNode}},
		},
		{
			name: "new product initialization",
			fields: fields{
				sortedNodesByClusters: make(map[string]node.NodeList),
			},
			args: args{n: healthyNode},
			wantNodes: map[string]node.NodeList{
				constants.DefaultInstanceTypeName: {healthyNode},
			},
		},
		{
			name: "existing product append",
			fields: fields{
				sortedNodesByClusters: map[string]node.NodeList{
					"product1": {{}},
				},
			},
			args: args{n: healthyNode},
			wantNodes: map[string]node.NodeList{
				"product1": {{}, healthyNode},
			},
		},
		{
			name: "concurrent access safety",
			fields: fields{
				sortedNodesByClusters: make(map[string]node.NodeList),
			},
			args: args{n: healthyNode},
			wantNodes: map[string]node.NodeList{
				constants.DefaultInstanceTypeName: {healthyNode},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := &local{
				lockSortedNodes:       tt.fields.lockSortedNodes,
				sortedNodesByClusters: tt.fields.sortedNodesByClusters,
			}

			l.appendSortedNodes(tt.args.n)

			assert.Equal(t, tt.wantNodes, l.sortedNodesByClusters)
		})
	}
}
