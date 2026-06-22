// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package wrapredis

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
)

func init() {
	mydir, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	fmt.Printf("mydir=%s\n", mydir)
	if os.Getenv("CUBE_MASTER_CONFIG_PATH") == "" {
		os.Setenv("CUBE_MASTER_CONFIG_PATH", filepath.Clean(filepath.Join(mydir, "../../../test/conf.yaml")))
	}
	config.Init()
}

func TestDo(t *testing.T) {
	redis := GetRedisConnPoolWrap(constants.CubeMasterServiceID, config.GetConfig().RedisConf)
	_, err := redis.Do("SET", "test", "test")
	assert.NotNil(t, err)
}

func TestDoType(t *testing.T) {
	redis := GetRedis()
	assert.NotNil(t, redis)
	_, err := redis.Do("HGETALL", "test", "test")
	assert.NotNil(t, err)

	_, err = redis.Do("GET", "test", "test")
	assert.NotNil(t, err)

	_, err = redis.Do("SET", "test", "test")
	assert.NotNil(t, err)
}
