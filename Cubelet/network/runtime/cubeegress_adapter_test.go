package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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

func (f *fakeCubeEgressAdapter) PutPolicy(_ context.Context, _ string, _ cubeegress.SandboxID, _ *cubeegress.PolicyInput) error {
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

func TestEgressPolicyUsesSandboxID(t *testing.T) {
	oldDelays := egressCreatePushRetryDelays
	egressCreatePushRetryDelays = []time.Duration{0, 0}
	defer func() { egressCreatePushRetryDelays = oldDelays }()

	for _, tc := range []struct {
		name     string
		update   bool
		attempts int
	}{
		{name: "create", attempts: 1},
		{name: "retry", attempts: 2},
		{name: "update", update: true, attempts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := newEgressPolicyTestState()
			var pushed []map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPut || r.URL.Path != "/admin/v1/policies/"+state.SandboxIP {
					t.Errorf("request = %s %s, want PUT policy by sandbox IP", r.Method, r.URL.Path)
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode policy: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				pushed = append(pushed, body)
				if len(pushed) < tc.attempts {
					w.WriteHeader(http.StatusServiceUnavailable)
				}
			}))
			defer server.Close()
			controller := &NetworkController{
				cubeEgressAdapter: cubeegress.New(server.URL, time.Second),
				states:            map[string]*managedState{state.SandboxID: state},
			}
			ctx := context.Background()
			if tc.update {
				cfg := newEgressPolicyTestState().CubeNetworkConfig
				cfg.Rules[0].Name = "updated-rule"
				cfg.Rules[0].Action.Allow = false
				if err := controller.syncEgressPolicy(ctx, state, cfg); err != nil {
					t.Fatal(err)
				}
				state.CubeNetworkConfig = cfg
			} else if err := controller.pushEgressForState(ctx, state); err != nil {
				t.Fatal(err)
			}
			if len(pushed) != tc.attempts {
				t.Fatalf("PUT count = %d, want %d", len(pushed), tc.attempts)
			}
			dump, err := controller.DumpEgressPolicies(ctx)
			if err != nil {
				t.Fatal(err)
			}
			body := dump[state.SandboxIP]
			if len(dump) != 1 || body["policy_id"] != state.SandboxID {
				t.Errorf("dump = %v, want IP-keyed policy with ID %q", dump, state.SandboxID)
			}
			dumpJSON, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			for i, policy := range pushed {
				if policy["policy_id"] != state.SandboxID {
					t.Errorf("PUT %d policy_id = %v, want %q", i, policy["policy_id"], state.SandboxID)
				}
				pushJSON, err := json.Marshal(policy)
				if err != nil {
					t.Fatal(err)
				}
				if string(pushJSON) != string(dumpJSON) {
					t.Errorf("PUT %d = %s, bootstrap = %s", i, pushJSON, dumpJSON)
				}
			}
		})
	}
}

func TestEgressPolicyOmitsSandboxesWithoutRules(t *testing.T) {
	adapter := &fakeCubeEgressAdapter{configured: true}
	withRules := newEgressPolicyTestState()
	controller := &NetworkController{
		cubeEgressAdapter: adapter,
		states:            map[string]*managedState{withRules.SandboxID: withRules},
	}
	for i, cfg := range []*CubeNetworkConfig{nil, {}, {Rules: []*EgressRule{nil}}} {
		state := newEgressPolicyTestState()
		state.SandboxID = fmt.Sprintf("sandbox-without-rules-%d", i)
		state.SandboxIP = fmt.Sprintf("10.0.0.%d", i+3)
		state.CubeNetworkConfig = cfg
		controller.states[state.SandboxID] = state
		if err := controller.pushEgressForState(context.Background(), state); err != nil {
			t.Fatal(err)
		}
		dump, err := controller.DumpEgressPolicies(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if adapter.putCalls != 0 || len(dump) != 1 || dump[withRules.SandboxIP]["policy_id"] != withRules.SandboxID {
			t.Errorf("case %d: PUT count = %d, dump = %v, want only the existing policy", i, adapter.putCalls, dump)
		}
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
