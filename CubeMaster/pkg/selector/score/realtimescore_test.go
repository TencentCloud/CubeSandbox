// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package score

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestRealTimeScore(t *testing.T) {
	mt := time.Now().UnixMilli()
	lt := time.Now().UnixMilli()
	now := time.Now().UnixMilli()
	t.Logf("metricUpdateDiff: %f", getReciprocal(mt, now))
	t.Logf("metricLocalUpdateDiff: %f", getReciprocal(lt, now))
	time.Sleep(time.Second)
	mt = time.Now().Add(-time.Second * 10).UnixMilli()
	lt = time.Now().Add(-time.Second * 10).UnixMilli()
	now = time.Now().UnixMilli()
	t.Logf("metricUpdateDiff: %f", getReciprocal(mt, now))
	t.Logf("metricLocalUpdateDiff: %f", getReciprocal(lt, now))
	time.Sleep(time.Second)
	mt = time.Now().Add(-time.Hour).Unix()
	lt = time.Now().Add(-time.Hour).Unix()
	now = time.Now().Unix()
	t.Logf("metricUpdateDiff: %f", getReciprocal(mt, now))
	t.Logf("metricLocalUpdateDiff: %f", getReciprocal(lt, now))
	limitcnt := int64(math.Ceil(float64(30) * 0.9))
	t.Logf("limitcnt: %d", limitcnt)
	assert.Equal(t, int64(27), limitcnt)

	totalLimitCreate := int64(30 * 30)
	newHealthyNodes := int64(30)
	newMasterNodes := int64(3)
	newCreateLimitOfEveryNode := int64(math.Ceil(float64(totalLimitCreate * 1.0 / newHealthyNodes)))
	limitCreate := int64(math.Ceil(float64(newHealthyNodes * newCreateLimitOfEveryNode * 1.0 / newMasterNodes)))
	t.Logf("limitCreate: %d", limitCreate)
	assert.Equal(t, int64(300), limitCreate)
}
