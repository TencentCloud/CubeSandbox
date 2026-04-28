// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"encoding/json"
	"github.com/rcrowley/go-metrics"
	"github.com/smallnest/weighted"
	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/inner"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"k8s.io/apimachinery/pkg/api/resource"
)

var (
	mocktest_ScheduleReq = `{
		"requestID":"3548c646-212e-45cc-8eeb-f9b6b7c84fe5",
		"timeout":30,
		"containers":[
			{
				"name":"runtime-sidecar",
				"image":{
					"image":"busybox:latest"
				},
				"command":[
					"/data/sidecar/runtime-sidecar"
				],
				"resources":{
					"cpu":"100m",
					"mem":"64Mi"
				}
			}
		],
		"annotations":{
			"com.cube.debug":"true",
			"com.invoke_port":"8080",
			"com.netid":"gw-axgkcimt"
		}
	}`
)

func newHostInfo(delta int64) *models.HostInfo {
	hostInfo := &models.HostInfo{

		InsID:               fmt.Sprintf("ins_%d_%d", delta, atomic.LoadInt32(&mocktest_hostID)),
		IP:                  fmt.Sprintf("192.168.0.%d", atomic.LoadInt32(&mocktest_hostID)),
		CpuTotal:            mocktest_hostCpuTotal * int(delta),
		MemMBTotal:          mocktest_hostMemTotal * delta,
		Zone:                "ap-chongqing-1",
		LiveStatus:          constants.HeartbeatHealth,
		HostStatus:          constants.HostStatusRunning,
		QuotaCpu:            mocktest_hostQuotaCpu * delta,
		QuotaMem:            mocktest_hostQuotaMem * delta,
		CreateConcurrentNum: config.GetConfig().CubeletConf.CreateConcurrentLimit * delta,
		MaxMvmNum:           config.GetConfig().Scheduler.NodeMaxMvmNum * delta,
		InstanceType:        "BMI5.24XLARGE384",
		OssClusterLabel:     "cubebox",
		ClusterLabel:        "cubebox",
	}
	hostInfo.ID = uint(atomic.LoadInt32(&mocktest_hostID))
	atomic.AddInt32(&mocktest_hostID, 1)
	return hostInfo
}

func getBaseURL(url string) string {
	return fmt.Sprintf("http://localhost:%d", config.GetConfig().Common.HttpPort) + url
}

func doReqWithCommonRes(t *testing.T, url, method string, reqV interface{}) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, url,
		bytes.NewBuffer([]byte(utils.InterfaceToString(reqV))))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	retNode := &types.Res{}
	assert.Nil(t, getBodyData(resp, retNode))
	assert.Equal(t, 200, retNode.Ret.RetCode)
}

func getBodyData(rsp *http.Response, object interface{}) error {
	if rsp.Body == nil {
		return fmt.Errorf("response body is nil")
	}
	defer rsp.Body.Close()
	data, err := io.ReadAll(rsp.Body)
	if err != nil {
		return err
	}
	err = json.Unmarshal(data, object)
	if err != nil {
		return err
	}
	return nil
}

func getBodyDataBuffer(rsp *http.Response, buff *bytes.Buffer) error {
	if rsp.Body == nil {
		return fmt.Errorf("response body is nil")
	}
	defer rsp.Body.Close()
	_, err := buff.ReadFrom(rsp.Body)
	if err != nil {
		return err
	}
	return nil
}

func mocktest_getoverhead(cnts []*cubebox.ContainerConfig) *selctx.RequestResource {
	res := &selctx.RequestResource{
		Cpu: resource.MustParse("0"),
		Mem: resource.MustParse("0"),
	}
	for _, ctr := range cnts {
		ctncpuQuantity, err := resource.ParseQuantity(ctr.GetResources().GetCpu())
		if err != nil {
			continue
		}
		ctnmemQuantity, err := resource.ParseQuantity(ctr.GetResources().GetMem())
		if err != nil {
			continue
		}
		res.Cpu.Add(ctncpuQuantity)
		res.Mem.Add(ctnmemQuantity)
	}
	return res
}

func add_quota_cpu_usage(insID string, i int64) {
	unlock := mocktest_metricLock.Lock("quota_cpu_usage")
	defer unlock()
	if _, ok := mocktest_quota_cpu_usageM[insID]; !ok {
		mocktest_quota_cpu_usageM[insID] = 0
	}
	mocktest_quota_cpu_usageM[insID] += i
}
func get_quota_cpu_usage(insID string) int64 {
	unlock := mocktest_metricLock.Lock("quota_cpu_usage")
	defer unlock()
	return mocktest_quota_cpu_usageM[insID]
}

func add_cpu_usage(insID string, i float64) {
	unlock := mocktest_metricLock.Lock("cpu_util")
	defer unlock()
	if _, ok := mocktest_cpu_usageM[insID]; !ok {
		mocktest_cpu_usageM[insID] = 0.0
	}
	factor := 0.1
	i = i * factor / float64(mocktest_hostCpuTotal)
	mocktest_cpu_usageM[insID] += i
}

func get_cpu_usage(insID string) float64 {
	unlock := mocktest_metricLock.Lock("cpu_util")
	defer unlock()
	return mocktest_cpu_usageM[insID]
}

