// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package log

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

func TestDefaultLogger(t *testing.T) {
	ctx := context.Background()
	assert.NotNil(t, GetLogger(ctx))
}

func TestWithLogger(t *testing.T) {
	ctx := context.Background()
	logger := NewWrapperLogEntry(CubeLog.WithContext(ctx))
	ctx = WithLogger(ctx, logger)
	if GetLogger(ctx) != logger {
		t.Errorf("Expected logger to be %v, but got %v", logger, GetLogger(ctx))
	}
}

func TestReNewLoggerDefault(t *testing.T) {
	ctx := context.Background()
	assert.NotNil(t, ReNewLogger(ctx))
}

func TestReNewLogger(t *testing.T) {
	ctx := context.Background()
	logger := NewWrapperLogEntry(CubeLog.WithContext(ctx))
	ctx = WithLogger(ctx, logger)
	ctx = ReNewLogger(ctx)
	if GetLogger(ctx) == logger {
		t.Errorf("Expected logger to be different from %v, but got %v", logger, GetLogger(ctx))
	}
}
