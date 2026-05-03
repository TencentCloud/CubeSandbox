// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package localcache

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
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

	allFromDb := make(map[string]struct{})
	for _, elem := range all {
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
				err := json.Unmarshal([]byte(v.VirtualNodeQuota), &n.VirtualNodeQuotaArray)
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
				// MIGRATION: Using standard encoding/json
				err := json.Unmarshal([]byte(v.VirtualNodeQuota), &n.VirtualNodeQuotaArray)
				if err != nil {
					log.G(context.Background()).Errorf("VirtualNodeQuota error: %v", err)
				}
			}
		} else {
			log.G(context.Background()).Fatalf("HostSubInfo is empty: %v", n.InsID)
		}
		nodes = append(nodes, n)
	}
	return nodes, nil
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