// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package integration

import (
	"context"
	"fmt"
	stdlog "log"
	"math"
	"math/rand"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey"
	"github.com/alicebob/miniredis/v2"
	"github.com/gomodule/redigo/redis"
	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	cubeleterrorcode "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/images/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/wrapredis"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"gorm.io/gorm"
	"k8s.io/apimachinery/pkg/api/resource"
)

var (
	httpClient = http.Client{
		Timeout: time.Second * 10,
	}
	mocktest_hostCpuTotal  int   = 96
	mocktest_hostMemTotal  int64 = 300 * 1024
	mocktest_hostQuotaCpu  int64 = int64(mocktest_hostCpuTotal * 1000 * 10)
	mocktest_hostQuotaMem  int64 = int64(math.Round(float64(mocktest_hostMemTotal) * 2.5))
	mocktest_hostID        int32 = 1
	mocktest_hostPort      int32 = 10000
	mocktest_hostInfoLock  sync.RWMutex
	mocktest_allnodeIds    = []string{}
	mocktest_allnodesMap   = make(map[string]*models.HostInfo)
	mocktest_allnodesSlice = []*models.HostInfo{}

	mocktest_ip2HostInfo sync.Map

	mocktest_totalnode = 10

	mocktest_metricLock           = utils.NewResourceLocks()
	mocktest_quota_cpu_usageM     = map[string]int64{}
	mocktest_quota_mem_mb_usageM  = map[string]int64{}
	mocktest_mem_load_mb_usageM   = map[string]int64{}
	mocktest_mvm_numM             = map[string]int64{}
	mocktest_realtime_create_numM = map[string]int64{}

	mocktest_cpu_load_usageM = map[string]float64{}
	mocktest_cpu_usageM      = map[string]float64{}

	mocktest_mapProxy sync.Map

	mocktest_bypassProxyMap = sync.Map{}
	mocktest_cubeInsInfoMap = sync.Map{}

	mocktest_cubeletLock sync.RWMutex

	mocktest_cubeletSandboxMap = make(map[string]map[string][]*cubebox.Container)

	mocktest_cubeimagelock sync.RWMutex

	mocktest_cubeimageMap = make(map[string]map[string]*images.ImageSpec)

	mocktest_errorCodeMap = sync.Map{}

	mocktest_errorMap = sync.Map{}

	mocktest_sleepMap = sync.Map{}

	mocktest_allInstancesSliceId  int64
	mocktest_allInstancesSlice    = []*models.InstanceInfo{}
	mocktest_allInstancesMap      = make(map[string]*models.InstanceInfo)
	mocktest_RedisSrv             *miniredis.Miniredis
	mocktest_OssDb                *gorm.DB
	mocktest_Ctx, mocktest_Cancel = context.WithCancel(context.Background())
)

type mockVirtualPrivateCloud struct {
	PrivateIPAddresses []string `json:"private_ip_addresses"`
	SpecifyIPAddresses []string `json:"specify_ip_addresses"`
}

func mock_sort_allnodes() {
	sort.SliceStable(mocktest_allnodesSlice, func(i, j int) bool {
		return mocktest_allnodesSlice[i].ID < mocktest_allnodesSlice[j].ID
	})
}

func MockInit() {
	mocktest_totalnode = config.GetConfig().Common.MockNodeNum
	mock_db()
	mock_Redis()

	mock_cubelet()
}

func mocktest_InitGlobalResources() {

	init_HostInfo()
	time.Sleep(config.GetConfig().Common.SyncMetaDataInterval * 2)
	stdlog.Printf("mocktest_InitGlobalResources done")
}

