// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package middleware

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"net/http/httputil"
	"runtime/debug"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/ret"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

// GinRequestMiddleware is the Gin adaptation of MiddlewareLogging.
// It performs request trace setup, context enrichment, debug dump,
// panic recovery, mock-mode short-circuit, and authentication —
// then calls c.Next() to proceed to the matched handler.
func GinRequestMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		rt := &CubeLog.RequestTrace{
			Action:         c.Request.Method,
			CallerIP:       c.Request.RemoteAddr,
			Caller:         getCaller(c.Request),
			Callee:         constants.CubeMasterServiceID,
			CalleeAction:   c.Request.URL.Path,
			CalleeEndpoint: "localhost",
		}

		// Context setup (identical to the original MiddlewareLogging)
		ctx := getHTTPUA(c.Request.Context(), c.Request)
		if callerHostIP := getCallerHostIP(c.Request); callerHostIP != "" {
			ctx = constants.WithHostIP(ctx, callerHostIP)
		}
		ctx = CubeLog.WithRequestTrace(ctx, rt)
		ctx = log.WithLogger(ctx, CubeLog.WithContext(ctx))
		c.Request = c.Request.WithContext(ctx)

		var dump []byte
		if log.IsDebug() {
			dump, _ = httputil.DumpRequest(c.Request, config.GetConfig().Common.DebugDumpHttpBody)
		}

		defer func() {
			if err := recover(); err != nil {
				log.G(ctx).Fatalf("HandlerFunc panic:%s", string(debug.Stack()))
				common.WriteAPI(c, &types.Res{
					Ret: &types.Ret{
						RetCode: -1,
						RetMsg:  http.StatusText(http.StatusInternalServerError),
					},
				})
				c.Abort()
			}
			rt.Cost = time.Since(start)
			select {
			case <-ctx.Done():
				if errors.Is(ctx.Err(), context.Canceled) {
					rt.RetCode = int64(errorcode.ErrorCode_ClientCancel)
				}
			default:
			}
			CubeLog.Trace(rt)
			if log.IsDebug() {
				log.G(ctx).WithFields(map[string]interface{}{
					"CallerIP":  c.Request.RemoteAddr,
					"RequestId": rt.RequestID,
				}).Debugf("http_request_coming: %s", string(dump))
			}
		}()

		// Mock mode
		if config.GetConfig().Common.MockHttpDirect {
			common.WriteAPI(c, &types.Res{
				Ret: &types.Ret{
					RetCode: int(errorcode.ErrorCode_Success),
					RetMsg:  errorcode.ErrorCode_Success.String(),
				},
			})
			time.Sleep(time.Duration(1+rand.IntN(2)) * time.Millisecond)
			c.Abort()
			return
		}

		// Authentication
		if err := checkAuth(ctx, c.Request); err != nil {
			status, _ := ret.FromError(err)
			rt.RetCode = int64(status.Code())
			common.WriteAPI(c, &types.Res{
				Ret: &types.Ret{
					RetCode: int(status.Code()),
					RetMsg:  status.Message(),
				},
			})
			c.Abort()
			return
		}

		c.Next()
	}
}
