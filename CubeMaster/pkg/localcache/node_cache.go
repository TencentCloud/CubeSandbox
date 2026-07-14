// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package localcache

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/patrickmn/go-cache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/nodehealth"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/recov"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet/grpcconn"
	"github.com/tencentcloud/CubeSandbox/cubelog"
	"gorm.io/gorm"
)

type local struct {
	cache             *cache.Cache
	imageCache        *cache.Cache
	templateNodeCache *cache.Cache
	event             chan *Event
	db                *gorm.DB
	dbAddr            string
	lockMetaData      sync.Mutex
	lockSortedNodes   sync.RWMutex

	sortedNodesByClusters map[string]node.NodeList
	totalSelfNodes        int64
}

var l = &local{}

func (l *local) loopSelfNodes(ctx context.Context) {
	loadNum := func() int64 {

		realNum := config.GetConfig().Common.DefaultHeadlessServiceNodesNum
		if config.GetConfig().Common.HeadlessServiceName != "" {
			ips, err := net.LookupHost(config.GetConfig().Common.HeadlessServiceName)
			if err != nil {
				CubeLog.WithContext(context.Background()).Fatalf("LookupHost:%v fail:%v",
					config.GetConfig().Common.HeadlessServiceName, err)
			} else {
				realNum = int64(len(ips))
			}
		}

		if realNum <= 0 {
			realNum = config.GetConfig().Common.DefaultHeadlessServiceNodesNum
		}
		return realNum
	}
	l.totalSelfNodes = loadNum()
	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				recov.WithRecover(func() {
					atomic.StoreInt64(&l.totalSelfNodes, loadNum())
				}, func(panicError interface{}) {
					CubeLog.WithContext(context.Background()).Fatalf("loopSelfNodes panic:%v", panicError)
				})
			}
		}
	}()
}

func (l *local) loop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	checkDeadline := time.Now().Add(config.GetConfig().Common.SyncMetaDataInterval)
	for {
		select {
		case <-ticker.C:
			recov.WithRecover(func() {
				if checkDeadline.After(time.Now()) {

					return
				}
				defer func() {
					checkDeadline = time.Now().Add(config.GetConfig().Common.SyncMetaDataInterval)
				}()

				if err := l.syncAllFromDB(true); err != nil {
					CubeLog.WithContext(context.Background()).Errorf("loop_all:%s", err)
				}
				if log.IsDebug() {
					CubeLog.WithContext(context.Background()).Debugf("loop_all:%s", GetNodes(-1).String())
				}
				CubeLog.WithContext(context.Background()).Errorf("loop_all,size:%d", l.cache.ItemCount())
			}, func(panicError interface{}) {
				checkDeadline = time.Now().Add(config.GetConfig().Common.SyncMetaDataInterval)
				CubeLog.WithContext(context.Background()).Fatalf("loop panic:%v", panicError)
			})
		case <-ctx.Done():
			return
		}
	}
}

func (l *local) dealEvent(ctx context.Context) {
	for {
		select {
		case e := <-l.event:
			recov.WithRecover(func() {
				if e == nil {
					return
				}
				if DEL == e.Type {
					for _, nodeID := range e.InsIDs {
						l.delNodeCache(&node.Node{
							InsID: nodeID,
						})
						CubeLog.WithContext(context.Background()).Warnf("Host delete:%v", nodeID)
					}
				} else if ADD == e.Type || UPDATE == e.Type {
					if nodes, err := l.loadFromDBByIDs(e.InsIDs); err == nil {
						for _, node := range nodes {
							if err := l.updateNodeFromMetaData(node); err != nil {

								l.addNodeCache(node)
							}
						}
					} else {
						CubeLog.WithContext(context.Background()).Errorf("loadFromDBByIDs fail:%v", err)
					}
				} else if DOWN == e.Type {
					if nodes, err := l.loadFromDBByIDs(e.InsIDs); err == nil {
						for _, n := range nodes {
							l.downNodeCache(n)
							CubeLog.WithContext(context.Background()).Warnf("Host down:%v", n.ID())
						}
					} else {
						CubeLog.WithContext(context.Background()).Errorf("loadFromDBByIDs fail:%v", err)
					}
				}
			}, func(panicError interface{}) {
				CubeLog.WithContext(context.Background()).Fatalf("dealEvent panic:%v,%v", panicError, e)
			})
		case <-ctx.Done():
			return
		}
	}
}

func (l *local) checkDirty(allFromDb map[string]struct{}) {
	if len(allFromDb) == 0 {
		CubeLog.WithContext(context.Background()).Warnf("checkDirty allFromDb is empty")
		return
	}
	elems := l.cache.Items()
	for _, v := range elems {
		h, ok := v.Object.(*node.Node)
		if ok {
			if _, ok := allFromDb[h.InsID]; !ok {
				CubeLog.WithContext(context.Background()).Errorf("node %s is dirty", h.InsID)
				l.delNodeCache(h)
			}
		}
	}
}

func (l *local) addNodeCache(n *node.Node) {
	if n == nil {
		CubeLog.WithContext(context.Background()).Warnf("node is nil")
		return
	}
	l.cache.SetDefault(n.ID(), n)
	l.appendSortedNodes(n)
}

func (l *local) delNodeCache(n *node.Node) {
	if n == nil {
		CubeLog.WithContext(context.Background()).Warnf("node is nil")
		return
	}
	l.cache.Delete(n.ID())
	l.delSortedNodes(n)

	grpcconn.CloseWorkerConn(cubelet.GetCubeletAddr(n.HostIP()))
}

