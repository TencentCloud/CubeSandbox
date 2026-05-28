// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package instancecache

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/version"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"gorm.io/gorm"
)

type local struct {
	db     *gorm.DB
	dbAddr string
}

var l = &local{}

var describeSupportFilters = map[string]string{
	constants.FilterVpcID:            "vpc_id",
	constants.FilterInstanceID:       "ins_id",
	constants.FilterPrivateIPAddress: "private_ip_addresses",
	constants.FilterInstanceState:    "ins_state",
	constants.FilterZone:             "zone",
	constants.FilterCPUType:          "cpu_type",
}

type DescribeFilter struct {
	Name   string
	Values []string
}

type DescribeInstancesQuery struct {
	Region  string
	Offset  int64
	Limit   int64
	Filters []DescribeFilter
}

func Init(ctx context.Context) error {
	_ = ctx
	// Schema is owned by pkg/base/dao/migrate and applied at startup
	// before this package's Init runs.
	l.db = db.Init(config.GetInstanceConfig())
	l.dbAddr = config.GetInstanceConfig().Addr
	return nil
}

func Create(ctx context.Context, ins *models.InstanceInfo) error {
	startTime := time.Now()
	var err error
	defer func() {
		traceReport(ctx, startTime, constants.MySQL, l.dbAddr, constants.ActionDBCreate, err)
	}()
	log.G(ctx).Infof("Create:%s", utils.InterfaceToString(ins))
	for range config.GetConfig().Common.DbMaxRetryCount {
		err = l.DB().Table(constants.InstanceInfoTableName).Create(ins).Error
		if err != nil {
			if errors.Is(err, gorm.ErrDuplicatedKey) || strings.Contains(err.Error(), "1062") ||
				strings.Contains(err.Error(), "Duplicate entry") {
				err = ErrDuplicateEntry
				return err
			}
			time.Sleep(config.GetConfig().Common.DbRetryInterval)
			continue
		}
		break
	}
	return err
}

func CreateUserData(ctx context.Context, insID, userData string) error {
	startTime := time.Now()
	var err error
	defer func() {
		traceReport(ctx, startTime, constants.MySQL, l.dbAddr, constants.ActionDBCreate, err)
	}()
	log.G(ctx).Infof("CreateUserData:%s", insID)
	ud := &models.InstanceUserData{
		InsID:    insID,
		UserData: userData,
	}
	for range config.GetConfig().Common.DbMaxRetryCount {
		err = l.DB().Table(constants.InstanceUserDataTableName).Create(ud).Error
		if err != nil {
			if errors.Is(err, gorm.ErrDuplicatedKey) || strings.Contains(err.Error(), "1062") ||
				strings.Contains(err.Error(), "Duplicate entry") {
				err = ErrDuplicateEntry
				return err
			}
			time.Sleep(config.GetConfig().Common.DbRetryInterval)
			continue
		}
		break
	}
	return err
}

func GetUserDataByInsID(ctx context.Context, insID string) (string, error) {
	startTime := time.Now()
	var err error
	defer func() {
		traceReport(ctx, startTime, constants.MySQL, l.dbAddr, constants.ActionDBGetById, err)
	}()
	insIDGet := keyInsID + " = ?"
	db := l.DB().Table(constants.InstanceUserDataTableName).Where(insIDGet, insID)
	var ins models.InstanceUserData
	for range config.GetConfig().Common.DbMaxRetryCount {
		err = db.First(&ins).Error
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				err = ErrorKeyNotFound
				return "", err
			}
			continue
		}
		break
	}

	if err != nil {
		return "", err
	}

	return ins.UserData, nil
}

func wrapUpdates(ctx context.Context, insID string, values map[string]any) error {
	startTime := time.Now()
	var err error
	defer func() {
		traceReport(ctx, startTime, constants.MySQL, l.dbAddr, constants.ActionDBUpdate, err)
	}()
	insIDUpdate := keyInsID + " = ?"
	db := l.DB().Table(constants.InstanceInfoTableName).Where(insIDUpdate, insID)
	for range config.GetConfig().Common.DbMaxRetryCount {
		err = db.Updates(values).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			time.Sleep(config.GetConfig().Common.DbRetryInterval)
			continue
		}
		break
	}
	return err
}