func mocktest_CleanupGlobalResources() {

	conn := wrapredis.GetRedis(wrapredis.RedisWrite)

	redisKeysBefore, _ := redis.Int(conn.Do("DBSIZE"))
	conn.Do("FLUSHDB")

	redisRecordsCleaned := redisKeysBefore
	stdlog.Printf("Redis database cleanup completed, cleaned %d keys", redisRecordsCleaned)

	if mocktest_OssDb != nil {

		totalRecordsCleaned := 0

		tables := []string{
			constants.InstanceUserDataTableName,
			constants.InstanceInfoTableName,
			constants.HostSubInfoTableName,
			constants.HostTypeTableName,
			constants.MetadataTableName,
		}

		for _, table := range tables {

			var countBefore int64
			mocktest_OssDb.Table(table).Count(&countBefore)

			result := mocktest_OssDb.Exec("DELETE FROM " + table)

			if result.Error == nil {
				rowsAffected := result.RowsAffected
				totalRecordsCleaned += int(rowsAffected)
				if rowsAffected > 0 {
					stdlog.Printf("Table %s cleanup completed, cleaned %d records", table, rowsAffected)
				}
			} else {
				stdlog.Printf("Table %s cleanup failed: %v", table, result.Error)
			}
		}
		stdlog.Printf("MySQL database cleanup completed,cleaned %d records", totalRecordsCleaned)
	} else {
		stdlog.Println("MySQL database connection is nil, skip cleanup")
	}
	time.Sleep(config.GetConfig().Common.SyncMetaDataInterval * 2)
}

func registerCleanup(t *testing.T) {
	t.Cleanup(func() {
		mocktest_CleanupGlobalResources()
	})
}

func mock_Redis() {
	mocktest_RedisSrv = miniredis.NewMiniRedis()

	mocktest_RedisSrv.StartAddr(":0")
	mocktest_RedisSrv.RequireAuth(config.GetConfig().RedisConf.Password)

	if config.GetConfig().RedisConf != nil {
		config.GetConfig().RedisConf.Nodes = mocktest_RedisSrv.Addr()
	}
	if config.GetConfig().RedisReadConf != nil {
		config.GetConfig().RedisReadConf.Nodes = mocktest_RedisSrv.Addr()
	}
	if config.GetConfig().RedisWriteConf != nil {
		config.GetConfig().RedisWriteConf.Nodes = mocktest_RedisSrv.Addr()
	}
	go func() {
		for {
			select {
			case <-mocktest_Ctx.Done():
				mocktest_RedisSrv.Close()
				return
			default:
				time.Sleep(config.GetConfig().Common.SyncMetricDataInterval)
				for _, v := range mocktest_allnodesSlice {
					mocktest_reportMetric(v.InsID)
				}
			}
		}
	}()
}

func mocktest_reportMetric(insID string) {
	redisNode := &localcache.RedisNodeInfo{
		InsID:             insID,
		QuotaCpuUsage:     get_quota_cpu_usage(insID),
		QuotaMemUsage:     get_quota_mem_mb_usage(insID),
		CpuLoadUsage:      get_cpu_load_usage(insID),
		CpuUtil:           get_cpu_usage(insID),
		MemUsage:          get_mem_load_mb_usage(insID),
		MvmNum:            get_mvm_num(insID),
		RealTimeCreateNum: get_realtime_create_num(insID),
		MetricUpdate:      string(metricNow()),
	}
	wrapredis.GetRedis(wrapredis.RedisWrite).Do("HSET", redis.Args{insID}.AddFlat(redisNode)...)
}

func mock_getHostInfoByIP(ip string) *models.HostInfo {
	info, ok := mocktest_ip2HostInfo.Load(ip)
	if ok {
		return info.(*models.HostInfo)
	}
	return &models.HostInfo{}
}

func mock_setCubletErrorCode(action string, errorCode errorcode.ErrorCode) {
	mocktest_errorCodeMap.Store(action, errorCode)
}

func mock_delCubletErrCode(action string) {
	mocktest_errorCodeMap.Delete(action)
}

func mock_setCubletError(action string, err error) {
	mocktest_errorMap.Store(action, err)
}

func mock_delCubletErr(action string) {
	mocktest_errorMap.Delete(action)
}

