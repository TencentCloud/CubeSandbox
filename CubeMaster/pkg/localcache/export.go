// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package localcache provides local cache
package localcache

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync/atomic"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/patrickmn/go-cache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db"
	fwk "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/framework"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/nodehealth"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/rediskey"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/wrapredis"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

type EventType string

const (
	ADD    EventType = "add"
	DEL    EventType = "del"
	UPDATE EventType = "update"
	DOWN   EventType = "down"
)

type Event struct {
	Type   EventType
	InsIDs []string
}

var externalNodeLoader func(context.Context) ([]*node.Node, error)

func RegisterNodeLoader(loader func(context.Context) ([]*node.Node, error)) {
	externalNodeLoader = loader
}

func Init(ctx context.Context) error {
	start := time.Now()
	l.event = make(chan *Event, 1000)
	l.cache = cache.New(0, 0)
	l.imageCache = cache.New(0, 0)
	l.templateNodeCache = cache.New(0, 0)
	l.db = db.Init(config.GetDbConfig())
	l.dbAddr = config.GetConfig().OssDBConfig.Addr
	l.totalSelfNodes = config.GetConfig().Common.DefaultHeadlessServiceNodesNum
	l.sortedNodesByClusters = make(map[string]node.NodeList)
	l.sortedNodesByClusters[constants.DefaultInstanceTypeName] = node.NodeList{}
	for _, k := range config.GetSchedulerInstanceTypeConfs() {
		l.sortedNodesByClusters[k] = node.NodeList{}
	}

	if err := l.loadAllFromDB(); err != nil {
		return fmt.Errorf("loadAllFromDB:%v", err)
	}

	if err := l.loadMetricFromRedis(); err != nil {
		CubeLog.WithContext(context.Background()).Errorf("loadMetricFromRedis:%v", err)
	}

	l.loopSelfNodes(ctx)

	go l.loop(ctx)
	go l.dealEvent(ctx)

	go l.loopUpdateMetric(ctx)
	CubeLog.WithContext(context.Background()).Debugf("init_all,cost:%v,:%v", time.Since(start), GetNodes(-1).String())

	go l.cleanSandboxCache(ctx)
	return nil
}

func GetCacheItems() map[string]cache.Item {
	return l.cache.Items()
}

func GetNodes(n int) node.NodeList {
	nodes := node.NodeList{}
	elems := l.cache.Items()
	now := time.Now()
	for _, v := range elems {
		h, ok := v.Object.(*node.Node)
		if ok {
			nodes.Append(cloneNodeWithCurrentHealth(h, now))
		}
		if n > 0 && nodes.Len() >= n {
			break
		}
	}
	return nodes
}

func GetHealthyNodes(n int) node.NodeList {
	nodes := node.NodeList{}
	elems := l.cache.Items()
	now := time.Now()
	for _, v := range elems {
		if n >= 0 && nodes.Len() >= n {
			break
		}
		h, ok := v.Object.(*node.Node)
		if ok {
			current := cloneNodeWithCurrentHealth(h, now)
			if current.Healthy && !current.Isolated {
				nodes.Append(current)
			}
		}

	}
	return nodes
}

func GetHealthyNodesByInstanceType(n int, product string) node.NodeList {

	l.lockSortedNodes.RLock()
	clusterNodes, exists := l.sortedNodesByClusters[product]
	l.lockSortedNodes.RUnlock()
	if !exists {

		return GetHealthyNodes(n)
	}

	nodes := node.NodeList{}
	now := time.Now()

	for _, v := range clusterNodes {

		if n >= 0 && nodes.Len() >= n {
			break
		}

		current := cloneNodeWithCurrentHealth(v, now)
		if current.Healthy && !current.Isolated {
			nodes.Append(current)
		}
	}

	return nodes
}

