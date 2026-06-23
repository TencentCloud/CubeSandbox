package prefilter

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
)

func init() {
	mydir, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	if os.Getenv("CUBE_MASTER_CONFIG_PATH") == "" {
		os.Setenv("CUBE_MASTER_CONFIG_PATH", filepath.Clean(filepath.Join(mydir, "../../../conf.yaml")))
	}
	if _, err := config.Init(); err != nil {
		panic(err)
	}
}

func TestPreFilterExcludesUnhealthyNode(t *testing.T) {
	now := time.Now()
	fresh := &node.Node{
		InsID:               "node-fresh",
		IP:                  "10.0.0.1",
		Healthy:             true,
		MetaDataUpdateAt:    now,
		MetricUpdate:        now,
		MetricLocalUpdateAt: now,
	}
	stale := &node.Node{
		InsID:               "node-stale",
		IP:                  "10.0.0.2",
		Healthy:             false,
		UnhealthyReason:     "HeartbeatExpired",
		MetaDataUpdateAt:    now,
		MetricUpdate:        now,
		MetricLocalUpdateAt: now,
	}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(localcache.GetHealthyNodesByInstanceType, func(n int, product string) node.NodeList {
		return node.NodeList{fresh, stale}
	})

	got, err := NewPreFilter().Select(&selctx.SelectorCtx{
		Ctx:          context.Background(),
		InstanceType: "valid",
	})
	if err != nil {
		t.Fatalf("Select returned error: %v", err)
	}
	if got.Len() != 1 {
		t.Fatalf("got %d nodes want 1", got.Len())
	}
	if got[0].ID() != fresh.ID() {
		t.Fatalf("got node %s want %s", got[0].ID(), fresh.ID())
	}
}

func TestPreFilterExcludesIsolatedNode(t *testing.T) {
	now := time.Now()
	schedulable := &node.Node{
		InsID:               "node-ok",
		IP:                  "10.0.0.1",
		Healthy:             true,
		MetaDataUpdateAt:    now,
		MetricUpdate:        now,
		MetricLocalUpdateAt: now,
	}
	isolated := &node.Node{
		InsID:               "node-isolated",
		IP:                  "10.0.0.2",
		Healthy:             true,
		Isolated:            true,
		MetaDataUpdateAt:    now,
		MetricUpdate:        now,
		MetricLocalUpdateAt: now,
	}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	// Return the isolated node from the cache as defense-in-depth coverage: even
	// if the read-path filter were bypassed, prefilter must drop it.
	patches.ApplyFunc(localcache.GetHealthyNodesByInstanceType, func(n int, product string) node.NodeList {
		return node.NodeList{schedulable, isolated}
	})

	got, err := NewPreFilter().Select(&selctx.SelectorCtx{
		Ctx:          context.Background(),
		InstanceType: "valid",
	})
	if err != nil {
		t.Fatalf("Select returned error: %v", err)
	}
	if got.Len() != 1 {
		t.Fatalf("got %d nodes want 1", got.Len())
	}
	if got[0].ID() != schedulable.ID() {
		t.Fatalf("got node %s want %s", got[0].ID(), schedulable.ID())
	}
}

func TestPreFilterExcludesMetricTimeoutNode(t *testing.T) {
	now := time.Now()
	timeout := config.GetConfig().Scheduler.MetricUpdateTimeout
	fresh := &node.Node{
		InsID:               "node-fresh",
		IP:                  "10.0.0.1",
		Healthy:             true,
		MetaDataUpdateAt:    now,
		MetricUpdate:        now,
		MetricLocalUpdateAt: now,
	}
	staleMetric := &node.Node{
		InsID:               "node-stale-metric",
		IP:                  "10.0.0.2",
		Healthy:             true,
		MetaDataUpdateAt:    now,
		MetricUpdate:        now.Add(-(timeout + time.Second)),
		MetricLocalUpdateAt: now.Add(-(timeout + time.Second)),
	}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(localcache.GetHealthyNodesByInstanceType, func(n int, product string) node.NodeList {
		return node.NodeList{fresh, staleMetric}
	})

	got, err := NewPreFilter().Select(&selctx.SelectorCtx{
		Ctx:          context.Background(),
		InstanceType: "valid",
	})
	if err != nil {
		t.Fatalf("Select returned error: %v", err)
	}
	if got.Len() != 1 {
		t.Fatalf("got %d nodes want 1", got.Len())
	}
	if got[0].ID() != fresh.ID() {
		t.Fatalf("got node %s want %s", got[0].ID(), fresh.ID())
	}
}
