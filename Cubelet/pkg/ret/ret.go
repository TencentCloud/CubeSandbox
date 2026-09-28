// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package ret

import (
	"fmt"

	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
	"google.golang.org/protobuf/proto"
)

type Status struct {
	s *errorcode.Ret
}

func New(c errorcode.ErrorCode, msg string) *Status {
	return &Status{s: &errorcode.Ret{RetCode: c, RetMsg: msg}}
}

func Newf(c errorcode.ErrorCode, format string, a ...interface{}) *Status {
	return New(c, fmt.Sprintf(format, a...))
}

func FromProto(s *errorcode.Ret) *Status {
	return &Status{s: proto.Clone(s).(*errorcode.Ret)}
}

func Err(c errorcode.ErrorCode, msg string) error {
	return New(c, msg).Err()
}

func Errorf(c errorcode.ErrorCode, format string, a ...interface{}) error {
	return Err(c, fmt.Sprintf(format, a...))
}

func (s *Status) Code() errorcode.ErrorCode {
	if s == nil || s.s == nil {
		return errorcode.ErrorCode_Success
	}
	return s.s.RetCode
}

func (s *Status) Message() string {
	if s == nil || s.s == nil {
		return ""
	}
	return s.s.RetMsg
}

func (s *Status) Proto() *errorcode.Ret {
	if s == nil {
		return nil
	}
	return proto.Clone(s.s).(*errorcode.Ret)
}

func (s *Status) Err() error {
	if s.Code() == errorcode.ErrorCode_OK ||
		s.Code() == errorcode.ErrorCode_Success {
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

func (e *Error) Is(target error) bool {
	tse, ok := target.(*Error)
	if !ok {
		return false
	}
	return proto.Equal(e.s.s, tse.s.s)
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

func IsSuccessCode(code errorcode.ErrorCode) bool {
	return code == errorcode.ErrorCode_Success || code == errorcode.ErrorCode_OK
}

func IsErrorCode(err error, c errorcode.ErrorCode) bool {
	s, ok := FromError(err)
	return ok && s.Code() == c
}

func WrapWithDefaultError(err error, defaultErr errorcode.ErrorCode) error {
	if err == nil {
		return nil
	}
	_, ok := err.(*Error)
	if ok {
		return err
	}
	return Errorf(defaultErr, "%s", err.Error())
}

func FetchErrorCode(err error) string {
	s, ok := FromError(err)
	if ok {
		return s.Code().String()
	} else {
		return s.Message()
	}
}
