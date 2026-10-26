// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"

	containerd "github.com/containerd/containerd/v2/client"
)

type fakeResumeTask struct {
	containerd.Task

	resumeErr error
	status    containerd.ProcessStatus
	statusErr error

	resumeCalls int
	statusCalls int
}

func (f *fakeResumeTask) Resume(context.Context) error {
	f.resumeCalls++
	return f.resumeErr
}

func (f *fakeResumeTask) Status(context.Context) (containerd.Status, error) {
	f.statusCalls++
	return containerd.Status{Status: f.status}, f.statusErr
}

type fakeDestroyTask = fakeResumeTask
