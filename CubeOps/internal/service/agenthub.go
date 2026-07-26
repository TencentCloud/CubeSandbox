// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/cubemaster"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/store"
	"gorm.io/gorm"
)

// CubeMasterClient is the subset of the CubeMaster client surface that the
// AgentHub service layer depends on. The real *cubemaster.Client satisfies
// it; handlers pass their own (possibly faked) client in. Defined here so
// the service package does not import the handler package's CubeMasterClient
// (which would create an import cycle).
type CubeMasterClient interface {
	DeleteSandbox(ctx context.Context, body interface{}) (json.RawMessage, error)
	CreateSandbox(ctx context.Context, body interface{}) (json.RawMessage, error)
	UpdateSandbox(ctx context.Context, body interface{}) (json.RawMessage, error)
	ListTemplates(ctx context.Context, templateID string, includeRequest bool) (json.RawMessage, error)
	CreateSnapshot(ctx context.Context, body interface{}) (json.RawMessage, error)
	DeleteSnapshot(ctx context.Context, snapshotID string) (json.RawMessage, error)
	RollbackSandbox(ctx context.Context, sandboxID string, body interface{}) (json.RawMessage, error)
}

// SDKInstanceType is the CubeMaster instance_type used for AgentHub sandboxes.
const SDKInstanceType = "cubebox"

// ── Error ───────────────────────────────────────────────────────────────────

// Error is the service-layer error type. It carries an HTTP status code so
// handlers can map service errors to HTTP responses uniformly, without each
// handler hardcoding status codes. The Status field defaults to 500 when
// zero; callers should use NewError / NewBadRequest / etc. to construct.
type Error struct {
	Status  int
	Code    string // optional, e.g. "not_found", "conflict"; "" for generic
	Message string
	Cause   error // underlying error, if any; not serialized
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Cause)
	}
	return e.Message
}

func (e *Error) Unwrap() error { return e.Cause }

// NewError constructs an Error with the given status and message.
func NewError(status int, msg string) *Error { return &Error{Status: status, Message: msg} }

// NewBadRequest is a shortcut for NewError(http.StatusBadRequest, msg).
func NewBadRequest(msg string) *Error { return &Error{Status: 400, Message: msg} }

// NewNotFound is a shortcut for NewError(http.StatusNotFound, msg).
func NewNotFound(msg string) *Error { return &Error{Status: 404, Message: msg, Code: "not_found"} }

// NewConflict is a shortcut for NewError(http.StatusConflict, msg).
func NewConflict(msg string) *Error { return &Error{Status: 409, Message: msg, Code: "conflict"} }

// NewBadGateway is a shortcut for NewError(http.StatusBadGateway, msg).
func NewBadGateway(msg string) *Error { return &Error{Status: 502, Message: msg} }

// NewInternal is a shortcut for NewError(http.StatusInternalServerError, msg).
func NewInternal(msg string) *Error { return &Error{Status: 500, Message: msg} }

// wrapCMError maps a CubeMaster client error to a service Error. If the
// error is a *cubemaster.CMError, the ret_code is mapped to the same HTTP
// status that the handler previously produced via writeCMError.
func wrapCMError(err error) *Error {
	var cmErr *cubemaster.CMError
	if !errors.As(err, &cmErr) {
		return NewBadGateway("cubemaster: " + err.Error())
	}
	switch {
	case cmErr.IsNotFound():
		return &Error{Status: 404, Code: "not_found", Message: cmErr.RetMsg}
	case cmErr.IsConflict() || cmErr.IsCapacity():
		return &Error{Status: 409, Code: "conflict", Message: cmErr.RetMsg}
	case cmErr.RetryAfter() > 0:
		// 503 is handled by the handler setting Retry-After; the service
		// returns a plain 503 Error and the handler inspects Cause for
		// Retry-After. For simplicity we embed Retry-After in Code.
		return &Error{Status: 503, Code: fmt.Sprintf("retry-after:%d", cmErr.RetryAfter()), Message: cmErr.RetMsg, Cause: cmErr}
	default:
		return NewBadGateway(fmt.Sprintf("cubemaster error %d: %s", cmErr.RetCode, cmErr.RetMsg))
	}
}

// ── AgentStore interface ────────────────────────────────────────────────────

// AgentStore is the subset of *store.Store that AgentHubService depends on.
// Defining it as an interface (rather than depending on *store.Store
// directly) lets tests substitute a fake store without spinning up MySQL.
// The real *store.Store satisfies this interface implicitly.
type AgentStore interface {
	GetSetting(ctx context.Context, key string) (string, error)
	GetInstance(ctx context.Context, agentID string) (*store.AgentInstance, error)
	UpsertInstance(ctx context.Context, inst *store.AgentInstance) error
	SoftDeleteInstance(ctx context.Context, agentID string) error
	UpdateInstanceStatus(ctx context.Context, agentID, status string) error
	UpdateInstanceModel(ctx context.Context, agentID, model string) error
	GetAgentWecomConfig(ctx context.Context, agentID string) (botID, botSecret string, err error)
	UpdateAgentWecomConfig(ctx context.Context, agentID, botID, botSecret string) error
	GetAgentSnapshot(ctx context.Context, agentID, snapshotID string) (*store.AgentSnapshot, error)
	DeleteAgentSnapshot(ctx context.Context, agentID, snapshotID string) error
	GetAgentTemplate(ctx context.Context, templateID string) (*store.AgentTemplate, error)
	RecordOperation(ctx context.Context, agentID, sandboxID, operationType, status, errMsg string) error
	LatestHealthySnapshot(ctx context.Context, agentID string) (string, error)
	SetBaseSnapshotID(ctx context.Context, agentID, snapshotID string) error
	// Reverse-sync: find AgentHub templates backed by an infra template/snapshot
	// id, and soft-delete them. Used after E2B/SDK template/snapshot delete.
	FindTemplateIDsByInfraID(ctx context.Context, infraID string) ([]string, error)
	SoftDeleteAgentHubTemplate(ctx context.Context, templateID string) error
	DB() *gorm.DB
}

// Compile-time assertion: *store.Store satisfies AgentStore.
var _ AgentStore = (*store.Store)(nil)

// ── envd / OpenClaw function types (for test injection) ─────────────────────

// applyOpenclawFn is the signature of ApplyOpenclawRuntime. Declared as a
// type so AgentHubService can hold it as an injectable field.
type applyOpenclawFn func(httpClient *http.Client, sandboxID, domain string, plan *LLMRuntimePlan, opts *OpenclawApplyOptions) (*CommandOutput, error)

// resolveGatewayFn is the signature of ResolveGatewayToken.
type resolveGatewayFn func(httpClient *http.Client, sandboxID, domain, hostStatePath, fallbackToken string) string

// restartOpenclawFn is the signature of RestartOpenclawForInstance.
type restartOpenclawFn func(inst *store.AgentInstance) (*CommandOutput, error)

// upgradeOpenclawFn is the signature of UpgradeOpenclawForInstance.
type upgradeOpenclawFn func(inst *store.AgentInstance) (*CommandOutput, error)

// ── AgentHubService ─────────────────────────────────────────────────────────

// AgentHubService encapsulates the AgentHub business logic that was
// previously inlined in handler methods. It owns the CubeMaster client and
// the store, exposing coarse-grained operations that handlers call after
// decoding the HTTP request. Handlers stay thin: bind → call service →
// write response.
//
// The envd / OpenClaw entry points (applyFn, resolveGatewayFn, restartFn,
// upgradeFn) are function fields rather than direct calls so tests can
// inject fakes — otherwise every CreateInstance/CloneAgent test would try
// to dial a real sandbox via envd. NewAgentHubService wires them to the
// real implementations; newAgentHubServiceForTest overrides them.
type AgentHubService struct {
	Store            AgentStore
	CM               CubeMasterClient
	envdClient       *http.Client
	applyFn          applyOpenclawFn
	resolveGatewayFn resolveGatewayFn
	restartFn        restartOpenclawFn
	upgradeFn        upgradeOpenclawFn
}

// NewAgentHubService constructs an AgentHubService from a store and a
// CubeMaster client, wiring the envd/OpenClaw function fields to their
// real implementations.
func NewAgentHubService(s *store.Store, cm CubeMasterClient) *AgentHubService {
	return &AgentHubService{
		Store:            s,
		CM:               cm,
		envdClient:       EnvdHTTPClient(),
		applyFn:          ApplyOpenclawRuntime,
		resolveGatewayFn: ResolveGatewayToken,
		restartFn:        RestartOpenclawForInstance,
		upgradeFn:        UpgradeOpenclawForInstance,
	}
}

