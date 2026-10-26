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

	got := false
	go func() {
		q.BPop()
		got = true
	}()

	if got {
		t.Error("BPop should block when queue is empty")
	}
	q.Push(1)
	time.Sleep(time.Second)
	if !got {
		t.Error("BPop should got value when queue is not empty")
	}
}

func TestQueueWorker(t *testing.T) {
	opt := &Options{
		QueueSize: 2,
		WorkerNum: 3,
	}
	wh := func(data interface{}) error {
		time.Sleep(time.Second)
		return nil
	}
	qw := NewQueueWorker(opt, wh)

	if err := qw.Push(1); err != nil {
		t.Errorf("Push error: %v", err)
	}

	if err := qw.Push(2); err != nil {
		t.Errorf("Push error: %v", err)
	}

	if err := qw.Push(3); err == nil {
		t.Error("Push should return error when queue is full")
	}
	time.Sleep(2 * time.Second)
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
