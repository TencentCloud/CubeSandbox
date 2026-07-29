package sandbox

import (
	"context"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/qos"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestDecorateSandboxQosReportsConfiguredAndApplied(t *testing.T) {
	items := []*types.SandboxData{{SandboxID: "sb-1"}, nil}
	decorateSandboxQos(items, map[string]string{
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
		"missing": nil,
		"invalid": {constants.CubeAnnotationsNetWork: `{}`},
	} {
		t.Run(name, func(t *testing.T) {
			item := &types.SandboxData{SandboxID: "sb-1"}
			decorateSandboxQos([]*types.SandboxData{item}, annotations)
			if item.ConfiguredQos != nil || item.QosApplied {
				t.Fatalf("unexpected qos state: %+v", item)
			}
		})
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