func GetNode(id string) (*node.Node, bool) {
	elem, exist := l.cache.Get(id)
	if !exist {
		return nil, exist
	}
	h, ok := elem.(*node.Node)
	if ok {
		return cloneNodeWithCurrentHealth(h, time.Now()), true
	}
	return nil, false
}

func metadataHealthTimeout() time.Duration {
	return nodehealth.MetadataTimeout(config.GetConfig().Common.SyncMetaDataInterval)
}

func cloneNodeWithCurrentHealth(n *node.Node, now time.Time) *node.Node {
	if n == nil {
		return nil
	}
	current := n.Clone()
	status := nodehealth.EvaluateFromFacts(n.ReportedReady, n.MetaDataUpdateAt, now, metadataHealthTimeout())
	current.Healthy = status.Healthy
	current.UnhealthyReason = status.UnhealthyReason
	return current
}

func GetNodesByIp(ip string) (*node.Node, bool) {
	elems := l.cache.Items()
	now := time.Now()
	for _, v := range elems {
		h, ok := v.Object.(*node.Node)
		if ok && h.IP == ip {
			return cloneNodeWithCurrentHealth(h, now), true
		}
	}
	return nil, false
}

func UpsertNode(n *node.Node) {
	if n == nil {
		return
	}
	if err := l.updateNodeFromMetaData(n); err != nil {
		l.addNodeCache(n)
	}
}

func NotifyEvent(e *Event) error {
	select {
	case l.event <- e:
		return nil
	default:
		return errors.New("event full")
	}
}

func SetSandboxProxyMap(ctx context.Context, proxyInfo *types.SandboxProxyMap) error {
	return l.setByPassProsyToRedis(ctx, rediskey.SandboxProxy(proxyInfo.SandboxID), proxyInfo)
}

func GetSandboxProxyMap(ctx context.Context, sandboxID string) (*types.SandboxProxyMap, bool) {
	for _, key := range rediskey.ReadKeysWithFallback(rediskey.SandboxProxy(sandboxID), rediskey.LegacySandboxProxy(sandboxID)) {
		proxyMap, err := l.getByPassProsyFromRedis(ctx, key)
		if err == nil && proxyMap != nil {
			return proxyMap, true
		}
	}
	return nil, false
}

func GetInstanceInfoMap(ctx context.Context, insID string) (*types.InstanceInfoMap, bool) {
	for _, key := range rediskey.ReadKeysWithFallback(rediskey.InstanceInfo(insID), rediskey.LegacyInstanceInfo(insID)) {
		insMap, err := l.getInsInfoFromRedis(ctx, key)
		if err == nil && insMap != nil {
			return insMap, true
		}
	}
	return nil, false
}

func SetInstanceInfoMap(ctx context.Context, insInfo *types.InstanceInfoMap) error {
	return l.setInstanceInfoMapToRedis(ctx, rediskey.InstanceInfo(insInfo.InsID), insInfo)
}

func SetInstanceInfoField(ctx context.Context, insID string, kv ...string) error {
	fieldValues := []interface{}{}
	for i := 0; i < len(kv); i += 2 {
		fieldValues = append(fieldValues, kv[i], kv[i+1])
	}
	key := rediskey.InstanceInfo(insID)
	_, err := wrapredis.GetRedis().Do("HSET", redis.Args{key}.AddFlat(fieldValues)...)
	if err != nil {
		log.G(ctx).Errorf("redis set error, key: %s, err: %s", key, err)
		return err
	}
	if log.IsDebug() {
		log.G(ctx).Debugf("SetInstanceInfoField:%s,%s", insID, utils.InterfaceToString(fieldValues))
	}
	return nil
}