func mock_setCubletSleep(action string, t time.Duration) {
	mocktest_sleepMap.Store(action, t)
}

func mock_delCubletSleep(action string) {
	mocktest_sleepMap.Delete(action)
}

func mock_getContainers(req *cubebox.RunCubeSandboxRequest, sandboxID string, insID string) []*cubebox.Container {
	var cnts []*cubebox.Container
	for i, c := range req.GetContainers() {
		cntr := &cubebox.Container{
			Id:   uuid.New().String(),
			Type: "container",
			Resources: &cubebox.Resource{
				Cpu: c.GetResources().Cpu,
				Mem: c.GetResources().Mem,
			},
			CreatedAt: time.Now().UnixNano(),
			State:     cubebox.ContainerState_CONTAINER_RUNNING,
			Labels:    make(map[string]string),
		}
		if i == 0 {
			cntr.Id = sandboxID
			cntr.Type = "sandbox"
			if req.GetLabels() != nil {
				cntr.Labels = req.GetLabels()
			}
			cntr.Labels["io.kubernetes.cri.container-type"] = "sandbox"
		} else {
			if req.GetLabels() != nil {
				cntr.Labels = req.GetLabels()
			}
		}
		cnts = append(cnts, cntr)
	}
	return cnts
}

func mock_addResourceStat(insID string, cnts []*cubebox.ContainerConfig) {
	reqResWithOc := mocktest_getoverhead(cnts)
	add_quota_mem_mb_usage(insID, reqResWithOc.Mem.Value()/1024/1024)
	add_quota_cpu_usage(insID, reqResWithOc.Cpu.MilliValue())

	add_cpu_usage(insID, float64(reqResWithOc.Cpu.MilliValue()))
	add_mem_load_mb_usage(insID, reqResWithOc.Mem.Value()/1024/1024)
	add_mvm_num(insID, 1)
}

func mock_cubeletCreate() {
	gomonkey.ApplyFunc(cubelet.Create, func(ctx context.Context, calleeEp string,
		req *cubebox.RunCubeSandboxRequest) (*cubebox.RunCubeSandboxResponse, error) {
		rsp := &cubebox.RunCubeSandboxResponse{
			RequestID: req.RequestID,
			Ret: &cubeleterrorcode.Ret{
				RetCode: cubeleterrorcode.ErrorCode_Success,
			},
		}
		if value, ok := mocktest_errorCodeMap.Load("Create"); ok {
			rsp.Ret.RetCode = cubeleterrorcode.ErrorCode(value.(errorcode.ErrorCode))
			rsp.Ret.RetMsg = rsp.Ret.RetCode.String()
			return rsp, nil
		}

		if value, ok := mocktest_errorMap.Load("Create"); ok {
			rsp.Ret.RetCode = cubeleterrorcode.ErrorCode_Unknown
			rsp.Ret.RetMsg = cubeleterrorcode.ErrorCode_Unknown.String()
			return rsp, value.(error)
		}

		hostInfo := mock_getHostInfoByIP(strings.Split(calleeEp, ":")[0])
		if hostInfo == nil {
			rsp.Ret.RetCode = cubeleterrorcode.ErrorCode_InvalidParamFormat
			rsp.Ret.RetMsg = "no such mock host" + calleeEp
			return rsp, nil
		}
		mock_addResourceStat(hostInfo.InsID, req.GetContainers())
		sandboxID := uuid.New().String()
		cnts := mock_getContainers(req, sandboxID, hostInfo.InsID)
		if value, ok := mocktest_sleepMap.Load("Create"); ok {
			sleep := value.(time.Duration)
			time.Sleep(sleep)
		}
		time.Sleep(config.GetConfig().Common.MockCreateSleep)
		mocktest_cubeletLock.Lock()
		m, ok := mocktest_cubeletSandboxMap[calleeEp]
		if ok {
			m[sandboxID] = cnts
		} else {
			mocktest_cubeletSandboxMap[calleeEp] = map[string][]*cubebox.Container{sandboxID: cnts}
		}
		mocktest_cubeletLock.Unlock()

		rsp.RequestID = req.RequestID
		rsp.SandboxID = sandboxID
		rsp.SandboxIP = mock_getstr()
		if ipstr := req.GetAnnotations()[constants.CubeAnnotationsInsVirtualPrivateCloud]; ipstr != "" {
			vpc := &mockVirtualPrivateCloud{}
			utils.JSONTool.UnmarshalFromString(ipstr, vpc)
			if vpc.PrivateIPAddresses != nil {
				rsp.SandboxIP = vpc.PrivateIPAddresses[0]
			}
			if vpc.SpecifyIPAddresses != nil {
				rsp.SandboxIP = vpc.SpecifyIPAddresses[0]
			}
		}
		if len(req.GetExposedPorts()) != 0 {
			for _, v := range req.GetExposedPorts() {
				rsp.PortMappings = append(rsp.PortMappings, &cubebox.PortMapping{
					ContainerPort: int32(v),
					HostPort:      atomic.AddInt32(&mocktest_hostPort, 1),
				})
			}
		}
		return rsp, nil
	})
}