// CompensateDeleteSandbox is a best-effort cleanup used when Agent creation
// fails after the sandbox has already been created by CubeMaster. It deletes
// the sandbox so CPU/memory/quota are not leaked. Errors are logged but not
// returned — the caller already has an error to report to the client.
// See R10.
func (s *AgentHubService) CompensateDeleteSandbox(ctx context.Context, sandboxID, reason string) {
	if _, err := s.CM.DeleteSandbox(ctx, map[string]interface{}{
		"requestID":     fmt.Sprintf("cubeops-compensate-%s-%d", reason, time.Now().UnixNano()),
		"sandbox_id":    sandboxID,
		"instance_type": SDKInstanceType,
	}); err != nil {
		slog.Error("CompensateDeleteSandbox: failed to delete sandbox after creation failure",
			"sandboxID", sandboxID, "reason", reason, "err", err)
	} else {
		slog.Info("CompensateDeleteSandbox: sandbox deleted after creation failure",
			"sandboxID", sandboxID, "reason", reason)
	}
}

// ReverseSyncAgentHubTemplate best-effort soft-deletes any AgentHub template
// registration backed by the just-deleted infrastructure template/snapshot.
//
// This migrates the old Rust reverse_sync_agenthub_template
// (CubeAPI/src/handlers/templates.rs:192) into CubeOps. The old CubeAPI
// called it from the E2B delete_template handler after the infra delete
// succeeded; now that CubeOps owns the SDK path (SDKHandler.DeleteTemplate)
// and the AgentHub path (AgentHubHandler.DeleteTemplate), both must trigger
// the reverse-sync so the AgentHub registry does not keep pointing at a
// snapshot/template that no longer exists.
//
// Failures are logged, never propagated — a reverse-sync failure must not
// block the primary deletion that already succeeded (FIX-5b, L15/H5).
func (s *AgentHubService) ReverseSyncAgentHubTemplate(ctx context.Context, infraID string) {
	ids, err := s.Store.FindTemplateIDsByInfraID(ctx, infraID)
	if err != nil {
		slog.Warn("ReverseSyncAgentHubTemplate: query AgentHub templates failed",
			"infraID", infraID, "err", err)
		return
	}
	for _, id := range ids {
		if err := s.Store.SoftDeleteAgentHubTemplate(ctx, id); err != nil {
			slog.Warn("ReverseSyncAgentHubTemplate: failed to soft-delete AgentHub template",
				"templateID", id, "err", err)
		} else {
			slog.Info("ReverseSyncAgentHubTemplate: soft-deleted AgentHub template",
				"templateID", id, "infraID", infraID)
		}
	}
}

// ── CreateSandbox request builder (shared by CreateInstance + CloneAgent) ───

// CreateSandboxRequest is the typed input for BuildCreateSandboxRequest.
type CreateSandboxRequest struct {
	RequestID         string
	Name              string
	Engine            string
	PersistenceMode   string
	RootfsSourceType  string
	RootfsSourceID    string
	TemplateID        string
	Annotations       map[string]string
	Labels            map[string]string
	NetworkConfig     map[string]interface{} // optional, nil to omit
	DistributionScope []string               // optional, nil to omit
}

// BuildCreateSandboxRequest assembles the CubeMaster CreateSandbox request
// body from a typed input.
func BuildCreateSandboxRequest(req CreateSandboxRequest) map[string]interface{} {
	labels := req.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	labels["agenthub"] = "true"
	if req.Name != "" {
		labels["agenthub.name"] = req.Name
	}
	if req.Engine != "" {
		labels["agenthub.engine"] = req.Engine
	}
	if req.PersistenceMode != "" {
		labels["agenthub.persistence_mode"] = req.PersistenceMode
	}
	labels["agenthub.rootfs_source_type"] = req.RootfsSourceType
	labels["agenthub.rootfs_source_id"] = req.RootfsSourceID

	annotations := req.Annotations
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations["cube.master.appsnapshot.template.id"] = req.TemplateID
	annotations["cube.master.appsnapshot.template.version"] = "v2"

	cmReq := map[string]interface{}{
		"requestID":     req.RequestID,
		"instance_type": SDKInstanceType,
		"timeout":       86400,
		"containers":    []interface{}{},
		"exposed_ports": []interface{}{},
		"annotations":   annotations,
		"labels":        labels,
		"network_type":  "tap",
		"auto_pause":    false,
		"auto_resume":   false,
	}
	if req.NetworkConfig != nil {
		cmReq["cube_network_config"] = req.NetworkConfig
	}
	if req.DistributionScope != nil {
		cmReq["distribution_scope"] = req.DistributionScope
	}
	return cmReq
}

// ── URL / port helpers ──────────────────────────────────────────────────────

// ResolveEnvPort queries CubeMaster for the template's image_info and returns
// the env port the frontend should use to reach the sandbox desktop /
// code interpreter.
//
//   - All-in-One images (image_info contains "lightweight") → 49999
//   - Otherwise → 8080
//
// On any error the default (8080) is returned so the caller can still proceed.
func (s *AgentHubService) ResolveEnvPort(ctx context.Context, templateID string) int {
	envPort := 8080
	if templateID == "" {
		return envPort
	}
	raw, err := s.CM.ListTemplates(ctx, templateID, false)
	if err != nil {
		return envPort
	}
	var tmpl struct {
		ImageInfo string `json:"image_info"`
	}
	if json.Unmarshal(raw, &tmpl) != nil {
		return envPort
	}
	if strings.Contains(strings.ToLower(tmpl.ImageInfo), "lightweight") {
		return 49999
	}
	return envPort
}

// ResolveEnvPortFromInstance infers the env port from an existing instance's
// EnvURL (used by CloneAgent, which inherits the source agent's port).
func ResolveEnvPortFromInstance(envURL string) int {
	if envURL == "" {
		return 8080
	}
	u, err := url.Parse(envURL)
	if err != nil {
		return 8080
	}
	match := regexp.MustCompile(`^(\d+)-`).FindStringSubmatch(u.Hostname())
	if match == nil {
		return 8080
	}
	if p, err := strconv.Atoi(match[1]); err == nil {
		return p
	}
	return 8080
}

// GatewayURL builds the OpenClaw gateway URL for a sandbox. If gatewayToken
// is non-empty it is appended as a URL fragment.
func GatewayURL(sandboxID, domain, gatewayToken string) string {
	gwURL := fmt.Sprintf("https://18789-%s.%s", sandboxID, domain)
	if gatewayToken != "" {
		gwURL = gwURL + "#token=" + gatewayToken
	}
	return gwURL
}

// EnvURL builds the sandbox env URL for a given port.
func EnvURL(envPort int, sandboxID, domain string) string {
	return fmt.Sprintf("http://%d-%s.%s", envPort, sandboxID, domain)
}

// AvailableBots computes the bots still available for binding on a clone /
// new instance, given the list of already-bound bots.
func AvailableBots(bound []string) []string {
	available := []string{}
	for _, b := range []string{"wecom"} {
		found := false
		for _, active := range bound {
			if active == b {
				found = true
				break
			}
		}
		if !found {
			available = append(available, b)
		}
	}
	return available
}

// ── CreateInstance ──────────────────────────────────────────────────────────

// CreateInstanceRequest is the typed input for AgentHubService.CreateInstance.
type CreateInstanceRequest struct {
	Name            string
	Engine          string
	Model           string // optional override; "" = use setting default
	TemplateID      string // optional
	SnapshotID      string // optional
	PersistenceMode string // optional, "shared_files" or "full_snapshot"
	BotID           string // optional, must pair with BotSecret
	BotSecret       string // optional, must pair with BotID
}

// CreateInstanceResult is the typed output of CreateInstance — the persisted
// AgentInstance record, ready to be serialised as the HTTP response body.
type CreateInstanceResult struct {
	Instance *store.AgentInstance
}

