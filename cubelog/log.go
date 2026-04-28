// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package CubeLog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

type stdJSONCodec struct{}

func (stdJSONCodec) Marshal(v interface{}) ([]byte, error) {
	return json.Marshal(v)
}

var jsonCodec = stdJSONCodec{}
