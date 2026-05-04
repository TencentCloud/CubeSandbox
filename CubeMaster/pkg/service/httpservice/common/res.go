// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package common

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/bufferpool"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

func WriteResponse(w http.ResponseWriter, code int, data interface{}) {
	w.WriteHeader(code)
	w.Header().Set("Content-Type", "application/json")
	d, _ := types.Fastestjson.Marshal(data)
	w.Write(d)
}

func WriteListResponse(w http.ResponseWriter, code int, data interface{}) {
	buffer := reqRspPool.Get()
	defer func() {
		if buffer != nil {
			reqRspPool.Put(buffer)
		}
	}()

	enc := json.NewEncoder(buffer)
	enc.SetEscapeHTML(false)
	err := enc.Encode(data)
	if err != nil {
		CubeLog.Fatalf("WriteListResponse fail:%v", err)
		WriteResponse(w, http.StatusOK, &types.Res{
			Ret: &types.Ret{
				RetCode: -1,
				RetMsg:  http.StatusText(http.StatusInternalServerError),
			},
		})
		return
	}
	w.WriteHeader(code)
	w.Header().Set("Content-Type", "application/json")
	w.Write(buffer.Bytes())
}

func WithStartTime(ctx context.Context, start time.Time) context.Context {
	return context.WithValue(ctx, types.StartTime, start)
}

func GetStartTime(ctx context.Context) time.Time {
	old := ctx.Value(types.StartTime)
	if old != nil {
		return old.(time.Time)
	}
	return time.Now()
}

func GetRetCodeByReflect(res interface{}) int {
	if res == nil {
		return 0
	}
	responseValue := reflect.ValueOf(res)
	if responseValue.Kind() == reflect.Ptr {
		responseValue = reflect.ValueOf(res).Elem()
	}

	if responseValue.Kind() != reflect.Struct {
		return 0
	}
	retField := responseValue.FieldByName("Ret")

	if retField.Kind() == reflect.Struct {
		retCodeField := retField.FieldByName("RetCode")
		if retCodeField.IsValid() {

			retCode := retCodeField.Int()
			return int(retCode)
		}
	}
	if retField.IsValid() && !retField.IsNil() {
		retCodeField := retField.Elem().FieldByName("RetCode")
		if retCodeField.IsValid() {

			retCode := retCodeField.Int()
			return int(retCode)
		}
	}
	return 0
}

func GetBodyReq(r *http.Request, object interface{}) (err error) {
	buffer := reqRspPool.Get()
	defer func() {
		if buffer != nil {
			reqRspPool.Put(buffer)
		}
	}()

	_, err = io.Copy(buffer, r.Body)
	if err != nil {
		return err
	}
	err = types.Fastestjson.Unmarshal(buffer.Bytes(), object)
	if err != nil {
		return err
	}
	return nil
}

type stdJSONTool struct{}

func (stdJSONTool) Marshal(v interface{}) ([]byte, error) {
	buffer := bytes.NewBuffer(nil)
	enc := json.NewEncoder(buffer)
	enc.SetEscapeHTML(false)

	if err := enc.Encode(v); err != nil {
		return nil, err
	}

	// json.Encoder adds a trailing newline; trim it to behave like jsoniter.Marshal.
	out := buffer.Bytes()
	if n := len(out); n > 0 && out[n-1] == '\n' {
		out = out[:n-1]
	}
	return out, nil
}

func (stdJSONTool) Unmarshal(data []byte, v interface{}) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode(v)
}

var FastestJsoniter = stdJSONTool{}

var reqRspPool bufferpool.BufferPool

func init() {
	reqRspPool = bufferpool.New(8192)
}