// CreateInstance orchestrates the full agent creation flow:
//
//  1. Resolve LLM config + domain + egress network config from settings.
//  2. Resolve rootfs source (snapshot vs template, with published-template
//     fast-path detection).
//  3. Set up shared-files host state directory if needed.
//  4. Build & send the CubeMaster CreateSandbox request.
//  5. Apply the OpenClaw runtime config (LLM provider, model, gateway token,
//     optional WeCom channel).
//  6. Read back the gateway token after apply.
//  7. Persist the AgentInstance record to the DB.
func (s *AgentHubService) CreateInstance(ctx context.Context, req CreateInstanceRequest) (*CreateInstanceResult, error) {
	// --- Validate ---
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return nil, NewBadRequest("agent name is required")
	}
	if req.Engine != "openclaw" {
		return nil, NewBadRequest("only openclaw engine is currently supported")
	}
	hasBotID := strings.TrimSpace(req.BotID) != ""
	hasBotSecret := strings.TrimSpace(req.BotSecret) != ""
	if hasBotID != hasBotSecret {
		return nil, NewBadRequest("Bot ID and Secret must be provided together")
	}
	shouldBindWecom := hasBotID && hasBotSecret

	// --- Persistence mode ---
	persistenceMode := "full_snapshot"
	if req.PersistenceMode != "" {
		pm := strings.TrimSpace(req.PersistenceMode)
		if pm == "shared_files" || pm == "full_snapshot" {
			persistenceMode = pm
		}
	}

	// --- Rootfs source ---
	snapshotID := strings.TrimSpace(req.SnapshotID)
	rootfsSourceType := "template"
	rootfsSourceID := ""
	if snapshotID != "" {
		rootfsSourceType = "snapshot"
		rootfsSourceID = snapshotID
	} else {
		if req.TemplateID != "" {
			rootfsSourceID = req.TemplateID
		} else {
			rootfsSourceID = "wecom-ds-openclaw"
		}
	}
	templateID := rootfsSourceID
	explicitTemplateID := req.TemplateID

	// --- LLM config + domain + network ---
	llmCfg, err := ResolveLLMConfig(ctx, s.Store)
	if err != nil {
		return nil, NewBadRequest(err.Error())
	}
	llmModel := llmCfg.Model
	if req.Model != "" {
		llmModel = req.Model
	}

	domain, _ := s.Store.GetSetting(ctx, "gateway_domain")
	if domain == "" {
		domain = "cube.app"
	}

	networkConfig, err := AgenthubNetworkConfig(llmCfg)
	if err != nil {
		return nil, NewInternal("failed to build network config: " + err.Error())
	}

	// --- Shared-files + published-template fast-path detection ---
	sharedFiles := persistenceMode == "shared_files"
	var openclawPersistID, openclawStatePath string
	var templateOpenclawStateSource string

	var agentTemplate *store.AgentTemplate
	if rootfsSourceType == "template" {
		agentTemplate, _ = s.Store.GetAgentTemplate(ctx, templateID)
	}
	if agentTemplate != nil && agentTemplate.SourceAgentID != "market" {
		if snap, _ := s.Store.GetAgentSnapshot(ctx, agentTemplate.SourceAgentID, agentTemplate.SourceSnapshotID); snap != nil {
			if snap.RootfsSnapshotID != nil && *snap.RootfsSnapshotID != "" {
				rootfsSourceType = "snapshot"
				rootfsSourceID = *snap.RootfsSnapshotID
				templateID = *snap.RootfsSnapshotID
			}
			if snap.OpenclawStateSnapshotPath != nil && *snap.OpenclawStateSnapshotPath != "" {
				templateOpenclawStateSource = *snap.OpenclawStateSnapshotPath
			}
		}
	}

	if sharedFiles {
		openclawPersistID = NewOpenclawPersistID()
		statePath, err := PrepareOpenclawStateDir(openclawPersistID)
		if err != nil {
			return nil, NewInternal(err.Error())
		}
		openclawStatePath = statePath
		if templateOpenclawStateSource != "" {
			if err := CopyOpenclawStateDir(templateOpenclawStateSource, statePath); err != nil {
				slog.Warn("failed to copy OpenClaw state from template", "source", templateOpenclawStateSource, "err", err)
			}
		}
	}

	// --- Build CubeMaster request ---
	requestID := fmt.Sprintf("req-%d", time.Now().UnixNano())
	extraLabels := map[string]string{}
	extraAnnotations := map[string]string{}
	if sharedFiles && openclawPersistID != "" {
		mountMeta, err := OpenclawHostMountMetadata(openclawStatePath)
		if err != nil {
			return nil, NewInternal(err.Error())
		}
		extraLabels["agenthub.openclaw.persist_id"] = openclawPersistID
		extraAnnotations[HostdirMountKey] = mountMeta
	}
	var distributionScope []string
	if scope := AgenthubDistributionScope(persistenceMode, rootfsSourceType); scope != nil {
		distributionScope = scope
	}

	cmReq := BuildCreateSandboxRequest(CreateSandboxRequest{
		RequestID:         requestID,
		Name:              name,
		Engine:            "openclaw",
		PersistenceMode:   persistenceMode,
		RootfsSourceType:  rootfsSourceType,
		RootfsSourceID:    rootfsSourceID,
		TemplateID:        templateID,
		Annotations:       extraAnnotations,
		Labels:            extraLabels,
		NetworkConfig:     networkConfig,
		DistributionScope: distributionScope,
	})

	// --- Create sandbox ---
	sandboxResp, err := s.CM.CreateSandbox(ctx, cmReq)
	if err != nil {
		return nil, NewBadGateway("failed to create sandbox: " + err.Error())
	}
	var sbResult struct {
		SandboxID string `json:"sandbox_id"`
		Ret       struct {
			RetCode int    `json:"ret_code"`
			RetMsg  string `json:"ret_msg"`
		} `json:"ret"`
	}
	if err := json.Unmarshal(sandboxResp, &sbResult); err != nil {
		return nil, NewInternal("failed to parse sandbox response: " + err.Error())
	}
	if sbResult.SandboxID == "" {
		return nil, NewBadGateway("CubeMaster returned empty sandbox_id")
	}
	sandboxID := sbResult.SandboxID
	agentID := fmt.Sprintf("agent-%s", sandboxID)

	// --- Apply OpenClaw runtime ---
	plan := ResolveRuntimePlan(llmCfg, llmModel)
	var generatedToken string
	var applyOpts *OpenclawApplyOptions

	useTemplateFastpath := false
	if rootfsSourceType == "template" && !shouldBindWecom {
		if tmpl, _ := s.Store.GetAgentTemplate(ctx, templateID); tmpl != nil && tmpl.SourceAgentID != "market" {
			useTemplateFastpath = true
		}
	}
	hasOpenclawState := useTemplateFastpath && (!sharedFiles || templateOpenclawStateSource != "")

	if shouldBindWecom {
		generatedToken = GenerateGatewayToken()
		applyOpts = &OpenclawApplyOptions{
			Mode:           ApplyModeFullInit,
			GatewayToken:   generatedToken,
			ConfigureWecom: true,
			BotID:          strings.TrimSpace(req.BotID),
			BotSecret:      strings.TrimSpace(req.BotSecret),
		}
	} else if hasOpenclawState {
		applyOpts = &OpenclawApplyOptions{
			Mode:                 ApplyModeMergeLLM,
			PreserveGatewayToken: true,
		}
	} else {
		generatedToken = GenerateGatewayToken()
		applyOpts = &OpenclawApplyOptions{
			Mode:         ApplyModeFullInit,
			GatewayToken: generatedToken,
		}
	}
	applyOutput, err := s.applyFn(s.envdClient, sandboxID, domain, plan, applyOpts)
	if err != nil {
		s.CompensateDeleteSandbox(ctx, sandboxID, "apply_openclaw")
		return nil, NewBadGateway("failed to apply OpenClaw config: " + err.Error())
	}

	gatewayToken := s.resolveGatewayFn(s.envdClient, sandboxID, domain, openclawStatePath, generatedToken)

	// --- Build instance record ---
	bots := []string{}
	if shouldBindWecom {
		bots = []string{"wecom"}
	}
	botsAvailable := AvailableBots(bots)

	finalTemplateID := templateID
	if explicitTemplateID != "" {
		finalTemplateID = explicitTemplateID
	}

	envPort := s.ResolveEnvPort(ctx, templateID)
	gatewayURL := GatewayURL(sandboxID, domain, gatewayToken)
	envURL := EnvURL(envPort, sandboxID, domain)

	inst := &store.AgentInstance{
		ID:               agentID,
		Name:             name,
		Status:           "running",
		Engine:           "openclaw",
		Env:              "linux",
		Model:            llmModel,
		Version:          "2026.4.5-t.27",
		Bots:             bots,
		BotsAvailable:    botsAvailable,
		Avatar:           name,
		AvatarTone:       "sky",
		SandboxID:        sandboxID,
		TemplateID:       finalTemplateID,
		GatewayURL:       gatewayURL,
		GatewayToken:     gatewayToken,
		EnvURL:           envURL,
		PersistenceMode:  &persistenceMode,
		RootfsSourceType: &rootfsSourceType,
		RootfsSourceID:   &rootfsSourceID,
		Domain:           domain,
		Setup: &store.AgentSetupResult{
			ExitCode: applyOutput.ExitCode,
			Stdout:   applyOutput.Stdout,
			Stderr:   applyOutput.Stderr,
		},
	}
	if sharedFiles && openclawPersistID != "" {
		inst.OpenclawPersistID = &openclawPersistID
		inst.OpenclawStatePath = &openclawStatePath
	}
	if shouldBindWecom {
		inst.WecomConfig = &store.AgentWecomConfig{
			BotID:     strings.TrimSpace(req.BotID),
			BotSecret: strings.TrimSpace(req.BotSecret),
		}
	}

	if err := s.Store.UpsertInstance(ctx, inst); err != nil {
		s.CompensateDeleteSandbox(ctx, sandboxID, "upsert_instance")
		return nil, NewInternal("failed to create instance record: " + err.Error())
	}

	return &CreateInstanceResult{Instance: inst}, nil
}