func add_cpu_load_usage(insID string, i float64) {
	unlock := mocktest_metricLock.Lock("cpu_load_usage")
	defer unlock()
	if _, ok := mocktest_cpu_load_usageM[insID]; !ok {
		mocktest_cpu_load_usageM[insID] = 0.0
	}

	factor := 0.1
	i = i * factor
	mocktest_cpu_load_usageM[insID] += i
}
func get_cpu_load_usage(insID string) float64 {
	unlock := mocktest_metricLock.Lock("cpu_load_usage")
	defer unlock()
	return mocktest_cpu_load_usageM[insID]
}

func add_quota_mem_mb_usage(insID string, i int64) {
	unlock := mocktest_metricLock.Lock("quota_mem_mb_usage")
	defer unlock()
	if _, ok := mocktest_quota_mem_mb_usageM[insID]; !ok {
		mocktest_quota_mem_mb_usageM[insID] = 0
	}
	mocktest_quota_mem_mb_usageM[insID] += i
}
func get_quota_mem_mb_usage(insID string) int64 {
	unlock := mocktest_metricLock.Lock("quota_mem_mb_usage")
	defer unlock()
	return mocktest_quota_mem_mb_usageM[insID]
}

func add_mem_load_mb_usage(insID string, i int64) {
	unlock := mocktest_metricLock.Lock("mem_load_mb_usage")
	defer unlock()
	if _, ok := mocktest_mem_load_mb_usageM[insID]; !ok {
		mocktest_mem_load_mb_usageM[insID] = 0
	}

	factor := 0.1
	i = int64(math.Ceil(float64(i) * factor))
	mocktest_mem_load_mb_usageM[insID] += i
}

func get_mem_load_mb_usage(insID string) int64 {
	unlock := mocktest_metricLock.Lock("mem_load_mb_usage")
	defer unlock()
	return mocktest_mem_load_mb_usageM[insID]
}

func add_mvm_num(insID string, i int64) {
	unlock := mocktest_metricLock.Lock("mvm_num")
	defer unlock()
	if _, ok := mocktest_mvm_numM[insID]; !ok {
		mocktest_mvm_numM[insID] = 0
	}
	mocktest_mvm_numM[insID] += i
}

func get_mvm_num(insID string) int64 {
	unlock := mocktest_metricLock.Lock("mvm_num")
	defer unlock()
	return mocktest_mvm_numM[insID]
}

func add_realtime_create_num(insID string, i int64) {
	unlock := mocktest_metricLock.Lock("realtime_create_num")
	defer unlock()
	if _, ok := mocktest_realtime_create_numM[insID]; !ok {
		mocktest_realtime_create_numM[insID] = 0
	}
	mocktest_realtime_create_numM[insID] += i
}

func get_realtime_create_num(insID string) int64 {
	unlock := mocktest_metricLock.Lock("realtime_create_num")
	defer unlock()
	return mocktest_realtime_create_numM[insID]
}

func isAllMvmNumZero(product string) bool {
	unlock := mocktest_metricLock.Lock("mvm_num")
	defer unlock()
	for _, v := range mocktest_mvm_numM {
		if v != 0 {
			return false
		}
	}

	for _, n := range localcache.GetHealthyNodesByInstanceType(-1, product) {
		if n.MvmNum != int64(0) {
			return false
		}
	}
	return true
}

func testGetScheduleReq() *types.CreateCubeSandboxReq {
	reqC := &types.CreateCubeSandboxReq{}
	json.Unmarshal([]byte(mocktest_ScheduleReq), reqC)
	return reqC
}

type resourceFormat struct {
	Weight int
	Res    *types.Resource
}

type LoadBalancer struct {
	SW *weighted.SW
}

func NewLoadBalancer(servers []*resourceFormat) *LoadBalancer {
	sw := weighted.SW{}
	for _, s := range servers {
		sw.Add(s.Res, s.Weight)
	}
	return &LoadBalancer{
		SW: &sw,
	}
}

func (lb *LoadBalancer) GetCreateCubeSandboxReq() *types.CreateCubeSandboxReq {
	item := lb.SW.Next()
	res, ok := item.(*types.Resource)
	if ok {
		reqC := testGetScheduleReq()
		reqC.Containers[0].Resources = res
		return reqC
	}
	return nil
}

func printResultStat() {
	result := inner.GetStatPercents()
	for _, k := range []string{"pcpuUsage", "pcpuPercent", "pmemUsage", "pmemPercent", "pmvmNum", "pmvmPercent"} {
		fmt.Printf("%s: %s\n", k, result[k])
	}
}

func getStdDev() (float64, float64, float64) {
	cpuQuotaUsagePercent := []int64{}
	memQuotaUsagePercent := []int64{}
	mvmNumPercent := []int64{}
	for _, n := range localcache.GetHealthyNodes(-1) {
		cpuQuotaUsagePercent = append(cpuQuotaUsagePercent, n.QuotaCpuUsage*100/n.QuotaCpu)
		memQuotaUsagePercent = append(memQuotaUsagePercent, n.QuotaMemUsage*100/n.QuotaMem)
		mvmNumPercent = append(mvmNumPercent, n.MvmNum*100/n.MaxMvmLimit)
	}

	return metrics.SampleStdDev(cpuQuotaUsagePercent),
		metrics.SampleStdDev(memQuotaUsagePercent),
		metrics.SampleStdDev(mvmNumPercent)
}
