// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package node

import (
	"crypto/rand"
	"fmt"
	"math"
	"math/big"
	pseudorand "math/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestRemove(t *testing.T) {
	nodes := NodeList{}
	testNum := 10
	for i := 1; i <= testNum; i++ {
		n := &Node{
			Index: i,
			InsID: fmt.Sprintf("%d", i),
		}
		nodes.Append(n)
	}
	if testNum != nodes.Len() {
		t.Fatalf("testNum != nodes.Len(), testNum: %d, nodes.Len(): %d", testNum, nodes.Len())
	}
	nodes.Add(&Node{InsID: "1"}, &Node{InsID: "2"}, &Node{InsID: "3"})
	if testNum != nodes.Len() {
		t.Fatalf("after Add,testNum != nodes.Len(), testNum: %d, nodes.Len(): %d", testNum, nodes.Len())
	}
	l := nodes.AllSortByIndex()
	l.Remove(&Node{InsID: fmt.Sprint(testNum)})
}
func TestSorted(t *testing.T) {
	nodes := NodeList{}
	testNum := 100
	for i := 1; i <= testNum; i++ {
		n := &Node{
			Index: i,
			InsID: fmt.Sprintf("%d", i),
		}
		nodes.Append(n)
	}
	if testNum != nodes.Len() {
		t.Fatalf("testNum != nodes.Len(), testNum: %d, nodes.Len(): %d", testNum, nodes.Len())
	}
	nodes.Add(&Node{InsID: "1"}, &Node{InsID: "2"}, &Node{InsID: "3"})
	if testNum != nodes.Len() {
		t.Fatalf("after Add,testNum != nodes.Len(), testNum: %d, nodes.Len(): %d", testNum, nodes.Len())
	}

	l := nodes.AllSortByIndex()
	for i := 1; i < testNum; i++ {
		assert.Less(t, l[i-1].Index, l[i].Index)
	}
	oldNum := nodes.Len()
	nodes.Remove(&Node{InsID: "1"}, &Node{InsID: "2"}, &Node{InsID: "3"})
	if oldNum-3 != nodes.Len() {
		t.Fatalf(
			"oldNum-3 != nodes.Len(), oldNum: %d, nodes.Len(): %d",
			oldNum-3,
			nodes.Len())
	}
	oldNum = nodes.Len()
	nodes.Remove(&Node{InsID: "1"}, &Node{InsID: "99"}, &Node{InsID: "4"})
	if oldNum-2 != nodes.Len() {
		t.Fatalf(
			"oldNum-2 != nodes.Len(), oldNum: %d, nodes.Len(): %d",
			oldNum-2,
			nodes.Len())
	}

	oldNum = nodes.Len()
	nodes.Remove(&Node{InsID: "1"}, &Node{InsID: "99"}, &Node{InsID: "4"})
	if oldNum != nodes.Len() {
		t.Fatalf(
			"oldNum != nodes.Len(), oldNum: %d, nodes.Len(): %d",
			oldNum,
			nodes.Len())
	}
}

func TestNodes_IndexByPage(t *testing.T) {
	nodes := NodeList{}
	assert.Equal(t, 0, nodes.Len())
	nodes.AllSortByIndex()
	result, endIndex := nodes.IndexByPage(2, 5)
	if len(result) != 0 {
		t.Errorf("Expected %d got %d", 0, len(result))
	}
	testNum := 100
	for i := 1; i <= testNum; i++ {
		n := &Node{
			Index: i,
			InsID: fmt.Sprintf("%d", i),
		}
		nodes.Append(n)
	}
	assert.Equal(t, testNum, nodes.Len())
	nodes.AllSortByIndex()

	result, endIndex = nodes.IndexByPage(2, 5)

	if len(result) != 5 {
		t.Errorf("Expected %d got %d", 5, len(result))
	}

	assert.Equal(t, result[4].Index, endIndex)
	if result[4].Index != 6 {
		t.Errorf("Expected [nodes 2 3 4 5 6] but got %+v", result)
	}

	for i := 1; i < len(result); i++ {
		assert.Less(t, result[i-1].Index, result[i].Index)
	}

	result, endIndex = nodes.IndexByPage(0, 1)
	assert.Equal(t, 1, len(result))
	assert.Equal(t, 1, endIndex)

	result, endIndex = nodes.IndexByPage(0, 5)
	assert.Equal(t, 5, len(result))
	assert.Equal(t, 5, endIndex)

	result, endIndex = nodes.IndexByPage(0, testNum+1)
	assert.Equal(t, testNum, len(result))
	assert.Equal(t, testNum, endIndex)

	result, endIndex = nodes.IndexByPage(0, testNum+10)
	assert.Equal(t, testNum, len(result))
	assert.Equal(t, testNum, endIndex)
}