// ── DeleteInstance ──────────────────────────────────────────────────────────

// DeleteInstance deletes the agent: CubeMaster sandbox first (tolerating
// "not found"), then host-side OpenClaw state dir, then the DB record.
func (s *AgentHubService) DeleteInstance(ctx context.Context, agentID string) error {
	inst, err := s.Store.GetInstance(ctx, agentID)
	if err != nil {
		return NewInternal("failed to get instance: " + err.Error())
	}
	if inst == nil {
		return NewNotFound("instance not found")
	}
	if _, err := s.CM.DeleteSandbox(ctx, map[string]interface{}{
		"requestID":     fmt.Sprintf("cubeops-del-%d", time.Now().UnixNano()),
		"sandbox_id":    inst.SandboxID,
		"instance_type": SDKInstanceType,
	}); err != nil {
		var cmErr *cubemaster.CMError
		if errors.As(err, &cmErr) && cmErr.IsNotFound() {
			slog.Info("DeleteInstance: sandbox not found in CubeMaster, treating as already deleted",
				"agentID", agentID, "sandboxID", inst.SandboxID)
		} else {
			return wrapCMError(err)
		}
	}
	if inst.OpenclawStatePath != nil && *inst.OpenclawStatePath != "" {
		if err := os.RemoveAll(*inst.OpenclawStatePath); err != nil {
			slog.Warn("DeleteInstance: failed to clean up OpenClaw state directory",
				"agentID", agentID, "path", *inst.OpenclawStatePath, "err", err)
		} else {
			slog.Info("DeleteInstance: cleaned up OpenClaw state directory",
				"agentID", agentID, "path", *inst.OpenclawStatePath)
		}
	}
	if err := s.Store.SoftDeleteInstance(ctx, agentID); err != nil {
		return NewInternal("failed to delete instance: " + err.Error())
	}
	return nil
}

// ── RestartAgent / UpgradeAgent ─────────────────────────────────────────────

// RestartAgentResult holds the envd command output from a restart/upgrade.
type RestartAgentResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// RestartAgent restarts the OpenClaw process inside the agent's sandbox.
func (s *AgentHubService) RestartAgent(ctx context.Context, agentID string) (*RestartAgentResult, error) {
	inst, err := s.Store.GetInstance(ctx, agentID)
	if err != nil {
		return nil, NewInternal("failed to get instance: " + err.Error())
	}
	if inst == nil {
		return nil, NewNotFound("instance not found")
	}
	output, err := s.restartFn(inst)
	if err != nil {
		return nil, NewBadGateway("failed to restart: " + err.Error())
	}
	return &RestartAgentResult{ExitCode: output.ExitCode, Stdout: output.Stdout, Stderr: output.Stderr}, nil
}

// UpgradeAgent upgrades OpenClaw to the latest version and restarts it.
func (s *AgentHubService) UpgradeAgent(ctx context.Context, agentID string) (*RestartAgentResult, error) {
	inst, err := s.Store.GetInstance(ctx, agentID)
	if err != nil {
		return nil, NewInternal("failed to get instance: " + err.Error())
	}
	if inst == nil {
		return nil, NewNotFound("instance not found")
	}
	output, err := s.upgradeFn(inst)
	if err != nil {
		return nil, NewBadGateway("failed to upgrade: " + err.Error())
	}
	return &RestartAgentResult{ExitCode: output.ExitCode, Stdout: output.Stdout, Stderr: output.Stderr}, nil
}

// ── Pause / Resume (sandboxAction) ──────────────────────────────────────────

// SandboxAction pauses or resumes the agent's sandbox. action must be
// "pause" or "resume". Returns the updated AgentInstance.
func (s *AgentHubService) SandboxAction(ctx context.Context, agentID, action string) (*store.AgentInstance, error) {
	inst, err := s.Store.GetInstance(ctx, agentID)
	if err != nil {
		return nil, NewInternal("failed to get instance: " + err.Error())
	}
	if inst == nil {
		return nil, NewNotFound("instance not found")
	}

	reqBody := map[string]interface{}{
		"requestID":     fmt.Sprintf("req-%d", time.Now().UnixNano()),
		"sandbox_id":    inst.SandboxID,
		"instance_type": SDKInstanceType,
		"action":        action,
	}
	if action == "resume" {
		reqBody["timeout"] = 86400
	}
	if _, err := s.CM.UpdateSandbox(ctx, reqBody); err != nil {
		return nil, NewBadGateway("failed to " + action + " sandbox: " + err.Error())
	}

	newStatus := "running"
	if action == "pause" {
		newStatus = "stopped"
	}
	if err := s.Store.UpdateInstanceStatus(ctx, agentID, newStatus); err != nil {
		return nil, NewInternal("failed to update status: " + err.Error())
	}
	updated, err := s.Store.GetInstance(ctx, agentID)
	if err != nil || updated == nil {
		// Return a minimal stub so the handler can still respond.
		return &store.AgentInstance{ID: agentID, Status: newStatus}, nil
	}
	return updated, nil
}

// ── UpdateModel ─────────────────────────────────────────────────────────────

// UpdateModel updates the model field on the agent and returns the updated
// AgentInstance.
func (s *AgentHubService) UpdateModel(ctx context.Context, agentID, model string) (*store.AgentInstance, error) {
	if err := s.Store.UpdateInstanceModel(ctx, agentID, model); err != nil {
		return nil, NewInternal("failed to update model: " + err.Error())
	}
	updated, err := s.Store.GetInstance(ctx, agentID)
	if err != nil || updated == nil {
		return &store.AgentInstance{ID: agentID, Model: model}, nil
	}
	return updated, nil
}

// ── UpdateWecomConfig ───────────────────────────────────────────────────────

// UpdateWecomConfig applies the new WeCom config to the running sandbox
// (via OpenClaw merge_llm apply) and then persists it to the DB. Returns
// the updated AgentInstance.
func (s *AgentHubService) UpdateWecomConfig(ctx context.Context, agentID, botID, botSecret string) (*store.AgentInstance, error) {
	botID = strings.TrimSpace(botID)
	botSecret = strings.TrimSpace(botSecret)
	if (botID == "") != (botSecret == "") {
		return nil, NewBadRequest("botId and botSecret must both be set or both be empty")
	}

	inst, err := s.Store.GetInstance(ctx, agentID)
	if err != nil {
		return nil, NewInternal("failed to get instance: " + err.Error())
	}
	if inst == nil {
		return nil, NewNotFound("instance not found")
	}

	if botID != "" && inst.SandboxID != "" {
		llmCfg, err := ResolveLLMConfig(ctx, s.Store)
		if err != nil {
			slog.Warn("UpdateWecomConfig: LLM config not available, using defaults", "err", err)
			llmCfg = DefaultLLMConfig()
		}
		plan := ResolveRuntimePlan(llmCfg, "")
		domain := inst.Domain
		if domain == "" {
			domain = os.Getenv("CUBE_API_SANDBOX_DOMAIN")
		}
		applyOpts := &OpenclawApplyOptions{
			Mode:           ApplyModeMergeLLM,
			ConfigureWecom: true,
			BotID:          botID,
			BotSecret:      botSecret,
		}
		if _, err := s.applyFn(s.envdClient, inst.SandboxID, domain, plan, applyOpts); err != nil {
			slog.Error("UpdateWecomConfig: failed to apply to running agent", "err", err)
			return nil, NewBadGateway("failed to apply WeCom config to running agent: " + err.Error())
		}
	}

	if err := s.Store.UpdateAgentWecomConfig(ctx, agentID, botID, botSecret); err != nil {
		return nil, NewInternal("failed to update wecom config: " + err.Error())
	}
	updated, err := s.Store.GetInstance(ctx, agentID)
	if err != nil || updated == nil {
		return &store.AgentInstance{ID: agentID, WecomConfig: &store.AgentWecomConfig{BotID: botID, BotSecret: botSecret}}, nil
	}
	return updated, nil
}

// ── DeleteSnapshot ──────────────────────────────────────────────────────────

