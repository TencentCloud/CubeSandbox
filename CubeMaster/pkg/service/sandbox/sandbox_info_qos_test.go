package sandbox

import (
	"context"
	"errors"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/qos"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestDecorateSandboxQosReportsConfiguredAndApplied(t *testing.T) {
	items := []*types.SandboxData{{SandboxID: "sb-1"}, nil}
	decorateSandboxQos(context.Background(), items, map[string]string{
		constants.CubeAnnotationsNetWork: `{"Qos":{"BandWidth":{"Size":1250000,"RefillTime":100},"OPS":{"Size":5000,"RefillTime":1000}},"Version":1}`,
		constants.CubeAnnotationsBlkQos:  `{"bandwidth":{"size":67108864,"refill_time":1000},"ops":{"size":1000,"refill_time":1000}}`,
	})

	got := items[0]
	if got.ConfiguredQos == nil || got.ConfiguredQos.Network == nil || got.ConfiguredQos.Network.BandwidthMbps != 100 || got.ConfiguredQos.Network.PacketsPerSecond != 5000 {
		t.Fatalf("configured qos=%+v, want 100 Mbps and 5000 packets/s", got.ConfiguredQos)
	}
	if got.ConfiguredQos.BlockIO == nil || got.ConfiguredQos.BlockIO.ThroughputMiBps != 64 || got.ConfiguredQos.BlockIO.IOPS != 1000 {
		t.Fatalf("configured qos=%+v, want 64 MiB/s and 1000 IOPS", got.ConfiguredQos)
	}
	if !got.QosApplied {
		t.Fatal("expected qos_applied=true")
	}
}

func TestDecorateSandboxQosIgnoresMissingOrInvalidAnnotation(t *testing.T) {
	for name, annotations := range map[string]map[string]string{
		"missing":  nil,
		"disabled": {constants.CubeAnnotationsNetWork: `{}`},
		"invalid":  {constants.CubeAnnotationsNetWork: `{"Qos":{}}`},
	} {
		t.Run(name, func(t *testing.T) {
			item := &types.SandboxData{SandboxID: "sb-1"}
			decorateSandboxQos(context.Background(), []*types.SandboxData{item}, annotations)
			if item.ConfiguredQos != nil || item.QosApplied {
				t.Fatalf("unexpected qos state: %+v", item)
			}
		})
	}
}

func TestDecorateSandboxQosReportsBlockQosWithDisabledNetwork(t *testing.T) {
	item := &types.SandboxData{SandboxID: "sb-1"}
	decorateSandboxQos(context.Background(), []*types.SandboxData{item}, map[string]string{
		constants.CubeAnnotationsNetWork: `{}`,
		constants.CubeAnnotationsBlkQos:  `{"bandwidth":{"size":67108864,"refill_time":1000}}`,
	})

	if item.ConfiguredQos == nil || item.ConfiguredQos.Network != nil || item.ConfiguredQos.BlockIO == nil || item.ConfiguredQos.BlockIO.ThroughputMiBps != 64 {
		t.Fatalf("configured qos=%+v, want block-only QoS", item.ConfiguredQos)
	}
	if !item.QosApplied {
		t.Fatal("expected qos_applied=true for block QoS")
	}
}

func TestDecorateSandboxQosFallsBackToTemplate(t *testing.T) {
	SetTemplateQosLookupHook(func(_ context.Context, templateID string) (*qos.Config, error) {
		if templateID != "tpl-1" {
			t.Fatalf("templateID=%q, want tpl-1", templateID)
		}
		return &qos.Config{
			Network: &qos.NetworkConfig{BandwidthMbps: 100},
		}, nil
	})
	t.Cleanup(func() { SetTemplateQosLookupHook(nil) })

	item := &types.SandboxData{SandboxID: "sb-1", TemplateID: "tpl-1"}
	decorateSandboxQosFromTemplate(context.Background(), []*types.SandboxData{item})

	if item.ConfiguredQos == nil || item.ConfiguredQos.Network.BandwidthMbps != 100 {
		t.Fatalf("configured qos=%+v, want 100 Mbps", item.ConfiguredQos)
	}
	if item.QosApplied {
		t.Fatal("template fallback cannot prove qos_applied")
	}
}

