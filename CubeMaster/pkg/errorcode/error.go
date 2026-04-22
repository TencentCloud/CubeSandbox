// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package errorcode provides error code definition.
package errorcode

import (
	"sync"

	cubeleterrorcode "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
)

type ErrorCode cubeleterrorcode.ErrorCode

var (
	ErrorCode_Unknown                ErrorCode = -1
	ErrorCode_Success                ErrorCode = 200
	ErrorCode_GWFailed               ErrorCode = 130599
	ErrorCode_SelectNodesFailed      ErrorCode = 130598
	ErrorCode_SelectNodesNoRes       ErrorCode = 130597
	ErrorCode_ConnHostFailed         ErrorCode = 130596
	ErrorCode_ReqCubeAPIFailed       ErrorCode = 130595
	ErrorCode_DBError                ErrorCode = 130594
	ErrorCode_MasterInternalError    ErrorCode = 130593
	ErrorCode_MasterRateLimitedError ErrorCode = 130592

	ErrorCode_MasterParamsError ErrorCode = 130400
	ErrorCode_AuthFailed        ErrorCode = 130401
	ErrorCode_NotFound          ErrorCode = 130404
	ErrorCode_NotFoundAtCubelet ErrorCode = 130406
	ErrorCode_CubeletUnHealthy  ErrorCode = 130408
	ErrorCode_TooManyRequests   ErrorCode = 130429
	ErrorCode_ClientCancel      ErrorCode = 130499

	errorCode_value = map[string]int32{
		"Success":                200,
		"GWFailed":               130599,
		"SelectNodesFailed":      130598,
		"SelectNodesNoRes":       130597,
		"ConnHostFailed":         130596,
		"ReqCubeAPIFailed":       130595,
		"DBError":                130594,
		"MasterInternalError":    130593,
		"MasterRateLimitedError": 130592,
		"MasterParamsError":      130400,
		"NotFound":               130404,
		"NotFoundAtCubelet":      130406,
		"TooManyRequests":        130429,
		"ClientCancel":           130499,
		"AuthFailed":             130401,
	}
	errorCode_name = map[int32]string{
		200:    "Success",
		130599: "GWFailed",
		130598: "SelectNodesFailed",
		130597: "SelectNodesNoRes",
		130596: "ConnHostFailed",
		130595: "ReqCubeAPIFailed",
		130594: "DBError",
		130593: "MasterInternalError",
		130592: "MasterRateLimitedError",
		130400: "MasterParamsError",
		130401: "AuthFailed",
		130404: "NotFound",
		130406: "NotFoundAtCubelet",
		130429: "TooManyRequests",
		130499: "ClientCancel",
	}
)

func (x ErrorCode) String() string {
	return errorCode_name[int32(x)]
}

func MasterCode(x cubeleterrorcode.ErrorCode) ErrorCode {
	return ErrorCode(x)
}

var cubeCodeRetryMap *sync.Map

var cubeCodeLoopRetryMap *sync.Map

var cubeReuseCodeRetryMap *sync.Map

var excludeLoopRetryCodes *sync.Map

var cubecircuitBreakerCodeMap *sync.Map

var cubeBackoffCodeMap *sync.Map

type listenConfig struct {
}

func (l *listenConfig) OnEvent(data *config.Config) {
	if data == nil {
		return
	}
	initMap(data)
}

var l listenConfig

func InitCubeCodeRetryMap(cfg *config.Config) {
	config.AppendConfigWatcher(&l)
	initMap(cfg)
}

func initMap(cfg *config.Config) {

	initRetryMap(cfg)

	initReuseMap(cfg)

	initCirteBreakerCodeMap(cfg)

	initLoopRetryMap(cfg)

	initExcludesRetryCodeMap(cfg)

	initBackoffRetryCodeMap(cfg)
}

func initRetryMap(cfg *config.Config) {
	retryMap := new(sync.Map)
	retryMap.Store(ErrorCode_ConnHostFailed, struct{}{})
	retryMap.Store(MasterCode(cubeleterrorcode.ErrorCode_PullImageFailed), struct{}{})
	retryMap.Store(MasterCode(cubeleterrorcode.ErrorCode_DownloadCodeFailed), struct{}{})
	retryMap.Store(MasterCode(cubeleterrorcode.ErrorCode_DownloadLayerFailed), struct{}{})
	retryMap.Store(MasterCode(cubeleterrorcode.ErrorCode_CreateImageFailed), struct{}{})
	retryMap.Store(MasterCode(cubeleterrorcode.ErrorCode_DestroyNetworkFailed), struct{}{})
	retryMap.Store(MasterCode(cubeleterrorcode.ErrorCode_DestroyCgroupFailed), struct{}{})
	retryMap.Store(MasterCode(cubeleterrorcode.ErrorCode_DestroyStorageFailed), struct{}{})

	retryCodes := cfg.CubeletConf.RetryCode
	for _, code := range retryCodes {

		errorCode, ok := cubeleterrorcode.ErrorCode_value[code]
		if ok {
			retryMap.Store(ErrorCode(errorCode), struct{}{})
		}

		errorCode, ok = errorCode_value[code]
		if ok {
			retryMap.Store(ErrorCode(errorCode), struct{}{})
		}
	}
	cubeCodeRetryMap = retryMap
}

