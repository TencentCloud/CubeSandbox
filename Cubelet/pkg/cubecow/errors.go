// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubecow

import "fmt"

type SemanticCode int32

const (
	SemOK                 SemanticCode = 0
	SemNotFound           SemanticCode = -1
	SemAlreadyExists      SemanticCode = -2
	SemResourceExhausted  SemanticCode = -3
	SemInvalidArgument    SemanticCode = -4
	SemIoError            SemanticCode = -6
	SemConfigError        SemanticCode = -10
	SemPreconditionFailed SemanticCode = -11
	SemNullPointer        SemanticCode = -12
	SemInvalidUTF8        SemanticCode = -13
	SemPanic              SemanticCode = -99
	ErrClosed             SemanticCode = -1000
)

type Action uint8

const (
	ActFail Action = iota
	ActIdempotentOK
	ActRetryOnce
	ActBug
	ActOpsIntervention
	ActMarkStoragePanic
	ActMarkStorageUnhealthy
)

func (a Action) String() string {
	switch a {
	case ActFail:
		return "fail"
	case ActIdempotentOK:
		return "idempotent_ok"
	case ActRetryOnce:
		return "retry_once"
	case ActBug:
		return "bug"
	case ActOpsIntervention:
		return "ops_intervention"
	case ActMarkStoragePanic:
		return "mark_storage_panic"
	case ActMarkStorageUnhealthy:
		return "mark_storage_unhealthy"
	default:
		return fmt.Sprintf("unknown_action(%d)", a)
	}
}

func (c SemanticCode) String() string {
	switch c {
	case SemOK:
		return "ok"
	case SemNotFound:
		return "not_found"
	case SemAlreadyExists:
		return "already_exists"
	case SemResourceExhausted:
		return "resource_exhausted"
	case SemInvalidArgument:
		return "invalid_argument"
	case SemIoError:
		return "io_error"
	case SemConfigError:
		return "config_error"
	case SemPreconditionFailed:
		return "precondition_failed"
	case SemNullPointer:
		return "null_pointer"
	case SemInvalidUTF8:
		return "invalid_utf8"
	case SemPanic:
		return "panic"
	case ErrClosed:
		return "engine_closed"
	default:
		return fmt.Sprintf("unknown_semantic_code(%d)", c)
	}
}

type CowError struct {
	Code    SemanticCode
	Action  Action
	RawRC   int32
	Message string
}

func (e *CowError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Message == "" {
		return fmt.Sprintf("cubecow error code=%s raw_rc=%d action=%s", e.Code, e.RawRC, e.Action)
	}
	return fmt.Sprintf("cubecow error code=%s raw_rc=%d action=%s: %s", e.Code, e.RawRC, e.Action, e.Message)
}

func MapError(rc int32) (SemanticCode, Action) {
	switch rc {
	case int32(SemOK):
		return SemOK, ActFail
	case int32(SemNotFound):
		return SemNotFound, ActFail
	case int32(SemAlreadyExists):
		return SemAlreadyExists, ActFail
	case int32(SemResourceExhausted):
		return SemResourceExhausted, ActFail
	case int32(SemInvalidArgument):
		return SemInvalidArgument, ActBug
	case int32(SemIoError):
		return SemIoError, ActMarkStorageUnhealthy
	case int32(SemConfigError):
		return SemConfigError, ActOpsIntervention
	case int32(SemPreconditionFailed):
		return SemPreconditionFailed, ActFail
	case int32(SemNullPointer):
		return SemNullPointer, ActBug
	case int32(SemInvalidUTF8):
		return SemInvalidUTF8, ActBug
	case int32(SemPanic):
		return SemPanic, ActMarkStoragePanic
	default:
		return SemanticCode(rc), ActFail
	}
}
