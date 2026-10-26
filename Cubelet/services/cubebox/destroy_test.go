// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"testing"
	"time"

	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestLifeTimeMeric(t *testing.T) {

	type TestLifeTimeMeric struct {
		StartTime        int64
		FinishTime       int64
		CpuQ             resource.Quantity
		MemQ             resource.Quantity
		ExpectedLifeTime int64
		ExpectedCpu      int64
		ExpectedMem      int64
	}

	lifeTimeMeric := func(startTime int64, finishTime int64, cpuQ resource.Quantity, memQ resource.Quantity) (int64, int64, int64) {
		lifeTime := time.Now().UnixNano() - startTime
		if finishTime != 0 {
			lifeTime = finishTime - startTime
		}
		lifeTime /= 1e9
		cpuCoreSeconds := cpuQ.Value() * lifeTime
		memGigaBytesSeconds := memQ.Value() / 1024 / 1024 / 1024 * lifeTime
		return lifeTime, cpuCoreSeconds, memGigaBytesSeconds
	}

	tests := []TestLifeTimeMeric{
		{
			StartTime:        time.Now().UnixNano() - 10*time.Second.Nanoseconds(),
			FinishTime:       0,
			CpuQ:             resource.MustParse("2"),
			MemQ:             resource.MustParse("4Gi"),
			ExpectedLifeTime: 10,
			ExpectedCpu:      20,
			ExpectedMem:      40,
		},
		{
			StartTime:        time.Now().UnixNano() - 5*time.Second.Nanoseconds(),
			FinishTime:       time.Now().UnixNano() - 1*time.Second.Nanoseconds(),
			CpuQ:             resource.MustParse("1"),
			MemQ:             resource.MustParse("2Gi"),
			ExpectedLifeTime: 4,
			ExpectedCpu:      4,
			ExpectedMem:      8,
		},
	}
	for _, test := range tests {
		lifeTime, cpuCoreSeconds, memGigaBytesSeconds := lifeTimeMeric(test.StartTime, test.FinishTime, test.CpuQ, test.MemQ)
		if lifeTime != test.ExpectedLifeTime || cpuCoreSeconds != test.ExpectedCpu || memGigaBytesSeconds != test.ExpectedMem {
			t.Errorf("Test failed: got (%d, %d, %d), want (%d, %d, %d)", lifeTime, cpuCoreSeconds, memGigaBytesSeconds,
				test.ExpectedLifeTime, test.ExpectedCpu, test.ExpectedMem)
		}
	}
}

