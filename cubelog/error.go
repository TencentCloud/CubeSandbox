// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package CubeLog

type ErrorCode int

const (
	CodeSuccess ErrorCode = iota
	CodeInternalError
	CodeInvalidParameter
	CodeInvalidParameterValue
	CodeAuthFailure
	CodeResourceNotFound
	CodeResourceUnavailable
	CodeUnauthorizedOperation
	CodeFailedOperation
	CodeUnsupportedOperation
	CodeLimitExceeded
	CodeResourceInUse
	CodeMissingParameter
	CodeResourceInsufficient
	CodeUnknownError

	codeXingYunAlarmError
	codeHaboWriteMetricError
)

func (s ErrorCode) String() string {
	switch s {
	case CodeSuccess:
		return "Success"
	case CodeInternalError:
		return "InternalError"
	case CodeInvalidParameter:
		return "InvalidParameter"
	case CodeInvalidParameterValue:
		return "InvalidParameterValue"
	case CodeAuthFailure:
		return "AuthFailure"
	case CodeResourceNotFound:
		return "ResourceNotFound"
	case CodeResourceUnavailable:
		return "ResourceUnavailable"
	case CodeUnauthorizedOperation:
		return "UnauthorizedOperation"
	case CodeFailedOperation:
		return "FailedOperation"
	case CodeUnsupportedOperation:
		return "UnsupportedOperation"
	case CodeLimitExceeded:
		return "LimitExceeded"
	case CodeResourceInUse:
		return "ResourceInUse"
	case CodeMissingParameter:
		return "MissingParameter"
	case CodeResourceInsufficient:
		return "ResourceInsufficient"
	case codeXingYunAlarmError:
		return "XingYunAlarmError"
	case codeHaboWriteMetricError:
		return "HaboWriteMetricError"
	default:
		return "UnknownError"
	}
}