func mock_delResourceStat(insID string, cnts []*cubebox.ContainerConfig) {
	reqResWithOc := mocktest_getoverhead(cnts)
	add_quota_mem_mb_usage(insID, -reqResWithOc.Mem.Value()/1024/1024)
	add_quota_cpu_usage(insID, -reqResWithOc.Cpu.MilliValue())

	add_cpu_usage(insID, -float64(reqResWithOc.Cpu.MilliValue()))
	add_mem_load_mb_usage(insID, -reqResWithOc.Mem.Value()/1024/1024)
	add_mvm_num(insID, -1)
}

func mock_cubeletDestroy() {
	gomonkey.ApplyFunc(cubelet.Destroy, func(ctx context.Context, calleeEp string,
		req *cubebox.DestroyCubeSandboxRequest) (*cubebox.DestroyCubeSandboxResponse, error) {
		rsp := &cubebox.DestroyCubeSandboxResponse{
			RequestID: req.RequestID,
			Ret: &cubeleterrorcode.Ret{
				RetCode: cubeleterrorcode.ErrorCode_Success,
			},
		}
		if value, ok := mocktest_errorCodeMap.Load("Destroy"); ok {
			rsp.Ret.RetCode = cubeleterrorcode.ErrorCode(value.(errorcode.ErrorCode))
			rsp.Ret.RetMsg = rsp.Ret.RetCode.String()
			return rsp, nil
		}
		if value, ok := mocktest_errorMap.Load("Destroy"); ok {
			rsp.Ret.RetCode = cubeleterrorcode.ErrorCode_Unknown
			rsp.Ret.RetMsg = cubeleterrorcode.ErrorCode_Unknown.String()
			return rsp, value.(error)
		}

		if value, ok := mocktest_sleepMap.Load("Destroy"); ok {
			sleep := value.(time.Duration)
			time.Sleep(sleep)
		}

		mocktest_cubeletLock.Lock()
		defer mocktest_cubeletLock.Unlock()
		m, ok := mocktest_cubeletSandboxMap[calleeEp]
		if ok {
			hostInfo := mock_getHostInfoByIP(strings.Split(calleeEp, ":")[0])
			if hostInfo == nil {
				rsp.Ret.RetCode = cubeleterrorcode.ErrorCode_InvalidParamFormat
				rsp.Ret.RetMsg = "no such mock host" + calleeEp
				return rsp, nil
			}
			if sandboxes, ok := m[req.SandboxID]; ok {
				Cpu := resource.MustParse("0")
				Mem := resource.MustParse("0")
				for _, c := range sandboxes {
					Cpu.Add(resource.MustParse(c.GetResources().Cpu))
					Mem.Add(resource.MustParse(c.GetResources().Mem))
				}
				cnts := []*cubebox.ContainerConfig{
					{
						Resources: &cubebox.Resource{
							Cpu: Cpu.String(),
							Mem: Mem.String(),
						},
					},
				}
				mock_delResourceStat(hostInfo.InsID, cnts)
			}
			delete(m, req.SandboxID)
			if len(m) == 0 {
				delete(mocktest_cubeletSandboxMap, calleeEp)
			}
		}
		return rsp, nil
	})
}
func mock_cubeletList() {
	gomonkey.ApplyFunc(cubelet.List, func(ctx context.Context, calleeEp string,
		req *cubebox.ListCubeSandboxRequest) (*cubebox.ListCubeSandboxResponse, error) {
		rsp := &cubebox.ListCubeSandboxResponse{}
		mocktest_cubeletLock.RLock()
		defer mocktest_cubeletLock.RUnlock()
		m, ok := mocktest_cubeletSandboxMap[calleeEp]
		if ok {
			if req.GetId() != "" {

				if c, ok := m[req.GetId()]; ok {
					boxes := &cubebox.CubeSandbox{}
					for _, cntr := range c {
						if cntr.Type == "sandbox" {
							boxes.Id = cntr.Id
						}
						boxes.Containers = append(boxes.Containers, cntr)
					}
					rsp.Items = append(rsp.Items, boxes)
				}
				return rsp, nil
			}

			var sbs []*cubebox.CubeSandbox
			for _, c := range m {
				boxes := &cubebox.CubeSandbox{}
				for _, cntr := range c {
					if cntr.Type == "sandbox" {
						boxes.Id = cntr.Id
					}
					boxes.Containers = append(boxes.Containers, cntr)
				}
				sbs = append(sbs, boxes)
			}

			filter := req.GetFilter()
			if filter == nil {
				rsp.Items = sbs
				return rsp, nil
			}
			for _, sb := range sbs {
				var matchedContainer []*cubebox.Container
				for _, cntr := range sb.Containers {
					if filter.GetState() != nil && filter.GetState().GetState() != cntr.State {
						continue
					}
					if filter.GetLabelSelector() != nil {
						match := true
						for k, v := range filter.GetLabelSelector() {
							got, ok := cntr.Labels[k]
							if !ok || got != v {
								match = false
								break
							}
						}
						if !match {
							continue
						}
					}
					matchedContainer = append(matchedContainer, cntr)
				}

				if len(matchedContainer) == 0 {
					continue
				}

				sb.Containers = matchedContainer
				rsp.Items = append(rsp.Items, sb)
			}
		}

		return rsp, nil
	})
}
func mock_cubeletImage() {
	gomonkey.ApplyFunc(cubelet.CreateImage, func(ctx context.Context, calleeEp string,
		req *images.CreateImageRequest) (*images.CreateImageRequestResponse, error) {
		rsp := &images.CreateImageRequestResponse{
			RequestID: req.RequestID,
			Ret: &cubeleterrorcode.Ret{
				RetCode: cubeleterrorcode.ErrorCode_Success,
			},
		}
		if value, ok := mocktest_errorCodeMap.Load("CreateImage"); ok {
			rsp.Ret.RetCode = cubeleterrorcode.ErrorCode(value.(errorcode.ErrorCode))
			rsp.Ret.RetMsg = rsp.Ret.RetCode.String()
			return rsp, nil
		}
		if value, ok := mocktest_errorMap.Load("CreateImage"); ok {
			rsp.Ret.RetCode = cubeleterrorcode.ErrorCode_Unknown
			rsp.Ret.RetMsg = cubeleterrorcode.ErrorCode_Unknown.String()
			return rsp, value.(error)
		}

		mocktest_cubeimagelock.Lock()
		defer mocktest_cubeimagelock.Unlock()
		m, ok := mocktest_cubeimageMap[calleeEp]
		if ok {
			m[req.GetSpec().Image] = req.GetSpec()
		} else {
			m = make(map[string]*images.ImageSpec)
			m[req.GetSpec().Image] = req.GetSpec()
			mocktest_cubeimageMap[calleeEp] = m
		}

		return rsp, nil
	})
	gomonkey.ApplyFunc(cubelet.DeleteImage, func(ctx context.Context, calleeEp string,
		req *images.DestroyImageRequest) (*images.DestroyImageResponse, error) {
		rsp := &images.DestroyImageResponse{
			Ret: &cubeleterrorcode.Ret{
				RetCode: cubeleterrorcode.ErrorCode_Success,
			},
		}
		if value, ok := mocktest_errorCodeMap.Load("DeleteImage"); ok {
			rsp.Ret.RetCode = cubeleterrorcode.ErrorCode(value.(errorcode.ErrorCode))
			rsp.Ret.RetMsg = rsp.Ret.RetCode.String()
			return rsp, nil
		}

		if value, ok := mocktest_errorMap.Load("DeleteImage"); ok {
			rsp.Ret.RetCode = cubeleterrorcode.ErrorCode_Unknown
			rsp.Ret.RetMsg = cubeleterrorcode.ErrorCode_Unknown.String()
			return rsp, value.(error)
		}

		mocktest_cubeimagelock.Lock()
		defer mocktest_cubeimagelock.Unlock()
		m, ok := mocktest_cubeimageMap[calleeEp]
		if ok {
			delete(m, req.GetSpec().Image)
		}

		return rsp, nil
	})
}
func mock_cubelet() {
	mock_cubeletCreate()
	mock_cubeletDestroy()
	mock_cubeletList()
	mock_cubeletImage()
}