func UpdateInstanceCreateInfo(ctx context.Context, insID string, status, sandboxID, hostIP, sandboxIP, mac string) error {
	updates := map[string]any{
		keyInsState: status,
	}
	if sandboxID != "" {
		updates["uuid"] = sandboxID
	}

	if hostIP != "" {
		updates["host_ip"] = hostIP
		n, ok := localcache.GetNodesByIp(hostIP)
		if ok {
			updates["cpu_type"] = n.CPUType
			updates["host_id"] = n.InsID
			updates["zone"] = n.Zone
		}
	}
	if sandboxIP != "" {
		updates[keyPrivateIpAddresses] = utils.InterfaceToString([]string{sandboxIP})
		if version.Version >= "1.3.2" {
			updates[keyPrivateIp] = sandboxIP
			updates[keyPrivateIpCnt] = 1
			updates["mac_address"] = mac
		}
	}

	log.G(ctx).Infof("UpdateInstanceCreateInfo:%s,%+v", insID, updates)
	return wrapUpdates(ctx, insID, updates)
}

func UpdateInstanceHostInfo(ctx context.Context, insID string, hostIP, hostID string) error {
	updates := map[string]any{
		"host_ip": hostIP,
		"host_id": hostID,
	}
	log.G(ctx).Infof("UpdateInstanceHostInfo:%s,%+v", insID, updates)
	return wrapUpdates(ctx, insID, updates)
}

func UpdateInstanceStatus(ctx context.Context, insID string, status string) error {
	log.G(ctx).Infof("UpdateInstanceStatus:%s,%s", insID, status)
	updates := map[string]any{
		keyInsState: status,
	}
	var errs error
	if err := wrapUpdates(ctx, insID, updates); err != nil {
		errs = errors.Join(errs, err)
	}
	if err := localcache.SetInstanceInfoField(ctx, insID, keyInsState, status); err != nil {
		errs = errors.Join(errs, err)
	}
	return errs
}

func UpdateInstanceFailMsg(ctx context.Context, insID string, msg string) error {
	log.G(ctx).Infof("UpdateInstanceFailMsg:%s,%+v", insID, msg)
	updates := map[string]any{
		keyInsState: constants.InstanceStateFailed,
		keyFailMsg:  msg,
	}
	return wrapUpdates(ctx, insID, updates)
}

func GetInstandesByInsID(ctx context.Context, insID string) (*models.InstanceInfo, error) {
	startTime := time.Now()
	var err error
	defer func() {
		traceReport(ctx, startTime, constants.MySQL, l.dbAddr, constants.ActionDBGetById, err)
	}()

	db := l.DB().Table(constants.InstanceInfoTableName).Where(keyInsID+" = ?", insID)
	var ins models.InstanceInfo
	for range config.GetConfig().Common.DbMaxRetryCount {
		err = db.First(&ins).Error
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				err = ErrorKeyNotFound
				return nil, err
			}
			time.Sleep(config.GetConfig().Common.DbRetryInterval)
			continue
		}
		break
	}

	if err != nil {
		return nil, err
	}

	return &ins, nil
}

func GetInstandesByUUID(ctx context.Context, uuid string) (*models.InstanceInfo, error) {
	startTime := time.Now()
	var err error
	defer func() {
		traceReport(ctx, startTime, constants.MySQL, l.dbAddr, constants.ActionDBGetById, err)
	}()
	db := l.DB().Table(constants.InstanceInfoTableName).Where(keyUUID+" = ?", uuid)
	var ins models.InstanceInfo
	for range config.GetConfig().Common.DbMaxRetryCount {
		err = db.First(&ins).Error
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				err = ErrorKeyNotFound
				return nil, err
			}
			continue
		}
		break
	}

	if err != nil {
		return nil, err
	}

	return &ins, nil
}

