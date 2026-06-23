// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package localcache

import (
	"context"
	"errors"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"gorm.io/gorm"
)

func (l *local) DB() *gorm.DB {
	if l.db.Error == nil || errors.Is(l.db.Error, gorm.ErrRecordNotFound) {
		return l.db
	}

	if errors.Is(l.db.Error, mysql.ErrInvalidConn) {
		pinger, ok := l.db.ConnPool.(interface{ Ping() error })
		if ok {
			go func() { _ = pinger.Ping() }()
		}
	}
	return l.db
}

func (l *local) loadAllFromDB() error {
	return l.syncAllFromDB(false)
}

func (l *local) syncAllFromDB(update bool) error {
	startTime := time.Now()
	retCode := 200
	defer func() {
		log.TraceReport("", startTime, constants.MySQL, l.dbAddr, constants.ActionLoadDBAll, retCode)
	}()

	if externalNodeLoader != nil {
		nodes, err := externalNodeLoader(context.Background())
		if err != nil {
			retCode = 500
			return err
		}

		allFromDb := make(map[string]struct{}, len(nodes))
		for _, n := range nodes {
			if n == nil {
				continue
			}
			if update {
				if err := l.updateNodeFromMetaData(n); err != nil {
					l.addNodeCache(n)
				}
			} else {
				l.addNodeCache(n)
			}
			allFromDb[n.InsID] = struct{}{}
		}
		if update {
			l.checkDirty(allFromDb)
		}
		return nil
	}

	all := make([]*models.HostInfo, 0)

	if err := l.DB().Table(constants.MetadataTableName).Find(&all).Error; err != nil {
		retCode = 500
		return err
	}

	var results []*models.HostTypeInfo
	if err := l.DB().Table(constants.HostTypeTableName).Select([]string{"instance_type",
		"cpu_type"}).Find(&results).Error; err != nil {
		retCode = 500
		return err
	}
	instanceCpuType := make(map[string]*models.HostTypeInfo)
	for _, v := range results {
		instanceCpuType[v.InstanceType] = v
	}

	machineInfos := make([]*models.MachineInfo, 0)
	if err := l.DB().Table(constants.HostSubInfoTableName).Find(&machineInfos).Error; err != nil {
		log.G(context.Background()).Errorf("select HostSubInfoTableName error: %v", err)
	}
	machinesMap := make(map[string]*models.MachineInfo)
	if len(machineInfos) > 0 {
		for _, m := range machineInfos {
			machinesMap[m.InsID] = m
		}
	}

	allHostIDs := make([]string, 0, len(all))
	for _, elem := range all {
		allHostIDs = append(allHostIDs, elem.InsID)
	}
	isolatedSet, isolatedErr := l.loadIsolatedSet(allHostIDs)

	allFromDb := make(map[string]struct{})
	for _, elem := range all {
		n := constructNode(elem)
		l.applyIsolationState(n, isolatedSet, isolatedErr, update)
		if v, ok := instanceCpuType[elem.InstanceType]; ok {
			n.CPUType = v.CPUType
		}
		if v, ok := machinesMap[n.InsID]; ok {
			n.DeviceClass = v.DeviceClass
			n.DedicatedClusterId = v.DedicatedClusterId
			n.DeviceID = v.DeviceID
			n.MachineHostIP = v.HostIP
			n.InstanceFamily = v.InstanceFamily
			if v.VirtualNodeQuota != "" {
				err := utils.JSONTool.UnmarshalFromString(v.VirtualNodeQuota, &n.VirtualNodeQuotaArray)
				if err != nil {
					log.G(context.Background()).Errorf("VirtualNodeQuota error: %v", err)
				}
			}
		}
		if update {
			if err := l.updateNodeFromMetaData(n); err != nil {

				l.addNodeCache(n)
			}
		} else {
			l.addNodeCache(n)
		}
		allFromDb[n.InsID] = struct{}{}
	}

	if update {
		l.checkDirty(allFromDb)
	}
	return nil
}

