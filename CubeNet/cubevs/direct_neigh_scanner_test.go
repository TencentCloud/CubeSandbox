package cubevs

import (
	"sort"
	"testing"
)

// fakeNeighStore is an in-memory directNeighStore for scanner unit tests.
type fakeNeighStore struct {
	entries map[uint32]directNeighbor
}

func newFakeNeighStore() *fakeNeighStore {
	return &fakeNeighStore{entries: map[uint32]directNeighbor{}}
}

func (f *fakeNeighStore) forEach(fn func(uint32, directNeighbor) bool) {
	keys := make([]uint32, 0, len(f.entries))
	for k := range f.entries {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, k := range keys {
		if !fn(k, f.entries[k]) {
			return
		}
	}
}

func (f *fakeNeighStore) update(key uint32, val directNeighbor) error {
	f.entries[key] = val
	return nil
}

func (f *fakeNeighStore) delete(key uint32) error {
	delete(f.entries, key)
	return nil
}

type scannerFixture struct {
	store     *fakeNeighStore
	scanner   *directNeighScanner
	triggered []uint32
	nowNs     uint64
}

func newScannerFixture() *scannerFixture {
	fx := &scannerFixture{store: newFakeNeighStore()}
	fx.scanner = newDirectNeighScanner(fx.store, func(ip uint32) error {
		fx.triggered = append(fx.triggered, ip)
		return nil
	}, func() uint64 { return fx.nowNs })
	return fx
}

func (fx *scannerFixture) scan() { fx.scanner.scanOnce() }

func (fx *scannerFixture) add(key uint32, val directNeighbor) { fx.store.entries[key] = val }

func TestScannerLearningBackoffSequenceAndCap(t *testing.T) {
	fx := newScannerFixture()
	fx.add(0x0a000001, directNeighbor{LastUsedNs: 0, FibOk: 0})

	// t=0: first trigger fires immediately, backoff = 1s.
	fx.scan()
	if len(fx.triggered) != 1 {
		t.Fatalf("first scan must trigger once, got %d", len(fx.triggered))
	}
	e := fx.store.entries[0x0a000001]
	if e.Step != 1 || e.NextAttemptNs != 0+directNeighBackoffBaseNs {
		t.Fatalf("after first trigger step=%d next=%d, want step=1 next=1s", e.Step, e.NextAttemptNs)
	}

	// t=0.5s: not yet due.
	fx.nowNs = directNeighBackoffBaseNs / 2
	fx.scan()
	if len(fx.triggered) != 1 {
		t.Fatalf("early scan must not trigger, got %d", len(fx.triggered))
	}

	// Walk the backoff: the immediate first trigger scheduled +1s (asserted
	// above); subsequent intervals are 2s, 4s, 8s, then capped at 16s.
	wantBackoffs := []uint64{2, 4, 8, 16, 16, 16}
	prev := fx.store.entries[0x0a000001].NextAttemptNs
	for i, wantSec := range wantBackoffs {
		fx.nowNs = prev // exactly when due
		fx.scan()
		e = fx.store.entries[0x0a000001]
		got := (e.NextAttemptNs - prev) / 1e9
		if got != wantSec {
			t.Fatalf("step %d: backoff=%ds, want %ds", i, got, wantSec)
		}
		prev = e.NextAttemptNs
	}
}

func TestScannerLearnedEntrySwitchesToKeepalive(t *testing.T) {
	fx := newScannerFixture()
	// A learning entry that just resolved (datapath set fib_ok=1 mid-backoff).
	fx.add(0x0a000002, directNeighbor{
		LastUsedNs:    0,
		FibOk:         1,
		Step:          3,
		NextRefreshNs: 0,
	})

	fx.scan()
	e := fx.store.entries[0x0a000002]
	if e.Step != 0 {
		t.Fatalf("learned entry must reset step, got %d", e.Step)
	}
	if len(fx.triggered) != 1 {
		t.Fatalf("keepalive must fire when next_refresh due, got %d", len(fx.triggered))
	}
	if e.NextRefreshNs != fx.nowNs+directNeighRefreshNs {
		t.Fatalf("next_refresh=%d, want now+16s", e.NextRefreshNs)
	}

	// Next scan before the refresh period: no new trigger.
	fx.nowNs = directNeighRefreshNs / 2
	fx.scan()
	if len(fx.triggered) != 1 {
		t.Fatalf("keepalive must not fire before the refresh period, got %d", len(fx.triggered))
	}
}

func TestScannerGarbageCollectsIdle(t *testing.T) {
	fx := newScannerFixture()
	// Active entry (recently used) must survive; idle entry must be GC'd.
	fx.nowNs = 100 * 1e9
	fx.add(0x0a000003, directNeighbor{LastUsedNs: fx.nowNs, FibOk: 1})                  // active
	fx.add(0x0a000004, directNeighbor{LastUsedNs: fx.nowNs - directNeighGCAfterNs - 1}) // idle

	fx.scan()
	if _, ok := fx.store.entries[0x0a000003]; !ok {
		t.Fatal("active entry must not be GC'd")
	}
	if _, ok := fx.store.entries[0x0a000004]; ok {
		t.Fatal("idle entry must be GC'd")
	}
	// The GC'd entry gets no trigger.
	for _, ip := range fx.triggered {
		if ip == 0x0a000004 {
			t.Fatal("GC'd entry must not be triggered")
		}
	}
}

func TestScannerTokenBucketLimitsTriggers(t *testing.T) {
	fx := newScannerFixture()
	// More unlearned, all-due entries than the token bucket allows in one scan.
	const n = directNeighTriggerBurst + 50
	for i := 0; i < n; i++ {
		fx.add(0x0a000100+uint32(i), directNeighbor{LastUsedNs: 0, FibOk: 0})
	}
	fx.scan()
	if len(fx.triggered) > directNeighTriggerBurst {
		t.Fatalf("token bucket must cap triggers at %d, got %d",
			directNeighTriggerBurst, len(fx.triggered))
	}
	if len(fx.triggered) == 0 {
		t.Fatal("at least some triggers must fire")
	}
}

func TestScannerSkipsTriggerWhenNotDue(t *testing.T) {
	fx := newScannerFixture()
	// Learning entry not yet due for its next attempt.
	fx.add(0x0a000005, directNeighbor{
		LastUsedNs:    0,
		FibOk:         0,
		NextAttemptNs: 10 * 1e9, // due at t=10s
	})
	fx.nowNs = 5 * 1e9
	fx.scan()
	if len(fx.triggered) != 0 {
		t.Fatalf("must not trigger before next_attempt, got %d", len(fx.triggered))
	}
}
