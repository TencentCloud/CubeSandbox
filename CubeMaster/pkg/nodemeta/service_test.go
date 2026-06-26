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

	writer := func(nodeID string, got []ComponentVersion) error {
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
		s.persistVersionsWithWriter(context.Background(), "node-1", versions, writer)
	}()

	<-firstWriteStarted

	done2 := make(chan struct{})
	go func() {
		defer close(done2)
		s.persistVersionsWithWriter(context.Background(), "node-1", versions, writer)
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
