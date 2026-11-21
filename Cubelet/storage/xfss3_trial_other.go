// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//go:build !linux

package storage

import (
	"context"
	"errors"
)

func runXFSS3TrialProcess(context.Context, string, *xfss3TrialAttempt, []string) ([]byte, bool, error) {
	return nil, true, errors.New("trial restore needs linux network namespaces")
}