func TestDecorateSandboxQosForInfoFallsBackWhenSpecHasNoQos(t *testing.T) {
	SetTemplateQosLookupHook(func(_ context.Context, templateID string) (*qos.Config, error) {
		if templateID != "tpl-1" {
			t.Fatalf("templateID=%q, want tpl-1", templateID)
		}
		return &qos.Config{Network: &qos.NetworkConfig{BandwidthMbps: 100}}, nil
	})
	t.Cleanup(func() { SetTemplateQosLookupHook(nil) })

	for name, annotations := range map[string]map[string]string{
		"empty":     nil,
		"unrelated": {"cube.master.example": "value"},
	} {
		t.Run(name, func(t *testing.T) {
			item := &types.SandboxData{SandboxID: "sb-1", TemplateID: "tpl-1"}
			decorateSandboxQosForInfo(context.Background(), item.SandboxID, []*types.SandboxData{item}, func(_ context.Context, sandboxID string) (map[string]string, error) {
				if sandboxID != item.SandboxID {
					t.Fatalf("sandboxID=%q, want %q", sandboxID, item.SandboxID)
				}
				return annotations, nil
			})
			if item.ConfiguredQos == nil || item.ConfiguredQos.Network == nil || item.ConfiguredQos.Network.BandwidthMbps != 100 {
				t.Fatalf("configured qos=%+v, want template bandwidth", item.ConfiguredQos)
			}
			if item.QosApplied {
				t.Fatal("missing persisted QoS annotation must not claim application")
			}
		})
	}
}

func TestDecorateSandboxQosForInfoFallsBackOnSpecError(t *testing.T) {
	SetTemplateQosLookupHook(func(context.Context, string) (*qos.Config, error) {
		return &qos.Config{BlockIO: &qos.BlockIOConfig{IOPS: 1000}}, nil
	})
	t.Cleanup(func() { SetTemplateQosLookupHook(nil) })

	item := &types.SandboxData{SandboxID: "sb-1", TemplateID: "tpl-1"}
	decorateSandboxQosForInfo(context.Background(), item.SandboxID, []*types.SandboxData{item}, func(context.Context, string) (map[string]string, error) {
		return nil, errors.New("spec unavailable")
	})
	if item.ConfiguredQos == nil || item.ConfiguredQos.BlockIO == nil || item.ConfiguredQos.BlockIO.IOPS != 1000 || item.QosApplied {
		t.Fatalf("fallback qos state=%+v", item)
	}
}

func TestDecorateSandboxQosRetainsValidSectionWhenOtherIsMalformed(t *testing.T) {
	for _, tt := range []struct {
		name        string
		annotations map[string]string
		wantNetwork bool
		wantBlockIO bool
	}{
		{
			name: "valid block io with malformed network",
			annotations: map[string]string{
				constants.CubeAnnotationsNetWork: `{"Qos":{}}`,
				constants.CubeAnnotationsBlkQos:  `{"ops":{"size":1000,"refill_time":1000}}`,
			},
			wantBlockIO: true,
		},
		{
			name: "valid network with malformed block io",
			annotations: map[string]string{
				constants.CubeAnnotationsNetWork: `{"Qos":{"BandWidth":{"Size":1250000,"RefillTime":100},"OPS":{}},"Version":1}`,
				constants.CubeAnnotationsBlkQos:  `{"ops":{"size":0,"refill_time":1000}}`,
			},
			wantNetwork: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			item := &types.SandboxData{SandboxID: "sb-1"}
			decorateSandboxQos(context.Background(), []*types.SandboxData{item}, tt.annotations)
			if item.ConfiguredQos == nil || (item.ConfiguredQos.Network != nil) != tt.wantNetwork || (item.ConfiguredQos.BlockIO != nil) != tt.wantBlockIO || !item.QosApplied {
				t.Fatalf("configured qos=%+v, applied=%t", item.ConfiguredQos, item.QosApplied)
			}
		})
	}
}
