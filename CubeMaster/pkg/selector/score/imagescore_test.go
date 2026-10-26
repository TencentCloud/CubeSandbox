// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package score

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	fwk "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/framework"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
)

func stubImageStateLookup(t *testing.T, lookup func(string, string) *fwk.ImageStateSummary) {
	t.Helper()
	original := getImageStateByNode
	getImageStateByNode = lookup
	t.Cleanup(func() { getImageStateByNode = original })
}

func missingImageState(string, string) *fwk.ImageStateSummary { return nil }

func imageState(score int64) *fwk.ImageStateSummary {
	return &fwk.ImageStateSummary{ScaledImageScore: score}
}

func TestNewImageScore(t *testing.T) {
	t.Run("正常创建imageScore实例", func(t *testing.T) {

		originalConfig := config.GetConfig().Scheduler.Score.ScorePluginConf.ImageScore
		defer func() {
			config.GetConfig().Scheduler.Score.ScorePluginConf.ImageScore = originalConfig
		}()

		config.GetConfig().Scheduler.Score.ScorePluginConf.ImageScore = &config.ImageScore{
			Weight:              1.0,
			EnableWeightFactors: []string{"image_id", "template_id"},
			Disable:             false,
		}

		score := NewImageScore()
		assert.NotNil(t, score)
		assert.Equal(t, "Score/image_score", score.ID())
		assert.Equal(t, "Score/image_score", score.String())
		assert.Equal(t, 1.0, score.Weight())
		assert.False(t, score.Disable())
	})

	t.Run("配置为空时panic", func(t *testing.T) {

		originalConfig := config.GetConfig().Scheduler.Score.ScorePluginConf.ImageScore
		defer func() {
			config.GetConfig().Scheduler.Score.ScorePluginConf.ImageScore = originalConfig
		}()

		config.GetConfig().Scheduler.Score.ScorePluginConf.ImageScore = nil

		assert.Panics(t, func() {
			NewImageScore()
		})
	})
}

func TestGetImageScoreTotalWeight(t *testing.T) {
	t.Run("配置为空时返回错误", func(t *testing.T) {
		originalConfig := config.GetConfig().Scheduler.Score.ScorePluginConf.ImageScore
		defer func() {
			config.GetConfig().Scheduler.Score.ScorePluginConf.ImageScore = originalConfig
		}()

		config.GetConfig().Scheduler.Score.ScorePluginConf.ImageScore = nil

		weight, err := getImageScoreTotalWeight()
		assert.Error(t, err)
		assert.Equal(t, 0.0, weight)
	})

	t.Run("正常计算总权重", func(t *testing.T) {
		originalConfig := config.GetConfig().Scheduler.Score.ScorePluginConf.ImageScore
		originalWeights := config.GetConfig().Scheduler.Score.ResourceWeights
		defer func() {
			config.GetConfig().Scheduler.Score.ScorePluginConf.ImageScore = originalConfig
			config.GetConfig().Scheduler.Score.ResourceWeights = originalWeights
		}()

		config.GetConfig().Scheduler.Score.ResourceWeights = map[string]float64{
			constants.WeightFactorImageID:    0.6,
			constants.WeightFactorTemplateID: 0.4,
		}
		config.GetConfig().Scheduler.Score.ScorePluginConf.ImageScore = &config.ImageScore{
			EnableWeightFactors: []string{"image_id", "template_id"},
		}

		weight, err := getImageScoreTotalWeight()
		assert.NoError(t, err)
		assert.Equal(t, 1.0, weight)
	})
}

