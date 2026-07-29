package workflow

import (
	"testing"

	api "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
)

func TestGetQosFromReqDecodesTemplateBlockIOLimits(t *testing.T) {
	req := &api.RunCubeSandboxRequest{Annotations: map[string]string{
		constants.MasterAnnotationsBlkQos: `{"bandwidth":{"size":67108864,"refill_time":1000},"ops":{"size":1000,"refill_time":1000}}`,
	}}

	got, err := GetQosFromReq(req, constants.MasterAnnotationsBlkQos)
	if err != nil {
		t.Fatalf("GetQosFromReq error=%v", err)
	}
	if got == nil || got.Bandwidth == nil || got.Ops == nil {
		t.Fatalf("disk qos=%+v, want bandwidth and ops buckets", got)
	}
	if got.Bandwidth.Size == nil || *got.Bandwidth.Size != 67108864 ||
		got.Bandwidth.RefillTime == nil || *got.Bandwidth.RefillTime != 1000 {
		t.Fatalf("bandwidth bucket=%+v, want 64 MiB/s", got.Bandwidth)
	}
	if got.Ops.Size == nil || *got.Ops.Size != 1000 ||
		got.Ops.RefillTime == nil || *got.Ops.RefillTime != 1000 {
		t.Fatalf("ops bucket=%+v, want 1000 IOPS", got.Ops)
	}
}
