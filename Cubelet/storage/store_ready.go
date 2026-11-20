// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"errors"
	"time"
)

// storeReadyPollInterval is how often WaitStoreReady re-checks a backend
// whose background init loop has not published its handle yet.
var storeReadyPollInterval = 500 * time.Millisecond

// storeForBackendFn is swapped in tests.
var storeForBackendFn = StoreFor

// storeStillInitializing reports the transient errors a backend returns
// while its init loop is still running. Everything else, including "not
// configured", is final for this node and is not waited on.
func storeStillInitializing(err error) bool {
	return errors.Is(err, ErrS3NotReady) || errors.Is(err, ErrJFSNotReady)
}

// WaitStoreReady blocks until the Store for backend is published, ctx ends,
// or limit elapses, whichever comes first.
//
// The S3 and JFS handles are published asynchronously after Cubelet starts,
// so a node that has just joined can already be Ready and schedulable while
// its remote-backed storage is still initializing. A request placed on such
// a node only because it is new (for example a cross-node restore onto
// capacity that was just added) should wait out that short window instead of
// failing. A node that never becomes ready still fails, with the same
// not-ready error the caller would have seen without waiting.
func WaitStoreReady(ctx context.Context, backend string, limit time.Duration) error {
	_, err := storeForBackendFn(backend)
	if err == nil || !storeStillInitializing(err) {
		return err
	}
	waitCtx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	ticker := time.NewTicker(storeReadyPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-waitCtx.Done():
			return err
		case <-ticker.C:
		}
		if _, err = storeForBackendFn(backend); err == nil || !storeStillInitializing(err) {
			return err
		}
	}
}