func GetInstandesByInsIDs(ctx context.Context, insIDs []string) ([]*models.InstanceInfo, error) {
	startTime := time.Now()
	var err error
	defer func() {
		traceReport(ctx, startTime, constants.MySQL, l.dbAddr, constants.ActionDBGetById, err)
	}()

	db := l.DB().Table(constants.InstanceInfoTableName).Where(keyInsID+" in ?", insIDs)
	var ins []*models.InstanceInfo
	for range config.GetConfig().Common.DbMaxRetryCount {

		err = db.Scan(&ins).Error
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				err = ErrorKeyNotFound
				return nil, err
			}
			time.Sleep(config.GetConfig().Common.DbRetryInterval)
			continue
		}
		break
	}

	if err != nil {
		return nil, err
	}

	return ins, nil
}

func DeleteInstance(ctx context.Context, insID string) error {
	startTime := time.Now()
	var err error
	defer func() {
		traceReport(ctx, startTime, constants.MySQL, l.dbAddr, constants.ActionDBDelete, err)
	}()
	insIDUpdate := keyInsID + " = ?"
	log.G(ctx).Infof("DeleteInstance:%s,%s", insIDUpdate, insID)

	err = l.DB().Transaction(func(tx *gorm.DB) error {

		db := tx.Table(constants.InstanceInfoTableName).Where(insIDUpdate, insID)
		var deleteErr error
		for range config.GetConfig().Common.DbMaxRetryCount {
			if config.GetConfig().Common.DisableHardDelete {

				deleteErr = db.Delete(&models.InstanceInfo{}).Error
			} else {

				deleteErr = db.Unscoped().Delete(&models.InstanceInfo{}).Error
			}
			if deleteErr != nil && !errors.Is(deleteErr, gorm.ErrRecordNotFound) {
				time.Sleep(config.GetConfig().Common.DbRetryInterval)
				continue
			}
			break
		}
		if deleteErr != nil && !errors.Is(deleteErr, gorm.ErrRecordNotFound) {
			return deleteErr
		}

		for range config.GetConfig().Common.DbMaxRetryCount {
			deleteErr = tx.Table(constants.InstanceUserDataTableName).Where(insIDUpdate, insID).
				Delete(&models.InstanceUserData{}).Error
			if deleteErr != nil && !errors.Is(deleteErr, gorm.ErrRecordNotFound) {
				time.Sleep(config.GetConfig().Common.DbRetryInterval)
				continue
			}
			break
		}
		if deleteErr != nil && !errors.Is(deleteErr, gorm.ErrRecordNotFound) {
			return deleteErr
		}
		return deleteErr
	})

	return err
}

func buildLikeConditions(db *gorm.DB, field string, values []string) *gorm.DB {
	if len(values) == 0 {
		return db
	}

	var queryBuilder strings.Builder

	for i := range values {
		if i > 0 {
			queryBuilder.WriteString(" OR ")
		}
		queryBuilder.WriteString(field + " LIKE ?")
	}

	likeValues := make([]any, len(values))
	for i, v := range values {
		likeValues[i] = fmt.Sprintf("%%%s%%", v)
	}

	return db.Where(queryBuilder.String(), likeValues...)
}

func ListInstances(ctx context.Context, request *DescribeInstancesQuery) ([]*models.InstanceInfo, error) {
	startTime := time.Now()
	var err error
	defer func() {
		traceReport(ctx, startTime, constants.MySQL, l.dbAddr, constants.ActionDBGetByIndex, err)
	}()
	if version.Version >= "1.3.2" && config.GetConfig().Common.EnablePrivateIpQuery {
		return ListInstancesExt(ctx, request)
	}

	queryConditions := make(map[string]interface{})
	queryConditions[keyRegion] = request.Region

	listSoftDelete := false
	db := l.DB().Table(constants.InstanceInfoTableName).Where(keyRegion+" = ?", request.Region)
	for _, v := range request.Filters {
		if keyName, ok := describeSupportFilters[v.Name]; ok {
			queryConditions[keyName] = v.Values
			switch keyName {
			case keyPrivateIpAddresses:
				db = buildLikeConditions(db, keyName, v.Values)
			case keyInsState:
				db = whereFormat(keyName, v.Values, db)
				if slices.Contains(v.Values, constants.InstanceStateTerminated) ||
					slices.Contains(v.Values, constants.InstanceStateFailed) {
					listSoftDelete = true
				}
			default:
				db = whereFormat(keyName, v.Values, db)
			}
		}
	}
	log.G(ctx).Infof("ListInstances:%+v", queryConditions)
	db = db.Offset(int(request.Offset)).Limit(int(request.Limit))
	var insList []*models.InstanceInfo

	if listSoftDelete {
		if err := db.Order("id desc").Scan(&insList).Error; err != nil {
			return nil, fmt.Errorf("ListInstances:%w", err)
		}
	} else {
		if err := db.Order("id desc").Find(&insList).Error; err != nil {
			return nil, fmt.Errorf("ListInstances:%w", err)
		}
	}

	return insList, nil
}