var (
	redismetrics = []string{"ins_id", "quota_cpu_usage", "quota_mem_mb_usage", "cpu_load_usage",
		"cpu_util", "mem_load_mb_usage", "mvm_num", "update_at", "realtime_create_num"}
)

func mocktest_delHost(insId string) {
	mocktest_hostInfoLock.Lock()
	defer mocktest_hostInfoLock.Unlock()
	mocktest_OssDb.Delete(&models.HostInfo{}, "ins_id = ?", insId)
	delete(mocktest_allnodesMap, insId)

	for i, v := range mocktest_allnodeIds {
		if v == insId {
			mocktest_allnodeIds = append(mocktest_allnodeIds[:i], mocktest_allnodeIds[i+1:]...)
		}
	}
	for i, v := range mocktest_allnodesSlice {
		if v.InsID == insId {
			if i == len(mocktest_allnodesSlice) {
				mocktest_allnodesSlice = mocktest_allnodesSlice[:i]
			} else {
				mocktest_allnodesSlice = append(mocktest_allnodesSlice[:i], mocktest_allnodesSlice[i+1:]...)
			}
			mocktest_ip2HostInfo.Delete(v.IP)
		}
	}

	mock_sort_allnodes()
}

func mocktest_AddHost(hostInfo *models.HostInfo) {
	mocktest_hostInfoLock.Lock()
	defer mocktest_hostInfoLock.Unlock()

	mocktest_OssDb.Create(hostInfo)

	mocktest_allnodesMap[hostInfo.InsID] = hostInfo
	mocktest_ip2HostInfo.Store(hostInfo.IP, hostInfo)
	mocktest_allnodesSlice = append(mocktest_allnodesSlice, hostInfo)
	mocktest_allnodeIds = append(mocktest_allnodeIds, hostInfo.InsID)

	mock_sort_allnodes()
}

