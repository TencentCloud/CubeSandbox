// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package common

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

// These tests lock the response contract that the gin migration must preserve:
// every business response is HTTP 200 + Content-Type "application/json"
// (FastestJsoniter, NOT gin's default c.JSON which emits
// "application/json; charset=utf-8" via encoding/json), carrying the
// {ret:{ret_code,ret_msg}} envelope.

func newGin(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	return gin.New()
}

func decodeEnvelope(t *testing.T, body []byte) *types.Res {
	t.Helper()
	var r struct {
		Ret *types.Ret `json:"ret"`
	}
	requireNoErr(t, json.Unmarshal(body, &r))
	return &types.Res{Ret: r.Ret}
}

func requireNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestWriteAPI_EnvelopeContract: success path is HTTP 200 + the JSON envelope
// via the FastestJsoniter write path (Content-Type exactly "application/json").
func TestWriteAPI_EnvelopeContract(t *testing.T) {
	r := newGin(t)
	r.GET("/x", func(c *gin.Context) {
		WriteAPI(c, &types.Res{Ret: &types.Ret{RetCode: 0, RetMsg: "ok"}})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	// NOT gin's default "application/json; charset=utf-8" — proves the custom
	// FastestJsoniter write path is used, not c.JSON.
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))

	res := decodeEnvelope(t, w.Body.Bytes())
	if assert.NotNil(t, res.Ret) {
		assert.Equal(t, 0, res.Ret.RetCode)
		assert.Equal(t, "ok", res.Ret.RetMsg)
	}
}

// TestWriteErr_EnvelopeContract: the error helper emits 200 + a business error
// code/msg in the same envelope (HTTP status stays 200 by contract).
func TestWriteErr_EnvelopeContract(t *testing.T) {
	r := newGin(t)
	r.GET("/x", func(c *gin.Context) {
		WriteErr(c, 130001, "bad input")
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
	res := decodeEnvelope(t, w.Body.Bytes())
	if assert.NotNil(t, res.Ret) {
		assert.Equal(t, 130001, res.Ret.RetCode)
		assert.Equal(t, "bad input", res.Ret.RetMsg)
	}
}

// TestWriteListAPI_UsesListEncoder: the list path routes through
// WriteListResponse (bufferpool-backed), still HTTP 200 + application/json.
func TestWriteListAPI_UsesListEncoder(t *testing.T) {
	r := newGin(t)
	r.GET("/x", func(c *gin.Context) {
		WriteListAPI(c, &types.Res{Ret: &types.Ret{RetCode: 0, RetMsg: "ok"}})
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
	// list encoder writes the same envelope JSON
	decodeEnvelope(t, w.Body.Bytes())
}
