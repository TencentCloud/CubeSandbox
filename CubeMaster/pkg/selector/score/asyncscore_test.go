// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package score

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
)

func init() {
	mydir, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	fmt.Printf("mydir=%s\n", mydir)
	if os.Getenv("CUBE_MASTER_CONFIG_PATH") == "" {
		os.Setenv("CUBE_MASTER_CONFIG_PATH", filepath.Clean(filepath.Join(mydir, "../../../conf.yaml")))
	}
	config.Init()
	if cfg := config.GetConfig(); cfg.Scheduler.Score == nil {
		cfg.Scheduler.Score = &config.SchedulerScoreConf{
			ResourceWeights: map[string]float64{
				constants.WeightFactorMvmNum:            1,
				constants.WeightFactorQuotaCpu:          1,
				constants.WeightFactorQuotaMem:          1,
				constants.WeightFactorCpuUtil:           1,
				constants.WeightFactorMemUsage:          1,
				constants.WeightFactorCpuLoadUsage:      1,
				constants.WeightFactorRealTimeCreateNum: 1,
				constants.WeightFactorLocalCreateNum:    1,
			},
			ScorePluginConf: config.ScorePluginConf{
				MultiFactorWeightedAverage: &config.MultiFactorWeightedAverage{
					EnableWeightFactors: []string{
						constants.WeightFactorMvmNum,
						constants.WeightFactorQuotaCpu,
						constants.WeightFactorQuotaMem,
						constants.WeightFactorCpuUtil,
						constants.WeightFactorMemUsage,
						constants.WeightFactorCpuLoadUsage,
						constants.WeightFactorRealTimeCreateNum,
						constants.WeightFactorLocalCreateNum,
					},
				},
			},
		}
	}
}

func TestGetMultiFactorWeightedAverageScore(t *testing.T) {
	totalWeight, err := getAsyncMultiFactorTotalWeight()
	if err != nil || totalWeight == 0 {
		t.FailNow()
	}
	t.Logf("totalWeight:%v\n", totalWeight)
	samelist := []*node.Node{
		{
			InsID:               "1",
			IP:                  "192.168.105.3",
			CpuTotal:            96,
			MemMBTotal:          393216,
			CreateConcurrentNum: 50,
			MaxMvmLimit:         2000,
			QuotaCpu:            2880000,
			QuotaMem:            983040,
			Score:               0,
			QuotaCpuUsage:       4800,
			QuotaMemUsage:       2401,
			CpuUtil:             0.6875,
			CpuLoadUsage:        0.28,
			MemUsage:            74817,
			MvmNum:              7,
			RealTimeCreateNum:   0,
			LocalCreateNum:      0,
		},
		{
			InsID:               "2",
			IP:                  "192.168.106.16",
			CpuTotal:            96,
			MemMBTotal:          393216,
			CreateConcurrentNum: 50,
			MaxMvmLimit:         2000,
			QuotaCpu:            2880000,
			QuotaMem:            983040,
			Score:               0,
			QuotaCpuUsage:       4800,
			QuotaMemUsage:       2401,
			CpuUtil:             0.6875,
			CpuLoadUsage:        0.28,
			MemUsage:            74817,

			MvmNum:            7,
			RealTimeCreateNum: 0,
			LocalCreateNum:    0,
		},
	}
	score1 := getMultiFactorWeightedAverageScore(samelist[0]) / totalWeight
	score2 := getMultiFactorWeightedAverageScore(samelist[1]) / totalWeight
	t.Logf("score1:%v, score2:%v\n", score1, score2)
	assert.Equal(t, score1, score2)

	nlist := []*node.Node{
		{
			InsID:               "1",
			IP:                  "192.168.105.3",
			CpuTotal:            96,
			MemMBTotal:          393216,
			CreateConcurrentNum: 50,
			MaxMvmLimit:         2000,
			QuotaCpu:            2880000,
			QuotaMem:            983040,
			Score:               0,
			QuotaCpuUsage:       4800,
			QuotaMemUsage:       2401,
			CpuUtil:             0.6875,
			CpuLoadUsage:        0.28,
			MemUsage:            74817,
			MvmNum:              7,
			RealTimeCreateNum:   1,
			LocalCreateNum:      0,
		},
		{
			InsID:               "2",
			IP:                  "192.168.106.16",
			CpuTotal:            96,
			MemMBTotal:          393216,
			CreateConcurrentNum: 50,
			MaxMvmLimit:         2000,
			QuotaCpu:            2880000,
			QuotaMem:            983040,
			Score:               0,
			QuotaCpuUsage:       25100,
			QuotaMemUsage:       12574,
			CpuUtil:             1.552083,
			CpuLoadUsage:        0.77,
			MemUsage:            78348,
			MvmNum:              28,
			RealTimeCreateNum:   5,
			LocalCreateNum:      0,
		},
	}
	score1 = getMultiFactorWeightedAverageScore(nlist[0]) / totalWeight
	score2 = getMultiFactorWeightedAverageScore(nlist[1]) / totalWeight
	t.Logf("score1:%v, score2:%v\n", score1, score2)
	assert.Greater(t, score1, score2)
}