func mocktest_updateHost(hostInfo *models.HostInfo) {
	mocktest_hostInfoLock.Lock()
	defer mocktest_hostInfoLock.Unlock()
	h, ok := mocktest_allnodesMap[hostInfo.InsID]
	if ok {
		h.CpuTotal = hostInfo.CpuTotal
		h.MemMBTotal = hostInfo.MemMBTotal
		h.QuotaCpu = hostInfo.QuotaCpu
		h.QuotaMem = hostInfo.QuotaMem
		h.LiveStatus = hostInfo.LiveStatus
		h.HostStatus = hostInfo.HostStatus
		h.MaxMvmNum = hostInfo.MaxMvmNum
		h.CreateConcurrentNum = hostInfo.CreateConcurrentNum
		mocktest_OssDb.Save(h)
	}
}

func init_HostInfo() {
	for i := 1; i <= mocktest_totalnode; i++ {
		h := newHostInfo(1)

		h.ClusterLabel = "cubebox"
		h.OssClusterLabel = "cubebox"

		if i%4 == 0 {
			h.Zone = "ap-chongqing-2"
		} else {
			h.Zone = "ap-chongqing-1"
		}
		mocktest_AddHost(h)
	}
}

func metricNow() []byte {
	now, _ := time.Now().MarshalText()
	return now
}