func TestGetImageWeightedAverageScore(t *testing.T) {

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stubImageStateLookup(t, missingImageState)

	t.Run("配置为空时返回0", func(t *testing.T) {
		originalConfig := config.GetConfig().Scheduler.Score.ScorePluginConf.ImageScore
		defer func() {
			config.GetConfig().Scheduler.Score.ScorePluginConf.ImageScore = originalConfig
		}()

		config.GetConfig().Scheduler.Score.ScorePluginConf.ImageScore = nil

		score := getImageWeightedAverageScore(ctx, nil, nil)
		assert.Equal(t, 0.0, score)
	})

	t.Run("只启用image_id权重因子", func(t *testing.T) {
		stubImageStateLookup(t, func(imageID, nodeID string) *fwk.ImageStateSummary {
			assert.Equal(t, "nginx:latest", imageID)
			assert.Equal(t, "node-1", nodeID)
			return imageState(40000 * mb)
		})
		originalConfig := config.GetConfig().Scheduler.Score.ScorePluginConf.ImageScore
		originalWeights := config.GetConfig().Scheduler.Score.ResourceWeights
		defer func() {
			config.GetConfig().Scheduler.Score.ScorePluginConf.ImageScore = originalConfig
			config.GetConfig().Scheduler.Score.ResourceWeights = originalWeights
		}()

		config.GetConfig().Scheduler.Score.ResourceWeights = map[string]float64{
			constants.WeightFactorImageID: 0.8,
		}
		config.GetConfig().Scheduler.Score.ScorePluginConf.ImageScore = &config.ImageScore{
			EnableWeightFactors: []string{"image_id"},
		}

		res := &selctx.RequestResource{
			ErofsImages: []*selctx.ImageSpec{
				{ImageID: "nginx:latest"},
			},
		}
		nodeInfo := &node.Node{InsID: "node-1"}

		score := getImageWeightedAverageScore(ctx, res, nodeInfo)
		assert.Equal(t, float64(calculatePriority(40000*mb, 1))*0.8, score)
		assert.Positive(t, score)
	})
}

func TestGetImageScore(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stubImageStateLookup(t, missingImageState)
	t.Run("空参数返回0", func(t *testing.T) {
		score := getImageScore(ctx, nil, nil)
		assert.Equal(t, 0.0, score)
	})

	t.Run("正常计算镜像分数", func(t *testing.T) {
		stubImageStateLookup(t, func(imageID, nodeID string) *fwk.ImageStateSummary {
			assert.Equal(t, "node-1", nodeID)
			scores := map[string]int64{
				"nginx:latest": 60000 * mb,
				"redis:latest": 40000 * mb,
			}
			return imageState(scores[imageID])
		})
		images := []*selctx.ImageSpec{
			{ImageID: "nginx:latest"},
			{ImageID: "redis:latest"},
		}
		nodeInfo := &node.Node{InsID: "node-1"}

		score := getImageScore(ctx, images, nodeInfo)
		assert.Equal(t, float64(calculatePriority(100000*mb, 2)), score)
		assert.Positive(t, score)
	})
}

func TestGetTemplateScore(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stubImageStateLookup(t, missingImageState)
	t.Run("空参数返回0", func(t *testing.T) {
		score := getTemplateScore(ctx, "", nil)
		assert.Equal(t, 0.0, score)
	})

	t.Run("正常计算模板分数", func(t *testing.T) {
		stubImageStateLookup(t, func(templateID, nodeID string) *fwk.ImageStateSummary {
			assert.Equal(t, "template-123", templateID)
			assert.Equal(t, "node-1", nodeID)
			return imageState(40000 * mb)
		})
		templateID := "template-123"
		nodeInfo := &node.Node{InsID: "node-1"}

		score := getTemplateScore(ctx, templateID, nodeInfo)
		assert.Equal(t, float64(calculatePriority(40000*mb, 1)), score)
		assert.Positive(t, score)
	})
}

func TestCalculatePriority(t *testing.T) {
	testCases := []struct {
		name          string
		sumScores     int64
		numContainers int
		expectedScore int64
	}{
		{
			name:          "分数低于最小值时使用最小值",
			sumScores:     10 * 1024 * 1024,
			numContainers: 1,
			expectedScore: 0,
		},
		{
			name:          "分数在范围内时正常计算",
			sumScores:     40000 * 1024 * 1024,
			numContainers: 1,
			expectedScore: 49,
		},
		{
			name:          "分数超过最大值时使用最大值",
			sumScores:     90000 * 1024 * 1024,
			numContainers: 1,
			expectedScore: fwk.MaxNodeScore,
		},
		{
			name:          "多容器时调整最大阈值",
			sumScores:     100000 * 1024 * 1024,
			numContainers: 2,
			expectedScore: 62,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			score := calculatePriority(tc.sumScores, tc.numContainers)
			assert.Equal(t, tc.expectedScore, score)
		})
	}
}

