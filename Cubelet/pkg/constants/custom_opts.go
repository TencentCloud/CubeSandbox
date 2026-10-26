// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package constants

import (
	"context"
	"strings"
	"time"

	cubeimages "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/images/v1"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

type cubeRuntimeKey struct{}

func WithRuntimeType(ctx context.Context, t string) context.Context {
	if strings.HasPrefix(t, "io.containerd.cube") {
		return context.WithValue(ctx, cubeRuntimeKey{}, struct{}{})
	}

	return ctx
}

func IsCubeRuntime(ctx context.Context) bool {
	return ctx.Value(cubeRuntimeKey{}) != nil
}

type runtimeOptionKey struct{}

type CubeRuntimeOption struct {
	PerPodShim bool

	SandboxId string
	Address   string
}

func WithCubeRuntimeOption(ctx context.Context, sandboxId string) context.Context {
	opt := ctx.Value(runtimeOptionKey{})
	if opt != nil {
		copt := opt.(*CubeRuntimeOption)
		copt.PerPodShim = true
		copt.SandboxId = sandboxId
		return ctx
	}

	copt := &CubeRuntimeOption{
		PerPodShim: true,
		SandboxId:  sandboxId,
	}
	return context.WithValue(ctx, runtimeOptionKey{}, copt)
}

func GetCubeRuntimeOption(ctx context.Context) *CubeRuntimeOption {
	v := ctx.Value(runtimeOptionKey{})
	if v == nil {
		return nil
	}
	return v.(*CubeRuntimeOption)
}

type failoverOperationKey struct{}

func WithFailoverOperation(ctx context.Context) context.Context {
	return context.WithValue(ctx, failoverOperationKey{}, struct{}{})
}

func IsFailoverOperation(ctx context.Context) bool {
	return ctx.Value(failoverOperationKey{}) != nil
}

type collectMemoryKey struct{}

func WithCollectMemory(ctx context.Context) context.Context {
	return context.WithValue(ctx, collectMemoryKey{}, struct{}{})
}

func IsCollectMemory(ctx context.Context) bool {
	return ctx.Value(collectMemoryKey{}) != nil
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

func GetProxyUserAppID(ctx context.Context) int64 {
	v := ctx.Value(proxyUserAppID{})
	if v == nil {
		return 0
	}
	return v.(int64)
}

type region struct{}

func WithRegion(ctx context.Context, value string) context.Context {
	return context.WithValue(ctx, region{}, value)
}

func GetRegion(ctx context.Context) string {
	v := ctx.Value(region{})
	if v == nil {
		return ""
	}
	return v.(string)
}

type disable_vm_cgroup struct{}

func WithDisableVMCgroup(ctx context.Context, value bool) context.Context {
	return context.WithValue(ctx, disable_vm_cgroup{}, value)
}

func GetDisableVMCgroup(ctx context.Context) bool {
	v := ctx.Value(disable_vm_cgroup{})
	if v == nil {
		return false
	}
	return v.(bool)
}

type disable_host_cgroup struct{}

func WithDisableHostCgroup(ctx context.Context, value bool) context.Context {
	return context.WithValue(ctx, disable_host_cgroup{}, value)
}

func GetDisableHostCgroup(ctx context.Context) bool {
	v := ctx.Value(disable_host_cgroup{})
	if v == nil {
		return false
	}
	return v.(bool)
}

type appImageIDKey struct{}

func WithAppImageID(ctx context.Context, ID string) context.Context {
	return context.WithValue(ctx, appImageIDKey{}, ID)
}

func GetAppImageID(ctx context.Context) string {
	v := ctx.Value(appImageIDKey{})
	if v == nil {
		return ""
	}
	return v.(string)
}

type imageSpecKey struct{}

func WithImageSpec(ctx context.Context, imageReq *cubeimages.ImageSpec) context.Context {
	return context.WithValue(ctx, imageSpecKey{}, imageReq)
}

func GetImageSpec(ctx context.Context) *cubeimages.ImageSpec {
	v := ctx.Value(imageSpecKey{})
	if v == nil {
		return nil
	}
	return v.(*cubeimages.ImageSpec)
}

type funcTypeKey struct{}

func WithFuncType(ctx context.Context, value string) context.Context {
	return context.WithValue(ctx, funcTypeKey{}, value)
}
func GetFuncType(ctx context.Context) string {
	v := ctx.Value(funcTypeKey{})
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

type cubeboxCreatedKey struct{}

func WithCubeboxCreated(ctx context.Context) context.Context {
	return context.WithValue(ctx, cubeboxCreatedKey{}, struct{}{})
}

func IsCubeboxCreated(ctx context.Context) bool {
	return ctx.Value(cubeboxCreatedKey{}) != nil
}

type imageCredentialsKey struct{}

func WithImageCredentials(ctx context.Context, authConfig *runtime.AuthConfig) context.Context {
	return context.WithValue(ctx, imageCredentialsKey{}, authConfig)
}

func GetImageCredentials(ctx context.Context) *runtime.AuthConfig {
	v := ctx.Value(imageCredentialsKey{})
	if v == nil {
		return nil
	}
	return v.(*runtime.AuthConfig)
}

type terminatingPodKey struct{}

func WithTerminatingPod(ctx context.Context) context.Context {
	return context.WithValue(ctx, terminatingPodKey{}, struct{}{})
}

func IsTerminatingPod(ctx context.Context) bool {
	return ctx.Value(terminatingPodKey{}) != nil
}

type startPullImageTimeKey struct{}

func WithStartPullImageTime(ctx context.Context, startPullImageTime *time.Time) context.Context {
	return context.WithValue(ctx, startPullImageTimeKey{}, startPullImageTime)
}

func GetStartPullImageTime(ctx context.Context) *time.Time {
	v := ctx.Value(startPullImageTimeKey{})
	if v == nil {
		return nil
	}
	return v.(*time.Time)
}

type sandboxContainer struct{}

func WithSandboxContainer(ctx context.Context) context.Context {
	return context.WithValue(ctx, sandboxContainer{}, struct{}{})
}

func IsSandboxContainer(ctx context.Context) bool {
	return ctx.Value(sandboxContainer{}) != nil
}

type preStopTypeKey struct{}

func WithPreStopType(ctx context.Context, value string) context.Context {
	return context.WithValue(ctx, preStopTypeKey{}, value)
}
func GetPreStopType(ctx context.Context) string {
	v := ctx.Value(preStopTypeKey{})
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

type skipRuntimeAPI struct{}

func WithSkipRuntimeAPI(ctx context.Context) context.Context {
	return context.WithValue(ctx, skipRuntimeAPI{}, true)
}

func SkipRuntimeAPI(ctx context.Context) bool {
	v, ok := ctx.Value(skipRuntimeAPI{}).(bool)
	if !ok {
		return false
	}
	return v
}
