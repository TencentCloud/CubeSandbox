package nodemeta

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestPersistVersionsSkipsDuplicateConcurrentWrite(t *testing.T) {
	s := &service{
		nodes: make(map[string]*NodeSnapshot),
	}
	versions := []ComponentVersion{
		{Component: "cubelet", Version: "v1.2.3"},
	}

	firstWriteStarted := make(chan struct{})
	releaseFirstWrite := make(chan struct{})
	secondWriteStarted := make(chan struct{}, 1)
	var writeCount atomic.Int32

	writer := func(nodeID string, got []ComponentVersion, incomplete bool) error {
		_ = incomplete
		switch writeCount.Add(1) {
		case 1:
			close(firstWriteStarted)
			<-releaseFirstWrite
		case 2:
			secondWriteStarted <- struct{}{}
		}
		return nil
	}

	done1 := make(chan struct{})
	go func() {
		defer close(done1)
		s.persistVersionsWithWriter(context.Background(), "node-1", versions, false, writer)
	}()

	<-firstWriteStarted

	done2 := make(chan struct{})
	go func() {
		defer close(done2)
		s.persistVersionsWithWriter(context.Background(), "node-1", versions, false, writer)
	}()

	select {
	case <-secondWriteStarted:
		t.Fatal("duplicate version write started before the first write completed")
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseFirstWrite)
	<-done1
	<-done2

	if got := writeCount.Load(); got != 1 {
		t.Fatalf("write count = %d, want 1", got)
	}

	s.mu.RLock()
	snap := s.nodes["node-1"]
	s.mu.RUnlock()
	if snap == nil {
		t.Fatal("node snapshot was not created")
	}
	if snap.versionsHash != versionsHash(versions) {
		t.Fatalf("versionsHash = %q, want %q", snap.versionsHash, versionsHash(versions))
	}
	if len(snap.Versions) != len(versions) || snap.Versions[0] != versions[0] {
		t.Fatalf("versions = %#v, want %#v", snap.Versions, versions)
	}
}

func TestPersistVersionsIncompleteKeepsPriorComponents(t *testing.T) {
	s := &service{nodes: make(map[string]*NodeSnapshot)}
	full := []ComponentVersion{
		{Component: "cubelet", Version: "v1"},
		{Component: "guest-image", Version: "g1"},
	}
	partial := []ComponentVersion{
		{Component: "cubelet", Version: "v2"},
	}
	var incompleteWrites atomic.Int32
	var completeWrites atomic.Int32
	writer := func(nodeID string, got []ComponentVersion, incomplete bool) error {
		if incomplete {
			incompleteWrites.Add(1)
			if len(got) != 1 {
				t.Fatalf("incomplete writer got %#v", got)
			}
			return nil
		}
		completeWrites.Add(1)
		return nil
	}
	s.persistVersionsWithWriter(context.Background(), "n1", full, false, writer)
	s.persistVersionsWithWriter(context.Background(), "n1", partial, true, writer)
	if incompleteWrites.Load() != 1 || completeWrites.Load() != 1 {
		t.Fatalf("complete=%d incomplete=%d", completeWrites.Load(), incompleteWrites.Load())
	}
	s.mu.RLock()
	snap := s.nodes["n1"]
	s.mu.RUnlock()
	if len(snap.Versions) != 2 {
		t.Fatalf("merged versions len=%d want 2: %#v", len(snap.Versions), snap.Versions)
	}
	by := map[string]string{}
	for _, v := range snap.Versions {
		by[v.Component] = v.Version
	}
	if by["cubelet"] != "v2" || by["guest-image"] != "g1" {
		t.Fatalf("merged map=%v", by)
	}
}
