package sandbox

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestMain(m *testing.M) {
	server, err := miniredis.Run()
	if err != nil {
		panic(err)
	}
	if cfg := config.GetConfig(); cfg != nil {
		cfg.RedisConf = &config.RedisConf{
			Nodes:       server.Addr(),
			MaxActive:   4,
			MaxIdle:     1,
			MaxRetry:    1,
			DbNo:        0,
			IdleTimeout: 30,
		}
	}
	code := m.Run()
	server.Close()
	os.Exit(code)
}

type recordingTimeoutProvider struct {
	sandboxID      string
	timeoutSeconds int
	calls          int
}

func (p *recordingTimeoutProvider) RefreshTimeout(_ context.Context, sandboxID string, timeoutSeconds int) (int64, error) {
	p.sandboxID = sandboxID
	p.timeoutSeconds = timeoutSeconds
	p.calls++
	return 123, nil
}

func (*recordingTimeoutProvider) LookupEndAt(context.Context, string) (int64, error) {
	return 0, nil
}

func TestUpdateSuccessfulResumeAppliesExplicitTimeout(t *testing.T) {
	const sandboxID = "sb-resume-timeout"
	cfg := ensureSandboxTestConfig(t)
	originalMockUpdateAction := cfg.Common.MockUpdateAction
	cfg.Common.MockUpdateAction = true
	t.Cleanup(func() { cfg.Common.MockUpdateAction = originalMockUpdateAction })

	localcache.SetSandboxCache(sandboxID, &localcache.SandboxCache{
		SandboxID: sandboxID,
		HostIP:    "127.0.0.1",
	})
	t.Cleanup(func() { localcache.DeleteSandboxCache(sandboxID) })

	provider := &recordingTimeoutProvider{}
	SetTimeoutProvider(provider)
	t.Cleanup(func() { SetTimeoutProvider(nil) })

	var req types.UpdateRequest
	if err := json.Unmarshal([]byte(`{
		"requestID":"req-resume-timeout",
		"sandbox_id":"sb-resume-timeout",
		"instance_type":"cubebox",
		"action":"resume",
		"timeout":120
	}`), &req); err != nil {
		t.Fatalf("decode update request: %v", err)
	}

	rsp := Update(context.Background(), &req)
	if rsp.Ret.RetCode != int(errorcode.ErrorCode_Success) {
		t.Fatalf("resume should succeed in mock mode, got ret=%+v", rsp.Ret)
	}
	if provider.calls != 1 || provider.sandboxID != sandboxID || provider.timeoutSeconds != 120 {
		t.Fatalf("successful resume did not apply timeout: provider=%+v", provider)
	}
}

func TestUpdateDoesNotChangeTimeoutWhenOmittedOrPausing(t *testing.T) {
	const sandboxID = "sb-update-timeout-unchanged"
	cfg := ensureSandboxTestConfig(t)
	originalMockUpdateAction := cfg.Common.MockUpdateAction
	cfg.Common.MockUpdateAction = true
	t.Cleanup(func() { cfg.Common.MockUpdateAction = originalMockUpdateAction })

	localcache.SetSandboxCache(sandboxID, &localcache.SandboxCache{
		SandboxID: sandboxID,
		HostIP:    "127.0.0.1",
	})
	t.Cleanup(func() { localcache.DeleteSandboxCache(sandboxID) })
	t.Cleanup(func() { SetTimeoutProvider(nil) })

	for _, req := range []*types.UpdateRequest{
		{
			RequestID:    "req-resume-omitted",
			SandboxID:    sandboxID,
			InstanceType: "cubebox",
			Action:       "resume",
		},
		{
			RequestID:    "req-pause-timeout",
			SandboxID:    sandboxID,
			InstanceType: "cubebox",
			Action:       "pause",
			Timeout:      types.TimeoutPtr(120),
		},
	} {
		provider := &recordingTimeoutProvider{}
		SetTimeoutProvider(provider)
		if rsp := Update(context.Background(), req); rsp.Ret.RetCode != int(errorcode.ErrorCode_Success) {
			t.Fatalf("update should succeed, got ret=%+v", rsp.Ret)
		}
		if provider.calls != 0 {
			t.Fatalf("action=%s timeout=%v unexpectedly changed timeout", req.Action, req.Timeout)
		}
	}
}

func TestUpdateRejectsInvalidTimeout(t *testing.T) {
	rsp := Update(context.Background(), &types.UpdateRequest{
		RequestID:    "req-invalid-resume-timeout",
		SandboxID:    "sb-invalid-resume-timeout",
		InstanceType: "cubebox",
		Action:       "resume",
		Timeout:      types.TimeoutPtr(-2),
	})

	if rsp.Ret.RetCode != int(errorcode.ErrorCode_MasterParamsError) ||
		rsp.Ret.RetMsg != "timeout must be >= -1 (use -1 for never timeout)" {
		t.Fatalf("invalid timeout should fail before sandbox lookup, got ret=%+v", rsp.Ret)
	}
}