func TestCollectSandboxLifeTimeMeric(t *testing.T) {
	ctx := context.Background()
	local := &local{}

	t.Run("MainStatus is nil", func(t *testing.T) {
		sb := &cubeboxstore.CubeBox{
			Metadata: cubeboxstore.Metadata{
				ID: "test-sandbox-1",
			},
			ContainersMap: &cubeboxstore.ContainersMap{
				ContainerMap: make(map[string]*cubeboxstore.Container),
			},
		}

		local.collectSandboxLifeTimeMetric(ctx, sb)

	})

	t.Run("LifeTimeMetricReported is true", func(t *testing.T) {
		status := &cubeboxstore.StatusStorage{
			Status: cubeboxstore.Status{
				LifeTimeMetricReported: true,
			},
		}

		container := &cubeboxstore.Container{
			Metadata: cubeboxstore.Metadata{
				ID: "test-container",
			},
			Status: status,
		}

		sb := &cubeboxstore.CubeBox{
			Metadata: cubeboxstore.Metadata{
				ID:        "test-sandbox-2",
				CreatedAt: time.Now().UnixNano() - 10*time.Second.Nanoseconds(),
			},
			FirstContainerName: "test-container",
			ContainersMap: &cubeboxstore.ContainersMap{
				ContainerMap: map[string]*cubeboxstore.Container{
					"test-container": container,
				},
			},
		}

		local.collectSandboxLifeTimeMetric(ctx, sb)

	})

	t.Run("ResourceWithOverHead is nil", func(t *testing.T) {
		status := &cubeboxstore.StatusStorage{
			Status: cubeboxstore.Status{
				LifeTimeMetricReported: false,
			},
		}

		container := &cubeboxstore.Container{
			Metadata: cubeboxstore.Metadata{
				ID: "test-container",
			},
			Status: status,
		}

		sb := &cubeboxstore.CubeBox{
			Metadata: cubeboxstore.Metadata{
				ID:        "test-sandbox-3",
				CreatedAt: time.Now().UnixNano() - 10*time.Second.Nanoseconds(),
			},
			FirstContainerName: "test-container",
			ContainersMap: &cubeboxstore.ContainersMap{
				ContainerMap: map[string]*cubeboxstore.Container{
					"test-container": container,
				},
			},
		}

		local.collectSandboxLifeTimeMetric(ctx, sb)

		if !status.Status.LifeTimeMetricReported {
			t.Error("Expected LifeTimeMetricReported to be true after function call")
		}
	})

	t.Run("With ResourceWithOverHead and running sandbox", func(t *testing.T) {
		status := &cubeboxstore.StatusStorage{
			Status: cubeboxstore.Status{
				LifeTimeMetricReported: false,
				FinishedAt:             0,
			},
		}

		container := &cubeboxstore.Container{
			Metadata: cubeboxstore.Metadata{
				ID: "test-container",
			},
			Status: status,
		}

		cpuQ := resource.MustParse("2")
		memQ := resource.MustParse("4Gi")

		sb := &cubeboxstore.CubeBox{
			Metadata: cubeboxstore.Metadata{
				ID: "test-sandbox-4",
				ResourceWithOverHead: &cubeboxstore.ResourceWithOverHead{
					VmCpuQ: cpuQ,
					VmMemQ: memQ,
				},
				CreatedAt: time.Now().UnixNano() - 5*time.Second.Nanoseconds(),
			},
			FirstContainerName: "test-container",
			ContainersMap: &cubeboxstore.ContainersMap{
				ContainerMap: map[string]*cubeboxstore.Container{
					"test-container": container,
				},
			},
		}

		local.collectSandboxLifeTimeMetric(ctx, sb)

		if !status.Status.LifeTimeMetricReported {
			t.Error("Expected LifeTimeMetricReported to be true after function call")
		}
	})

	t.Run("With ResourceWithOverHead and finished sandbox", func(t *testing.T) {
		startTime := time.Now().UnixNano() - 10*time.Second.Nanoseconds()
		finishTime := time.Now().UnixNano() - 5*time.Second.Nanoseconds()

		status := &cubeboxstore.StatusStorage{
			Status: cubeboxstore.Status{
				LifeTimeMetricReported: false,
				FinishedAt:             finishTime,
			},
		}

		container := &cubeboxstore.Container{
			Metadata: cubeboxstore.Metadata{
				ID: "test-container",
			},
			Status: status,
		}

		cpuQ := resource.MustParse("1")
		memQ := resource.MustParse("2Gi")

		sb := &cubeboxstore.CubeBox{
			Metadata: cubeboxstore.Metadata{
				ID: "test-sandbox-5",
				ResourceWithOverHead: &cubeboxstore.ResourceWithOverHead{
					VmCpuQ: cpuQ,
					VmMemQ: memQ,
				},
				CreatedAt: startTime,
			},
			FirstContainerName: "test-container",
			ContainersMap: &cubeboxstore.ContainersMap{
				ContainerMap: map[string]*cubeboxstore.Container{
					"test-container": container,
				},
			},
		}

		local.collectSandboxLifeTimeMetric(ctx, sb)

		if !status.Status.LifeTimeMetricReported {
			t.Error("Expected LifeTimeMetricReported to be true after function call")
		}
	})
}

func TestSandboxDeletable(t *testing.T) {

	sb := &cubeboxstore.CubeBox{}
	if !sandboxDeletable(sb, nil) {
		t.Error("Expected true, got false")
	}

	filter := &cubebox.CubeSandboxFilter{}
	if !sandboxDeletable(sb, filter) {
		t.Error("Expected true, got false")
	}

	sb.Labels = map[string]string{"app": "myapp", "env": "prod"}
	filter = &cubebox.CubeSandboxFilter{LabelSelector: map[string]string{"app": "myapp"}}
	if !sandboxDeletable(sb, filter) {
		t.Error("Expected true, got false")
	}

	filter = &cubebox.CubeSandboxFilter{LabelSelector: map[string]string{"app": "myapp", "env": "dev"}}
	if sandboxDeletable(sb, filter) {
		t.Error("Expected false, got true")
	}

	filter = &cubebox.CubeSandboxFilter{LabelSelector: map[string]string{"foo": "bar"}}
	if !sandboxDeletable(sb, filter) {
		t.Error("Expected true, got false")
	}

	filter = &cubebox.CubeSandboxFilter{LabelSelector: map[string]string{"app": ""}}
	if sandboxDeletable(sb, filter) {
		t.Error("Expected false, got true")
	}
}
