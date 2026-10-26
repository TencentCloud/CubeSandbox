// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package ret provides a type for representing the return value of a function.
package ret

import (
	"fmt"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
)

type Status struct {
	RetCode errorcode.ErrorCode
	RetMsg  string
}

func New(c errorcode.ErrorCode, msg string) *Status {
	return &Status{RetCode: c, RetMsg: msg}
}

func Newf(c errorcode.ErrorCode, format string, a ...interface{}) *Status {
	return New(c, fmt.Sprintf(format, a...))
}

func Err(c errorcode.ErrorCode, msg string) error {
	return New(c, msg).Err()
}

func Errorf(c errorcode.ErrorCode, format string, a ...interface{}) error {
	return Err(c, fmt.Sprintf(format, a...))
}

func (s *Status) Code() errorcode.ErrorCode {
	if s == nil {
		return 200
	}
	return s.RetCode
}

func (s *Status) Message() string {
	if s == nil {
		return ""
	}
	return s.RetMsg
}

func (s *Status) Err() error {
	if s.Code() == errorcode.ErrorCode_Success || s.Code() == errorcode.ErrorCode(0) {
		return nil
	}
	return &Error{s: s}
}

type Error struct {
	s *Status
}

func (e *Error) Error() string {
	return e.s.Message()
}

func (e *Error) GRPCStatus() *Status {
	return e.s
}

func FromError(err error) (s *Status, ok bool) {
	if err == nil {
		return nil, true
	}
	if se, ok := err.(interface {
		GRPCStatus() *Status
	}); ok {
		return se.GRPCStatus(), true
	}
	return New(errorcode.ErrorCode_Unknown, err.Error()), false
}
