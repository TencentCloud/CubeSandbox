// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package localcache

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/recov"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/rediskey"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/wrapredis"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

type RedisNodeInfo struct {
	InsID string `json:"InstanceID" redis:"ins_id"`

	QuotaCpuUsage int64 `json:"QuotaCpuUsage" redis:"quota_cpu_usage"`

	QuotaMemUsage int64 `json:"QuotaMemUsage" redis:"quota_mem_mb_usage"`

	CpuUtil float64 `json:"CpuUtil" redis:"cpu_util"`

	CpuLoadUsage float64 `json:"CpuLoadUsage" redis:"cpu_load_usage"`

	MemUsage int64 `json:"MemUsage" redis:"mem_load_mb_usage"`

	DataDiskUsagePer    float64 `json:"DataDiskUsagePer" redis:"data_disk_usage_per"`
	StorageDiskUsagePer float64 `json:"StorageDiskUsagePer" redis:"storage_disk_usage_per"`
	SysDiskUsagePer     float64 `json:"SysDiskUsagePer" redis:"sys_disk_usage_per"`

	MvmNum int64 `json:"mvm_num" redis:"mvm_num"`

	MetricUpdate string `json:"MetricUpdateAt" redis:"update_at"`

	RealTimeCreateNum int64 `json:"RealTimeCreateNum,omitempty" redis:"realtime_create_num"`

	NICQueues int64 `json:"nic_queues,omitempty" redis:"nic_queues"`
}

// NodeMetric is the in-API-process view of a cubelet's resource report.
// It is shared by the heartbeat handler and Redis fan-out so all paths
// agree on field semantics. All numeric fields use the same units as
// RedisNodeInfo (milli-cpu, MB, 0~100 percent) so cross-replica scheduling
// stays stable regardless of whether a master saw the cubelet directly or
// learned about it through Redis.
//
// HasAllocated and HasDisk distinguish "cubelet reported zero" from
// "cubelet did not report this group". Without these flags an empty
// section in a heartbeat would clobber the previous valid values in
// Redis, because HSET cannot tell apart "set to zero" from "leave
// untouched". The two flags map 1:1 to the Allocated / DiskUsage sub
// structures on UpdateNodeStatusRequest, so a partial heartbeat
// (allocated-only, disk-only, or anything in between) round-trips
// without corrupting the other group's prior state.
type NodeMetric struct {
	NodeID     string
	MetricTime time.Time

	HasAllocated  bool
	MilliCPUUsage int64
	MemoryMBUsage int64
	MvmNum        int64
	NicQueues     int64

	HasDisk             bool
	DataDiskUsagePer    float64
	StorageDiskUsagePer float64
	SysDiskUsagePer     float64
}

const (
	templateImageJobPullProgressExpireSeconds = int64(time.Hour / time.Second)
	templateImageJobPullProgressSetScript     = `
redis.call('HSET', KEYS[1], unpack(ARGV, 2))
redis.call('EXPIRE', KEYS[1], ARGV[1])
return 1
`
)

func templateImageJobPullProgressKey(jobID string) string {
	return "template_image_job_pull_progress" + ":" + jobID
}

func templateImageJobPullProgressSetArgs(key string, progress *types.TemplateImageJobPullProgressMap) redis.Args {
	return redis.Args{
		templateImageJobPullProgressSetScript,
		1,
		key,
		templateImageJobPullProgressExpireSeconds,
	}.AddFlat(progress)
}

