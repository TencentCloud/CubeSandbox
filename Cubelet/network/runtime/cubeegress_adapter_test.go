package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/network/runtime/cubeegress"
)

type fakeCubeEgressAdapter struct {
	configured  bool
	putErr      error
	putErrs     []error
	deleteErr   error
	verifyErr   error
	putCalls    int
	deleteCalls int
	verifyCalls int
	recorder    *createOrderRecorder
}

func (f *fakeCubeEgressAdapter) Configured() bool {
	return f.configured
}

func (f *fakeCubeEgressAdapter) PutPolicy(_ context.Context, _ string, _ *cubeegress.PolicyInput) error {
	if f.recorder != nil {
		f.recorder.record("cubeegress_put")
	}
	f.putCalls++
	if len(f.putErrs) > 0 {
		err := f.putErrs[0]
		f.putErrs = f.putErrs[1:]
		return err
	}
	return f.putErr
}

func (f *fakeCubeEgressAdapter) DeletePolicy(_ context.Context, _ string) error {
	if f.recorder != nil {
		f.recorder.record("cubeegress_delete")
	}
	f.deleteCalls++
	return f.deleteErr
}

func (f *fakeCubeEgressAdapter) VerifyPolicyAbsent(_ context.Context, _ string) error {
	if f.recorder != nil {
		f.recorder.record("cubeegress_verify")
	}
	f.verifyCalls++
	return f.verifyErr
}

func TestPushEgressForStateRetriesTransientFailures(t *testing.T) {
	oldDelays := egressCreatePushRetryDelays
	egressCreatePushRetryDelays = []time.Duration{0, 0}
	defer func() { egressCreatePushRetryDelays = oldDelays }()

	adapter := &fakeCubeEgressAdapter{configured: true, putErrs: []error{
		errors.New("temporary unavailable"),
		errors.New("connection refused"),
		nil,
	}}
	controller := &NetworkController{cubeEgressAdapter: adapter}
	state := newEgressPolicyTestState()

	if err := controller.pushEgressForState(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	if adapter.putCalls != 3 {
		t.Fatalf("putCalls = %d, want 3", adapter.putCalls)
	}
}

func TestPushEgressForStateReturnsTransientErrorAfterRetries(t *testing.T) {
	oldDelays := egressCreatePushRetryDelays
	egressCreatePushRetryDelays = []time.Duration{0, 0}
	defer func() { egressCreatePushRetryDelays = oldDelays }()

	adapter := &fakeCubeEgressAdapter{configured: true, putErr: errors.New("temporary unavailable")}
	controller := &NetworkController{cubeEgressAdapter: adapter}
	state := newEgressPolicyTestState()

	if err := controller.pushEgressForState(context.Background(), state); err == nil {
		t.Fatal("expected put error")
	}
	if adapter.putCalls != 3 {
		t.Fatalf("putCalls = %d, want 3", adapter.putCalls)
	}
}

func TestPushEgressForStateDoesNotRetryPermanentError(t *testing.T) {
	adapter := &fakeCubeEgressAdapter{configured: true, putErr: &cubeegress.PermanentError{Status: 400, Body: "bad request"}}
	controller := &NetworkController{cubeEgressAdapter: adapter}
	state := newEgressPolicyTestState()

	if err := controller.pushEgressForState(context.Background(), state); err == nil {
		t.Fatal("expected permanent error")
	}
	if adapter.putCalls != 1 {
		t.Fatalf("putCalls = %d, want 1", adapter.putCalls)
	}
}

func newEgressPolicyTestState() *managedState {
	return &managedState{persistedState: persistedState{
		SandboxID: "sandbox1",
		SandboxIP: "10.0.0.2",
		CubeNetworkConfig: &CubeNetworkConfig{Rules: []*EgressRule{{
			Name:   "rule1",
			Action: &EgressRuleAction{Allow: true},
		}}},
	}}
}

func TestDeleteEgressForStateDeletesAndVerifies(t *testing.T) {
	adapter := &fakeCubeEgressAdapter{configured: true}
	controller := &NetworkController{cubeEgressAdapter: adapter}

	if err := controller.deleteEgressForState(context.Background(), "sandbox1", "10.0.0.2"); err != nil {
		t.Fatal(err)
	}
	if adapter.deleteCalls != 1 || adapter.verifyCalls != 1 {
		t.Fatalf("deleteCalls=%d verifyCalls=%d, want 1/1", adapter.deleteCalls, adapter.verifyCalls)
	}
}
