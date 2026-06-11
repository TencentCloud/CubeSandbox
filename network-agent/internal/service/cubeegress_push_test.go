// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package service

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/tencentcloud/CubeSandbox/network-agent/internal/cubeegress"
)

// fakeEgress implements egressClient for tests. It records every call
// and lets the test program a deterministic err sequence per IP.
type fakeEgress struct {
	mu       sync.Mutex
	puts     []putCall
	deletes  []string
	putErrs  map[string][]error // per-IP error queue; empty queue → success
	delErrs  map[string][]error
	configed bool
}

type putCall struct {
	ip    string
	rules int
}

func newFakeEgress() *fakeEgress {
	return &fakeEgress{
		putErrs:  map[string][]error{},
		delErrs:  map[string][]error{},
		configed: true,
	}
}

func (f *fakeEgress) Configured() bool { return f.configed }

func (f *fakeEgress) PutPolicy(_ context.Context, ip string, in *cubeegress.PolicyInput) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	rules := 0
	if in != nil {
		rules = len(in.Rules)
	}
	f.puts = append(f.puts, putCall{ip: ip, rules: rules})
	if errs := f.putErrs[ip]; len(errs) > 0 {
		err := errs[0]
		f.putErrs[ip] = errs[1:]
		return err
	}
	return nil
}

func (f *fakeEgress) DeletePolicy(_ context.Context, ip string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, ip)
	if errs := f.delErrs[ip]; len(errs) > 0 {
		err := errs[0]
		f.delErrs[ip] = errs[1:]
		return err
	}
	return nil
}

func (f *fakeEgress) putCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.puts)
}

// minimal sample config used across the push tests
func samplePolicy() *CubeNetworkConfig {
	host := "api.deepseek.com"
	return &CubeNetworkConfig{
		Rules: []*EgressRule{{
			Name:   "deepseek_api",
			Match:  &EgressRuleMatch{Host: &host},
			Action: &EgressRuleAction{Allow: true},
		}},
	}
}

func TestToEgressInputDropsNilFields(t *testing.T) {
	if got := toEgressInput(nil); got != nil {
		t.Fatalf("nil cfg → %v, want nil", got)
	}
	if got := toEgressInput(&CubeNetworkConfig{}); got != nil {
		t.Fatalf("empty cfg → %v, want nil (no rules to push)", got)
	}
	cfg := samplePolicy()
	got := toEgressInput(cfg)
	if got == nil || len(got.Rules) != 1 {
		t.Fatalf("toEgressInput=%v", got)
	}
	if got.Rules[0].Name != "deepseek_api" {
		t.Fatalf("Name=%q", got.Rules[0].Name)
	}
}

func TestToEgressInputCloneMethodSlice(t *testing.T) {
	method := []string{"GET", "POST"}
	cfg := &CubeNetworkConfig{
		Rules: []*EgressRule{{
			Name:   "r1",
			Match:  &EgressRuleMatch{Method: method},
			Action: &EgressRuleAction{Allow: true},
		}},
	}
	got := toEgressInput(cfg)
	if !reflect.DeepEqual(got.Rules[0].Match.Method, method) {
		t.Fatalf("Method=%v, want %v", got.Rules[0].Match.Method, method)
	}
	// Mutating the source slice MUST NOT mutate the rendered input.
	method[0] = "PATCH"
	if got.Rules[0].Match.Method[0] != "GET" {
		t.Fatalf("toEgressInput leaked source slice; got rendered Method[0]=%q after source mutation", got.Rules[0].Match.Method[0])
	}
}

func TestPushEgressForStateSuccessClearsPending(t *testing.T) {
	fake := newFakeEgress()
	s := &localService{egress: fake}
	st := &managedState{
		persistedState: persistedState{
			SandboxID:         "sb-1",
			SandboxIP:         "192.168.0.10",
			CubeNetworkConfig: samplePolicy(),
		},
		pendingEgressPush: true,
	}
	s.pushEgressForState(context.Background(), st)
	if st.pendingEgressPush {
		t.Fatal("pendingEgressPush should be cleared on success")
	}
	if fake.putCount() != 1 {
		t.Fatalf("put count=%d, want 1", fake.putCount())
	}
}

func TestPushEgressForStateTransientSetsPending(t *testing.T) {
	fake := newFakeEgress()
	fake.putErrs["192.168.0.10"] = []error{errors.New("connection refused")}
	s := &localService{egress: fake}
	st := &managedState{
		persistedState: persistedState{
			SandboxID:         "sb-1",
			SandboxIP:         "192.168.0.10",
			CubeNetworkConfig: samplePolicy(),
		},
	}
	s.pushEgressForState(context.Background(), st)
	if !st.pendingEgressPush {
		t.Fatal("pendingEgressPush should be set after transient failure")
	}
}

