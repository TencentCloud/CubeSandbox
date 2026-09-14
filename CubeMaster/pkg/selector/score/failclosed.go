// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package score

import (
	"errors"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/ret"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
)

// FailClosedError is returned by external_http_score when failure_policy is
// fail_closed. runScoreFilter aborts scheduling only for this typed error;
// other scorer errors keep the historical skip behavior.
type FailClosedError struct {
	Err error
}

func (e *FailClosedError) Error() string {
	if e == nil || e.Err == nil {
		return "score plugin fail_closed"
	}
	return e.Err.Error()
}

func (e *FailClosedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// GRPCStatus maps fail_closed create failures to a schedulable error code so
// API clients do not see ErrorCode_Unknown (-1). The message uses the same
// sanitized category vocabulary as the plugin's Warn/metric path.
//
// Contract with ret.FromError: that helper matches GRPCStatus via a *direct*
// type assertion, not errors.As. IsFailClosed uses errors.As, so wrapping this
// value (fmt.Errorf("…: %w", err)) still aborts Score but would fall through
// FromError to ErrorCode_Unknown with the raw Error() text on the create
// response path. Keep FailClosedError the outermost return from Select /
// runScoreFilter; do not wrap it before handleCubelet / ret.FromError.
func (e *FailClosedError) GRPCStatus() *ret.Status {
	msg := "score plugin fail_closed"
	if e != nil && e.Err != nil {
		msg = sanitizeExternalHTTPScoreFailure(e.Err)
	}
	return ret.New(errorcode.ErrorCode_SelectNodesFailed, msg)
}

// IsFailClosed reports whether err should abort the Score phase.
func IsFailClosed(err error) bool {
	var closed *FailClosedError
	return errors.As(err, &closed)
}