func DeleteInstanceInfoMap(ctx context.Context, insID string) error {
	var firstErr error
	for _, key := range rediskey.DeleteKeys(rediskey.InstanceInfo(insID), rediskey.LegacyInstanceInfo(insID)) {
		if err := l.deleteKeyFromRedis(ctx, key); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func DeleteSandboxProxyMap(ctx context.Context, sandboxID string) error {
	var firstErr error
	for _, key := range rediskey.DeleteKeys(rediskey.SandboxProxy(sandboxID), rediskey.LegacySandboxProxy(sandboxID)) {
		if err := l.deleteKeyFromRedis(ctx, key); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func SetDescribeTask(ctx context.Context, taskInfo *types.DescribeTaskMap) error {
	return l.setDescribeTaskToRedis(ctx, rediskey.DescribeTask(taskInfo.TaskID), taskInfo)
}

func GetDescribeTask(ctx context.Context, taskID string) (*types.DescribeTaskMap, bool) {
	for _, key := range rediskey.ReadKeysWithFallback(rediskey.DescribeTask(taskID), rediskey.LegacyDescribeTask(taskID)) {
		taskInfo, err := l.getDescribeTaskFromRedis(ctx, key)
		if err == nil && taskInfo != nil {
			return taskInfo, true
		}
	}
	return nil, false
}

func SetTemplateImageJobPullProgress(ctx context.Context, progress *types.TemplateImageJobPullProgressMap) error {
	if progress == nil || progress.JobID == "" {
		return errors.New("template image job pull progress requires job id")
	}
	return l.setTemplateImageJobPullProgressToRedis(ctx, templateImageJobPullProgressKey(progress.JobID), progress)
}

func SetTemplateImageJobPullProgressNoTTL(ctx context.Context, progress *types.TemplateImageJobPullProgressMap) error {
	if progress == nil || progress.JobID == "" {
		return errors.New("template image job pull progress requires job id")
	}
	return l.setTemplateImageJobPullProgressFieldsToRedis(ctx, templateImageJobPullProgressKey(progress.JobID), progress)
}

func GetTemplateImageJobPullProgress(ctx context.Context, jobID string) (*types.TemplateImageJobPullProgressMap, bool) {
	if jobID == "" {
		return nil, false
	}
	progress, err := l.getTemplateImageJobPullProgressFromRedis(ctx, templateImageJobPullProgressKey(jobID))
	if err != nil || progress == nil {
		return nil, false
	}
	return progress, true
}

func DeleteTemplateImageJobPullProgress(ctx context.Context, jobID string) error {
	if jobID == "" {
		return nil
	}
	return l.deleteKeyFromRedis(ctx, templateImageJobPullProgressKey(jobID))
}

func RangeDBHost(index, size int, product string) ([]*node.Node, int) {
	l.lockSortedNodes.RLock()
	defer l.lockSortedNodes.RUnlock()
	clusterNodes, exists := l.sortedNodesByClusters[product]
	if !exists {

		clusterNodes = l.sortedNodesByClusters[constants.DefaultInstanceTypeName]
	}
	if clusterNodes == nil {
		return nil, 0
	}
	return clusterNodes.IndexByPage(index, size)
}

func MaxMvmLimit(n *node.Node) (num int64) {
	defer func() {
		if num == 0 {
			num = 3000
		}
	}()
	if n == nil {
		num = config.GetConfig().Scheduler.NodeMaxMvmNum
		return
	}
	num = n.MaxMvmLimit
	confNum := config.GetConfig().Scheduler.GetNodeMaxMvmNumConf(n.InstanceType).MvmNum
	if num <= 0 {

		num = confNum
	}

	return num
}

func RealMaxMvmLimit(n *node.Node) (num int64) {
	if n == nil {
		num = 0
		return
	}
	percent := config.GetConfig().Scheduler.GetNodeMaxMvmNumConf(n.InstanceType).MvmNumReserveNumPercent
	maxnum := MaxMvmLimit(n)

	return int64(math.Ceil(float64(maxnum) * percent))
}

func CreateConcurrentLimit(n *node.Node) (num int64) {
	defer func() {
		if num == 0 {
			num = 50
		}
	}()
	if n == nil {
		num = config.GetConfig().CubeletConf.CreateConcurrentLimit
		return
	}
	num = n.CreateConcurrentNum
	if num <= config.GetConfig().CubeletConf.CreateConcurrentLimit {
		num = config.GetConfig().CubeletConf.CreateConcurrentLimit
	}

	return num
}

func RealTimeCreateConcurrentLimit(n *node.Node) (num int64) {
	if n == nil {
		return 0
	}
	return n.RealTimeCreateNum
}

func LocalCreateConcurrentLimit(n *node.Node) (num int64) {
	if n == nil {
		return 0
	}
	return atomic.LoadInt64(&n.LocalCreateNum)
}

func IncrNodeConcurrent(n *node.Node) error {
	if n == nil {
		return nil
	}
	if cached, ok := getMutableNode(n); ok {
		cached.LocalCreateNumIncrBy(1)
		return nil
	}
	n.LocalCreateNumIncrBy(1)
	return nil
}

func DecrNodeConcurrent(n *node.Node) error {
	if n == nil {
		return nil
	}
	if cached, ok := getMutableNode(n); ok {
		cached.LocalCreateNumIncrBy(-1)
		return nil
	}
	n.LocalCreateNumIncrBy(-1)
	return nil
}

func getMutableNode(n *node.Node) (*node.Node, bool) {
	if n == nil {
		return nil, false
	}
	elem, ok := l.cache.Get(n.ID())
	if !ok {
		return nil, false
	}
	cached, ok := elem.(*node.Node)
	if !ok || cached == nil {
		return nil, false
	}
	return cached, true
}

func HealthyMasterNodes() (num int64) {
	defer func() {
		if num == 0 {
			num = 1
		}
	}()
	return atomic.LoadInt64(&l.totalSelfNodes)
}

func GetImageStateByNode(imageName string, nodeName string) *fwk.ImageStateSummary {
	state := l.getImageCache(imageName)
	if state == nil {
		return nil
	}
	if state.HasNode(nodeName) {
		return state
	}
	return nil
}

func RegisterTemplateReplica(templateID, nodeID string, sizeBytes int64) {
	registerTemplateReplica(templateID, nodeID, sizeBytes, true)
}

func DeregisterTemplateReplica(templateID, nodeID string) {
	deregisterTemplateReplica(templateID, nodeID, true)
}

func registerTemplateReplica(templateID, nodeID string, sizeBytes int64, syncNodeTemplates bool) {
	if templateID == "" || nodeID == "" {
		return
	}
	state := l.getImageCache(templateID)
	if state == nil {
		ossClusterLabel := ""
		if n, ok := GetNode(nodeID); ok && n != nil {
			ossClusterLabel = n.OssClusterLabel
		}
		state = fwk.NewImageStateSummary(sizeBytes, ossClusterLabel, nodeID)
		l.addImageCache(templateID, state)
	} else {
		if sizeBytes > 0 {
			state.Size = sizeBytes
		}
		state.AddNode(nodeID)
		state.UpdateAt = time.Now()
	}
	if state.OssClusterLabel != "" {
		state.ScaledImageScore = scaledImageScore(state, GetHealthyNodesByInstanceType(-1, state.OssClusterLabel).Len())
	}
	if syncNodeTemplates {
		recordNodeTemplateMembership(nodeID, templateID)
	}
}

func deregisterTemplateReplica(templateID, nodeID string, syncNodeTemplates bool) {
	if templateID == "" || nodeID == "" {
		return
	}
	state := l.getImageCache(templateID)
	if state != nil {
		state.RemoveNode(nodeID)
		if state.GetNumNodes() == 0 && l.imageCache != nil {
			l.imageCache.Delete(templateID)
		}
	}
	if syncNodeTemplates {
		removeNodeTemplateMembership(nodeID, templateID)
	}
}

func InvalidateImageState(imageName string) {
	if imageName == "" || l.imageCache == nil {
		return
	}
	l.imageCache.Delete(imageName)
	removeTemplateMembershipFromAllNodes(imageName)
}
