// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package score

import (
	"context"
	"errors"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	fwk "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/framework"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/ret"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
)

const (
	mb                    int64 = 1024 * 1024
	minThreshold          int64 = 23 * mb
	maxContainerThreshold int64 = 80000 * mb
)

type imageScore struct {
	weight float64
}

var getImageStateByNode = localcache.GetImageStateByNode

func NewImageScore() *imageScore {
	if config.GetConfig().Scheduler.Score.ScorePluginConf.ImageScore == nil {
		panic("config.Scheduler.Score.ScorePluginConf.ImageScore is nil")
	}
	return &imageScore{
		weight: config.GetConfig().Scheduler.Score.ScorePluginConf.ImageScore.Weight,
	}
}

func (l *imageScore) ID() string {
	return constants.SelectorScoreID + "/" + "image_score"
}

func (l *imageScore) String() string {
	return l.ID()
}

func (l *imageScore) Weight() float64 {
	return l.weight
}
func (l *imageScore) Disable() bool {
	return config.GetConfig().Scheduler.Score.ScorePluginConf.ImageScore.Disable
}

func (l *imageScore) Select(selCtx *selctx.SelectorCtx) (nodes node.NodeScoreList,
	err error) {
	defer func() {
		if r := recover(); r != nil {
			err = ret.Errorf(errorcode.ErrorCode_MasterInternalError, "imageScore panic:%s", r)
		}
	}()

	sconf := config.GetConfig().Scheduler
	if sconf == nil || sconf.Score == nil || sconf.Score.ScorePluginConf.ImageScore == nil {
		return nodes, nil
	}

	if l.Disable() {
		return nil, nil
	}

	if selCtx.ReqRes == nil {
		return nil, nil
	}
	totalWeight, err := getImageScoreTotalWeight()
	if err != nil || totalWeight == 0 {
		return nodes, nil
	}

	inList := selCtx.Nodes()
	nodes = make(node.NodeScoreList, 0, inList.Len())
	for i := range inList {
		nscore := getImageWeightedAverageScore(selCtx.Ctx, selCtx.ReqRes, inList[i]) / totalWeight
		nodes.Append(&node.NodeScore{
			InsID:    inList[i].ID(),
			Score:    nscore,
			MvmNum:   inList[i].MvmNum,
			OrigNode: inList[i],
		})
	}

	return nodes, nil
}
func getImageScoreTotalWeight() (float64, error) {
	sconf := config.GetConfig().Scheduler.Score.ScorePluginConf.ImageScore
	if sconf == nil {
		return 0, errors.New("ImageScore conf is nil")
	}
	w := float64(0)
	for _, v := range sconf.EnableWeightFactors {
		w += getFactorWeight(v)
	}
	return w, nil
}

func getImageWeightedAverageScore(ctx context.Context, res *selctx.RequestResource, nodeInfo *node.Node) float64 {
	sconf := config.GetConfig().Scheduler.Score.ScorePluginConf.ImageScore
	if sconf == nil || res == nil || nodeInfo == nil {
		return 0
	}

	scores := float64(0)
	for _, v := range sconf.EnableWeightFactors {
		switch v {
		case constants.WeightFactorImageID:
			scores += getImageScore(ctx, res.ErofsImages, nodeInfo) * getFactorWeight(v)
		case constants.WeightFactorTemplateID:
			scores += getTemplateScore(ctx, res.TemplateID, nodeInfo) * getFactorWeight(v)
		}
	}
	return scores
}
func getImageScore(ctx context.Context, images []*selctx.ImageSpec, nodeInfo *node.Node) float64 {
	_ = ctx
	if images == nil || nodeInfo == nil {
		return 0
	}
	imageScores := sumImageScores(nodeInfo, images)
	score := calculatePriority(imageScores, len(images))

	return float64(score)
}

func getTemplateScore(ctx context.Context, templateID string, nodeInfo *node.Node) float64 {
	_ = ctx
	if templateID == "" || nodeInfo == nil {
		return 0
	}
	imageScores := sumTemplateScores(nodeInfo, templateID)
	score := calculatePriority(imageScores, 1)
	return float64(score)
}

func calculatePriority(sumScores int64, numContainers int) int64 {
	maxThreshold := maxContainerThreshold * int64(numContainers)
	if sumScores < minThreshold {
		sumScores = minThreshold
	} else if sumScores > maxThreshold {
		sumScores = maxThreshold
	}

	return fwk.MaxNodeScore * (sumScores - minThreshold) / (maxThreshold - minThreshold)
}

func sumImageScores(nodeInfo *node.Node, images []*selctx.ImageSpec) int64 {
	var sum int64 = 0
	for _, image := range images {
		if state := getImageStateByNode(image.ImageID, nodeInfo.ID()); state != nil {
			sum += int64(state.ScaledImageScore)
		}
	}
	return sum
}

func sumTemplateScores(nodeInfo *node.Node, templateID string) int64 {
	var sum int64 = 0
	if state := getImageStateByNode(templateID, nodeInfo.ID()); state != nil {
		sum += int64(state.ScaledImageScore)
	}
	return sum
}
