// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package score

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"text/tabwriter"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/recov"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

// loopAsyncScore periodically computes the multi-factor weighted average score
// for every node in the local cache and writes it back to the node's Score
// field (async scoring, read directly by the multi_factor plugin). The
// interval is controlled by MultiFactorWeightedAverage.ScoreInterval.
func loopAsyncScore(ctx context.Context) {
	cfg := config.GetConfig().Scheduler.Score.ScorePluginConf.MultiFactorWeightedAverage
	if cfg == nil {
		return
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	var w *tabwriter.Writer
	if config.GetConfig().Common.MockDebug {
		reqLogWriter := CubeLog.NewRollFileWriter(config.GetLogConfig().Path, "score", 10, 200)
		w = tabwriter.NewWriter(reqLogWriter, 4, 8, 4, ' ', 0)
	}

	checkDeadline := time.Now().Add(cfg.ScoreInterval)
	mockDeadline := time.Now().Add(time.Second)
	for {
		select {
		case <-ticker.C:
			recov.WithRecover(func() {
				if checkDeadline.After(time.Now()) {

					return
				}
				defer func() {
					checkDeadline = time.Now().Add(cfg.ScoreInterval)
				}()

				sconf := config.GetConfig().Scheduler
				if sconf == nil || sconf.Score == nil || sconf.Score.ResourceWeights == nil {
					return
				}

				totalWeight, err := getAsyncMultiFactorTotalWeight()
				if err != nil || totalWeight == 0 {
					return
				}

				elems := localcache.GetCacheItems()
				for _, v := range elems {
					n, ok := v.Object.(*node.Node)
					if ok {

						n.Score = getMultiFactorWeightedAverageScore(n) / totalWeight
					}
				}
				if config.GetConfig().Common.MockDebug {
					if mockDeadline.After(time.Now()) {
						return
					}
					printAsyncScores(w)
					mockDeadline = time.Now().Add(time.Second)
				}
			}, func(panicError interface{}) {
				checkDeadline = time.Now().Add(cfg.ScoreInterval)
				log.G(ctx).Fatalf("loopAsyncScore panic:%v", string(debug.Stack()))
			})
		case <-ctx.Done():
			return
		}
	}
}

// printAsyncScores is a debug helper: writes the healthy nodes' multi-factor
// scores into tabwriter output (MockDebug mode).
func printAsyncScores(w *tabwriter.Writer) {
	if w == nil {
		return
	}
	nodes := node.NodeScoreList{}
	for _, n := range localcache.GetHealthyNodes(-1) {
		nodes.Append(&node.NodeScore{
			InsID:    n.ID(),
			Score:    n.Score,
			MvmNum:   n.MvmNum,
			OrigNode: n,
		})
	}
	nodes.AllSortByScore()
	fmt.Fprintln(w, "host_id\tmvm_nun\tmulti_score")
	for _, v := range nodes {
		fmt.Fprintf(w, "%s\t%d\t%f\n", v.ID(), v.MvmNum, v.Score)
	}
	w.Flush()
}

// getAsyncMultiFactorTotalWeight sums the weights of the enabled multi-factor
// async scoring factors.
func getAsyncMultiFactorTotalWeight() (float64, error) {
	sconf := config.GetConfig().Scheduler.Score.ScorePluginConf.MultiFactorWeightedAverage
	if sconf == nil {
		return 0, errors.New("AsyncMultiFactor conf is nil")
	}
	w := float64(0)
	for _, v := range sconf.EnableWeightFactors {
		w += getFactorWeight(v)
	}
	return w, nil
}

// getMultiFactorWeightedAverageScore computes a node's multi-factor weighted
// average score (unnormalized).
func getMultiFactorWeightedAverageScore(n *node.Node) float64 {
	sconf := config.GetConfig().Scheduler.Score.ScorePluginConf.MultiFactorWeightedAverage
	if sconf == nil {
		return 0
	}
	scores := getFactorWeightedAverageScore(n, sconf.EnableWeightFactors)
	return scores
}
