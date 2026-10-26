// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"google.golang.org/protobuf/types/known/emptypb"
)

func TestVersion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	resp, err := versionClient.Version(ctx, &emptypb.Empty{})
	assert.NoError(t, err)

	assert.NotEmpty(t, resp.Version)
	assert.NotEmpty(t, resp.Revision)
}