func initReuseMap(cfg *config.Config) {
	reuseRetryMap := new(sync.Map)
	reuseRetryMap.Store(MasterCode(cubeleterrorcode.ErrorCode_DeployLayerTimeout), struct{}{})
	reuseRetryMap.Store(MasterCode(cubeleterrorcode.ErrorCode_DeployCodeTimeout), struct{}{})
	reuseRetryMap.Store(MasterCode(cubeleterrorcode.ErrorCode_PullImageTimeOut), struct{}{})
	reuseRetryCodes := cfg.CubeletConf.ReuseRetryCode
	for _, code := range reuseRetryCodes {

		errorCode, ok := cubeleterrorcode.ErrorCode_value[code]
		if ok {
			reuseRetryMap.Store(ErrorCode(errorCode), struct{}{})
		}

		errorCode, ok = errorCode_value[code]
		if ok {
			reuseRetryMap.Store(ErrorCode(errorCode), struct{}{})
		}
	}
	cubeReuseCodeRetryMap = reuseRetryMap
}

func initLoopRetryMap(cfg *config.Config) {
	loopRetryMap := new(sync.Map)
	loopRetryMap.Store(ErrorCode_MasterRateLimitedError, struct{}{})
	loopRetryMap.Store(MasterCode(cubeleterrorcode.ErrorCode_ConcurrentLimited), struct{}{})
	loopRetryMap.Store(MasterCode(cubeleterrorcode.ErrorCode_ConcurrentFailed), struct{}{})
	loopRetryMap.Store(MasterCode(cubeleterrorcode.ErrorCode_DeployLayerTimeout), struct{}{})
	loopRetryMap.Store(MasterCode(cubeleterrorcode.ErrorCode_DeployCodeTimeout), struct{}{})
	loopRetryMap.Store(MasterCode(cubeleterrorcode.ErrorCode_PullImageTimeOut), struct{}{})
	loopRetryMap.Store(MasterCode(cubeleterrorcode.ErrorCode_HostDiskNotEnough), struct{}{})
	loopRetryMap.Store(MasterCode(cubeleterrorcode.ErrorCode_NoSpaceLeftOnDevice), struct{}{})
	loopRetryCodes := cfg.CubeletConf.LoopRetryCode
	for _, code := range loopRetryCodes {

		errorCode, ok := cubeleterrorcode.ErrorCode_value[code]
		if ok {
			loopRetryMap.Store(ErrorCode(errorCode), struct{}{})
		}

		errorCode, ok = errorCode_value[code]
		if ok {
			loopRetryMap.Store(ErrorCode(errorCode), struct{}{})
		}
	}
	cubeCodeLoopRetryMap = loopRetryMap
}
func initCirteBreakerCodeMap(cfg *config.Config) {
	cbMap := new(sync.Map)
	cbMap.Store(MasterCode(cubeleterrorcode.ErrorCode_HostDiskNotEnough), struct{}{})
	cbMap.Store(MasterCode(cubeleterrorcode.ErrorCode_NoSpaceLeftOnDevice), struct{}{})
	cbMap.Store(ErrorCode_ConnHostFailed, struct{}{})
	cbMap.Store(ErrorCode_ReqCubeAPIFailed, struct{}{})
	circuitCodes := cfg.CubeletConf.CircuitBreakCode
	for _, code := range circuitCodes {

		errorCode, ok := cubeleterrorcode.ErrorCode_value[code]
		if ok {
			cbMap.Store(ErrorCode(errorCode), struct{}{})
		}

		errorCode, ok = errorCode_value[code]
		if ok {
			cbMap.Store(ErrorCode(errorCode), struct{}{})
		}
	}
	cubecircuitBreakerCodeMap = cbMap
}

func initExcludesRetryCodeMap(cfg *config.Config) {
	excludeloopCodeMap := new(sync.Map)
	excludeloopCodeMap.Store(ErrorCode_ClientCancel, struct{}{})
	excludeLoopCodes := cfg.CubeletConf.ExcludeLoopRetryCode
	for _, code := range excludeLoopCodes {

		errorCode, ok := cubeleterrorcode.ErrorCode_value[code]
		if ok {
			excludeloopCodeMap.Store(ErrorCode(errorCode), struct{}{})
		}

		errorCode, ok = errorCode_value[code]
		if ok {
			excludeloopCodeMap.Store(ErrorCode(errorCode), struct{}{})
		}
	}
	excludeLoopRetryCodes = excludeloopCodeMap
}

func initBackoffRetryCodeMap(cfg *config.Config) {
	backoffCodeMap := new(sync.Map)
	backoffCodeMap.Store(MasterCode(cubeleterrorcode.ErrorCode_ConcurrentLimited), struct{}{})
	backoffCodeMap.Store(MasterCode(cubeleterrorcode.ErrorCode_ConcurrentFailed), struct{}{})
	for _, code := range cfg.CubeletConf.BackoffRetryCode {

		errorCode, ok := cubeleterrorcode.ErrorCode_value[code]
		if ok {
			backoffCodeMap.Store(ErrorCode(errorCode), struct{}{})
		}

		errorCode, ok = errorCode_value[code]
		if ok {
			backoffCodeMap.Store(ErrorCode(errorCode), struct{}{})
		}
	}
	cubeBackoffCodeMap = backoffCodeMap
}

func IsRetryCode(code ErrorCode) bool {
	_, ok := cubeCodeRetryMap.Load(code)
	return ok
}

func IsLoopRetryCode(code ErrorCode) bool {
	_, ok := cubeCodeLoopRetryMap.Load(code)
	return ok
}

func IsReuseCode(code ErrorCode) bool {
	_, ok := cubeReuseCodeRetryMap.Load(code)
	return ok
}

func IsCircuitBreakCode(code ErrorCode) bool {
	_, ok := cubecircuitBreakerCodeMap.Load(code)
	return ok
}

func IsExcludesRetryCode(code ErrorCode) bool {
	_, ok := excludeLoopRetryCodes.Load(code)
	return ok
}

func IsBackoffRetryCode(code ErrorCode) bool {
	_, ok := cubeBackoffCodeMap.Load(code)
	return ok
}
