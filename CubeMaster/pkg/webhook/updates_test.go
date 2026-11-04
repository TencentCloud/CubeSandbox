// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"google.golang.org/protobuf/proto"
)

func TestNetworkProtoProjection(t *testing.T) {
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
func TestUpdateCaptureBudgetAndSnapshot(t *testing.T) {
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
