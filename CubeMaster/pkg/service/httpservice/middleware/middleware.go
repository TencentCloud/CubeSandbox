// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package middleware provides http useful middleware.
package middleware

import (
	"context"
	"net"
	"net/http"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/auth"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/ret"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
)

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
		log.G(ctx).Debugf("http_request_comming: %v", utils.InterfaceToString(sgnp))
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
