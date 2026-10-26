// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package log provides log
package log

import "gopkg.in/yaml.v3"

type Conf struct {
	Region          string `yaml:"region"`
	Cluster         string `yaml:"cluster"`
	Module          string `yaml:"module"`
	Path            string `yaml:"path"`
	FileSize        int    `yaml:"file_size"`
	FileNum         int    `yaml:"file_num"`
	Level           string `yaml:"level"`
	EnableLogMetric bool   `yaml:"enable_log_metric"`
}

// UnmarshalYAML accepts the snake_case keys used by the shipped configuration
// and the camelCase keys used by older deployments. When both spellings are
// present, the shipped snake_case spelling takes precedence.
func (c *Conf) UnmarshalYAML(value *yaml.Node) error {
	type plain Conf
	if err := value.Decode((*plain)(c)); err != nil {
		return err
	}

	var keys struct {
		FileSize              *int  `yaml:"file_size"`
		FileNum               *int  `yaml:"file_num"`
		EnableLogMetric       *bool `yaml:"enable_log_metric"`
		LegacyFileSize        *int  `yaml:"fileSize"`
		LegacyFileNum         *int  `yaml:"fileNum"`
		LegacyEnableLogMetric *bool `yaml:"enableLogMetric"`
	}
	if err := value.Decode(&keys); err != nil {
		return err
	}

	if keys.FileSize == nil && keys.LegacyFileSize != nil {
		c.FileSize = *keys.LegacyFileSize
	}
	if keys.FileNum == nil && keys.LegacyFileNum != nil {
		c.FileNum = *keys.LegacyFileNum
	}
	if keys.EnableLogMetric == nil && keys.LegacyEnableLogMetric != nil {
		c.EnableLogMetric = *keys.LegacyEnableLogMetric
	}
	return nil
}