func TestSumImageScores(t *testing.T) {
	stubImageStateLookup(t, missingImageState)
	t.Run("累加节点上的镜像状态分数", func(t *testing.T) {
		stubImageStateLookup(t, func(imageID, nodeID string) *fwk.ImageStateSummary {
			assert.Equal(t, "node-1", nodeID)
			return map[string]*fwk.ImageStateSummary{
				"nginx:latest": imageState(300 * mb),
				"redis:latest": imageState(200 * mb),
			}[imageID]
		})

		sum := sumImageScores(&node.Node{InsID: "node-1"}, []*selctx.ImageSpec{
			{ImageID: "nginx:latest"},
			{ImageID: "redis:latest"},
		})
		assert.Equal(t, int64(500*mb), sum)
	})

	t.Run("镜像状态为空时返回0", func(t *testing.T) {
		nodeInfo := &node.Node{}
		images := []*selctx.ImageSpec{
			{ImageID: "nginx:latest"},
			{ImageID: "redis:latest"},
		}

		sum := sumImageScores(nodeInfo, images)
		assert.Equal(t, int64(0), sum)
	})

	t.Run("空镜像列表返回0", func(t *testing.T) {
		nodeInfo := &node.Node{}
		var images []*selctx.ImageSpec

		sum := sumImageScores(nodeInfo, images)
		assert.Equal(t, int64(0), sum)
	})
}

func TestSumTemplateScores(t *testing.T) {
	stubImageStateLookup(t, missingImageState)
	t.Run("读取节点上的模板状态分数", func(t *testing.T) {
		stubImageStateLookup(t, func(templateID, nodeID string) *fwk.ImageStateSummary {
			assert.Equal(t, "template-123", templateID)
			assert.Equal(t, "node-1", nodeID)
			return imageState(500 * mb)
		})

		sum := sumTemplateScores(&node.Node{InsID: "node-1"}, "template-123")
		assert.Equal(t, int64(500*mb), sum)
	})

	t.Run("模板状态为空时返回0", func(t *testing.T) {
		nodeInfo := &node.Node{}
		templateID := "template-123"

		sum := sumTemplateScores(nodeInfo, templateID)
		assert.Equal(t, int64(0), sum)
	})

	t.Run("空模板ID返回0", func(t *testing.T) {
		nodeInfo := &node.Node{}
		templateID := ""

		sum := sumTemplateScores(nodeInfo, templateID)
		assert.Equal(t, int64(0), sum)
	})
}

