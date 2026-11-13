// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCreateDispatchCannotStartAfterSafeFailure(t *testing.T) {
	for i := 0; i < 1000; i++ {
		c := &createSandboxContext{}
		var wg sync.WaitGroup
		wg.Add(2)
		start := make(chan struct{})
		var dispatched, safeToRelease bool
		go func() { defer wg.Done(); <-start; dispatched = c.beginCreateDispatch() }()
		go func() { defer wg.Done(); <-start; safeToRelease = c.abortUndispatchedCreate() }()
		close(start)
		wg.Wait()
		require.NotEqual(t, dispatched, safeToRelease, "exactly one side must win the dispatch boundary")
		require.Equal(t, dispatched, c.beginCreateDispatch(), "aborted work cannot dispatch later; dispatched retries remain ambiguous")
		require.Equal(t, safeToRelease, c.abortUndispatchedCreate())
	}
}
