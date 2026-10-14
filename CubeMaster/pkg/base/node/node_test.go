// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package node

import (
	"encoding/json"
	"fmt"
	pseudorand "math/rand"
	"reflect"
	"strings"
	"testing"
	"time"

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

func TestNodeCloneDeepCopiesHostFacts(t *testing.T) {
	n := &Node{
		InsID: "node-1",
		HostFacts: &HostFacts{
			CPUVendor:             "GenuineIntel",
			CPUIDHash:             "sha256:cpu",
			HostKernelFingerprint: "sha256:kernel",
			KVMAPIVersion:         12,
		},
	}

	cloned := n.Clone()
	if cloned.HostFacts == n.HostFacts {
		t.Fatalf("clone must not share the HostFacts pointer")
	}
	cloned.HostFacts.CPUVendor = "AuthenticAMD"
	cloned.HostFacts.KVMAPIVersion = 13
	if n.HostFacts.CPUVendor != "GenuineIntel" || n.HostFacts.KVMAPIVersion != 12 {
		t.Errorf("mutating clone must not affect source: %+v", n.HostFacts)
	}
}

func TestNodeCloneNilHostFacts(t *testing.T) {
	n := &Node{InsID: "node-1"}
	cloned := n.Clone()
	if cloned.HostFacts != nil {
		t.Errorf("nil HostFacts must stay nil after clone, got %+v", cloned.HostFacts)
	}
}

func TestNodeCloneDeepCopiesLocalTemplates(t *testing.T) {
	source := &Node{InsID: "node-1", LocalTemplates: []string{"tpl-1"}}
	cloned := source.Clone()
	cloned.LocalTemplates[0] = "tpl-2"
	if source.LocalTemplates[0] != "tpl-1" {
		t.Fatalf("mutating cloned templates changed source: %v", source.LocalTemplates)
	}
}

func TestNodeHostFactsJSONRoundTrip(t *testing.T) {
	n := &Node{
		InsID: "node-1",
		HostFacts: &HostFacts{
			CPUVendor:            "GenuineIntel",
			CPUIDHash:            "sha256:cpu",
			KVMAPIVersion:        12,
			KVMModuleTaint:       "EO",
			KVMModuleFingerprint: "sha256:kvmmod",
		},
	}
	raw, err := json.Marshal(n)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Node
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.HostFacts == nil {
		t.Fatalf("HostFacts lost in round-trip: %s", raw)
	}
	if *out.HostFacts != *n.HostFacts {
		t.Errorf("round-trip mismatch:\n in=%+v\nout=%+v", n.HostFacts, out.HostFacts)
	}
}

func TestNodeHostFactsJSONOmittedWhenNil(t *testing.T) {
	raw, err := json.Marshal(&Node{InsID: "node-1"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "HostFacts") {
		t.Errorf("nil HostFacts must be omitted, got %s", raw)
	}
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

// TestNodeCloneCoversAllFields guards Clone's explicit field list: every
// exported field is filled with a non-zero value, the node is cloned, and the
// two are compared field by field. Adding a field to Node without copying it
// in Clone fails this test; the non-zero fill matters because a forgotten
// field would be the zero value on both sides and pass silently.
func TestNodeCloneCoversAllFields(t *testing.T) {
	source := &Node{}
	sourceValue := reflect.ValueOf(source).Elem()
	sourceType := sourceValue.Type()
	for i := 0; i < sourceType.NumField(); i++ {
		if sourceType.Field(i).IsExported() {
			fillNonZeroField(sourceValue.Field(i))
		}
	}
	source.SetSchedulingDisabled(true)

	cloned := source.Clone()
	clonedValue := reflect.ValueOf(cloned).Elem()
	for i := 0; i < sourceType.NumField(); i++ {
		field := sourceType.Field(i)
		if !field.IsExported() {
			// schedulingDisabled is copied through SetSchedulingDisabled;
			// labelsCache is intentionally reset.
			continue
		}
		if !reflect.DeepEqual(sourceValue.Field(i).Interface(), clonedValue.Field(i).Interface()) {
			t.Errorf("Clone dropped field %s: source=%v clone=%v",
				field.Name, sourceValue.Field(i).Interface(), clonedValue.Field(i).Interface())
		}
	}
	assert.Equal(t, source.SchedulingDisabled(), cloned.SchedulingDisabled())
}

func fillNonZeroField(v reflect.Value) {
	switch v.Kind() {
	case reflect.String:
		v.SetString("nonzero")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1.5)
	case reflect.Map:
		m := reflect.MakeMapWithSize(v.Type(), 1)
		key := reflect.New(v.Type().Key()).Elem()
		fillNonZeroField(key)
		value := reflect.New(v.Type().Elem()).Elem()
		fillNonZeroField(value)
		m.SetMapIndex(key, value)
		v.Set(m)
	case reflect.Slice:
		slice := reflect.New(v.Type()).Elem()
		elem := reflect.New(v.Type().Elem()).Elem()
		fillNonZeroField(elem)
		v.Set(reflect.Append(slice, elem))
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		fillNonZeroField(p.Elem())
		v.Set(p)
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			v.Set(reflect.ValueOf(time.Unix(1700000000, 0).UTC()))
			return
		}
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				fillNonZeroField(v.Field(i))
			}
		}
	}
}