// DeleteSnapshot deletes an agent snapshot, performing the correct physical
// cleanup (host dir for agenthub_state, CubeMaster DeleteSnapshot for
// full/sandbox snapshots) before the DB soft-delete. Rejects snapshots that
// are referenced by a published template.
func (s *AgentHubService) DeleteSnapshot(ctx context.Context, agentID, snapshotID string) error {
	snap, err := s.Store.GetAgentSnapshot(ctx, agentID, snapshotID)
	if err != nil {
		return NewInternal("failed to get snapshot: " + err.Error())
	}
	if snap == nil {
		return NewNotFound("snapshot not found")
	}
	if snap.TemplateRef {
		return NewConflict("snapshot is referenced by a published template; remove the template first")
	}
	if snap.SnapshotKind != nil {
		switch *snap.SnapshotKind {
		case "agenthub_state":
			if snap.OpenclawStateSnapshotPath != nil && *snap.OpenclawStateSnapshotPath != "" {
				if err := os.RemoveAll(*snap.OpenclawStateSnapshotPath); err != nil {
					slog.Warn("DeleteSnapshot: failed to clean up OpenClaw snapshot directory",
						"agentID", agentID, "snapshotID", snapshotID,
						"path", *snap.OpenclawStateSnapshotPath, "err", err)
				}
			}
		case "full_snapshot", "sandbox_snapshot":
			if _, err := s.CM.DeleteSnapshot(ctx, snapshotID); err != nil {
				return wrapCMError(err)
			}
		}
	}
	if err := s.Store.DeleteAgentSnapshot(ctx, agentID, snapshotID); err != nil {
		return NewInternal("failed to delete snapshot: " + err.Error())
	}
	return nil
}

// ── CreateSnapshot ──────────────────────────────────────────────────────────

// CreateSnapshotRequest is the typed input for CreateSnapshot.
type CreateSnapshotRequest struct {
	AgentID string
	Name    string // optional display name
}

// CreateSnapshotResult holds the new snapshot ID and a ready-to-serialise
// response body.
type CreateSnapshotResult struct {
	SnapshotID string
	Body       map[string]interface{}
}

// CreateSnapshot creates an agent snapshot. For shared_files mode it copies
// the OpenClaw host state directory; for full_snapshot mode it delegates to
// CubeMaster CreateSnapshot.
func (s *AgentHubService) CreateSnapshot(ctx context.Context, req CreateSnapshotRequest) (*CreateSnapshotResult, error) {
	agentID := req.AgentID
	inst, err := s.Store.GetInstance(ctx, agentID)
	if err != nil {
		return nil, NewInternal("failed to get instance: " + err.Error())
	}
	if inst == nil {
		return nil, NewNotFound("instance not found")
	}

	persistenceMode := ""
	if inst.PersistenceMode != nil {
		persistenceMode = *inst.PersistenceMode
	}
	sharedFiles := persistenceMode == "shared_files"

	if sharedFiles && inst.OpenclawStatePath != nil && *inst.OpenclawStatePath != "" {
		sourceOpenclawPath := *inst.OpenclawStatePath
		snapshotID := fmt.Sprintf("agenthub-%s", uuid.New().String())
		snapPath := OpenclawHostSnapshotPath(snapshotID)
		if err := CopyOpenclawStateDir(sourceOpenclawPath, snapPath); err != nil {
			return nil, NewInternal("failed to copy OpenClaw state: " + err.Error())
		}

		rootfsSnapshotID := inst.BaseSnapshotID
		if rootfsSnapshotID == "" && inst.RootfsSourceID != nil {
			rootfsSnapshotID = *inst.RootfsSourceID
		}
		if rootfsSnapshotID == "" {
			rootfsSnapshotID = inst.TemplateID
		}
		rootfsSourceType := "template"
		if inst.RootfsSourceType != nil && *inst.RootfsSourceType != "" {
			rootfsSourceType = *inst.RootfsSourceType
		}

		if err := s.Store.DB().WithContext(ctx).Exec(
			store.UpsertSnapshotSQL(
				`snapshot_id, agent_id, sandbox_id, name, status, snapshot_kind, origin_sandbox_id,
				rootfs_source_type, rootfs_source_id, rootfs_snapshot_id, openclaw_state_snapshot_path`,
				`?, ?, ?, ?, 'ready', 'agenthub_state', ?, ?, ?, ?, ?`,
				`agent_id = EXCLUDED.agent_id, sandbox_id = EXCLUDED.sandbox_id,
				name = EXCLUDED.name, status = EXCLUDED.status, snapshot_kind = EXCLUDED.snapshot_kind,
				origin_sandbox_id = EXCLUDED.origin_sandbox_id,
				rootfs_source_type = EXCLUDED.rootfs_source_type,
				rootfs_source_id = EXCLUDED.rootfs_source_id,
				rootfs_snapshot_id = EXCLUDED.rootfs_snapshot_id,
				openclaw_state_snapshot_path = EXCLUDED.openclaw_state_snapshot_path`,
				`agent_id = VALUES(agent_id), sandbox_id = VALUES(sandbox_id),
				name = VALUES(name), status = VALUES(status), snapshot_kind = VALUES(snapshot_kind),
				origin_sandbox_id = VALUES(origin_sandbox_id),
				rootfs_source_type = VALUES(rootfs_source_type),
				rootfs_source_id = VALUES(rootfs_source_id),
				rootfs_snapshot_id = VALUES(rootfs_snapshot_id),
				openclaw_state_snapshot_path = VALUES(openclaw_state_snapshot_path)`,
			),
			snapshotID, agentID, inst.SandboxID, req.Name, inst.SandboxID,
			rootfsSourceType, rootfsSnapshotID, rootfsSnapshotID, snapPath,
		).Error; err != nil {
			return nil, NewInternal("failed to record snapshot: " + err.Error())
		}

		_ = s.Store.RecordOperation(ctx, agentID, inst.SandboxID, "snapshot", "succeeded", "")
		return &CreateSnapshotResult{SnapshotID: snapshotID, Body: map[string]interface{}{
			"snapshotID": snapshotID,
			"names":      []string{},
			"status":     "ready",
		}}, nil
	}

	// full_snapshot mode
	requestID := fmt.Sprintf("req-%d", time.Now().UnixNano())
	snapResp, err := s.CM.CreateSnapshot(ctx, map[string]interface{}{
		"requestID":      requestID,
		"sandbox_id":     inst.SandboxID,
		"display_name":   req.Name,
		"create_request": map[string]interface{}{},
	})
	if err != nil {
		return nil, NewBadGateway("failed to create snapshot: " + err.Error())
	}
	var snapResult struct {
		Snapshot struct {
			SnapshotID string `json:"snapshot_id"`
			Status     string `json:"status"`
		} `json:"snapshot"`
		Ret struct {
			RetCode int    `json:"ret_code"`
			RetMsg  string `json:"ret_msg"`
		} `json:"ret"`
	}
	_ = json.Unmarshal(snapResp, &snapResult)
	if snapResult.Snapshot.SnapshotID == "" {
		return nil, NewBadGateway("CubeMaster returned empty snapshot_id: " + snapResult.Ret.RetMsg)
	}
	snapshotID := snapResult.Snapshot.SnapshotID

	err = s.Store.DB().WithContext(ctx).Exec(
		store.UpsertSnapshotSQL(
			`snapshot_id, agent_id, sandbox_id, name, status, snapshot_kind, origin_sandbox_id,
			rootfs_source_type, rootfs_source_id, rootfs_snapshot_id`,
			`?, ?, ?, ?, 'ready', 'sandbox_snapshot', ?, 'snapshot', ?, ?`,
			`agent_id = EXCLUDED.agent_id, sandbox_id = EXCLUDED.sandbox_id,
			status = EXCLUDED.status`,
			`agent_id = VALUES(agent_id), sandbox_id = VALUES(sandbox_id),
			status = VALUES(status)`,
		),
		snapshotID, agentID, inst.SandboxID, req.Name,
		inst.SandboxID, snapshotID, snapshotID,
	).Error
	if err != nil {
		return nil, NewInternal("failed to record snapshot: " + err.Error())
	}
	_ = s.Store.RecordOperation(ctx, agentID, inst.SandboxID, "snapshot", "succeeded", "")
	return &CreateSnapshotResult{SnapshotID: snapshotID, Body: map[string]interface{}{
		"snapshotID": snapshotID,
		"names":      []string{},
		"status":     "ready",
	}}, nil
}

// ── RollbackAgent ───────────────────────────────────────────────────────────

