// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"testing"
	"time"
)

func TestTemplateLifecycleLockSerializesFanoutAndDelete(t *testing.T) {
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		if err := withTemplateLifecycleLocalLock("tpl-lock", func() error {
			close(firstEntered)
			<-releaseFirst
			return nil
		}); err != nil {
			t.Errorf("first lifecycle lock: %v", err)
		}
	}()
	<-firstEntered

	secondEntered := make(chan struct{})
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		if err := withTemplateLifecycleLocalLock("tpl-lock", func() error {
			close(secondEntered)
			return nil
		}); err != nil {
			t.Errorf("second lifecycle lock: %v", err)
		}
	}()
	select {
	case <-secondEntered:
		t.Fatal("delete lifecycle section entered while eager fan-out still held the lock")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFirst)
	<-firstDone
	select {
	case <-secondEntered:
	case <-time.After(time.Second):
		t.Fatal("delete lifecycle section did not enter after eager fan-out released the lock")
	}
	<-secondDone
}
