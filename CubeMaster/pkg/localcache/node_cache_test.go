// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package localcache

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
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