// RollbackAgent rolls the agent back to the given snapshot. For
// agenthub_state snapshots it restores the host OpenClaw state dir and
// restarts OpenClaw; for full/sandbox snapshots it delegates to CubeMaster
// RollbackSandbox.
func (s *AgentHubService) RollbackAgent(ctx context.Context, agentID, snapshotID string) error {
	if snapshotID == "" {
		return NewBadRequest("snapshotId is required")
	}
	inst, err := s.Store.GetInstance(ctx, agentID)
	if err != nil {
		return NewInternal("failed to get instance: " + err.Error())
	}
	if inst == nil {
		return NewNotFound("instance not found")
	}

	snap, _ := s.Store.GetAgentSnapshot(ctx, agentID, snapshotID)
	if snap != nil && snap.SnapshotKind != nil && *snap.SnapshotKind == "agenthub_state" {
		if snap.OpenclawStateSnapshotPath == nil || *snap.OpenclawStateSnapshotPath == "" {
			return NewBadRequest("snapshot has no OpenClaw state path")
		}
		if inst.OpenclawStatePath == nil || *inst.OpenclawStatePath == "" {
			return NewBadRequest("instance has no active OpenClaw state path")
		}
		if err := CopyOpenclawStateDir(*snap.OpenclawStateSnapshotPath, *inst.OpenclawStatePath); err != nil {
			return NewInternal("failed to restore OpenClaw state: " + err.Error())
		}
		output, restartErr := s.restartFn(inst)
		if restartErr != nil || output.ExitCode != 0 {
			errMsg := "unknown error"
			if restartErr != nil {
				errMsg = restartErr.Error()
			} else if output != nil {
				errMsg = output.Stderr
			}
			return NewBadGateway("OpenClaw restart failed after state restore: " + errMsg)
		}
	} else {
		if _, err := s.CM.RollbackSandbox(ctx, inst.SandboxID, map[string]interface{}{
			"requestID":     fmt.Sprintf("cubeops-rollback-%d", time.Now().UnixNano()),
			"snapshot_id":   snapshotID,
			"sandbox_id":    inst.SandboxID,
			"instance_type": SDKInstanceType,
		}); err != nil {
			return wrapCMError(err)
		}
	}

	_ = s.Store.UpdateInstanceStatus(ctx, agentID, "running")
	_ = s.Store.RecordOperation(ctx, agentID, inst.SandboxID, "rollback", "succeeded", "")
	return nil
}

// ── RecoverAgent ────────────────────────────────────────────────────────────

// RecoverAgentResult describes the recovery outcome.
type RecoverAgentResult struct {
	Recovered  bool
	Method     string // "restart" or "rollback"
	SnapshotID string
}

// RecoverAgent attempts crash recovery: first a plain restart; if that
// fails, roll back to the latest healthy snapshot and restart again.
func (s *AgentHubService) RecoverAgent(ctx context.Context, agentID string) (*RecoverAgentResult, error) {
	inst, err := s.Store.GetInstance(ctx, agentID)
	if err != nil {
		return nil, NewInternal("failed to get instance: " + err.Error())
	}
	if inst == nil {
		return nil, NewNotFound("instance not found")
	}

	// Step 1: plain restart.
	output, err := s.restartFn(inst)
	if err == nil && output.ExitCode == 0 {
		_ = s.Store.UpdateInstanceStatus(ctx, agentID, "running")
		_ = s.Store.RecordOperation(ctx, agentID, inst.SandboxID, "recover", "succeeded", "")
		return &RecoverAgentResult{Recovered: true, Method: "restart"}, nil
	}

	restartErr := ""
	if err != nil {
		restartErr = err.Error()
	} else if output != nil {
		restartErr = output.Stderr
	}
	slog.Warn("recover: restart failed, trying snapshot rollback",
		"agentID", agentID, "error", restartErr)

	// Step 2: find latest healthy snapshot.
	snapshotID, err := s.Store.LatestHealthySnapshot(ctx, agentID)
	if err != nil {
		_ = s.Store.UpdateInstanceStatus(ctx, agentID, "error")
		_ = s.Store.RecordOperation(ctx, agentID, inst.SandboxID, "recover", "failed", "failed to look up healthy snapshot: "+err.Error())
		return nil, NewInternal("failed to look up healthy snapshot: " + err.Error())
	}
	if snapshotID == "" {
		_ = s.Store.UpdateInstanceStatus(ctx, agentID, "error")
		_ = s.Store.RecordOperation(ctx, agentID, inst.SandboxID, "recover", "failed", "no healthy snapshot available")
		return nil, NewConflict("OpenClaw is unhealthy and no healthy snapshot is available to recover from")
	}

	// Step 3: rollback to the healthy snapshot.
	snap, _ := s.Store.GetAgentSnapshot(ctx, agentID, snapshotID)
	if snap != nil && snap.SnapshotKind != nil && *snap.SnapshotKind == "agenthub_state" {
		if snap.OpenclawStateSnapshotPath == nil || *snap.OpenclawStateSnapshotPath == "" {
			_ = s.Store.UpdateInstanceStatus(ctx, agentID, "error")
			_ = s.Store.RecordOperation(ctx, agentID, inst.SandboxID, "recover", "failed", "healthy snapshot has no OpenClaw state path")
			return nil, NewInternal("cannot recover: healthy snapshot has no OpenClaw state path")
		}
		if inst.OpenclawStatePath == nil || *inst.OpenclawStatePath == "" {
			_ = s.Store.UpdateInstanceStatus(ctx, agentID, "error")
			_ = s.Store.RecordOperation(ctx, agentID, inst.SandboxID, "recover", "failed", "instance has no active OpenClaw state path")
			return nil, NewInternal("cannot recover: instance has no active OpenClaw state path")
		}
		if err := CopyOpenclawStateDir(*snap.OpenclawStateSnapshotPath, *inst.OpenclawStatePath); err != nil {
			_ = s.Store.UpdateInstanceStatus(ctx, agentID, "error")
			_ = s.Store.RecordOperation(ctx, agentID, inst.SandboxID, "recover", "failed", "state restore failed: "+err.Error())
			return nil, NewInternal("failed to restore OpenClaw state: " + err.Error())
		}
	} else {
		if _, err := s.CM.RollbackSandbox(ctx, inst.SandboxID, map[string]interface{}{
			"requestID":     fmt.Sprintf("cubeops-recover-%d", time.Now().UnixNano()),
			"snapshot_id":   snapshotID,
			"sandbox_id":    inst.SandboxID,
			"instance_type": SDKInstanceType,
		}); err != nil {
			_ = s.Store.UpdateInstanceStatus(ctx, agentID, "error")
			_ = s.Store.RecordOperation(ctx, agentID, inst.SandboxID, "recover", "failed", "rollback failed: "+err.Error())
			return nil, wrapCMError(err)
		}
	}

	// Step 4: restart OpenClaw again after rollback.
	output, err = s.restartFn(inst)
	if err != nil || output.ExitCode != 0 {
		postErr := restartErr
		if err != nil {
			postErr = err.Error()
		} else if output != nil {
			postErr = output.Stderr
		}
		_ = s.Store.UpdateInstanceStatus(ctx, agentID, "error")
		_ = s.Store.RecordOperation(ctx, agentID, inst.SandboxID, "recover", "failed", "post-rollback restart failed: "+postErr)
		return nil, NewInternal("OpenClaw restart failed after rollback: " + postErr)
	}

	_ = s.Store.SetBaseSnapshotID(ctx, agentID, snapshotID)
	_ = s.Store.UpdateInstanceStatus(ctx, agentID, "running")
	_ = s.Store.RecordOperation(ctx, agentID, inst.SandboxID, "recover", "succeeded", "")
	return &RecoverAgentResult{Recovered: true, Method: "rollback", SnapshotID: snapshotID}, nil
}

// ── CloneAgent ──────────────────────────────────────────────────────────────

// CloneAgentRequest is the typed input for CloneAgent.
type CloneAgentRequest struct {
	AgentID    string
	Name       string // optional; defaults to "{sourceName} 临时助手"
	SnapshotID string // optional; defaults to inst.BaseSnapshotID / RootfsSourceID / TemplateID
}

// CloneAgentResult holds the persisted clone AgentInstance.
type CloneAgentResult struct {
	Instance *store.AgentInstance
}