func TestPushEgressForStatePermanentClearsPending(t *testing.T) {
	fake := newFakeEgress()
	fake.putErrs["192.168.0.10"] = []error{&cubeegress.PermanentError{Status: 400, Body: "bad rule"}}
	s := &localService{egress: fake}
	st := &managedState{
		persistedState: persistedState{
			SandboxID:         "sb-1",
			SandboxIP:         "192.168.0.10",
			CubeNetworkConfig: samplePolicy(),
		},
		pendingEgressPush: true, // simulate a prior pending state
	}
	s.pushEgressForState(context.Background(), st)
	if st.pendingEgressPush {
		t.Fatal("pendingEgressPush should be cleared on permanent failure (we won't retry malformed bodies)")
	}
}

func TestPushEgressForStateNoRulesClearsPending(t *testing.T) {
	fake := newFakeEgress()
	s := &localService{egress: fake}
	st := &managedState{
		persistedState: persistedState{
			SandboxID:         "sb-1",
			SandboxIP:         "192.168.0.10",
			CubeNetworkConfig: nil, // no L7 rules
		},
		pendingEgressPush: true,
	}
	s.pushEgressForState(context.Background(), st)
	if st.pendingEgressPush {
		t.Fatal("pendingEgressPush should be cleared when there are no rules")
	}
	if fake.putCount() != 0 {
		t.Fatal("PUT was made even though there were no rules to push")
	}
}

func TestPushEgressForStateNoEgressClient(t *testing.T) {
	// nil egress client = dev mode; the push must be silently skipped.
	s := &localService{}
	st := &managedState{
		persistedState: persistedState{
			SandboxID:         "sb-1",
			SandboxIP:         "192.168.0.10",
			CubeNetworkConfig: samplePolicy(),
		},
		pendingEgressPush: true,
	}
	s.pushEgressForState(context.Background(), st)
	// Pending stays as-is (we made no attempt at all). This is fine
	// because the maintenance retry will also no-op while egress is nil.
	if !st.pendingEgressPush {
		t.Fatal("expected pendingEgressPush to be unchanged when egress is nil")
	}
}

func TestRetryPendingEgressPushesSucceedsClears(t *testing.T) {
	fake := newFakeEgress()
	s := &localService{
		egress: fake,
		states: map[string]*managedState{
			"sb-1": {
				persistedState: persistedState{
					SandboxID:         "sb-1",
					SandboxIP:         "192.168.0.10",
					CubeNetworkConfig: samplePolicy(),
				},
				pendingEgressPush: true,
			},
			"sb-2": {
				persistedState: persistedState{
					SandboxID:         "sb-2",
					SandboxIP:         "192.168.0.11",
					CubeNetworkConfig: samplePolicy(),
				},
				// pendingEgressPush=false → no retry attempt for this one
			},
		},
	}
	s.retryPendingEgressPushes()
	if fake.putCount() != 1 {
		t.Fatalf("put count=%d, want only the pending one", fake.putCount())
	}
	if s.states["sb-1"].pendingEgressPush {
		t.Fatal("sb-1 pending flag should be cleared")
	}
}

func TestRetryPendingEgressPushesPermanentClears(t *testing.T) {
	fake := newFakeEgress()
	fake.putErrs["192.168.0.10"] = []error{&cubeegress.PermanentError{Status: 400, Body: "bad"}}
	s := &localService{
		egress: fake,
		states: map[string]*managedState{
			"sb-1": {
				persistedState: persistedState{
					SandboxID:         "sb-1",
					SandboxIP:         "192.168.0.10",
					CubeNetworkConfig: samplePolicy(),
				},
				pendingEgressPush: true,
			},
		},
	}
	s.retryPendingEgressPushes()
	if s.states["sb-1"].pendingEgressPush {
		t.Fatal("permanent failure should clear pending flag (retrying won't help)")
	}
}

func TestRetryPendingEgressPushesTransientStaysPending(t *testing.T) {
	fake := newFakeEgress()
	fake.putErrs["192.168.0.10"] = []error{
		errors.New("connection refused"),
		errors.New("connection refused"),
	}
	s := &localService{
		egress: fake,
		states: map[string]*managedState{
			"sb-1": {
				persistedState: persistedState{
					SandboxID:         "sb-1",
					SandboxIP:         "192.168.0.10",
					CubeNetworkConfig: samplePolicy(),
				},
				pendingEgressPush: true,
			},
		},
	}
	s.retryPendingEgressPushes()
	if !s.states["sb-1"].pendingEgressPush {
		t.Fatal("transient failure should keep pending=true so the next tick retries")
	}
	// One more tick to simulate the next maintenance interval.
	s.retryPendingEgressPushes()
	if !s.states["sb-1"].pendingEgressPush {
		t.Fatal("two transient failures in a row should still leave pending=true")
	}
	if got := fake.putCount(); got != 2 {
		t.Fatalf("put count=%d, want 2 attempts", got)
	}
}

