// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package integration

import (
	"context"
	"fmt"
	stdlog "log"
	"math"
	"net"
	"os"
	"testing"

	"github.com/gomodule/redigo/redis"
	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/cmd/cubemaster/app"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/recov"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/wrapredis"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/instancecache"
)

func TestMain(m *testing.M) {

	_, err := config.Init()
	if err != nil {
		stdlog.Fatalf("config init fail:%v", recov.DumpStacktrace(3, err))
		return
	}

	MockInit()

	go func() {
		app := app.New()
		app.Run()
	}()
	wait_done()

	defer func() {
		if r := recover(); r != nil {
			fmt.Println(r)
		}
	}()
	os.Exit(func() int {
		defer func() {
			mocktest_Cancel()

		}()
		return m.Run()
	}())
}

func TestDemo(t *testing.T) {
	mocktest_InitGlobalResources()
	registerCleanup(t)
	t.Log("test main demo")
	_, err := wrapredis.GetRedis().Do("SET", "key", "value")
	assert.NoError(t, err)
	v, err := redis.String(wrapredis.GetRedis().Do("GET", "key"))
	assert.NoError(t, err)
	assert.Equal(t, "value", v)

	err = instancecache.CreateUserData(context.Background(), "cubebox-demo", "data")
	assert.NoError(t, err)
	data, err := instancecache.GetUserDataByInsID(context.Background(), "cubebox-demo")
	assert.NoError(t, err)
	assert.Equal(t, "data", data)
}

func TestRound(t *testing.T) {
	mocktest_InitGlobalResources()
	registerCleanup(t)
	healthyNodes := 10
	limitOfEveryNode := 10
	healhyMasterNodes := 1
	assert.Equal(t, int64(100), int64(math.Round(float64(healthyNodes*limitOfEveryNode*1.0/healhyMasterNodes))))
	healhyMasterNodes = 3
	assert.Equal(t, int64(33), int64(math.Round(float64(healthyNodes*limitOfEveryNode*1.0/healhyMasterNodes))))

	t.Logf("mocktest_hostQuotaCpu: %d", mocktest_hostQuotaCpu)
	t.Logf("mocktest_hostMemTotal: %d", mocktest_hostMemTotal)
	t.Logf("mocktest_hostQuotaMem: %d", mocktest_hostQuotaMem)
}

func TestNsLookup(t *testing.T) {
	mocktest_InitGlobalResources()
	registerCleanup(t)
	ips, err := net.LookupHost("www.qq.com")
	if err != nil {
		t.Fatal(err)
	}
	assert.True(t, len(ips) > 0)
	t.Logf("ips:%v", ips)
}
