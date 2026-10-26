// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package redisconf

import (
	"crypto/md5"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/marspere/goencrypt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetCipher(t *testing.T) {
	key := "testkey"
	expectedCryptoKey := fmt.Sprintf("%x", md5.Sum([]byte(key)))
	expectedCipher, err := goencrypt.NewAESCipher([]byte(expectedCryptoKey), []byte(expectedCryptoKey[:16]),
		goencrypt.CBCMode, goencrypt.PkcsZero, goencrypt.PrintBase64)
	assert.NoError(t, err)

	cipher, err := GetCipher(key)
	assert.NoError(t, err)
	assert.Equal(t, expectedCipher, cipher)
}

func TestGetRedisConf(t *testing.T) {
	testDir := t.TempDir()

	testFile := filepath.Join(testDir, "redisconf_test")
	os.WriteFile(testFile, []byte(`
redis.ip = "192.168.0.1"
redis.port = 6379
redis.auth = "57sLGyqFzYlop8ZCppaFDA=="
`), 0644)

	redisObj, err := GetRedisConf(testFile)
	require.NoError(t, err)
	assert.Equal(t, "192.168.0.1", redisObj.RedisHost)
	assert.Equal(t, 6379, redisObj.RedisPort)
	assert.Equal(t, "test", redisObj.RedisAuth)
}

func TestGetRedisConfWithInvalidAuth(t *testing.T) {
	testDir := t.TempDir()

	testFile := filepath.Join(testDir, "redisconf_test")
	os.WriteFile(testFile, []byte(`
redis.ip = "192.168.0.1"
redis.port = 6379
redis.auth = "57sLGyqFzY"
`), 0644)

	_, err := GetRedisConf(testFile)
	require.Error(t, err)
}