func isDescribeWhiteList(ctx context.Context, request *DescribeInstancesQuery) bool {
	_ = ctx
	_ = request
	return false
}

func whereFormat(keyName string, values []string, db *gorm.DB) *gorm.DB {
	if len(values) > 1 {
		db = db.Where(keyName+" IN ? ", values)
	} else if len(values) == 1 {
		db = db.Where(keyName+" = ?", values[0])
	}
	return db
}

func ListInstancesExt(ctx context.Context, request *DescribeInstancesQuery) ([]*models.InstanceInfo, error) {
	queryConditions := make(map[string]any)
	queryConditions[keyRegion] = request.Region

	listSoftDelete := false
	db := l.DB().Table(constants.InstanceInfoTableName).Where(keyRegion+" = ?", request.Region)
	queryByMultiIps := false
	queryIps := []string{}
	for _, v := range request.Filters {
		if keyName, ok := describeSupportFilters[v.Name]; ok {
			queryConditions[keyName] = v.Values
			switch keyName {
			case keyPrivateIpAddresses:
				if len(v.Values) > 1 {
					queryByMultiIps = true
					queryIps = append(queryIps, v.Values...)

					db = db.Where(
						fmt.Sprintf("%s IN ? OR %s > ?", keyPrivateIp, keyPrivateIpCnt),
						v.Values, 1)
				} else {
					db = whereFormat(keyPrivateIp, v.Values, db)
				}
			case keyInsState:
				db = whereFormat(keyName, v.Values, db)
				if slices.Contains(v.Values, constants.InstanceStateTerminated) ||
					slices.Contains(v.Values, constants.InstanceStateFailed) {
					listSoftDelete = true
				}
			default:
				db = whereFormat(keyName, v.Values, db)
			}
		}
	}
	log.G(ctx).Infof("ListInstances:%+v", queryConditions)
	db = db.Offset(int(request.Offset)).Limit(int(request.Limit))
	var insList []*models.InstanceInfo

	if listSoftDelete {
		if err := db.Order("id desc").Scan(&insList).Error; err != nil {
			return nil, fmt.Errorf("ListInstancesExt:%w", err)
		}
	} else {
		if err := db.Order("id desc").Find(&insList).Error; err != nil {
			return nil, fmt.Errorf("ListInstancesExt:%w", err)
		}
	}
	var realInsList []*models.InstanceInfo
	if queryByMultiIps {
		realInsList = filterIps(ctx, queryIps, insList)
	} else {
		realInsList = insList
	}
	return realInsList, nil
}

func filterIps(ctx context.Context, queryIps []string, insList []*models.InstanceInfo) []*models.InstanceInfo {
	realInsList := []*models.InstanceInfo{}
	for _, v := range insList {
		if v.PrivateIP == "" {
			continue
		}
		if v.PrirvateIPCnt > 1 {
			var ips []string
			_ = utils.JSONTool.Unmarshal([]byte(v.PrivateIPAddresses), &ips)
			if len(ips) > 1 {
				log.G(ctx).Debugf("ListInstancesExt:expect:%v,got:%v", utils.InterfaceToString(queryIps),
					utils.InterfaceToString(ips))

				for _, ip := range ips {
					if slices.Contains(queryIps, ip) {
						realInsList = append(realInsList, v)
						break
					}
				}
			}
		} else {

			realInsList = append(realInsList, v)
		}
	}
	return realInsList
}
