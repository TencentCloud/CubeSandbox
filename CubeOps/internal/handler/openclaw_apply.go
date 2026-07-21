// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package handler contains gin HTTP handlers for CubeOps.
//
// This file previously held the OpenClaw apply/runtime orchestration logic
// (LLM config resolution, envd command execution, host-side state management,
// OpenClaw restart/upgrade/apply scripts). That logic has been extracted to
// package service (internal/service/openclaw.go) so it can be unit-tested
// independently of gin.
//
// The aliases below preserve the original unexported names used throughout
// agenthub.go, so the rest of the handler package compiles without churn.
// New handler code should be written against the service.* API directly;
// these aliases exist purely to keep this refactoring commit small.
package handler

import (
	"net/http"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/service"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/store"
)

// Type aliases (struct kinds) — keep the original unexported names so the
// existing handler call sites compile unchanged.
type (
	CommandOutput        = service.CommandOutput
	llmConfig            = service.LLMConfig
	llmRuntimePlan       = service.LLMRuntimePlan
	openclawApplyMode    = service.OpenclawApplyMode
	openclawApplyOptions = service.OpenclawApplyOptions
)

// Constant aliases.
const (
	applyModeFullInit        = service.ApplyModeFullInit
	applyModeMergeLLM        = service.ApplyModeMergeLLM
	openclawUIPort           = service.OpenclawUIPort
	hostdirMountKey          = service.HostdirMountKey
	defaultLLMProvider       = service.DefaultLLMProviderStr
	defaultLLMBaseURL        = service.DefaultLLMBaseURLStr
	defaultLLMCredentialMode = service.DefaultLLMCredentialModeStr
	defaultOpenclawModel     = service.DefaultLLMModelStr
)

// Function aliases — preserved for the existing call sites in agenthub.go.
// These are unexported vars bound to the corresponding service.* functions.
var (
	envdHTTPClient             = service.EnvdHTTPClient()
	restartOpenclawForInstance = service.RestartOpenclawForInstance
	upgradeOpenclawForInstance = service.UpgradeOpenclawForInstance
	resolveGatewayToken        = service.ResolveGatewayToken
	generateGatewayToken       = service.GenerateGatewayToken
	resolveLLMConfig           = service.ResolveLLMConfig
	resolveRuntimePlan         = service.ResolveRuntimePlan
	applyOpenclawRuntime       = service.ApplyOpenclawRuntime
	openclawApplySpec          = service.OpenclawApplySpec
	agenthubNetworkConfig      = service.AgenthubNetworkConfig
	llmEgressRule              = service.LLMEgressRule
	decryptSetting             = service.DecryptSetting
	maskSecret                 = service.MaskSecret
	newOpenclawPersistID       = service.NewOpenclawPersistID
	openclawHostStatePath      = service.OpenclawHostStatePath
	openclawHostSnapshotPath   = service.OpenclawHostSnapshotPath
	prepareOpenclawStateDir    = service.PrepareOpenclawStateDir
	copyOpenclawStateDir       = service.CopyOpenclawStateDir
	openclawHostMountMetadata  = service.OpenclawHostMountMetadata
	agenthubDistributionScope  = service.AgenthubDistributionScope
)

// runEnvdCommand is kept as a thin wrapper so any future handler code that
// needs to run an envd command can do so without importing the service
// package's http.Client plumbing.
func runEnvdCommand(httpClient *http.Client, sandboxID, domain string, req map[string]interface{}) (*CommandOutput, error) {
	return service.RunEnvdCommand(httpClient, sandboxID, domain, req)
}

// Compile-time assertion that store.AgentInstance is still the type we pass
// to the restart/upgrade helpers (catches accidental drift if store is
// refactored later).
var _ = func(inst *store.AgentInstance) (*CommandOutput, error) {
	return restartOpenclawForInstance(inst)
}