func mock_db() {
	mocktest_OssDb = db.Init(config.GetDbConfig())
	// Schema (including t_cube_host_type) is owned by the dao.Migrate
	// path that the integration test bootstrap runs before tests.
}
func mock_getstr() string {
	return fmt.Sprintf("%d.%d.%d.%d", rand.Int31n(254), rand.Int31n(254), rand.Int31n(254), rand.Int31n(254))
}

func wait_done() {
	for {
		time.Sleep(time.Second)
		rsp, err := httpClient.Get(getBaseURL("/notify/health"))
		if err == nil {
			if rsp.StatusCode == http.StatusOK {
				return
			}
		}
		stdlog.Println("wait_for run")
	}
}

func GetAllFormatList() map[string][]*resourceFormat {
	return allFormatList
}

var (
	allFormatList = map[string][]*resourceFormat{
		"all": {
			{
				Weight: 7,
				Res:    &types.Resource{Cpu: "100m", Mem: "64Mi"},
			},
			{
				Weight: 23,
				Res:    &types.Resource{Cpu: "100m", Mem: "128Mi"},
			},
			{
				Weight: 30,
				Res:    &types.Resource{Cpu: "200m", Mem: "256Mi"},
			},
			{
				Weight: 1,
				Res:    &types.Resource{Cpu: "300m", Mem: "384Mi"},
			},
			{
				Weight: 8,
				Res:    &types.Resource{Cpu: "400m", Mem: "512Mi"},
			},
			{
				Weight: 1,
				Res:    &types.Resource{Cpu: "600m", Mem: "768Mi"},
			},
			{
				Weight: 1,
				Res:    &types.Resource{Cpu: "700m", Mem: "896Mi"},
			},
			{
				Weight: 7,
				Res:    &types.Resource{Cpu: "800m", Mem: "1024Mi"},
			},
			{
				Weight: 3,
				Res:    &types.Resource{Cpu: "1100m", Mem: "1408Mi"},
			},
			{
				Weight: 6,
				Res:    &types.Resource{Cpu: "1600m", Mem: "2048Mi"},
			},
			{
				Weight: 6,
				Res:    &types.Resource{Cpu: "2000m", Mem: "3072Mi"},
			},
			{
				Weight: 1,
				Res:    &types.Resource{Cpu: "4000m", Mem: "6144Mi"},
			},
		},
		"bj": {
			{
				Weight: 24,
				Res:    &types.Resource{Cpu: "100m", Mem: "64Mi"},
			},
			{
				Weight: 38,
				Res:    &types.Resource{Cpu: "100m", Mem: "128Mi"},
			},
			{
				Weight: 20,
				Res:    &types.Resource{Cpu: "200m", Mem: "256Mi"},
			},
			{
				Weight: 1,
				Res:    &types.Resource{Cpu: "300m", Mem: "384Mi"},
			},
			{
				Weight: 3,
				Res:    &types.Resource{Cpu: "400m", Mem: "512Mi"},
			},
			{
				Weight: 0,
				Res:    &types.Resource{Cpu: "600m", Mem: "768Mi"},
			},
			{
				Weight: 0,
				Res:    &types.Resource{Cpu: "700m", Mem: "896Mi"},
			},
			{
				Weight: 3,
				Res:    &types.Resource{Cpu: "800m", Mem: "1024Mi"},
			},
			{
				Weight: 0,
				Res:    &types.Resource{Cpu: "1100m", Mem: "1408Mi"},
			},
			{
				Weight: 7,
				Res:    &types.Resource{Cpu: "1600m", Mem: "2048Mi"},
			},
			{
				Weight: 4,
				Res:    &types.Resource{Cpu: "2000m", Mem: "3072Mi"},
			},
			{
				Weight: 0,
				Res:    &types.Resource{Cpu: "4000m", Mem: "6144Mi"},
			},
		},
		"sh": {
			{
				Weight: 2,
				Res:    &types.Resource{Cpu: "100m", Mem: "64Mi"},
			},
			{
				Weight: 36,
				Res:    &types.Resource{Cpu: "100m", Mem: "128Mi"},
			},
			{
				Weight: 4,
				Res:    &types.Resource{Cpu: "200m", Mem: "256Mi"},
			},
			{
				Weight: 3,
				Res:    &types.Resource{Cpu: "300m", Mem: "384Mi"},
			},
			{
				Weight: 7,
				Res:    &types.Resource{Cpu: "400m", Mem: "512Mi"},
			},
			{
				Weight: 4,
				Res:    &types.Resource{Cpu: "500m", Mem: "640Mi"},
			},
			{
				Weight: 7,
				Res:    &types.Resource{Cpu: "600m", Mem: "768Mi"},
			},
			{
				Weight: 7,
				Res:    &types.Resource{Cpu: "700m", Mem: "896Mi"},
			},
			{
				Weight: 11,
				Res:    &types.Resource{Cpu: "800m", Mem: "1024Mi"},
			},
			{
				Weight: 2,
				Res:    &types.Resource{Cpu: "1100m", Mem: "1408Mi"},
			},
			{
				Weight: 5,
				Res:    &types.Resource{Cpu: "1600m", Mem: "2048Mi"},
			},
			{
				Weight: 5,
				Res:    &types.Resource{Cpu: "2000m", Mem: "3072Mi"},
			},
			{
				Weight: 4,
				Res:    &types.Resource{Cpu: "4000m", Mem: "6144Mi"},
			},
			{
				Weight: 1,
				Res:    &types.Resource{Cpu: "4000m", Mem: "14336Mi"},
			},
		},
		"gz": {
			{
				Weight: 11,
				Res:    &types.Resource{Cpu: "100m", Mem: "64Mi"},
			},
			{
				Weight: 49,
				Res:    &types.Resource{Cpu: "100m", Mem: "128Mi"},
			},
			{
				Weight: 9,
				Res:    &types.Resource{Cpu: "200m", Mem: "256Mi"},
			},
			{
				Weight: 0,
				Res:    &types.Resource{Cpu: "300m", Mem: "384Mi"},
			},
			{
				Weight: 8,
				Res:    &types.Resource{Cpu: "400m", Mem: "512Mi"},
			},
			{
				Weight: 0,
				Res:    &types.Resource{Cpu: "600m", Mem: "768Mi"},
			},
			{
				Weight: 4,
				Res:    &types.Resource{Cpu: "700m", Mem: "896Mi"},
			},
			{
				Weight: 4,
				Res:    &types.Resource{Cpu: "800m", Mem: "1024Mi"},
			},
			{
				Weight: 0,
				Res:    &types.Resource{Cpu: "1100m", Mem: "1408Mi"},
			},
			{
				Weight: 2,
				Res:    &types.Resource{Cpu: "1600m", Mem: "2048Mi"},
			},
			{
				Weight: 9,
				Res:    &types.Resource{Cpu: "2000m", Mem: "3072Mi"},
			},
			{
				Weight: 2,
				Res:    &types.Resource{Cpu: "4000m", Mem: "6144Mi"},
			},
		},
	}
)