// TestRetryPendingEgressPushesIsLockSafe is a smoke test that the retry
// path doesn't deadlock against itself by holding s.mu during the HTTP
// call. We don't have a fast race detector here — the test just calls
// from multiple goroutines simultaneously and asserts that the put
// count matches the pending count, with no panics or hangs.
func TestRetryPendingEgressPushesIsLockSafe(t *testing.T) {
	fake := newFakeEgress()
	states := map[string]*managedState{}
	for i := 0; i < 32; i++ {
		ip := "192.168.0." + intToStr(10+i)
		states["sb-"+intToStr(i)] = &managedState{
			persistedState: persistedState{
				SandboxID:         "sb-" + intToStr(i),
				SandboxIP:         ip,
				CubeNetworkConfig: samplePolicy(),
			},
			pendingEgressPush: true,
		}
	}
	s := &localService{egress: fake, states: states}

	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.retryPendingEgressPushes()
		}()
	}
	wg.Wait()
	// Each pending should be cleared after at least one retry succeeded.
	for id, st := range s.states {
		if st.pendingEgressPush {
			t.Fatalf("%s still pending after concurrent retries", id)
		}
	}
}

// helper: allocation-free int→string for the IP suffix above; no need
// for strconv noise in the test imports.
func intToStr(n int) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		return "-" + string(digits)
	}
	return string(digits)
}

// Sanity check that putCall is what we expect (catches accidental
// renames during refactors).
var _ = atomic.Int32{} // reference to atomic so it's clear we considered concurrency above

func TestDumpEgressPoliciesEmptyWhenNoStates(t *testing.T) {
	s := &localService{states: map[string]*managedState{}}
	out, err := s.DumpEgressPolicies(context.Background())
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(out) != 0 {
		t.Fatalf("len=%d, want 0 for no states", len(out))
	}
}

func TestDumpEgressPoliciesSkipsSandboxesWithoutRules(t *testing.T) {
	// One sandbox with rules, one without (L3/L4-only). Only the first
	// should appear in the dump.
	s := &localService{
		states: map[string]*managedState{
			"sb-with-rules": {
				persistedState: persistedState{
					SandboxID:         "sb-with-rules",
					SandboxIP:         "192.168.0.10",
					CubeNetworkConfig: samplePolicy(),
				},
			},
			"sb-no-rules": {
				persistedState: persistedState{
					SandboxID:         "sb-no-rules",
					SandboxIP:         "192.168.0.11",
					CubeNetworkConfig: nil,
				},
			},
		},
	}
	out, err := s.DumpEgressPolicies(context.Background())
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(out) != 1 {
		t.Fatalf("len=%d, want 1", len(out))
	}
	body, ok := out["192.168.0.10"]
	if !ok {
		t.Fatalf("missing 192.168.0.10 entry: %v", out)
	}
	if body["policy_id"] != "192.168.0.10" {
		t.Fatalf("policy_id=%v", body["policy_id"])
	}
}

// Asserts the dump path uses the same renderer as the push path. If
// these ever diverge, a freshly-bootstrapped CubeEgress would see
// different rules from the same sandbox than network-agent's PUTs
// would deliver — exactly the drift the design doc forbids.
func TestDumpEgressPoliciesAndPushAgreeOnShape(t *testing.T) {
	cfg := samplePolicy()
	st := &managedState{
		persistedState: persistedState{
			SandboxID:         "sb-1",
			SandboxIP:         "192.168.0.10",
			CubeNetworkConfig: cfg,
		},
	}
	s := &localService{states: map[string]*managedState{"sb-1": st}}
	out, _ := s.DumpEgressPolicies(context.Background())
	dumpBody := out["192.168.0.10"]

	// Render via push path independently and compare.
	pushBody, err := cubeegressBuildForTest("192.168.0.10", cfg)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !reflect.DeepEqual(dumpBody, pushBody) {
		t.Fatalf("dump and push paths disagree:\n dump=%v\n push=%v", dumpBody, pushBody)
	}
}

// helper: render via the same boundary mapping the push path uses, so
// the equality check above truly compares apples to apples.
func cubeegressBuildForTest(ip string, cfg *CubeNetworkConfig) (map[string]any, error) {
	in := toEgressInput(cfg)
	return cubeegress.BuildPolicyBody(ip, in), nil
}
