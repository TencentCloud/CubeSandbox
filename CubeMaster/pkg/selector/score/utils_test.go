// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package score

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClampFactorScore(t *testing.T) {
	assert.Equal(t, 0.0, clampFactorScore(-1))
	assert.Equal(t, 0.0, clampFactorScore(-100.5))
	assert.Equal(t, 0.0, clampFactorScore(0))
	assert.Equal(t, 62.5, clampFactorScore(62.5))
	assert.Equal(t, 100.0, clampFactorScore(100))
	assert.Equal(t, 100.0, clampFactorScore(250))
}
