package cubevs

import (
	"errors"
	"testing"
	"time"
	"unsafe"

	"github.com/cilium/ebpf"
	"golang.org/x/sys/unix"
)

func newTestHashMap(t *testing.T, keySize, valueSize, maxEntries uint32) *ebpf.Map {
	t.Helper()

	m, err := ebpf.NewMap(&ebpf.MapSpec{
		Type:       ebpf.Hash,
		KeySize:    keySize,
		ValueSize:  valueSize,
		MaxEntries: maxEntries,
	})
	if err != nil {
		if bpfTestUnavailable(err) {
			t.Skipf("bpf map unavailable: %v", err)
		}
		t.Fatalf("new hash map: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// Deleting the key that GET_NEXT_KEY is about to use makes Linux start over
// at bucket 0. reapSessionMaps deletes the previous key on the next step.
func TestDeletingCurrentHashKeyRestartsIteration(t *testing.T) {
	const n = 32
	m := newTestHashMap(t, 4, 4, n)
	for i := uint32(0); i < n; i++ {
		if err := m.Put(i, i); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}

	order := hashMapOrder[uint32, uint32](t, m)
	if len(order) != n {
		t.Fatalf("iterated %d keys, want %d", len(order), n)
	}

	victim := order[n/2]
	var key, value uint32
	iter := m.Iterate()
	restarted := false
	for iter.Next(&key, &value) {
		if key != victim {
			continue
		}
		if err := m.Delete(&key); err != nil {
			t.Fatalf("delete %d: %v", key, err)
		}
		if !iter.Next(&key, &value) {
			t.Fatalf("iteration ended after deleting key %d: %v", victim, iter.Err())
		}
		if key != order[0] {
			t.Fatalf("key after deleting current key = %d, want restart at %d", key, order[0])
		}
		restarted = true
		break
	}
	if err := iter.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	if !restarted {
		t.Fatal("deleting the current key did not restart iteration")
	}
}

func TestReapSessionMapsDeletesExpiredWithoutRevisiting(t *testing.T) {
	const n = 32
	egress := newTestHashMap(t, uint32(unsafe.Sizeof(sessionKey{})), uint32(unsafe.Sizeof(natSession{})), n)
	ingress := newTestHashMap(t, uint32(unsafe.Sizeof(sessionKey{})), uint32(unsafe.Sizeof(ingressSessionValue{})), n)

	now := uint64(4 * time.Hour)
	ingressByEgress := make(map[sessionKey]sessionKey, n)
	for i := 0; i < n; i++ {
		key := sessionKey{
			SourceIP:   0x0a000001,
			TargetIP:   0x0a000002,
			SourcePort: uint16(10000 + i),
			TargetPort: 80,
			Protocol:   unix.IPPROTO_TCP,
		}
		sess := natSession{
			AccessTime: now,
			NodeIP:     0x0a000003,
			NodePort:   uint16(40000 + i),
			State:      uint8(tcpCTEstablished),
		}
		if err := egress.Put(&key, &sess); err != nil {
			t.Fatalf("put egress %d: %v", i, err)
		}
		inKey := ingressKeyFor(&key, &sess)
		ingressByEgress[key] = inKey
		var inVal ingressSessionValue
		if err := ingress.Put(&inKey, &inVal); err != nil {
			t.Fatalf("put ingress %d: %v", i, err)
		}
	}

	order := hashMapOrder[sessionKey, natSession](t, egress)
	if len(order) != n {
		t.Fatalf("iterated %d sessions, want %d", len(order), n)
	}

	// Expire keys that are not first in bucket order. Deleting the first key
	// and restarting would still walk each remaining key once, so it would
	// hide the bug.
	expired := map[sessionKey]struct{}{
		order[n/2]: {},
		order[n-1]: {},
	}
	for key := range expired {
		var sess natSession
		if err := egress.Lookup(&key, &sess); err != nil {
			t.Fatalf("lookup %v: %v", key, err)
		}
		sess.AccessTime = 0
		sess.State = uint8(tcpCTClose)
		if err := egress.Update(&key, &sess, ebpf.UpdateExist); err != nil {
			t.Fatalf("expire %v: %v", key, err)
		}
	}

	count, usageByNodeIP, err := reapSessionMapsWithUsage(egress, ingress, now)
	if err != nil {
		t.Fatalf("reapSessionMapsWithUsage: %v", err)
	}
	if count != n {
		t.Fatalf("visited %d sessions, want %d", count, n)
	}
	wantUsage := snatSessionUsage{
		sessionsInUse: n - uint64(len(expired)),
		portsInUse:    n - uint64(len(expired)),
	}
	if got := usageByNodeIP[0x0a000003]; got != wantUsage {
		t.Fatalf("SNAT usage = %+v, want %+v", got, wantUsage)
	}

	for _, key := range order {
		inKey, ok := ingressByEgress[key]
		if !ok {
			t.Fatalf("no ingress key recorded for %v", key)
		}
		var sess natSession
		lookupErr := egress.Lookup(&key, &sess)
		var inVal ingressSessionValue
		inErr := ingress.Lookup(&inKey, &inVal)
		if _, wantGone := expired[key]; wantGone {
			if !errors.Is(lookupErr, ebpf.ErrKeyNotExist) {
				t.Fatalf("expired egress key %v still present: %v", key, lookupErr)
			}
			if !errors.Is(inErr, ebpf.ErrKeyNotExist) {
				t.Fatalf("expired ingress key %v still present: %v", inKey, inErr)
			}
			continue
		}
		if lookupErr != nil {
			t.Fatalf("live egress key %v missing: %v", key, lookupErr)
		}
		if inErr != nil {
			t.Fatalf("live ingress key %v missing: %v", inKey, inErr)
		}
	}
}

func TestReapSessionMapsEmpty(t *testing.T) {
	egress := newTestHashMap(t, uint32(unsafe.Sizeof(sessionKey{})), uint32(unsafe.Sizeof(natSession{})), 8)
	ingress := newTestHashMap(t, uint32(unsafe.Sizeof(sessionKey{})), uint32(unsafe.Sizeof(ingressSessionValue{})), 8)

	count, err := reapSessionMaps(egress, ingress, 1)
	if err != nil {
		t.Fatalf("reapSessionMaps: %v", err)
	}
	if count != 0 {
		t.Fatalf("visited %d sessions, want 0", count)
	}
}

func ingressKeyFor(egressKey *sessionKey, sess *natSession) sessionKey {
	return sessionKey{
		SourceIP:   egressKey.TargetIP,
		TargetIP:   sess.NodeIP,
		SourcePort: egressKey.TargetPort,
		TargetPort: sess.NodePort,
		Protocol:   egressKey.Protocol,
	}
}

func hashMapOrder[K comparable, V any](t *testing.T, m *ebpf.Map) []K {
	t.Helper()

	var (
		key   K
		value V
		order []K
	)
	iter := m.Iterate()
	for iter.Next(&key, &value) {
		order = append(order, key)
	}
	if err := iter.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	return order
}
