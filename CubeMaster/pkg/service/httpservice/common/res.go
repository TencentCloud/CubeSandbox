// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package common

import (
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
	err := json.NewEncoder(buffer).Encode(data)
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

var reqRspPool bufferpool.BufferPool

func init() {
	reqRspPool = bufferpool.New(8192)
}