// CloneAgent clones an existing agent: resolves the snapshot source, sets
// up shared-files host state if needed, creates a new CubeMaster sandbox,
// applies OpenClaw runtime, reads back the gateway token, and persists the
// clone record. On failure after sandbox creation the clone sandbox is
// compensated.
func (s *AgentHubService) CloneAgent(ctx context.Context, req CloneAgentRequest) (*CloneAgentResult, error) {
	agentID := req.AgentID
	inst, err := s.Store.GetInstance(ctx, agentID)
	if err != nil {
		return nil, NewInternal("failed to get instance: " + err.Error())
	}
	if inst == nil {
		return nil, NewNotFound("instance not found")
	}

	// --- Resolve snapshot source ---
	snapshotID := req.SnapshotID
	if snapshotID == "" {
		if inst.BaseSnapshotID != "" {
			snapshotID = inst.BaseSnapshotID
		} else if inst.RootfsSourceID != nil && *inst.RootfsSourceID != "" {
			snapshotID = *inst.RootfsSourceID
		} else {
			snapshotID = inst.TemplateID
		}
	}

	rootfsSnapshotID := snapshotID
	var sourceOpenclawStatePath string
	if req.SnapshotID != "" {
		if snap, _ := s.Store.GetAgentSnapshot(ctx, agentID, snapshotID); snap != nil {
			if snap.RootfsSnapshotID != nil && *snap.RootfsSnapshotID != "" {
				rootfsSnapshotID = *snap.RootfsSnapshotID
			}
			if snap.OpenclawStateSnapshotPath != nil && *snap.OpenclawStateSnapshotPath != "" {
				sourceOpenclawStatePath = *snap.OpenclawStateSnapshotPath
			}
		}
	}
	if sourceOpenclawStatePath == "" && inst.OpenclawStatePath != nil {
		sourceOpenclawStatePath = *inst.OpenclawStatePath
	}

	cloneName := inst.Name + " 临时助手"
	if req.Name != "" {
		cloneName = req.Name
	}

	cloneSharedFiles := inst.PersistenceMode != nil && *inst.PersistenceMode == "shared_files"
	slog.Info("CloneAgent: debug",
		"agentID", agentID, "cloneSharedFiles", cloneSharedFiles,
		"sourceOpenclawStatePath", sourceOpenclawStatePath,
		"rootfsSnapshotID", rootfsSnapshotID, "snapshotID", snapshotID)

	var cloneOpenclawStatePath string
	var cloneOpenclawPersistID string
	if cloneSharedFiles && sourceOpenclawStatePath != "" {
		cloneOpenclawPersistID = NewOpenclawPersistID()
		statePath, err := PrepareOpenclawStateDir(cloneOpenclawPersistID)
		if err != nil {
			return nil, NewInternal(err.Error())
		}
		cloneOpenclawStatePath = statePath
		if err := CopyOpenclawStateDir(sourceOpenclawStatePath, cloneOpenclawStatePath); err != nil {
			slog.Warn("CloneAgent: failed to copy OpenClaw state for clone",
				"agentID", agentID, "err", err)
		}
	}

	// --- Build CubeMaster request ---
	requestID := fmt.Sprintf("req-%d", time.Now().UnixNano())
	extraLabels := map[string]string{}
	extraAnnotations := map[string]string{}
	if cloneSharedFiles && cloneOpenclawStatePath != "" {
		mountMeta, err := OpenclawHostMountMetadata(cloneOpenclawStatePath)
		if err != nil {
			return nil, NewInternal(err.Error())
		}
		extraLabels["agenthub.openclaw.persist_id"] = cloneOpenclawPersistID
		extraAnnotations[HostdirMountKey] = mountMeta
	}

	var distributionScope []string
	clonePersistenceMode := ""
	if cloneSharedFiles {
		clonePersistenceMode = "shared_files"
		if scope := AgenthubDistributionScope("shared_files", "snapshot"); scope != nil {
			distributionScope = scope
		}
	}
	var networkConfig map[string]interface{}
	if cloneLLMCfgForNet, _ := ResolveLLMConfig(ctx, s.Store); cloneLLMCfgForNet != nil {
		if nc, err := AgenthubNetworkConfig(cloneLLMCfgForNet); err == nil {
			networkConfig = nc
		}
	}

	cmReq := BuildCreateSandboxRequest(CreateSandboxRequest{
		RequestID:         requestID,
		Name:              cloneName,
		Engine:            inst.Engine,
		PersistenceMode:   clonePersistenceMode,
		RootfsSourceType:  "snapshot",
		RootfsSourceID:    rootfsSnapshotID,
		TemplateID:        rootfsSnapshotID,
		Annotations:       extraAnnotations,
		Labels:            extraLabels,
		NetworkConfig:     networkConfig,
		DistributionScope: distributionScope,
	})

	sandboxResp, err := s.CM.CreateSandbox(ctx, cmReq)
	if err != nil {
		if cloneOpenclawStatePath != "" {
			_ = os.RemoveAll(cloneOpenclawStatePath)
		}
		return nil, NewBadGateway("failed to create clone sandbox: " + err.Error())
	}
	var sbResult struct {
		SandboxID string `json:"sandbox_id"`
	}
	_ = json.Unmarshal(sandboxResp, &sbResult)
	if sbResult.SandboxID == "" {
		if cloneOpenclawStatePath != "" {
			_ = os.RemoveAll(cloneOpenclawStatePath)
		}
		return nil, NewBadGateway("CubeMaster returned empty sandbox_id")
	}

	botsAvailable := AvailableBots(inst.Bots)
	gatewayURL := GatewayURL(sbResult.SandboxID, inst.Domain, "")
	envPort := ResolveEnvPortFromInstance(inst.EnvURL)
	envURL := EnvURL(envPort, sbResult.SandboxID, inst.Domain)

	// --- Apply OpenClaw runtime ---
	cloneLLMCfg, err := ResolveLLMConfig(ctx, s.Store)
	if err != nil {
		slog.Warn("CloneAgent: failed to resolve LLM config", "err", err)
	}
	cloneGatewayToken := ""
	if err == nil {
		clonePlan := ResolveRuntimePlan(cloneLLMCfg, inst.Model)
		copiedOpenclawState := cloneSharedFiles && cloneOpenclawStatePath != "" && sourceOpenclawStatePath != ""
		hasOpenclawState := copiedOpenclawState || !cloneSharedFiles

		var cloneOpts *OpenclawApplyOptions
		if hasOpenclawState {
			cloneOpts = &OpenclawApplyOptions{
				Mode:                 ApplyModeMergeLLM,
				PreserveGatewayToken: true,
			}
		} else {
			cloneGatewayToken = GenerateGatewayToken()
			cloneOpts = &OpenclawApplyOptions{
				Mode:         ApplyModeFullInit,
				GatewayToken: cloneGatewayToken,
			}
		}

		applyOutput, applyErr := s.applyFn(s.envdClient, sbResult.SandboxID, inst.Domain, clonePlan, cloneOpts)
		if applyErr != nil {
			slog.Error("CloneAgent: failed to apply OpenClaw config, killing clone sandbox",
				"agentID", agentID, "sandboxID", sbResult.SandboxID, "err", applyErr)
			if _, derr := s.CM.DeleteSandbox(ctx, map[string]interface{}{
				"requestID":     fmt.Sprintf("cubeops-clone-%d", time.Now().UnixNano()),
				"sandbox_id":    sbResult.SandboxID,
				"instance_type": SDKInstanceType,
			}); derr != nil {
				slog.Warn("CloneAgent: best-effort delete failed", "sandboxID", sbResult.SandboxID, "err", derr)
			}
			if cloneOpenclawStatePath != "" {
				_ = os.RemoveAll(cloneOpenclawStatePath)
			}
			return nil, NewBadGateway("failed to apply OpenClaw config: " + applyErr.Error())
		}
		_ = applyOutput
	}

	time.Sleep(5 * time.Second)

	fallbackToken := cloneGatewayToken
	if fallbackToken == "" {
		fallbackToken = inst.GatewayToken
	}
	cloneGatewayToken = s.resolveGatewayFn(s.envdClient, sbResult.SandboxID, inst.Domain, cloneOpenclawStatePath, fallbackToken)
	if cloneGatewayToken != "" {
		gatewayURL = gatewayURL + "#token=" + cloneGatewayToken
	}

	cloneAgentID := fmt.Sprintf("agent-%s", sbResult.SandboxID)
	rootfsSnapshot := "snapshot"
	clone := &store.AgentInstance{
		ID:               cloneAgentID,
		Name:             cloneName,
		Status:           "running",
		Engine:           inst.Engine,
		Env:              inst.Env,
		Model:            inst.Model,
		Version:          inst.Version,
		Bots:             inst.Bots,
		BotsAvailable:    botsAvailable,
		Avatar:           inst.Avatar,
		AvatarTone:       inst.AvatarTone,
		SandboxID:        sbResult.SandboxID,
		TemplateID:       inst.TemplateID,
		GatewayURL:       gatewayURL,
		GatewayToken:     cloneGatewayToken,
		EnvURL:           envURL,
		PersistenceMode:  inst.PersistenceMode,
		RootfsSourceType: &rootfsSnapshot,
		RootfsSourceID:   &rootfsSnapshotID,
		Domain:           inst.Domain,
	}
	if cloneSharedFiles && cloneOpenclawPersistID != "" {
		clone.OpenclawPersistID = &cloneOpenclawPersistID
		clone.OpenclawStatePath = &cloneOpenclawStatePath
	}
	if inst.WecomConfig != nil {
		clone.WecomConfig = &store.AgentWecomConfig{
			BotID:     inst.WecomConfig.BotID,
			BotSecret: inst.WecomConfig.BotSecret,
		}
	}

	if err := s.Store.UpsertInstance(ctx, clone); err != nil {
		s.CompensateDeleteSandbox(ctx, sbResult.SandboxID, "clone_upsert")
		if cloneOpenclawStatePath != "" {
			_ = os.RemoveAll(cloneOpenclawStatePath)
		}
		return nil, NewInternal("failed to create clone record: " + err.Error())
	}

	_ = s.Store.RecordOperation(ctx, agentID, inst.SandboxID, "clone", "succeeded", "")
	return &CloneAgentResult{Instance: clone}, nil
}

