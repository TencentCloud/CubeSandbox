// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"google.golang.org/protobuf/proto"
)

func TestP5NetworkProtoProjection(t *testing.T) {
	for _, empty := range [][]string{nil, {}} {
		raw := &cubebox.CubeNetworkConfig{AllowOut: empty, DenyOut: empty}
		b, e := proto.Marshal(raw)
		require.NoError(t, e)
		decoded := &cubebox.CubeNetworkConfig{}
		require.NoError(t, proto.Unmarshal(b, decoded))
		n := mapNetwork(&NetworkFields{AllowOut: decoded.AllowOut, DenyOut: decoded.DenyOut})
		b, e = json.Marshal(n)
		require.NoError(t, e)
		require.JSONEq(t, `{"allowed_cidrs":null,"denied_cidrs":null,"allowed_domains":null}`, string(b))
	}
	for _, deny := range []bool{false, true} {
		n := mapNetwork(&NetworkFields{AllowOut: []string{"8.8.8.8", "10.1.2.3/8", "*.EXAMPLE.com."}, DenyOut: []string{"1.2.3.4"}, DenyInternet: deny})
		require.Equal(t, []string{"8.8.8.8/32", "10.0.0.0/8"}, n.AllowedCIDRs)
		require.Equal(t, []string{"*.example.com"}, n.AllowedDomains)
		if deny {
			require.Equal(t, []string{"0.0.0.0/0"}, n.DeniedCIDRs)
		} else {
			require.Equal(t, []string{"1.2.3.4/32"}, n.DeniedCIDRs)
		}
	}
}
func TestP5CaptureBudgetAndSnapshot(t *testing.T) {
	p, _ := NewBufferedPublisher(10)
	var h PublisherHolder
	h.Set(p)
	n := &NetworkFields{AllowOut: []string{"1.2.3.4"}}
	h.PublishContext(context.Background(), TypeUpdated, "sb", "", LifecycleFields{Network: n})
	n.AllowOut[0] = "mutated"
	d, e := MapToProtocol(<-p.Events())
	require.NoError(t, e)
	require.Equal(t, []string{"1.2.3.4/32"}, d.NetworkEgress.AllowedCIDRs)
	for _, n := range []*NetworkFields{{AllowOut: make([]string, 257)}, {AllowOut: []string{strings.Repeat("a", 1025)}}, {AllowOut: make([]string, 32)}} {
		if len(n.AllowOut) == 32 {
			for i := range n.AllowOut {
				n.AllowOut[i] = strings.Repeat("a", 1024)
			}
		}
		h.Publish(TypeUpdated, "sb", "", LifecycleFields{Network: n})
		require.Empty(t, p.Events())
	}
	for _, origin := range []Origin{OriginCreateFailover, OriginPauseTeardown, OriginResumeTimeoutRefresh} {
		h.PublishContext(WithOrigin(context.Background(), origin), TypeUpdated, "sb", "", LifecycleFields{EndAtMS: 1})
		require.Empty(t, p.Events())
	}
}
func TestP5CaptureDistribution(t *testing.T) {
	for _, kind := range []string{"timeout", "network", "killed"} {
		for _, scenario := range []string{"disabled", "enabled", "full", "stopped"} {
			t.Run(kind+"/"+scenario, func(t *testing.T) {
				p, _ := NewBufferedPublisher(4096)
				var h PublisherHolder
				h.Set(p)
				if scenario == "disabled" {
					h.Set(NopPublisher{})
				}
				if scenario == "stopped" {
					p.Stop()
				}
				if scenario == "full" {
					p, _ = NewBufferedPublisher(1)
					h.Set(p)
					h.Publish(TypeCreated, "fill", "", LifecycleFields{})
				}
				f := LifecycleFields{EndAtMS: 1700000000123}
				if kind == "network" {
					f = LifecycleFields{Network: &NetworkFields{AllowOut: []string{"8.8.8.8", "*.example.com"}}}
				}
				invoke := func() {
					if kind == "killed" {
						d := h.PrepareDestruction(context.Background(), "sb", "", "request", LifecycleFields{CPU: "1", Memory: "1Gi"})
						d.Complete(&h)
					} else {
						h.PublishContext(context.Background(), TypeUpdated, "sb", "", f)
					}
				}
				samples := make([]int64, 1000)
				for i := range samples {
					start := time.Now()
					invoke()
					samples[i] = time.Since(start).Nanoseconds()
				}
				sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
				alloc := testing.AllocsPerRun(100, invoke)
				t.Logf("n=1000 p50=%dns p95=%dns p99=%dns allocs=%g", samples[499], samples[949], samples[989], alloc)
			})
		}
	}
}