func TestImageScoreSelect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stubImageStateLookup(t, missingImageState)

	t.Run("空亲和性配置返回空节点列表", func(t *testing.T) {
		originalConfig := config.GetConfig().Scheduler.Score.ScorePluginConf.ImageScore
		defer func() {
			config.GetConfig().Scheduler.Score.ScorePluginConf.ImageScore = originalConfig
		}()

		config.GetConfig().Scheduler.Score.ScorePluginConf.ImageScore = &config.ImageScore{
			Weight:              1.0,
			EnableWeightFactors: []string{"image_id"},
			Disable:             false,
		}

		score := NewImageScore()
		selCtx := &selctx.SelectorCtx{
			Ctx:    ctx,
			ReqRes: &selctx.RequestResource{},
		}

		nodes, err := score.Select(selCtx)
		assert.NoError(t, err)
		assert.Empty(t, nodes)
	})

	t.Run("panic恢复测试", func(t *testing.T) {
		originalConfig := config.GetConfig().Scheduler.Score.ScorePluginConf.ImageScore
		defer func() {
			config.GetConfig().Scheduler.Score.ScorePluginConf.ImageScore = originalConfig
		}()

		config.GetConfig().Scheduler.Score.ScorePluginConf.ImageScore = &config.ImageScore{
			Weight:              1.0,
			EnableWeightFactors: []string{"image_id"},
			Disable:             false,
		}

		score := NewImageScore()

		selCtx := &selctx.SelectorCtx{
			Ctx:    ctx,
			ReqRes: &selctx.RequestResource{},
		}

		nodes, err := score.Select(selCtx)
		assert.NoError(t, err)
		assert.Empty(t, nodes)
	})

	t.Run("正常计算节点分数 - 镜像亲和性", func(t *testing.T) {
		stubImageStateLookup(t, func(imageID, nodeID string) *fwk.ImageStateSummary {
			if nodeID != "node-1" {
				return nil
			}
			return map[string]*fwk.ImageStateSummary{
				"nginx:latest": imageState(60000 * mb),
				"redis:latest": imageState(40000 * mb),
			}[imageID]
		})
		originalConfig := config.GetConfig().Scheduler.Score.ScorePluginConf.ImageScore
		originalWeights := config.GetConfig().Scheduler.Score.ResourceWeights
		defer func() {
			config.GetConfig().Scheduler.Score.ScorePluginConf.ImageScore = originalConfig
			config.GetConfig().Scheduler.Score.ResourceWeights = originalWeights
		}()

		config.GetConfig().Scheduler.Score.ResourceWeights = map[string]float64{
			constants.WeightFactorImageID: 1.0,
		}
		config.GetConfig().Scheduler.Score.ScorePluginConf.ImageScore = &config.ImageScore{
			Weight:              1.0,
			EnableWeightFactors: []string{"image_id"},
			Disable:             false,
		}

		score := NewImageScore()
		selCtx := &selctx.SelectorCtx{
			Ctx: ctx,
			ReqRes: &selctx.RequestResource{
				ErofsImages: []*selctx.ImageSpec{
					{ImageID: "nginx:latest"},
					{ImageID: "redis:latest"},
				},
			},
		}

		nodeList := node.NodeList{}
		node1 := &node.Node{InsID: "node-1"}
		nodeList = append(nodeList, node1)

		node2 := &node.Node{InsID: "node-2"}
		nodeList = append(nodeList, node2)

		selCtx.SetNodes(nodeList)

		nodes, err := score.Select(selCtx)
		assert.NoError(t, err)
		assert.NotNil(t, nodes)
		assert.Equal(t, 2, nodes.Len())

		assert.Equal(t, "node-1", nodes[0].InsID)
		assert.Equal(t, float64(calculatePriority(100000*mb, 2)), nodes[0].Score)
		assert.Positive(t, nodes[0].Score)
		assert.Equal(t, "node-2", nodes[1].InsID)
		assert.Zero(t, nodes[1].Score)
		assert.Greater(t, nodes[0].Score, nodes[1].Score)
	})

	t.Run("正常计算节点分数 - 模板亲和性", func(t *testing.T) {
		stubImageStateLookup(t, func(templateID, nodeID string) *fwk.ImageStateSummary {
			assert.Equal(t, "template-123", templateID)
			assert.Equal(t, "node-1", nodeID)
			return imageState(40000 * mb)
		})
		originalConfig := config.GetConfig().Scheduler.Score.ScorePluginConf.ImageScore
		originalWeights := config.GetConfig().Scheduler.Score.ResourceWeights
		defer func() {
			config.GetConfig().Scheduler.Score.ScorePluginConf.ImageScore = originalConfig
			config.GetConfig().Scheduler.Score.ResourceWeights = originalWeights
		}()

		config.GetConfig().Scheduler.Score.ResourceWeights = map[string]float64{
			constants.WeightFactorTemplateID: 1.0,
		}
		config.GetConfig().Scheduler.Score.ScorePluginConf.ImageScore = &config.ImageScore{
			Weight:              1.0,
			EnableWeightFactors: []string{"template_id"},
			Disable:             false,
		}

		score := NewImageScore()
		selCtx := &selctx.SelectorCtx{
			Ctx: ctx,
			ReqRes: &selctx.RequestResource{
				TemplateID: "template-123",
			},
		}

		nodeList := node.NodeList{}
		node1 := &node.Node{InsID: "node-1"}
		nodeList = append(nodeList, node1)

		selCtx.SetNodes(nodeList)

		nodes, err := score.Select(selCtx)
		assert.NoError(t, err)
		assert.NotNil(t, nodes)
		assert.Equal(t, 1, nodes.Len())

		nodeScore := nodes[0]
		assert.NotNil(t, nodeScore)
		assert.Equal(t, "node-1", nodeScore.InsID)

		assert.Equal(t, float64(calculatePriority(40000*mb, 1)), nodeScore.Score)
		assert.Positive(t, nodeScore.Score)
	})

	t.Run("多权重因子组合计算", func(t *testing.T) {
		stubImageStateLookup(t, func(id, nodeID string) *fwk.ImageStateSummary {
			assert.Equal(t, "node-1", nodeID)
			return map[string]*fwk.ImageStateSummary{
				"nginx:latest": imageState(50000 * mb),
				"template-123": imageState(30000 * mb),
			}[id]
		})
		originalConfig := config.GetConfig().Scheduler.Score.ScorePluginConf.ImageScore
		originalWeights := config.GetConfig().Scheduler.Score.ResourceWeights
		defer func() {
			config.GetConfig().Scheduler.Score.ScorePluginConf.ImageScore = originalConfig
			config.GetConfig().Scheduler.Score.ResourceWeights = originalWeights
		}()

		config.GetConfig().Scheduler.Score.ResourceWeights = map[string]float64{
			constants.WeightFactorImageID:    0.6,
			constants.WeightFactorTemplateID: 0.4,
		}
		config.GetConfig().Scheduler.Score.ScorePluginConf.ImageScore = &config.ImageScore{
			Weight:              1.0,
			EnableWeightFactors: []string{"image_id", "template_id"},
			Disable:             false,
		}

		score := NewImageScore()
		selCtx := &selctx.SelectorCtx{
			Ctx: ctx,
			ReqRes: &selctx.RequestResource{
				ErofsImages: []*selctx.ImageSpec{
					{ImageID: "nginx:latest"},
				},
				TemplateID: "template-123",
			},
		}

		nodeList := node.NodeList{}
		node1 := &node.Node{InsID: "node-1"}
		nodeList = append(nodeList, node1)

		selCtx.SetNodes(nodeList)

		nodes, err := score.Select(selCtx)
		assert.NoError(t, err)
		assert.NotNil(t, nodes)
		assert.Equal(t, 1, nodes.Len())

		nodeScore := nodes[0]
		assert.NotNil(t, nodeScore)
		assert.Equal(t, "node-1", nodeScore.InsID)

		expected := float64(calculatePriority(50000*mb, 1))*0.6 +
			float64(calculatePriority(30000*mb, 1))*0.4
		assert.Equal(t, expected, nodeScore.Score)
		assert.Positive(t, nodeScore.Score)
	})

	t.Run("禁用imageScore时返回空列表", func(t *testing.T) {
		originalConfig := config.GetConfig().Scheduler.Score.ScorePluginConf.ImageScore
		defer func() {
			config.GetConfig().Scheduler.Score.ScorePluginConf.ImageScore = originalConfig
		}()

		config.GetConfig().Scheduler.Score.ScorePluginConf.ImageScore = &config.ImageScore{
			Weight:              1.0,
			EnableWeightFactors: []string{"image_id"},
			Disable:             true,
		}

		score := NewImageScore()
		selCtx := &selctx.SelectorCtx{
			Ctx: ctx,
			ReqRes: &selctx.RequestResource{
				ErofsImages: []*selctx.ImageSpec{
					{ImageID: "nginx:latest"},
				},
			},
		}

		nodeList := node.NodeList{}
		node1 := &node.Node{InsID: "node-1"}
		nodeList = append(nodeList, node1)

		selCtx.SetNodes(nodeList)

		nodes, err := score.Select(selCtx)
		assert.NoError(t, err)
		assert.Empty(t, nodes)
	})
}
