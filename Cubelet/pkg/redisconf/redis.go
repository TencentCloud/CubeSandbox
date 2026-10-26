// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package redisconf

import (
	"context"
	"crypto/md5"
	"fmt"
	"time"

	"github.com/marspere/goencrypt"
	"github.com/pelletier/go-toml"
	"github.com/redis/go-redis/v9"
)

type RedisObj struct {
	RedisHost string `json:"ip"`
	RedisPort int    `json:"port"`
	RedisAuth string `json:"auth"`
}

func (r *RedisObj) InitRedis() (*redis.Client, error) {
	redisAddr := fmt.Sprintf("%s:%d", r.RedisHost, r.RedisPort)

	rdb := redis.NewClient(&redis.Options{
		Addr:     redisAddr,
		Password: r.RedisAuth,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_, err := rdb.Ping(ctx).Result()
	if err != nil {
		return nil, err
	}

	return rdb, nil
}

func GetCipher(key string) (*goencrypt.CipherAES, error) {
	cryptoKey := fmt.Sprintf("%x", md5.Sum([]byte(key)))
	cipher, err := goencrypt.NewAESCipher([]byte(cryptoKey), []byte(cryptoKey[:16]),
		goencrypt.CBCMode, goencrypt.PkcsZero, goencrypt.PrintBase64)
	if err != nil {
		return nil, fmt.Errorf("init AESCipher for [%s] err", key)
	}
	return cipher, nil
}

func GetRedisConf(path string) (*RedisObj, error) {
	redisConf, err := toml.LoadFile(path)
	if err != nil {
		return nil, fmt.Errorf("LoadFile failed: %v", err)
	}

	redisObj := &RedisObj{
		RedisHost: redisConf.Get("redis.ip").(string),
		RedisPort: int(redisConf.Get("redis.port").(int64)),
	}

	cipher, err := GetCipher(redisObj.RedisHost)
	if err != nil {
		return nil, fmt.Errorf("GetCipher failed: %v", err)
	}

	cipherText := redisConf.Get("redis.auth").(string)
	plainText, err := cipher.AESDecrypt(cipherText)
	if err != nil {
		return nil, fmt.Errorf("AESDecrypt failed: %v", err)
	}
	redisObj.RedisAuth = plainText

	return redisObj, nil
}
