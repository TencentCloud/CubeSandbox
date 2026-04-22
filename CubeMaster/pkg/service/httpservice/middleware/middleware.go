// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package middleware provides http useful middleware.
package middleware

import (
	"context"
	"errors"
	"math/rand"
	"net"
	"net/http"
	"net/http/httputil"
	"runtime/debug"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/auth"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/ret"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

func MiddlewareLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rt := &CubeLog.RequestTrace{
			Action:         r.Method,
			CallerIP:       r.RemoteAddr,
			Caller:         getCaller(r),
			Callee:         constants.CubeMasterServiceID,
			CalleeAction:   r.URL.Path,
			CalleeEndpoint: "localhost",
		}
		ctx := getHTTPUA(r.Context(), r)
		if callerHostIP := getCallerHostIP(r); callerHostIP != "" {
			ctx = constants.WithHostIP(ctx, callerHostIP)
		}
		ctx = CubeLog.WithRequestTrace(ctx, rt)
		ctx = log.WithLogger(ctx, CubeLog.WithContext(ctx))

		var dump []byte
		if log.IsDebug() {
			dump, _ = httputil.DumpRequest(r, config.GetConfig().Common.DebugDumpHttpBody)
		}
		defer func() {
			if err := recover(); err != nil {
				log.G(ctx).Fatalf("HandlerFunc panic:%s", string(debug.Stack()))
				common.WriteResponse(w, http.StatusOK, &types.Res{
					Ret: &types.Ret{
						RetCode: -1,
						RetMsg:  http.StatusText(http.StatusInternalServerError),
					},
				})
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
					"CallerIP":  r.RemoteAddr,
					"RequestId": rt.RequestID,
				}).Debugf("http_request_coming: %s", string(dump))
			}
		}()

		if config.GetConfig().Common.MockHttpDirect {
			common.WriteResponse(w, http.StatusOK, &types.Res{
				Ret: &types.Ret{
					RetCode: int(errorcode.ErrorCode_Success),
					RetMsg:  errorcode.ErrorCode_Success.String(),
				},
			})
			time.Sleep(time.Duration(1+rand.Intn(2)) * time.Millisecond)
			return
		}

		if err := checkAuth(ctx, r); err != nil {
			status, _ := ret.FromError(err)
			rt.RetCode = int64(status.Code())
			common.WriteResponse(w, http.StatusOK, &types.Res{
				Ret: &types.Ret{
					RetCode: int(status.Code()),
					RetMsg:  status.Message(),
				},
			})
			return
		}

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func getHTTPUA(ctx context.Context, r *http.Request) context.Context {
	if caller := getCaller(r); caller != "" {
		return constants.WithUA(ctx, caller)
	}
	ua := r.Header.Get(constants.AuthUserID)
	if ua == "" {
		ua = cubebox.InstanceType_cubebox.String()
	}
	return constants.WithUA(ctx, ua)
}

func getCaller(r *http.Request) string {
	if v := r.Header.Get(constants.Caller); v != "" {
		return v
	}
	return constants.Caller
}

func getCallerHostIP(r *http.Request) string {
	if v := r.Header.Get(constants.CallerHostIP); v != "" {
		return v
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return ""
}

func checkAuth(ctx context.Context, r *http.Request) error {
	if !config.GetConfig().AuthConf.Enable {
		return nil
	}

	userID := r.Header.Get(constants.AuthUserID)
	secretKey, ok := lookupSecretKeyByUserID(config.GetConfig().AuthConf.SecretKeyMap, userID)
	if !ok || secretKey == "" {
		return ret.Err(errorcode.ErrorCode_AuthFailed, "no secret key for userID: "+userID)
	}

	sign := r.Header.Get(constants.AuthSignature)
	if sign == "" {
		return ret.Err(errorcode.ErrorCode_AuthFailed, "signature is empty")
	}

	sgnp := &auth.SignatureParams{
		Version:   r.Header.Get(constants.AuthCubeVersion),
		UserID:    userID,
		Timestamp: r.Header.Get(constants.AuthTimestamp),
		Nonce:     r.Header.Get(constants.AuthNonce),
		SgnMethod: r.Header.Get(constants.AuthSignatureMethod),
		Signature: sign,
	}

	if sgnp.Version == "" {
		sgnp.Version = auth.DefaultVersion
	}

	if sgnp.SgnMethod == "" {
		sgnp.SgnMethod = auth.SHA1
	}

	if log.IsDebug() {
		log.G(ctx).Debugf("http_request_coming: %v", utils.InterfaceToString(sgnp))
	}
	err := auth.CheckSign(sgnp, []byte(secretKey), config.GetConfig().AuthConf.SignatureExpireTimeInsec)
	if err != nil {
		return ret.Err(errorcode.ErrorCode_AuthFailed, err.Error())
	}
	return nil
}

func lookupSecretKeyByUserID(secretKeyMap map[string]map[string]string, userID string) (string, bool) {
	for _, userSecrets := range secretKeyMap {
		if secretKey, ok := userSecrets[userID]; ok && secretKey != "" {
			return secretKey, true
		}
	}
	return "", false
}