func TestNodes_IndexByPageError(t *testing.T) {
	nodes := NodeList{}
	testNum := 100
	for i := 1; i <= testNum; i++ {
		n := &Node{
			Index: i,
			InsID: fmt.Sprintf("%d", i),
		}
		nodes.Append(n)
	}
	assert.Equal(t, testNum, nodes.Len())
	nodes.AllSortByIndex()
	result, endIndex := nodes.IndexByPage(-1, 5)
	assert.Equal(t, 0, len(result))
	assert.Equal(t, -1, endIndex)

	result, endIndex = nodes.IndexByPage(0, 0)
	assert.Equal(t, 0, len(result))
	assert.Equal(t, -1, endIndex)

	result, endIndex = nodes.IndexByPage(0, -1)
	assert.Equal(t, 0, len(result))
	assert.Equal(t, -1, endIndex)
}

func TestNodes_IndexByPageFallbackWithoutNodeIndex(t *testing.T) {
	nodes := NodeList{}
	for i := 1; i <= 3; i++ {
		nodes.Append(&Node{InsID: fmt.Sprintf("node-%d", i)})
	}

	result, endIndex := nodes.IndexByPage(1, 2)
	if len(result) != 2 {
		t.Fatalf("Expected %d got %d", 2, len(result))
	}
	assert.Equal(t, "node-1", result[0].InsID)
	assert.Equal(t, "node-2", result[1].InsID)
	assert.Equal(t, 2, endIndex)

	result, endIndex = nodes.IndexByPage(3, 2)
	if len(result) != 1 {
		t.Fatalf("Expected %d got %d", 1, len(result))
	}
	assert.Equal(t, "node-3", result[0].InsID)
	assert.Equal(t, 3, endIndex)
}

func TestNodes_IndexByPageFallbackTreatsZeroAsFirstPage(t *testing.T) {
	nodes := NodeList{
		&Node{InsID: "node-1"},
		&Node{InsID: "node-2"},
	}

	result, endIndex := nodes.IndexByPage(0, 1)
	if len(result) != 1 {
		t.Fatalf("Expected %d got %d", 1, len(result))
	}
	assert.Equal(t, "node-1", result[0].InsID)
	assert.Equal(t, 1, endIndex)
}

func TestNodes_IndexByPageFallbackRejectsOutOfRangeStart(t *testing.T) {
	nodes := NodeList{
		&Node{InsID: "node-1"},
		&Node{InsID: "node-2"},
	}

	result, endIndex := nodes.IndexByPage(3, 1)
	assert.Equal(t, 0, len(result))
	assert.Equal(t, -1, endIndex)
}

func TestRandNode(t *testing.T) {
	nodes := NodeList{}
	testNum := 10
	for i := 1; i <= testNum; i++ {
		n := &Node{
			Index: i,
			InsID: fmt.Sprintf("%d", i),
		}
		nodes.Append(n)
	}
	nodes.AllSortByIndex()
	statm := map[string]int{}
	max := nodes.Len()
	for i := 0; i < 100; i++ {
		n := nodes[pseudorand.Intn(max)]
		statm[n.InsID]++
	}

	expected := 100 / max
	expectdiff := float64(10)
	for _, v := range statm {
		assert.LessOrEqual(t, math.Abs(float64((int64(v - expected)))), expectdiff)
	}

	statmm := map[string]int{}
	for i := 0; i < 100; i++ {
		rindex, _ := rand.Int(rand.Reader, big.NewInt(int64(max)))
		n := nodes[int(rindex.Int64())]
		statmm[n.InsID]++
	}
	for _, v := range statmm {
		assert.LessOrEqual(t, math.Abs(float64((int64(v - expected)))), expectdiff)
	}

}