// WriteNodeMetric persists a cubelet-reported metric snapshot to Redis so
// all cubemaster replicas converge on the same view through their existing
// loopUpdateMetric tick. We deliberately overwrite update_at with the
// server-side timestamp passed in (which the heartbeat handler clamps to
// time.Now()) so clock skew on individual cubelets cannot push entries
// past or stall the MetricUpdateTimeout filter.
func WriteNodeMetric(ctx context.Context, m *NodeMetric) error {
	if m == nil || m.NodeID == "" {
		return errors.New("WriteNodeMetric: node id required")
	}
	if !m.HasAllocated && !m.HasDisk {
		// Nothing to do: cubelet reported neither group, and writing
		// just an update_at would falsely refresh MetricUpdate while
		// the underlying values are stale.
		return nil
	}
	updateAt, err := m.MetricTime.MarshalText()
	if err != nil {
		return fmt.Errorf("marshal metric time: %w", err)
	}
	// Enumerate only the groups the cubelet actually reported. AddFlat
	// of RedisNodeInfo would emit zero values for every untagged field
	// and would also clobber RealTimeCreateNum / CpuUtil that this
	// heartbeat never measured, so we hand-build the HSET field list.
	fields := []interface{}{
		"ins_id", m.NodeID,
		"update_at", string(updateAt),
	}
	if m.HasAllocated {
		fields = append(fields,
			"quota_cpu_usage", m.MilliCPUUsage,
			"quota_mem_mb_usage", m.MemoryMBUsage,
			"mvm_num", m.MvmNum,
			"nic_queues", m.NicQueues,
		)
	}
	if m.HasDisk {
		fields = append(fields,
			"data_disk_usage_per", m.DataDiskUsagePer,
			"storage_disk_usage_per", m.StorageDiskUsagePer,
			"sys_disk_usage_per", m.SysDiskUsagePer,
		)
	}
	conn := wrapredis.GetRedis()
	ttl := nodeMetricTTLSec()
	key := rediskey.NodeMetric(m.NodeID)
	if _, err := conn.Do("HSET", redis.Args{key}.Add(fields...)...); err != nil {
		log.G(ctx).Errorf("WriteNodeMetric HSET %s failed: %v", key, err)
		return err
	}
	// Refresh a safety TTL on every heartbeat so offline nodes expire
	// instead of lingering forever; live nodes keep refreshing it.
	if ttl > 0 {
		if _, err := conn.Do("EXPIRE", key, ttl); err != nil {
			log.G(ctx).Errorf("WriteNodeMetric EXPIRE %s failed: %v", key, err)
		}
	}
	if log.IsDebug() {
		log.G(ctx).Debugf("WriteNodeMetric: %+v", m)
	}
	return nil
}

// nodeMetricTTLSec returns the configured node-metric safety TTL in seconds.
func nodeMetricTTLSec() int {
	if c := config.GetConfig().RedisConf; c != nil {
		return c.NodeMetricTTLSec
	}
	return 0
}

// sandboxProxyTTLSec returns the configured sandbox-proxy safety TTL in seconds.
func sandboxProxyTTLSec() int {
	if c := config.GetConfig().RedisConf; c != nil {
		return c.SandboxProxyTTLSec
	}
	return 0
}

// getNodeMetricWithFallback reads a node metric following the migration read
// order (new key first, legacy bare-id fallback during the dual phase).
func (l *local) getNodeMetricWithFallback(ctx context.Context, nodeID string) (*node.Node, bool, error) {
	var lastErr error
	for _, key := range rediskey.ReadKeysWithFallback(rediskey.NodeMetric(nodeID), rediskey.LegacyNodeMetric(nodeID)) {
		n, found, err := l.getNodeMetricFromRedis(ctx, key)
		if err != nil {
			lastErr = err
			continue
		}
		if found {
			return n, true, nil
		}
	}
	return nil, false, lastErr
}

