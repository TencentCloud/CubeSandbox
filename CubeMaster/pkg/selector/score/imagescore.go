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
	minThreshold          int64 = 23 * mb    // 镜像总大小低于该值按该值计分（下限保护）
	maxContainerThreshold int64 = 80000 * mb // 镜像得分总和的上限基数：总和超过 该值×容器数 时按该上限计分（聚合上限，非单镜像上限）
)

// imageScore 镜像本地化评分插件：
// 节点上已缓存请求镜像时得分更高，优先选择镜像已就绪的节点，避免调度后拉镜像
type imageScore struct {
	weight float64
}

// getImageStateByNode 查询节点上镜像状态的函数（可替换，便于测试）
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

// Select 按启用因子计算每个节点的镜像得分并归一化：
// 支持按请求镜像列表（ImageID）与按模板 ID 两种维度打分
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

// getImageScoreTotalWeight 计算镜像评分启用因子的权重总和
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

// getImageWeightedAverageScore 按启用因子计算节点镜像加权得分
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

// getImageScore 按请求镜像列表给节点打分：已缓存镜像越多得分越高
func getImageScore(ctx context.Context, images []*selctx.ImageSpec, nodeInfo *node.Node) float64 {
	_ = ctx
	if images == nil || nodeInfo == nil {
		return 0
	}
	imageScores := sumImageScores(nodeInfo, images)
	score := calculatePriority(imageScores, len(images))

	return float64(score)
}

// getTemplateScore 按模板 ID 给节点打分：节点已缓存该模板镜像时，按
// 镜像大小 × 集群散布度经 calculatePriority 线性映射计分（未缓存则为 0，见函数注释）
func getTemplateScore(ctx context.Context, templateID string, nodeInfo *node.Node) float64 {
	_ = ctx
	if templateID == "" || nodeInfo == nil {
		return 0
	}
	imageScores := sumTemplateScores(nodeInfo, templateID)
	score := calculatePriority(imageScores, 1)
	return float64(score)
}

// calculatePriority 将镜像总分映射到 0~MaxNodeScore 区间：
// 总分为各已缓存镜像的 Size×集群散布度之和（localcache.scaledImageScore），
// 先截断到 [minThreshold, maxContainerThreshold×numContainers] 再线性映射——
// 缓存命中并非满分：约 5GB 镜像全集群散布仅 ≈6/100，总和接近上限才饱和
func calculatePriority(sumScores int64, numContainers int) int64 {
	maxThreshold := maxContainerThreshold * int64(numContainers)
	if sumScores < minThreshold {
		sumScores = minThreshold
	} else if sumScores > maxThreshold {
		sumScores = maxThreshold
	}

	return fwk.MaxNodeScore * (sumScores - minThreshold) / (maxThreshold - minThreshold)
}

// sumImageScores 汇总节点上已缓存的请求镜像的得分
func sumImageScores(nodeInfo *node.Node, images []*selctx.ImageSpec) int64 {
	var sum int64 = 0
	for _, image := range images {
		if state := getImageStateByNode(image.ImageID, nodeInfo.ID()); state != nil {
			sum += int64(state.ScaledImageScore)
		}
	}
	return sum
}

// sumTemplateScores 取节点上模板镜像的得分（未缓存则为 0）
func sumTemplateScores(nodeInfo *node.Node, templateID string) int64 {
	var sum int64 = 0
	if state := getImageStateByNode(templateID, nodeInfo.ID()); state != nil {
		sum += int64(state.ScaledImageScore)
	}
	return sum
}