func (l *local) loadFromDBByIDs(hostIDs []string) ([]*node.Node, error) {
	startTime := time.Now()
	retCode := 200
	defer func() {
		log.TraceReport("", startTime, constants.MySQL, l.dbAddr, constants.ActionLoadDBByIDs, retCode)
	}()
	if len(hostIDs) == 0 {
		return nil, errors.New("empty HostIDs")
	}
	elems := make([]*models.HostInfo, 0)
	if err := l.DB().Table(constants.MetadataTableName).Where("ins_id in ?", hostIDs).Scan(&elems).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errors.New("empty data found")
		}
		retCode = 500
		return nil, err
	}
	var results []*models.HostTypeInfo
	if err := l.DB().Table(constants.HostTypeTableName).Select([]string{"instance_type",
		"cpu_type"}).Find(&results).Error; err != nil {
		retCode = 500
		return nil, err
	}
	instanceCpuType := make(map[string]*models.HostTypeInfo)
	for _, v := range results {
		instanceCpuType[v.InstanceType] = v
	}

	machineInfos := make([]*models.MachineInfo, 0)
	if err := l.DB().Table(constants.HostSubInfoTableName).Find(&machineInfos).Error; err != nil {
		log.G(context.Background()).Error("select HostSubInfoTableName error: %v", err)
	}
	machinesMap := make(map[string]*models.MachineInfo)
	if len(machineInfos) > 0 {
		for _, m := range machineInfos {
			machinesMap[m.InsID] = m
		}
	}

	isolatedSet, isolatedErr := l.loadIsolatedSet(hostIDs)

	nodes := make([]*node.Node, 0, len(elems))
	for _, elem := range elems {
		n := constructNode(elem)
		if v, ok := instanceCpuType[elem.InstanceType]; ok {
			n.CPUType = v.CPUType
		}
		if v, ok := machinesMap[n.InsID]; ok {
			n.DeviceClass = v.DeviceClass
			n.DedicatedClusterId = v.DedicatedClusterId
			n.DeviceID = v.DeviceID
			n.MachineHostIP = v.HostIP
			n.InstanceFamily = v.InstanceFamily
			if v.VirtualNodeQuota != "" {
				err := utils.JSONTool.UnmarshalFromString(v.VirtualNodeQuota, &n.VirtualNodeQuotaArray)
				if err != nil {
					log.G(context.Background()).Errorf("VirtualNodeQuota error: %v", err)
				}
			}
		} else {
			log.G(context.Background()).Fatalf("HostSubInfo is empty: %v", n.InsID)
		}
		l.applyIsolationState(n, isolatedSet, isolatedErr, true)
		nodes = append(nodes, n)
	}
	return nodes, nil
}

// loadIsolatedSet reads the operator-applied isolation flag from the node
// registration table for the given node ids. The legacy host-meta load paths
// (constructNode) read t_cube_host_meta, which has no isolation column, so
// without this backfill a /notify/host ADD/UPDATE event would reset an
// isolated node's cordon to false in the scheduler cache until the next
// nodemeta reconcile. Errors are returned explicitly so callers can preserve
// the existing cache state instead of treating a failed read as "not isolated".
func (l *local) loadIsolatedSet(insIDs []string) (map[string]bool, error) {
	if len(insIDs) == 0 {
		return map[string]bool{}, nil
	}
	regs := make([]*models.NodeRegistration, 0, len(insIDs))
	if err := l.DB().Table(constants.NodeMetaRegistrationTable).
		Select("node_id", "isolated").
		Where("node_id in ?", insIDs).
		Find(&regs).Error; err != nil {
		log.G(context.Background()).Warnf("loadIsolatedSet failed: %v", err)
		return nil, err
	}
	out := make(map[string]bool, len(regs))
	for _, reg := range regs {
		out[reg.NodeID] = reg.Isolated
	}
	return out, nil
}

func (l *local) applyIsolationState(n *node.Node, isolatedSet map[string]bool, loadErr error, preserveCached bool) {
	if n == nil {
		return
	}
	if loadErr == nil {
		n.Isolated = isolatedSet[n.InsID]
		return
	}
	if !preserveCached {
		return
	}
	if cached, ok := l.cache.Get(n.ID()); ok {
		if old, ok := cached.(*node.Node); ok && old != nil {
			n.Isolated = old.Isolated
		}
	}
}

func constructNode(elem *models.HostInfo) *node.Node {
	n := &node.Node{
		Index:               int(elem.ID),
		InsID:               elem.InsID,
		UUID:                elem.UUID,
		IP:                  elem.IP,
		CpuTotal:            elem.CpuTotal,
		MemMBTotal:          elem.MemMBTotal,
		SystemDiskSize:      elem.SysDiskGB,
		DataDiskSize:        elem.DataDiskGB,
		Zone:                elem.Zone,
		Region:              elem.Region,
		InstanceType:        elem.InstanceType,
		HostStatus:          elem.HostStatus,
		MetaDataUpdateAt:    time.Now(),
		ReportedReady:       constants.HeartbeatHealth == elem.LiveStatus,
		Healthy:             constants.HeartbeatHealth == elem.LiveStatus,
		QuotaMem:            elem.QuotaMem,
		QuotaCpu:            elem.QuotaCpu,
		CreateConcurrentNum: elem.CreateConcurrentNum,
		MaxMvmLimit:         elem.MaxMvmNum,
		ClusterLabel:        elem.ClusterLabel,
		OssClusterLabel:     elem.OssClusterLabel,
	}
	return n
}