// UpdateNodeMetricInProcess pushes a metric directly into the receiving
// cubemaster's local cache so scheduling on this replica does not have to
// wait for the next Redis tick. Only the groups marked present on the
// NodeMetric (HasAllocated / HasDisk) are written; other fields keep
// whatever value the previous Redis tick or heartbeat installed, which
// preserves the partial-update semantics the Redis writer relies on.
//
// Returns an error if the node is unknown to this replica (which is
// normal during cold start and will self-heal when reload syncs the
// registration table).
func UpdateNodeMetricInProcess(m *NodeMetric) error {
	if m == nil {
		return errors.New("UpdateNodeMetricInProcess: nil metric")
	}
	if m.NodeID == "" {
		return errors.New("UpdateNodeMetricInProcess: node id required")
	}
	if !m.HasAllocated && !m.HasDisk {
		return nil
	}
	v, exist := l.cache.Get(m.NodeID)
	if !exist {
		return fmt.Errorf("item %s doesn't exist", m.NodeID)
	}
	old, ok := v.(*node.Node)
	if !ok || old == nil {
		return fmt.Errorf("cache entry for %s is not a *node.Node", m.NodeID)
	}
	l.lockMetaData.Lock()
	defer l.lockMetaData.Unlock()
	old.MetricUpdate = m.MetricTime
	old.MetricLocalUpdateAt = time.Now().Local()
	if m.HasAllocated {
		old.QuotaCpuUsage = m.MilliCPUUsage
		old.QuotaMemUsage = m.MemoryMBUsage
		old.MvmNum = m.MvmNum
		old.NicQueues = m.NicQueues
	}
	if m.HasDisk {
		old.DataDiskUsagePer = m.DataDiskUsagePer
		old.StorageDiskUsagePer = m.StorageDiskUsagePer
		old.SysDiskUsagePer = m.SysDiskUsagePer
	}
	return nil
}

func (l *local) loadMetricFromRedis() error {
	elems := l.cache.Items()
	for k := range elems {
		if tmpNode, found, err := l.getNodeMetricWithFallback(context.Background(), k); found && err == nil {
			if err := l.updateNodeMetric(tmpNode); err != nil {

				CubeLog.WithContext(context.Background()).Warnf("updateMetric fail:%v", err)
			}
		} else if err != nil {
			return err
		}
	}
	return nil
}

func (l *local) loopUpdateMetric(ctx context.Context) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	checkDeadline := time.Now().Add(config.GetConfig().Common.SyncMetricDataInterval)
	for {
		select {
		case <-ticker.C:
			recov.WithRecover(func() {
				if checkDeadline.After(time.Now()) {

					return
				}
				defer func() {
					checkDeadline = time.Now().Add(config.GetConfig().Common.SyncMetricDataInterval)
				}()
				ctx = context.WithValue(ctx, CubeLog.KeyRequestID, uuid.New().String())
				elems := l.cache.Items()
				for k := range elems {
					if tmpNode, found, err := l.getNodeMetricWithFallback(ctx, k); found && err == nil {
						if err := l.updateNodeMetric(tmpNode); err != nil {

							CubeLog.WithContext(context.Background()).Fatalf("updateMetric fail:%v", err)
						}
					}
				}
			}, func(panicError interface{}) {
				checkDeadline = time.Now().Add(config.GetConfig().Common.SyncMetricDataInterval)
				CubeLog.WithContext(context.Background()).Fatalf("loopUpdateMetric panic:%v", panicError)
			})
		case <-ctx.Done():
			return
		}
	}
}

func (l *local) getNodeMetricFromRedis(ctx context.Context, key string) (*node.Node, bool, error) {
	values, err := redis.Values(wrapredis.GetRedis().Do("HGETALL", key))
	if err != nil {
		CubeLog.WithContext(ctx).Fatalf("getNodeMetricFromRedis %s err:%s", key, err)
		return nil, false, err
	}
	if len(values) == 0 {
		CubeLog.WithContext(ctx).Warnf("redis hgetall empty, key: %s", key)
		return nil, false, nil
	}

	redisNode := &RedisNodeInfo{}
	if err := redis.ScanStruct(values, redisNode); err != nil {
		CubeLog.WithContext(ctx).Errorf("redis scanStruct error, key: %s, err: %s,values:%v", key, err, values)
		return nil, true, err
	}
	n := &node.Node{}
	n.InsID = redisNode.InsID
	n.QuotaCpuUsage = redisNode.QuotaCpuUsage
	n.QuotaMemUsage = redisNode.QuotaMemUsage
	n.CpuUtil = redisNode.CpuUtil
	n.CpuLoadUsage = redisNode.CpuLoadUsage
	n.MemUsage = redisNode.MemUsage
	n.DataDiskUsagePer = redisNode.DataDiskUsagePer
	n.StorageDiskUsagePer = redisNode.StorageDiskUsagePer
	n.SysDiskUsagePer = redisNode.SysDiskUsagePer
	n.MvmNum = redisNode.MvmNum
	n.MetricUpdate.UnmarshalText([]byte(redisNode.MetricUpdate))
	n.RealTimeCreateNum = redisNode.RealTimeCreateNum
	n.NicQueues = redisNode.NICQueues
	if log.IsDebug() {
		CubeLog.WithContext(ctx).Debugf("getNodeMetricFromRedis:%+v", utils.InterfaceToString(redisNode))
	}
	return n, true, nil
}

