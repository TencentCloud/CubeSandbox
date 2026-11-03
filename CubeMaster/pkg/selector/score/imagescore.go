// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package score

import (
	"context"
	"errors"
	"fmt"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	fwk "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/framework"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/ret"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

const (
	mb                    int64 = 1024 * 1024
	minThreshold          int64 = 23 * mb    // 镜像总大小低于该值按该值计分（下限保护）
	maxContainerThreshold int64 = 80000 * mb // 单个容器镜像大小超过该值按该值计分（上限保护）
)

// imageScore is the image-locality score plugin: nodes that already cache the
// requested images score higher, so image-ready nodes are preferred and
// post-scheduling image pulls are avoided.
type imageScore struct {
	weight float64
}

// getImageStateByNode looks up an image's state on a node (swappable for tests).
var getImageStateByNode = localcache.GetImageStateByNode

func imageScoreConf() *config.ImageScore {
	sched := config.GetConfig().Scheduler
	if sched == nil || sched.Score == nil {
		return nil
	}
	return sched.Score.ScorePluginConf.ImageScore
}

// NewImageScore tolerates a missing legacy plugin_conf block: the scorer then
// has no weight of its own (a profile entry must carry one) and Select reports
// ErrNotApplicable until the block is configured, because enable_weight_factors
// only exists in the legacy config tree.
func NewImageScore() *imageScore {
	conf := imageScoreConf()
	if conf == nil {
		CubeLog.Warnf("scheduler.score.plugin_conf.image_score is not configured; image_score scores nothing until it is")
		return &imageScore{}
	}
	return &imageScore{
		weight: conf.Weight,
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
	conf := imageScoreConf()
	return conf == nil || conf.Disable
}

// Select computes each node's image score from the enabled factors and
// normalizes it; scoring covers both the request's image list (ImageID) and
// the template ID dimensions.
func (l *imageScore) Select(selCtx *selctx.SelectorCtx) (nodes node.NodeScoreList,
	err error) {
	defer func() {
		if r := recover(); r != nil {
			err = ret.Errorf(errorcode.ErrorCode_MasterInternalError, "imageScore panic:%s", r)
		}
	}()

	sconf := config.GetConfig().Scheduler
	if sconf == nil || sconf.Score == nil || sconf.Score.ScorePluginConf.ImageScore == nil {
		return nil, fmt.Errorf("image_score legacy plugin_conf is not configured: %w", ErrNotApplicable)
	}

	if l.Disable() {
		return nil, fmt.Errorf("image_score is disabled in the legacy plugin_conf: %w", ErrNotApplicable)
	}

	if selCtx.ReqRes == nil {
		return nil, fmt.Errorf("image_score requires the request resource spec: %w", ErrNotApplicable)
	}
	totalWeight, err := getImageScoreTotalWeight()
	if err != nil || totalWeight == 0 {
		return nil, fmt.Errorf("image_score has no enabled factor with a non-zero resource weight: %w", ErrNotApplicable)
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

// getImageScoreTotalWeight sums the weights of the enabled image-score factors.
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

// getImageWeightedAverageScore computes a node's weighted image score over the
// enabled factors.
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

// getImageScore scores by the request's image list: the more requested images
// the node has cached, the higher the score.
func getImageScore(ctx context.Context, images []*selctx.ImageSpec, nodeInfo *node.Node) float64 {
	_ = ctx
	if images == nil || nodeInfo == nil {
		return 0
	}
	imageScores := sumImageScores(nodeInfo, images)
	score := calculatePriority(imageScores, len(images))

	return float64(score)
}

// getTemplateScore scores by template ID: full score when the node has cached
// the template image, 0 otherwise. The template factor is booleanized
// (aligned with the template_local fact exposed to CEL/gRPC plugins) and
// applies to the legacy `enable_scorers: [image_score]` path as well; see the
// upgrade notes in docs/guide/scheduler-plugin.md.
func getTemplateScore(ctx context.Context, templateID string, nodeInfo *node.Node) float64 {
	_ = ctx
	if templateID == "" || nodeInfo == nil {
		return 0
	}
	if getImageStateByNode(templateID, nodeInfo.ID()) != nil {
		return float64(fwk.MaxNodeScore)
	}
	return 0
}

// calculatePriority maps the image score total into the 0~MaxNodeScore range,
// clamped to the minThreshold score at the low end and capped at the top.
func calculatePriority(sumScores int64, numContainers int) int64 {
	maxThreshold := maxContainerThreshold * int64(numContainers)
	if sumScores < minThreshold {
		sumScores = minThreshold
	} else if sumScores > maxThreshold {
		sumScores = maxThreshold
	}

	return fwk.MaxNodeScore * (sumScores - minThreshold) / (maxThreshold - minThreshold)
}

// sumImageScores sums the scores of the requested images cached on the node.
func sumImageScores(nodeInfo *node.Node, images []*selctx.ImageSpec) int64 {
	var sum int64 = 0
	for _, image := range images {
		if state := getImageStateByNode(image.ImageID, nodeInfo.ID()); state != nil {
			sum += int64(state.ScaledImageScore)
		}
	}
	return sum
}
