// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package log

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestConfUnmarshalLogRotationSettings(t *testing.T) {
	var conf Conf
	err := yaml.Unmarshal([]byte("file_size: 100\nfile_num: 10\n"), &conf)
	if err != nil {
		t.Fatalf("unmarshal log config: %v", err)
	}

	if conf.FileSize != 100 {
		t.Fatalf("FileSize = %d, want 100", conf.FileSize)
	}
	if conf.FileNum != 10 {
		t.Fatalf("FileNum = %d, want 10", conf.FileNum)
	}
}

func TestConfUnmarshalLegacyLogRotationSettings(t *testing.T) {
	var conf Conf
	err := yaml.Unmarshal([]byte("fileSize: 200\nfileNum: 20\n"), &conf)
	if err != nil {
		t.Fatalf("unmarshal legacy log config: %v", err)
	}

	if conf.FileSize != 200 {
		t.Fatalf("FileSize = %d, want 200", conf.FileSize)
	}
	if conf.FileNum != 20 {
		t.Fatalf("FileNum = %d, want 20", conf.FileNum)
	}
}

func TestConfUnmarshalSnakeCaseTakesPrecedence(t *testing.T) {
	var conf Conf
	err := yaml.Unmarshal([]byte("file_size: 100\nfile_num: 10\nenable_log_metric: false\nfileSize: 200\nfileNum: 20\nenableLogMetric: true\n"), &conf)
	if err != nil {
		t.Fatalf("unmarshal mixed log config: %v", err)
	}

	if conf.FileSize != 100 {
		t.Fatalf("FileSize = %d, want 100", conf.FileSize)
	}
	if conf.FileNum != 10 {
		t.Fatalf("FileNum = %d, want 10", conf.FileNum)
	}
	if conf.EnableLogMetric {
		t.Fatalf("EnableLogMetric = true, want false")
	}
}

func TestConfUnmarshalLegacyLogMetricSetting(t *testing.T) {
	var conf Conf
	err := yaml.Unmarshal([]byte("enableLogMetric: true\n"), &conf)
	if err != nil {
		t.Fatalf("unmarshal legacy log metric config: %v", err)
	}

	if !conf.EnableLogMetric {
		t.Fatalf("EnableLogMetric = false, want true")
	}
}