func (l *local) getByPassProsyFromRedis(ctx context.Context, key string) (*types.SandboxProxyMap, error) {
	mapvalues, err := redis.StringMap(wrapredis.GetRedis().Do("HGETALL", key))
	if err != nil {
		if errors.Is(err, redis.ErrNil) {
			log.G(ctx).Debugf("no such key in redis:%s", key)
			return nil, nil
		}
		log.G(ctx).Warnf("getByPassProsyFromRedis %s err:%s", key, err)
		return nil, err
	}
	log.G(ctx).Debugf("getByPassProsyFromRedis:%s", key)
	if len(mapvalues) == 0 {
		log.G(ctx).Warnf("redis get empty, key: %s", key)
		return nil, nil
	}

	nodeIdIp := &types.SandboxProxyMap{}
	if ip, ok := mapvalues["HostIP"]; ok {
		nodeIdIp.HostIP = ip
		delete(mapvalues, "HostIP")
	} else {
		log.G(ctx).Warnf("redis get empty, key: %s", key)
		return nil, errors.New("get empty HostIP")
	}
	if createAt, ok := mapvalues["CreatedAt"]; ok {
		nodeIdIp.CreatedAt = createAt
		delete(mapvalues, "CreatedAt")
	}
	if sandboxIP, ok := mapvalues["SandboxIP"]; ok {
		nodeIdIp.SandboxIP = sandboxIP
		delete(mapvalues, "SandboxIP")
	}
	if v, ok := mapvalues["AllowPublicTraffic"]; ok {
		// Missing field on legacy entries is treated as the historical
		// default (true / publicly reachable).
		nodeIdIp.AllowPublicTraffic, _ = strconv.ParseBool(v)
		delete(mapvalues, "AllowPublicTraffic")
	} else {
		nodeIdIp.AllowPublicTraffic = true
	}
	if v, ok := mapvalues["TrafficAccessToken"]; ok {
		nodeIdIp.TrafficAccessToken = v
		delete(mapvalues, "TrafficAccessToken")
	}

	if len(mapvalues) == 0 {
		log.G(ctx).Warnf("key: %s,has no ContainerToHostPorts", key)
		return nodeIdIp, nil
	}
	nodeIdIp.ContainerToHostPorts = mapvalues
	if log.IsDebug() {
		log.G(ctx).Debugf("getByPassProsyFromRedis:%+v", nodeIdIp)
	}
	return nodeIdIp, nil
}

func (l *local) getInsInfoFromRedis(ctx context.Context, key string) (*types.InstanceInfoMap, error) {
	values, err := redis.Values(wrapredis.GetRedis().Do("HGETALL", key))
	if err != nil {
		if errors.Is(err, redis.ErrNil) {
			log.G(ctx).Debugf("no such key in redis:%s", key)
			return nil, nil
		}
		CubeLog.WithContext(ctx).Fatalf("getInsInfoFromRedis %s err:%s", key, err)
		return nil, err
	}
	if len(values) == 0 {
		CubeLog.WithContext(ctx).Warnf("redis hgetall empty, key: %s", key)
		return nil, nil
	}

	redisIns := &types.InstanceInfoMap{}
	if err := redis.ScanStruct(values, redisIns); err != nil {
		CubeLog.WithContext(ctx).Errorf("redis scanStruct error, key: %s, err: %s,values:%v", key, err, values)
		return nil, err
	}
	return redisIns, nil
}

