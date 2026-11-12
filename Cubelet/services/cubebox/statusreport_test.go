// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"testing"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/restartpolicy"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/statusclient"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
)

func TestStatusSeqZeroIsSentOnce(t *testing.T) {
	cb := &cubeboxstore.CubeBox{ContainersMap: &cubeboxstore.ContainersMap{}}
	cb.ID = "sb"
	cb.RestartState = restartpolicy.StateRunning
	r := &statusReporter{
		pending: map[string]statusclient.Item{},
		acked:   map[string]ackMark{},
	}
	item, ok := statusItem(cb, "host")
	if !ok || item.StatusSeq != 0 {
		t.Fatalf("item %+v ok=%v", item, ok)
	}
	r.enqueue(item)
	if _, exists := r.pending["sb"]; !exists {
		t.Fatal("first seq 0 report was skipped")
	}
	delete(r.pending, "sb")
	r.ack([]statusclient.Item{item})
	r.enqueue(item)
	if _, exists := r.pending["sb"]; exists {
		t.Fatal("seq 0 was sent again")
	}
	item.StatusSeq = 2
	r.enqueue(item)
	if got := r.pending["sb"].StatusSeq; got != 2 {
		t.Fatalf("newer seq %d", got)
	}
	older := item
	older.StatusSeq = 1
	r.enqueue(older)
	if got := r.pending["sb"].StatusSeq; got != 2 {
		t.Fatalf("older seq replaced %d", got)
	}
}

func TestLivenessFailureOmitsExitCode(t *testing.T) {
	cb := &cubeboxstore.CubeBox{ContainersMap: &cubeboxstore.ContainersMap{}}
	cb.ID = "sb"
	cb.RestartState = restartpolicy.StateBackOff
	cb.LastExitReason = string(restartpolicy.ReasonLivenessFailed)
	item, ok := statusItem(cb, "host")
	if !ok || item.LastExitCode != nil {
		t.Fatalf("exit code %+v ok=%v", item.LastExitCode, ok)
	}
}
