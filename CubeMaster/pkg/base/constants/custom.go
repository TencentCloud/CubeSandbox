// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package constants

import (
	"context"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
)

type callerKey struct{}

func WithCallerContext(ctx context.Context, caller string) context.Context {
	return context.WithValue(ctx, callerKey{}, caller)
}

func CurrentCaller(ctx context.Context) string {
	caller, _ := ctx.Value(callerKey{}).(string)
	return caller
}

type dataDisksKey struct{}

func WithDataDisks(ctx context.Context) context.Context {
	return context.WithValue(ctx, dataDisksKey{}, struct{}{})
}

func IsWithDataDisk(ctx context.Context) bool {
	return ctx.Value(dataDisksKey{}) != nil
}

type affinityNodeSelectorKey struct{}

func WithNodeSelector(ctx context.Context, value interface{}) context.Context {
	return context.WithValue(ctx, affinityNodeSelectorKey{}, value)
}

func GetNodeSelector(ctx context.Context) interface{} {
	return ctx.Value(affinityNodeSelectorKey{})
}

type backoffAffinityNodeSelectorKey struct{}

func WithBackoffNodeSelector(ctx context.Context, value interface{}) context.Context {
	return context.WithValue(ctx, backoffAffinityNodeSelectorKey{}, value)
}

func GetBackoffNodeSelector(ctx context.Context) interface{} {
	return ctx.Value(backoffAffinityNodeSelectorKey{})
}

type affinityPreferredSchedulingTermsKey struct{}

func WithPreferredSchedulingTerms(ctx context.Context, value interface{}) context.Context {
	return context.WithValue(ctx, affinityPreferredSchedulingTermsKey{}, value)
}

func GetPreferredSchedulingTerms(ctx context.Context) interface{} {
	return ctx.Value(affinityPreferredSchedulingTermsKey{})
}

type hostIPKey struct{}

func WithHostIP(ctx context.Context, value string) context.Context {
	return context.WithValue(ctx, hostIPKey{}, value)
}

func GetHostIP(ctx context.Context) string {
	v := ctx.Value(hostIPKey{})
	if v == nil {
		return ""
	}
	return v.(string)
}

type insUserDataKey struct{}

func WithUserData(ctx context.Context, value string) context.Context {
	return context.WithValue(ctx, insUserDataKey{}, value)
}

func GetUserData(ctx context.Context) string {
	v := ctx.Value(insUserDataKey{})
	if v == nil {
		return ""
	}
	return v.(string)
}

type proxyUserUin struct{}

func WithProxyUserUin(ctx context.Context, value string) context.Context {
	return context.WithValue(ctx, proxyUserUin{}, value)
}

func GetProxyUserUin(ctx context.Context) string {
	v := ctx.Value(proxyUserUin{})
	if v == nil {
		return ""
	}
	return v.(string)
}

type proxySubAccountUin struct{}

func WithProxySubAccountUin(ctx context.Context, value string) context.Context {
	return context.WithValue(ctx, proxySubAccountUin{}, value)
}

func GetProxySubAccountUin(ctx context.Context) string {
	v := ctx.Value(proxySubAccountUin{})
	if v == nil {
		return ""
	}
	return v.(string)
}

type proxyUserAppID struct{}

func WithProxyUserAppID(ctx context.Context, value int64) context.Context {
	return context.WithValue(ctx, proxyUserAppID{}, value)
}

func GetProxyUserAppID(ctx context.Context) *int64 {
	v := ctx.Value(proxyUserAppID{})
	if v == nil {
		return nil
	}
	return utils.Int64Ptr(v.(int64))
}

type podIPKey struct{}

func WithPodIP(ctx context.Context, value string) context.Context {
	return context.WithValue(ctx, podIPKey{}, value)
}

func GetPodIP(ctx context.Context) string {
	v := ctx.Value(podIPKey{})
	if v == nil {
		return ""
	}
	return v.(string)
}

type podIDKey struct{}

func WithPodID(ctx context.Context, value string) context.Context {
	return context.WithValue(ctx, podIDKey{}, value)
}

func GetPodID(ctx context.Context) string {
	v := ctx.Value(podIDKey{})
	if v == nil {
		return ""
	}
	return v.(string)
}

type uaKey struct{}

func WithUA(ctx context.Context, value string) context.Context {
	return context.WithValue(ctx, uaKey{}, value)
}

func GetUA(ctx context.Context) string {
	v := ctx.Value(uaKey{})
	if v == nil {
		return ""
	}
	return v.(string)
}
