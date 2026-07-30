// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import "context"

// recordingTimeoutProvider is shared by tests that need to verify calls to
// TimeoutProvider without using the lifecycle metadata store.
type recordingTimeoutProvider struct {
	sandboxID   string
	timeout     int
	calls       int
	rebaseCalls int
}

func (p *recordingTimeoutProvider) RefreshTimeout(_ context.Context, sandboxID string, timeoutSeconds int) (int64, error) {
	p.sandboxID = sandboxID
	p.timeout = timeoutSeconds
	p.calls++
	return 12345, nil
}

func (p *recordingTimeoutProvider) RebaseTimeoutWindow(_ context.Context, sandboxID string) (int64, error) {
	p.sandboxID = sandboxID
	p.rebaseCalls++
	return 12345, nil
}

func (p *recordingTimeoutProvider) LookupTimeout(_ context.Context, _ string) (int64, *int, error) {
	return 0, nil, nil
}
