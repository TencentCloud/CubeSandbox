package lifecycle

import (
	"context"
	"encoding/json"
	"testing"

	sandbox "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
)

func metaJSON(t *testing.T, meta *SandboxLifecycleMeta) string {
	t.Helper()
	b, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	return string(b)
}

func TestLookupDeadlinesMapsTheThreeStates(t *testing.T) {
	never := -1
	finite := 600
	reply := []interface{}{
		metaJSON(t, &SandboxLifecycleMeta{SandboxID: "sb-never", TimeoutSeconds: &never}),
		metaJSON(t, &SandboxLifecycleMeta{SandboxID: "sb-finite", EndAt: 1_800_000_000_000, TimeoutSeconds: &finite}),
		nil,
		"not-json",
	}
	provider := &storeTimeoutProvider{store: NewStore(&fakeRedis{hmgetReply: reply})}

	got, err := provider.LookupDeadlines(context.Background(),
		[]string{"sb-never", "sb-finite", "sb-missing", "sb-bad"})
	if err != nil {
		t.Fatalf("LookupDeadlines: %v", err)
	}

	neverInfo, ok := got["sb-never"]
	if !ok || !neverInfo.Found || neverInfo.EndAtMs != 0 ||
		neverInfo.TimeoutSeconds == nil || *neverInfo.TimeoutSeconds != -1 {
		t.Fatalf("never row: %+v (present=%v)", neverInfo, ok)
	}
	finiteInfo, ok := got["sb-finite"]
	if !ok || !finiteInfo.Found || finiteInfo.EndAtMs != 1_800_000_000_000 {
		t.Fatalf("finite row: %+v (present=%v)", finiteInfo, ok)
	}
	if _, ok := got["sb-missing"]; ok {
		t.Fatalf("a missing row must be absent, not found")
	}
	if _, ok := got["sb-bad"]; ok {
		t.Fatalf("an undecodable row must be absent, not found")
	}
}

func TestLookupDeadlinesDerivesEndAtOnlyForPositiveTimeouts(t *testing.T) {
	finite := 60
	never := -1
	zero := 0
	provider := &storeTimeoutProvider{store: NewStore(&fakeRedis{hmgetReply: []interface{}{
		metaJSON(t, &SandboxLifecycleMeta{SandboxID: "sb-derived", CreatedAt: 1000, TimeoutSeconds: &finite}),
		metaJSON(t, &SandboxLifecycleMeta{SandboxID: "sb-never", CreatedAt: 1000, TimeoutSeconds: &never}),
		metaJSON(t, &SandboxLifecycleMeta{SandboxID: "sb-zero", CreatedAt: 1000, TimeoutSeconds: &zero}),
		metaJSON(t, &SandboxLifecycleMeta{SandboxID: "sb-no-created", TimeoutSeconds: &finite}),
	}})}

	got, err := provider.LookupDeadlines(context.Background(),
		[]string{"sb-derived", "sb-never", "sb-zero", "sb-no-created"})
	if err != nil {
		t.Fatalf("LookupDeadlines: %v", err)
	}

	if got["sb-derived"].EndAtMs != 1000+60*1000 {
		t.Fatalf("derived endAt: %+v, want CreatedAt+timeout", got["sb-derived"])
	}
	if got["sb-never"].EndAtMs != 0 {
		t.Fatalf("never endAt: %+v, want 0", got["sb-never"])
	}
	if got["sb-zero"].EndAtMs != 0 {
		t.Fatalf("zero timeout endAt: %+v, want 0", got["sb-zero"])
	}
	if got["sb-no-created"].EndAtMs != 0 {
		t.Fatalf("no CreatedAt endAt: %+v, want 0", got["sb-no-created"])
	}
}

func TestLookupDeadlinesPropagatesRedisError(t *testing.T) {
	provider := &storeTimeoutProvider{store: NewStore(&fakeRedis{failHMGET: true})}

	got, err := provider.LookupDeadlines(context.Background(), []string{"sb-1"})
	if err == nil {
		t.Fatalf("a failed HMGET must surface an error, got map=%+v", got)
	}
}

func TestLookupDeadlinesWithoutStoreIsNoop(t *testing.T) {
	provider := &storeTimeoutProvider{}
	got, err := provider.LookupDeadlines(context.Background(), []string{"sb-1"})
	if err != nil || got != nil {
		t.Fatalf("nil store: got (%v, %v), want (nil, nil)", got, err)
	}
}

func TestLookupDeadlinesEmptyIDsSkipsRedis(t *testing.T) {
	r := &fakeRedis{}
	provider := &storeTimeoutProvider{store: NewStore(r)}

	for _, ids := range [][]string{nil, {}, {"", ""}} {
		got, err := provider.LookupDeadlines(context.Background(), ids)
		if err != nil || len(got) != 0 {
			t.Fatalf("ids=%v: got (%v, %v), want an empty result", ids, got, err)
		}
	}
	if calls := r.snapshot(); len(calls) != 0 {
		t.Fatalf("empty ids must not reach Redis: %+v", calls)
	}
}

func TestLookupDeadlinesProviderContract(t *testing.T) {
	// Guards the interface the sandbox package type-asserts on.
	var _ interface {
		LookupDeadlines(context.Context, []string) (map[string]sandbox.DeadlineInfo, error)
	} = (*storeTimeoutProvider)(nil)
}