func (l *local) setInstanceInfoMapToRedis(ctx context.Context, key string, info *types.InstanceInfoMap) (err error) {
	start := time.Now()
	defer func() {
		traceRedis(ctx, "Create", "HSET", key, start, err)
	}()
	_, err = wrapredis.GetRedis().Do("HSET", redis.Args{key}.AddFlat(info)...)
	if err != nil {
		log.G(ctx).Errorf("redis set error, key: %s, err: %s", key, err)
		return err
	}
	if log.IsDebug() {
		log.G(ctx).Debugf("setInstanceInfoMapToRedis:%s:%s", key, utils.InterfaceToString(info))
	}
	return nil
}

func (l *local) setByPassProsyToRedis(ctx context.Context, key string, byPassProsy *types.SandboxProxyMap) (err error) {
	start := time.Now()
	defer func() {
		traceRedis(ctx, "Create", "HSET", key, start, err)
	}()

	fieldValues := []interface{}{
		"HostIP", byPassProsy.HostIP,
		"CreatedAt", byPassProsy.CreatedAt,
		// Always written so CubeProxy can distinguish "explicit public" from
		// a legacy entry written before this field existed (legacy → field
		// missing → treated as public).
		"AllowPublicTraffic", strconv.FormatBool(byPassProsy.AllowPublicTraffic),
	}
	if byPassProsy.SandboxIP != "" {
		fieldValues = append(fieldValues, "SandboxIP", byPassProsy.SandboxIP)
	}
	if byPassProsy.TrafficAccessToken != "" {
		fieldValues = append(fieldValues, "TrafficAccessToken", byPassProsy.TrafficAccessToken)
	}
	for k, v := range byPassProsy.ContainerToHostPorts {
		fieldValues = append(fieldValues, k, v)
	}
	conn := wrapredis.GetRedis()
	_, err = conn.Do("HSET", redis.Args{key}.AddFlat(fieldValues)...)
	if err != nil {
		log.G(ctx).Errorf("redis set error, key: %s, err: %s", key, err)
		return err
	}
	// Refresh a safety fallback TTL so a missed DEL on teardown cannot leave a
	// stale route forever; normal teardown still removes the key explicitly.
	if ttl := sandboxProxyTTLSec(); ttl > 0 {
		if _, e := conn.Do("EXPIRE", key, ttl); e != nil {
			log.G(ctx).Errorf("setByPassProsyToRedis EXPIRE %s failed: %v", key, e)
		}
	}
	if log.IsDebug() {
		log.G(ctx).Debugf("setByPassProsyToRedis:%s,%s", key, fieldValues)
	}
	return nil
}

func (l *local) setDescribeTaskToRedis(ctx context.Context, key string, taskInfo *types.DescribeTaskMap) (err error) {
	start := time.Now()
	conn := wrapredis.GetRedis()
	defer func() {
		traceRedis(ctx, "Create", "HSET", key, start, err)
	}()
	defer func() {
		if err == nil {
			_, err := conn.Do("EXPIRE", key, config.GetConfig().Common.DescribeTaskExpireTime)
			if err != nil {
				log.G(ctx).Errorf("redis EXPIRE error, key: %s, err: %s", key, err)
			}
		}
	}()
	_, err = conn.Do("HSET", redis.Args{key}.AddFlat(taskInfo)...)
	if err != nil {
		log.G(ctx).Errorf("redis set error, key: %s, err: %s", key, err)
		return err
	}
	if log.IsDebug() {
		log.G(ctx).Debugf("setDescribeTaskToRedis:%s:%s", key, utils.InterfaceToString(taskInfo))
	}
	return nil
}

