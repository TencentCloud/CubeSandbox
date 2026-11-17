// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package CubeLog

import (
	"fmt"
	"os"
	"path/filepath"

	jsoniter "github.com/json-iterator/go"
)

// export type Fields
type Fields map[string]interface{}

type Net string

const (
	CloudVpc     Net = "CloudVpc"
	CloudSupport Net = "CloudSupport"
)

type Config struct {
	Net Net

	Path string

	Count int

	Size int

	AsyncFlush string
	asyncFlush bool
}

func Init(cfg Config) {
	config = cfg

	if cfg.Net == CloudVpc {
		if cfg.Path == "" {
			dir, err := filepath.Abs(filepath.Dir(os.Args[0]))
			if err != nil {
				fmt.Println(err)
				panic(err.Error())
			}
			config.Path = dir
			if err := os.MkdirAll(config.Path, 0755); err != nil {
				panic(err)
			}
		}

		return
	}

	config.asyncFlush = false
	if cfg.AsyncFlush == "true" {
		config.asyncFlush = true
	}
}

// SetAsyncFlush enables the non-blocking log queue. When the queue is full,
// new lines are dropped. CubeMaster calls this from its own config loader;
// CubeLog.Init is not on that path, so the AsyncFlush field of Init alone
// never reaches a running master.
func SetAsyncFlush(enabled bool) {
	config.asyncFlush = enabled
}

type RemoteConfig struct {
	EnableLocal string
	enablelocal bool

	RetmoteLogAddr string
	RetmoteLogPort int

	ReqTimeout int
}

var config Config

func GetLoggerByName(name string) *Logger {

	if name == "" || name == "Trace" {
		return GetLogger(name)
	}

	logger := GetLogger(name)
	logger.SetFileRoller(config.Path, config.Count, config.Size)
	return logger
}

var jsonCodec = jsoniter.Config{
	ValidateJsonRawMessage:  true,
	MarshalFloatWith6Digits: true,
}.Froze()
