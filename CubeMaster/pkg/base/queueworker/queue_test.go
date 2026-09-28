// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package queueworker

import (
	"context"
	"testing"
	"time"
)

func TestQueue(t *testing.T) {
	q := NewQueue(2)

	if err := q.Push(1); err != nil {
		t.Errorf("Push error: %v", err)
	}

	if err := q.Push(2); err != nil {
		t.Errorf("Push error: %v", err)
	}

	if err := q.Push(3); err == nil {
		t.Error("Push should return error when queue is full")
	}

	if v, err := q.Pop(); err != nil || v != 1 {
		t.Errorf("Pop error: %v, value: %v", err, v)
	}

	if v, err := q.Pop(); err != nil || v != 2 {
		t.Errorf("Pop error: %v, value: %v", err, v)
	}

	if _, err := q.Pop(); err == nil {
		t.Error("Pop should return error when queue is empty")
	}

	q.BPush(3)
	if v, ok := q.BPop(); !ok || v != 3 {
		t.Errorf("BPop error: %v, value: %v", ok, v)
	}

	if q.Len() != 0 {
		t.Errorf("Len error: %v", q.Len())
	}

	q.BPush(4)

	q.Close()
	if v, ok := q.BPop(); !ok || v != 4 {
		t.Errorf("BPop should can still read when queue is closed:%v", v)
	}

	if q.Len() != 0 {
		t.Errorf("Len error: %v", q.Len())
	}
}

func TestQueueBlock(t *testing.T) {
	q := NewQueue(5)

	popped := make(chan struct{})
	go func() {
		q.BPop()
		close(popped)
	}()

	select {
	case <-popped:
		t.Error("BPop should block when queue is empty")
	case <-time.After(50 * time.Millisecond):
	}
	if err := q.Push(1); err != nil {
		t.Fatalf("Push error: %v", err)
	}
	select {
	case <-popped:
	case <-time.After(time.Second):
		t.Fatal("BPop should get a value when queue is not empty")
	}
}

func TestQueueWorker(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	opt := &Options{
		QueueSize: 2,
		WorkerNum: 1,
	}
	wh := func(data interface{}) error {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return nil
	}
	qw := NewQueueWorker(opt, wh)

	if err := qw.Push(1); err != nil {
		t.Errorf("Push error: %v", err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not pick up the first item")
	}

	if err := qw.Push(2); err != nil {
		t.Errorf("Push error: %v", err)
	}
	if err := qw.Push(3); err != nil {
		t.Errorf("Push error: %v", err)
	}
	if err := qw.Push(4); err == nil {
		t.Error("Push should return error when queue is full")
	}

	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && qw.Len() != 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if v := qw.Len(); v != 0 {
		t.Errorf("Len should be 0: %v", v)
	}

	qw.GraceFullStop(context.Background())

	if _, ok := qw.BPop(); ok {
		t.Error("BPop should return false when queue is stopped")
	}
}

func TestQueueWorkerClose(t *testing.T) {
	opt := &Options{
		QueueSize: 3,
		WorkerNum: 1,
	}
	timeout := 3 * time.Second
	wh := func(data interface{}) error {
		time.Sleep(timeout)
		t.Logf("worker got data:%v, %v", time.Now(), data)
		return nil
	}
	qw := NewQueueWorker(opt, wh)

	if err := qw.Push(1); err != nil {
		t.Errorf("Push error: %v", err)
	}

	if err := qw.Push(2); err != nil {
		t.Errorf("Push error: %v", err)
	}

	if err := qw.Push(3); err != nil {
		t.Errorf("Push error: %v", err)
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	qw.GraceFullStop(ctx)
	if time.Since(start) < timeout {
		t.Error("Stopped should block after stop but has more data")
	}
}