func TestNodeScoreListSorted(t *testing.T) {
	nodes := NodeScoreList{}
	testNum := 100
	for i := 1; i <= testNum; i++ {
		n := &NodeScore{
			InsID: fmt.Sprintf("%d", i),
			Score: pseudorand.Float64(),
		}
		nodes.Append(n)
	}
	if testNum != nodes.Len() {
		t.Fatalf("testNum != nodes.Len(), testNum: %d, nodes.Len(): %d", testNum, nodes.Len())
	}
	l := nodes.AllSortByScore()
	for i := 1; i < testNum; i++ {
		assert.GreaterOrEqual(t, l[i-1].Score, l[i].Score)
	}
	oldNum := nodes.Len()
	nodes.Remove(&NodeScore{InsID: "1"}, &NodeScore{InsID: "2"}, &NodeScore{InsID: "3"})
	if oldNum-3 != nodes.Len() {
		t.Fatalf(
			"oldNum-3 != nodes.Len(), oldNum: %d, nodes.Len(): %d",
			oldNum-3,
			nodes.Len())
	}
	oldNum = nodes.Len()
	nodes.Remove(&NodeScore{InsID: "1"}, &NodeScore{InsID: "99"}, &NodeScore{InsID: "4"})
	if oldNum-2 != nodes.Len() {
		t.Fatalf(
			"oldNum-2 != nodes.Len(), oldNum: %d, nodes.Len(): %d",
			oldNum-2,
			nodes.Len())
	}

	oldNum = nodes.Len()
	nodes.Remove(&NodeScore{InsID: "1"}, &NodeScore{InsID: "99"}, &NodeScore{InsID: "4"})
	if oldNum != nodes.Len() {
		t.Fatalf(
			"oldNum != nodes.Len(), oldNum: %d, nodes.Len(): %d",
			oldNum,
			nodes.Len())
	}
}

func TestMemSizeLabel(t *testing.T) {
	n := Node{
		MemMBTotal: 1025,
	}
	afMemSize := resource.MustParse(fmt.Sprintf("%dMi", n.MemMBTotal))
	gotMem := resource.MustParse("1Gi")
	assert.True(t, afMemSize.Cmp(gotMem) > 0)
}

func TestQuotaCpu(t *testing.T) {
	n := Node{
		QuotaCpu: 90 * 1000,
	}
	afCpuSize := resource.MustParse(fmt.Sprintf("%dm", n.QuotaCpu))
	gotCpu := resource.MustParse("90")
	assert.Equal(t, gotCpu.Value(), afCpuSize.Value())
}

func TestNodeLabelsCachesMergedLabels(t *testing.T) {
	n := &Node{
		Zone:         "zone-a",
		ClusterLabel: "cluster-a",
		CPUType:      "x86",
		QuotaMem:     4096,
		QuotaCpu:     2000,
		InstanceType: "SA2",
		NodeLabels: map[string]string{
			"gpu":                         "true",
			constants.AffinityKeyZone:     "reported-zone",
			constants.AffinityKeyCPUCores: "reported-cpu",
		},
	}

	labels := n.Labels()
	assert.Equal(t, "true", labels["gpu"])
	assert.Equal(t, "zone-a", labels[constants.AffinityKeyZone])
	assert.Equal(t, "2000m", labels[constants.AffinityKeyCPUCores])

	labelsPtr := fmt.Sprintf("%p", labels)
	assert.Equal(t, labelsPtr, fmt.Sprintf("%p", n.Labels()))
	allocs := testing.AllocsPerRun(100, func() {
		_ = n.Labels()
	})
	assert.Zero(t, allocs)
}

func TestNodeLabelsCacheInvalidation(t *testing.T) {
	n := &Node{
		Zone:       "zone-a",
		QuotaMem:   4096,
		QuotaCpu:   2000,
		NodeLabels: map[string]string{"gpu": "true"},
	}

	oldLabels := n.Labels()
	n.Zone = "zone-b"
	n.QuotaMem = 8192
	n.NodeLabels = map[string]string{"ssd": "true"}
	n.InvalidateLabelsCache()

	newLabels := n.Labels()
	assert.NotEqual(t, fmt.Sprintf("%p", oldLabels), fmt.Sprintf("%p", newLabels))
	assert.Equal(t, "zone-b", newLabels[constants.AffinityKeyZone])
	assert.Equal(t, "8192Mi", newLabels[constants.AffinityKeyMemorySize])
	assert.NotContains(t, newLabels, "gpu")
	assert.Equal(t, "true", newLabels["ssd"])
}

func TestNodeCloneDoesNotShareLabelsCache(t *testing.T) {
	n := &Node{
		Zone:       "zone-a",
		QuotaMem:   4096,
		QuotaCpu:   2000,
		NodeLabels: map[string]string{"gpu": "true"},
	}
	_ = n.Labels()

	cloned := n.Clone()
	cloned.Zone = "zone-b"
	cloned.NodeLabels["gpu"] = "false"

	sourceLabels := n.Labels()
	clonedLabels := cloned.Labels()
	assert.Equal(t, "zone-a", sourceLabels[constants.AffinityKeyZone])
	assert.Equal(t, "true", sourceLabels["gpu"])
	assert.Equal(t, "zone-b", clonedLabels[constants.AffinityKeyZone])
	assert.Equal(t, "false", clonedLabels["gpu"])
}