func (l *local) downNodeCache(n *node.Node) {
	if n == nil {
		CubeLog.WithContext(context.Background()).Warnf("node is nil")
		return
	}

	if v, exist := l.cache.Get(n.ID()); exist {
		l.lockMetaData.Lock()
		old := v.(*node.Node)
		old.ReportedReady = false
		old.Healthy = false
		old.UnhealthyReason = nodehealth.ReasonReportedNotReady
		old.MetaDataUpdateAt = time.Now()
		l.lockMetaData.Unlock()
		l.delSortedNodes(n)
	}

	grpcconn.CloseWorkerConn(cubelet.GetCubeletAddr(n.HostIP()))
}

func (l *local) getInstanceTypeName(n *node.Node) string {
	product := config.GetInstanceTypeOfClusterLabel(n.OssClusterLabel)
	if product == "" {
		product = constants.DefaultInstanceTypeName
	}
	if _, exists := l.sortedNodesByClusters[product]; !exists {
		product = constants.DefaultInstanceTypeName
	}
	return product
}

func (l *local) appendSortedNodes(n *node.Node) {
	if n == nil || !n.Healthy {
		return
	}

	l.lockSortedNodes.Lock()
	defer l.lockSortedNodes.Unlock()
	product := l.getInstanceTypeName(n)
	ref := l.sortedNodesByClusters[product]
	if ref == nil {
		ref = node.NodeList{}
	}
	ref.Append(n)
	ref.AllSortByIndex()
	l.sortedNodesByClusters[product] = ref
}

func (l *local) delSortedNodes(n *node.Node) {
	l.lockSortedNodes.Lock()
	defer l.lockSortedNodes.Unlock()

	product := l.getInstanceTypeName(n)
	ref := l.sortedNodesByClusters[product]
	if ref == nil {
		ref = node.NodeList{}
	}
	ref.Remove(n)
	l.sortedNodesByClusters[product] = ref
}

func (l *local) updateSortedNodes(n *node.Node) {
	l.lockSortedNodes.Lock()
	defer l.lockSortedNodes.Unlock()
	product := l.getInstanceTypeName(n)
	ref := l.sortedNodesByClusters[product]
	if ref == nil {
		ref = node.NodeList{}
	}
	if !n.Healthy {
		ref.Remove(n)
	} else {

		ref.Add(n)

		ref.AllSortByIndex()
	}
	l.sortedNodesByClusters[product] = ref
}

func (l *local) updateNodeFromMetaData(n *node.Node) error {
	if n == nil {
		return errors.New("node is nil")
	}

	if v, exist := l.cache.Get(n.ID()); exist {

		l.lockMetaData.Lock()
		old := v.(*node.Node)
		old.IP = n.IP
		old.UUID = n.UUID
		old.CpuTotal = n.CpuTotal
		old.MemMBTotal = n.MemMBTotal
		old.SystemDiskSize = n.SystemDiskSize
		old.DataDiskSize = n.DataDiskSize
		old.Zone = n.Zone
		old.Region = n.Region
		old.QuotaCpu = n.QuotaCpu
		old.QuotaMem = n.QuotaMem
		old.ReportedReady = n.ReportedReady
		old.Healthy = n.Healthy
		old.UnhealthyReason = n.UnhealthyReason
		old.HostStatus = n.HostStatus
		old.CreateConcurrentNum = n.CreateConcurrentNum
		old.MaxMvmLimit = n.MaxMvmLimit
		old.MetaDataUpdateAt = n.MetaDataUpdateAt
		old.DeviceClass = n.DeviceClass
		old.DeviceID = n.DeviceID
		old.MachineHostIP = n.MachineHostIP
		old.InstanceFamily = n.InstanceFamily
		old.VirtualNodeQuotaArray = n.VirtualNodeQuotaArray
		old.ClusterLabel = n.ClusterLabel
		old.CPUType = n.CPUType
		old.InstanceType = n.InstanceType
		old.OssClusterLabel = n.OssClusterLabel
		var labels map[string]string
		if n.NodeLabels != nil {
			labels = make(map[string]string, len(n.NodeLabels))
			for k, v := range n.NodeLabels {
				labels[k] = v
			}
		}
		old.NodeLabels = labels
		old.InvalidateLabelsCache()
		old.SetSchedulingDisabled(n.SchedulingDisabled())
		l.lockMetaData.Unlock()

		l.updateSortedNodes(old)
		return nil
	} else {
		return fmt.Errorf("item [%s:%s] doesn't exist", n.ID(), n.IP)
	}
}

func (l *local) updateNodeMetric(n *node.Node) error {
	if n == nil {
		return errors.New("node is nil")
	}
	if v, exist := l.cache.Get(n.ID()); exist {

		l.lockMetaData.Lock()
		defer l.lockMetaData.Unlock()
		old := v.(*node.Node)
		old.MetricUpdate = n.MetricUpdate
		old.QuotaCpuUsage = n.QuotaCpuUsage
		old.QuotaMemUsage = n.QuotaMemUsage
		old.CpuLoadUsage = n.CpuLoadUsage
		old.CpuUtil = n.CpuUtil
		old.MemUsage = n.MemUsage
		old.DataDiskUsagePer = n.DataDiskUsagePer
		old.StorageDiskUsagePer = n.StorageDiskUsagePer
		old.SysDiskUsagePer = n.SysDiskUsagePer
		old.MvmNum = n.MvmNum
		old.RealTimeCreateNum = n.RealTimeCreateNum
		old.NicQueues = n.NicQueues
		old.MetricLocalUpdateAt = time.Now().Local()
		return nil
	} else {
		return fmt.Errorf("item %s doesn't exist", n.ID())
	}
}