// ── PublishTemplate ─────────────────────────────────────────────────────────

// PublishTemplateRequest is the typed input for PublishTemplate.
type PublishTemplateRequest struct {
	AgentID    string
	Name       string // optional template/snapshot name
	SnapshotID string // optional; if empty, a new snapshot is created
}

// PublishTemplateResult holds the IDs of the newly published template and
// its backing snapshot.
type PublishTemplateResult struct {
	TemplateID string
	SnapshotID string
}

// PublishTemplate publishes the agent as a reusable template: creates a
// snapshot (if not provided), then inserts a t_agenthub_template row and
// marks the snapshot as published, in a single DB transaction.
func (s *AgentHubService) PublishTemplate(ctx context.Context, req PublishTemplateRequest) (*PublishTemplateResult, error) {
	agentID := req.AgentID
	inst, err := s.Store.GetInstance(ctx, agentID)
	if err != nil {
		return nil, NewInternal("failed to get instance: " + err.Error())
	}
	if inst == nil {
		return nil, NewNotFound("instance not found")
	}

	snapshotID := req.SnapshotID

	persistenceMode := "full_snapshot"
	if inst.PersistenceMode != nil && *inst.PersistenceMode != "" {
		persistenceMode = *inst.PersistenceMode
	}
	sharedFiles := persistenceMode == "shared_files"
	snapName := req.Name

	// If no snapshot_id provided, create one.
	if snapshotID == "" {
		if sharedFiles {
			sourceOpenclawPath := ""
			if inst.OpenclawStatePath != nil && *inst.OpenclawStatePath != "" {
				sourceOpenclawPath = *inst.OpenclawStatePath
			}
			if sourceOpenclawPath == "" {
				return nil, NewBadRequest("current assistant does not have an OpenClaw host state directory")
			}
			snapshotID = fmt.Sprintf("agenthub-%s", uuid.New().String())
			snapPath := OpenclawHostSnapshotPath(snapshotID)
			if err := CopyOpenclawStateDir(sourceOpenclawPath, snapPath); err != nil {
				return nil, NewInternal("failed to copy OpenClaw state: " + err.Error())
			}

			rootfsSnapshotID := inst.BaseSnapshotID
			if rootfsSnapshotID == "" && inst.RootfsSourceID != nil {
				rootfsSnapshotID = *inst.RootfsSourceID
			}
			if rootfsSnapshotID == "" {
				rootfsSnapshotID = inst.TemplateID
			}
			rootfsSourceType := "template"
			if inst.RootfsSourceType != nil && *inst.RootfsSourceType != "" {
				rootfsSourceType = *inst.RootfsSourceType
			}

			_ = s.Store.DB().WithContext(ctx).Exec(
				store.UpsertSnapshotSQL(
					`snapshot_id, agent_id, sandbox_id, name, status, snapshot_kind, origin_sandbox_id,
				  rootfs_source_type, rootfs_source_id, rootfs_snapshot_id, openclaw_state_snapshot_path`,
					`?, ?, ?, ?, 'ready', 'agenthub_state', ?, ?, ?, ?, ?`,
					`agent_id = EXCLUDED.agent_id, sandbox_id = EXCLUDED.sandbox_id,
				  name = EXCLUDED.name, status = EXCLUDED.status, snapshot_kind = EXCLUDED.snapshot_kind,
				  origin_sandbox_id = EXCLUDED.origin_sandbox_id,
				  rootfs_source_type = EXCLUDED.rootfs_source_type,
				  rootfs_source_id = EXCLUDED.rootfs_source_id,
				  rootfs_snapshot_id = EXCLUDED.rootfs_snapshot_id,
				  openclaw_state_snapshot_path = EXCLUDED.openclaw_state_snapshot_path`,
					`agent_id = VALUES(agent_id), sandbox_id = VALUES(sandbox_id),
				  name = VALUES(name), status = VALUES(status), snapshot_kind = VALUES(snapshot_kind),
				  origin_sandbox_id = VALUES(origin_sandbox_id),
				  rootfs_source_type = VALUES(rootfs_source_type),
				  rootfs_source_id = VALUES(rootfs_source_id),
				  rootfs_snapshot_id = VALUES(rootfs_snapshot_id),
				  openclaw_state_snapshot_path = VALUES(openclaw_state_snapshot_path)`,
				),
				snapshotID, agentID, inst.SandboxID, snapName, inst.SandboxID,
				rootfsSourceType, rootfsSnapshotID, rootfsSnapshotID, snapPath,
			).Error
		} else {
			snapResp, err := s.CM.CreateSnapshot(ctx, map[string]interface{}{
				"requestID":     fmt.Sprintf("cubeops-publish-%d", time.Now().UnixNano()),
				"sandbox_id":    inst.SandboxID,
				"instance_type": SDKInstanceType,
			})
			if err != nil {
				return nil, wrapCMError(err)
			}
			// CubeMaster wraps snapshot_id inside a nested "snapshot" object:
			// {"ret":..., "snapshot": {"snapshot_id": "snap-xxx", ...}}
			var snapResult struct {
				Snapshot struct {
					SnapshotID string `json:"snapshot_id"`
				} `json:"snapshot"`
				// Fallback: some CubeMaster versions return snapshot_id at the
				// top level. Keep it for backward compatibility.
				SnapshotID string `json:"snapshot_id"`
			}
			_ = json.Unmarshal(snapResp, &snapResult)
			snapshotID = snapResult.Snapshot.SnapshotID
			if snapshotID == "" {
				snapshotID = snapResult.SnapshotID
			}
			if snapshotID == "" {
				return nil, NewBadGateway("CubeMaster returned empty snapshot_id: " + string(snapResp))
			}

			_ = s.Store.DB().WithContext(ctx).Exec(
				store.UpsertSnapshotSQL(
					`snapshot_id, agent_id, sandbox_id, name, status, snapshot_kind, origin_sandbox_id,
				  rootfs_source_type, rootfs_source_id, rootfs_snapshot_id`,
					`?, ?, ?, ?, 'ready', 'sandbox_snapshot', ?, 'snapshot', ?, ?`,
					`agent_id = EXCLUDED.agent_id, sandbox_id = EXCLUDED.sandbox_id,
				  status = EXCLUDED.status, snapshot_kind = EXCLUDED.snapshot_kind,
				  origin_sandbox_id = EXCLUDED.origin_sandbox_id,
				  rootfs_source_type = EXCLUDED.rootfs_source_type,
				  rootfs_source_id = EXCLUDED.rootfs_source_id,
				  rootfs_snapshot_id = EXCLUDED.rootfs_snapshot_id`,
					`agent_id = VALUES(agent_id), sandbox_id = VALUES(sandbox_id),
				  status = VALUES(status), snapshot_kind = VALUES(snapshot_kind),
				  origin_sandbox_id = VALUES(origin_sandbox_id),
				  rootfs_source_type = VALUES(rootfs_source_type),
				  rootfs_source_id = VALUES(rootfs_source_id),
				  rootfs_snapshot_id = VALUES(rootfs_snapshot_id)`,
				),
				snapshotID, agentID, inst.SandboxID, snapName, inst.SandboxID,
				snapshotID, snapshotID,
			).Error
		}
	}

	templateName := inst.Name + " 模板"
	if req.Name != "" {
		templateName = req.Name
	}
	templateID := fmt.Sprintf("tpl-%s", snapshotID)

	var persistenceModePtr *string
	if inst.PersistenceMode != nil && *inst.PersistenceMode != "" {
		persistenceModePtr = inst.PersistenceMode
	}

	txErr := s.Store.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(
			store.UpsertTemplateSQL(),
			templateID, templateName, agentID, snapshotID, inst.SandboxID,
			inst.Model, inst.Version, persistenceModePtr,
		).Error; err != nil {
			return fmt.Errorf("insert template: %w", err)
		}
		if err := tx.Exec(
			`UPDATE t_agenthub_snapshot SET published_template_id = ? WHERE snapshot_id = ? AND deleted_at IS NULL`,
			templateID, snapshotID,
		).Error; err != nil {
			return fmt.Errorf("update snapshot published_template_id: %w", err)
		}
		return nil
	})
	if txErr != nil {
		return nil, NewInternal("failed to publish template: " + txErr.Error())
	}

	if opErr := s.Store.RecordOperation(ctx, agentID, inst.SandboxID, "publish_template", "succeeded", ""); opErr != nil {
		slog.Warn("PublishTemplate: failed to record operation",
			"agentID", agentID, "templateID", templateID, "err", opErr)
	}

	return &PublishTemplateResult{TemplateID: templateID, SnapshotID: snapshotID}, nil
}
