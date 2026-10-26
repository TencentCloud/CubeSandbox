// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package instancecache

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"gorm.io/gorm"
)

const (
	keyRegion             = "region"
	keyInsID              = "ins_id"
	keyInsState           = "ins_state"
	keyVpcId              = "vpc_id"
	keySubnetId           = "subnet_id"
	keyPrivateIpAddresses = "private_ip_addresses"
	keyDiskState          = "disk_state"
	keyFailMsg            = "fail_msg"
	keyUUID               = "uuid"
	keyPrivateIp          = "private_ip"
	keyPrivateIpCnt       = "private_ip_cnt"
	keyDataDiskInfo       = "data_disks"
)

var (
	ErrorKeyNotFound  = gorm.ErrRecordNotFound
	ErrDuplicateEntry = gorm.ErrDuplicatedKey
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

func traceReport(ctx context.Context, startTime time.Time, callee, calleeEndPoint, action string, err error) {
	retCode := 200
	if err != nil {
		retCode = 500
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			retCode = 500
		}
		if errors.Is(err, gorm.ErrDuplicatedKey) || strings.Contains(err.Error(), "1062") {
			retCode = 409
		}
	}
	log.ReportExt(ctx, callee, calleeEndPoint, action, action, time.Since(startTime), int64(retCode))
}
