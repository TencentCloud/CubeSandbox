// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package ret

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
)

func TestNew(t *testing.T) {
	c := errorcode.ErrorCode_Unknown
	msg := "test message"
	s := New(c, msg)
	assert.NotNil(t, s)
	assert.Equal(t, c, s.Code())
	assert.Equal(t, msg, s.Message())
}

func TestNewf(t *testing.T) {
	c := errorcode.ErrorCode_Unknown
	format := "test message %d"
	a := 1
	s := Newf(c, format, a)
	assert.NotNil(t, s)
	assert.Equal(t, c, s.Code())
	assert.Equal(t, "test message 1", s.Message())

	str := "test no format"
	s = Newf(c, "%s", str)
	assert.NotNil(t, s)
	assert.Equal(t, c, s.Code())
	assert.Equal(t, str, s.Message())
}

func TestErr(t *testing.T) {
	c := errorcode.ErrorCode_Unknown
	msg := "test message"
	err := Err(c, msg)
	assert.NotNil(t, err)
	assert.Equal(t, c, err.(*Error).GRPCStatus().Code())
	assert.Equal(t, msg, err.(*Error).GRPCStatus().Message())

	assert.Nil(t, Err(errorcode.ErrorCode_Success, msg))
}

func TestErrorf(t *testing.T) {
	c := errorcode.ErrorCode_Unknown
	format := "test message %d"
	a := 1
	err := Errorf(c, format, a)
	assert.NotNil(t, err)
	assert.Equal(t, c, err.(*Error).GRPCStatus().Code())
	assert.Equal(t, "test message 1", err.(*Error).GRPCStatus().Message())
}

func TestCode(t *testing.T) {
	c := errorcode.ErrorCode_Unknown
	msg := "test message"
	s := New(c, msg)
	assert.Equal(t, c, s.Code())

	s1 := (*Status)(nil)
	assert.Equal(t, errorcode.ErrorCode_Success, s1.Code())
}

func TestMessage(t *testing.T) {
	c := errorcode.ErrorCode_Unknown
	msg := "test message"
	s := New(c, msg)
	assert.Equal(t, msg, s.Message())

	s1 := (*Status)(nil)
	assert.Equal(t, "", s1.Message())
}

func TestError(t *testing.T) {
	c := errorcode.ErrorCode_Unknown
	msg := "test message"
	s := New(c, msg)
	err := &Error{s: s}
	assert.NotNil(t, err)
	assert.Equal(t, msg, err.Error())
}

func TestGRPCStatus(t *testing.T) {
	c := errorcode.ErrorCode_Unknown
	msg := "test message"
	s := New(c, msg)
	err := &Error{s: s}
	assert.NotNil(t, err.GRPCStatus())
	assert.Equal(t, c, err.GRPCStatus().Code())
	assert.Equal(t, msg, err.GRPCStatus().Message())
}

func TestFromError(t *testing.T) {
	c := errorcode.ErrorCode_ConnHostFailed
	msg := "test message"
	s := New(c, msg)
	err := &Error{s: s}
	s2, ok := FromError(err)
	assert.True(t, ok)
	assert.NotNil(t, s2)
	assert.Equal(t, errorcode.ErrorCode_ConnHostFailed, s2.Code())
	assert.Equal(t, msg, s2.Message())

	s3, ok := FromError(nil)
	assert.True(t, ok)
	assert.Nil(t, s3)

	normalErr := fmt.Errorf("some error")
	s4, ok := FromError(normalErr)
	assert.False(t, ok)
	assert.NotNil(t, s4)
	assert.Equal(t, errorcode.ErrorCode_Unknown, s4.Code())
}
