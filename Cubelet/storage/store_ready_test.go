// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

func stubStoreReadiness(t *testing.T, fn func(string) (cow.Store, error)) {
	t.Helper()
	prevFn, prevInterval := storeForBackendFn, storeReadyPollInterval
	storeForBackendFn = fn
	storeReadyPollInterval = 5 * time.Millisecond
	t.Cleanup(func() {
		storeForBackendFn = prevFn
		storeReadyPollInterval = prevInterval
	})
}

func TestWaitStoreReadyWaitsForInitLoopToPublish(t *testing.T) {
	var calls atomic.Int32
	stubStoreReadiness(t, func(backend string) (cow.Store, error) {
		require.Equal(t, cow.BackendS3, backend)
		if calls.Add(1) < 4 {
			return nil, ErrS3NotReady
		}
		return &S3Cow{}, nil
	})

	require.NoError(t, WaitStoreReady(context.Background(), cow.BackendS3, time.Second))
	require.EqualValues(t, 4, calls.Load())
}

func TestWaitStoreReadyWaitsForJFS(t *testing.T) {
	var calls atomic.Int32
	stubStoreReadiness(t, func(string) (cow.Store, error) {
		if calls.Add(1) < 2 {
			return nil, ErrJFSNotReady
		}
		return &S3Cow{}, nil
	})

	require.NoError(t, WaitStoreReady(context.Background(), cow.BackendJFS, time.Second))
}

func TestWaitStoreReadyDoesNotWaitOnPermanentErrors(t *testing.T) {
	for _, permanent := range []error{ErrS3NotConfigured, ErrJFSNotConfigured, errors.New("storage is not initialized")} {
		var calls atomic.Int32
		stubStoreReadiness(t, func(string) (cow.Store, error) {
			calls.Add(1)
			return nil, permanent
		})

		start := time.Now()
		err := WaitStoreReady(context.Background(), cow.BackendS3, time.Second)
		require.ErrorIs(t, err, permanent)
		require.EqualValues(t, 1, calls.Load())
		require.Less(t, time.Since(start), 500*time.Millisecond)
	}
}

func TestWaitStoreReadyReturnsNotReadyAtLimit(t *testing.T) {
	stubStoreReadiness(t, func(string) (cow.Store, error) { return nil, ErrS3NotReady })

	start := time.Now()
	err := WaitStoreReady(context.Background(), cow.BackendS3, 40*time.Millisecond)
	require.ErrorIs(t, err, ErrS3NotReady)
	require.Less(t, time.Since(start), 500*time.Millisecond)
}

func TestWaitStoreReadyStopsWithCallerContext(t *testing.T) {
	stubStoreReadiness(t, func(string) (cow.Store, error) { return nil, ErrS3NotReady })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := WaitStoreReady(ctx, cow.BackendS3, time.Minute)
	require.ErrorIs(t, err, ErrS3NotReady)
	require.Less(t, time.Since(start), 500*time.Millisecond)
}