func (l *local) getDescribeTaskFromRedis(ctx context.Context, key string) (*types.DescribeTaskMap, error) {
	values, err := redis.Values(wrapredis.GetRedis().Do("HGETALL", key))
	if err != nil {
		if errors.Is(err, redis.ErrNil) {
			log.G(ctx).Debugf("no such key in redis:%s", key)
			return nil, nil
		}
		CubeLog.WithContext(ctx).Fatalf("getDescribeTaskFromRedis %s err:%s", key, err)
		return nil, err
	}
	if len(values) == 0 {
		CubeLog.WithContext(ctx).Warnf("redis hgetall empty, key: %s", key)
		return nil, nil
	}

	taskInfo := &types.DescribeTaskMap{}
	if err := redis.ScanStruct(values, taskInfo); err != nil {
		CubeLog.WithContext(ctx).Errorf("redis scanStruct error, key: %s, err: %s,values:%v", key, err, values)
		return nil, err
	}
	return taskInfo, nil
}

func (l *local) setTemplateImageJobPullProgressToRedis(ctx context.Context, key string, progress *types.TemplateImageJobPullProgressMap) (err error) {
	start := time.Now()
	defer traceRedis(ctx, "Create", "EVAL", key, start, err)
	_, err = wrapredis.GetRedis().Do("EVAL", templateImageJobPullProgressSetArgs(key, progress)...)
	if err != nil {
		log.G(ctx).Warnf("redis set template image job pull progress error, key: %s, err: %s", key, err)
		return err
	}
	if log.IsDebug() {
		log.G(ctx).Debugf("setTemplateImageJobPullProgressToRedis:%s:%s", key, utils.InterfaceToString(progress))
	}
	return nil
}

func (l *local) setTemplateImageJobPullProgressFieldsToRedis(ctx context.Context, key string, progress *types.TemplateImageJobPullProgressMap) (err error) {
	start := time.Now()
	defer traceRedis(ctx, "Create", "HSET", key, start, err)
	_, err = wrapredis.GetRedis().Do("HSET", redis.Args{key}.AddFlat(progress)...)
	if err != nil {
		log.G(ctx).Warnf("redis update template image job pull progress error, key: %s, err: %s", key, err)
		return err
	}
	if log.IsDebug() {
		log.G(ctx).Debugf("setTemplateImageJobPullProgressFieldsToRedis:%s:%s", key, utils.InterfaceToString(progress))
	}
	return nil
}

func (l *local) getTemplateImageJobPullProgressFromRedis(ctx context.Context, key string) (*types.TemplateImageJobPullProgressMap, error) {
	values, err := redis.Values(wrapredis.GetRedis().Do("HGETALL", key))
	if err != nil {
		if errors.Is(err, redis.ErrNil) {
			log.G(ctx).Debugf("no such key in redis:%s", key)
			return nil, nil
		}
		log.G(ctx).Warnf("getTemplateImageJobPullProgressFromRedis %s err:%s", key, err)
		return nil, err
	}
	if len(values) == 0 {
		log.G(ctx).Debugf("redis hgetall empty, key: %s", key)
		return nil, nil
	}

	progress := &types.TemplateImageJobPullProgressMap{}
	if err := redis.ScanStruct(values, progress); err != nil {
		log.G(ctx).Warnf("redis scanStruct template image job pull progress error, key: %s, err: %s, values:%v", key, err, values)
		return nil, err
	}
	return progress, nil
}

func (l *local) deleteKeyFromRedis(ctx context.Context, key string) (err error) {
	start := time.Now()
	defer func() {
		traceRedis(ctx, "Delete", "DEL", key, start, err)
	}()
	_, err = wrapredis.GetRedis().Do("DEL", key)
	if err != nil {
		log.G(ctx).Errorf("redis del error, key: %s, err: %s", key, err)
		return err
	}

	return nil
}

func traceRedis(ctx context.Context, action, redisOp, key string, start time.Time, err error) {
	cost := time.Since(start)
	baseRt := CubeLog.GetTraceInfo(ctx).DeepCopy()
	baseRt.Callee = constants.Redis
	baseRt.Action = action
	baseRt.CalleeAction = redisOp
	baseRt.InstanceID = key
	baseRt.Cost = cost
	baseRt.RetCode = int64(errorcode.ErrorCode_Success)
	if err != nil {
		baseRt.RetCode = int64(errorcode.ErrorCode_DBError)
	}
	CubeLog.Trace(baseRt)
}
