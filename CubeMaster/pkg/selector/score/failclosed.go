// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package score

import "errors"

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

// IsFailClosed reports whether err should abort the Score phase.
func IsFailClosed(err error) bool {
	var closed *FailClosedError
	return errors.As(err, &closed)
}
