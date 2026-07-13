// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

type TemplateBuildJob struct {
	JobID        string `json:"jobID"`
	TemplateID   string `json:"templateID"`
	Status       string `json:"status"`
	Phase        string `json:"phase"`
	Progress     int    `json:"progress"`
	ErrorMessage string `json:"errorMessage"`
}

type TemplateInfo struct {
	TemplateID          string `json:"templateID"`
	InstanceType        string `json:"instanceType,omitempty"`
	Version             string `json:"version,omitempty"`
	Status              string `json:"status,omitempty"`
	LastError           string `json:"lastError,omitempty"`
	CreatedAt           string `json:"createdAt,omitempty"`
	ImageInfo           string `json:"imageInfo,omitempty"`
	JobID               string `json:"jobID,omitempty"`
	NetworkType         string `json:"networkType,omitempty"`
	AllowInternetAccess *bool  `json:"allowInternetAccess,omitempty"`
}

type TemplateBuildStatus struct {
	BuildID    string `json:"buildID"`
	TemplateID string `json:"templateID"`
	Status     string `json:"status"`
	Progress   int    `json:"progress"`
	Message    string `json:"message"`
}

type BuildTemplateOptions struct {
	Image                string
	InstanceType         string
	WritableLayerSize    string
	ExposedPorts         []uint16
	ProbePort            *uint16
	ProbePath            string
	CPU                  *uint32
	Memory               *uint32
	Env                  map[string]string
	AllowInternetAccess  *bool
	NetworkType          string
	Nodes                []string
	RegistryUsername     string
	RegistryPassword     string
	Command              []string
	Args                 []string
	DNS                  []string
	AllowOut             []string
	DenyOut              []string
	// Extra is merged into the request payload after the named fields above,
	// so duplicate keys override those fields to match Python kwargs behavior.
	Extra                map[string]any
}

func (c *Client) ListTemplates(ctx context.Context) ([]TemplateInfo, error) {
	var templates []TemplateInfo
	if err := c.doJSON(ctx, http.MethodGet, "/templates", nil, &templates, http.StatusOK); err != nil {
		return nil, err
	}
	return templates, nil
}

func (c *Client) GetTemplate(ctx context.Context, templateID string) (*TemplateInfo, error) {
	if strings.TrimSpace(templateID) == "" {
		return nil, fmt.Errorf("templateID is required")
	}
	var templateInfo TemplateInfo
	path := "/templates/" + url.PathEscape(templateID)
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &templateInfo, http.StatusOK); err != nil {
		return nil, err
	}
	return &templateInfo, nil
}

func (c *Client) BuildTemplate(ctx context.Context, opts BuildTemplateOptions) (*TemplateBuildJob, error) {
	payload, err := buildTemplatePayload(opts)
	if err != nil {
		return nil, err
	}
	var job TemplateBuildJob
	if err := c.doJSON(ctx, http.MethodPost, "/templates", payload, &job, http.StatusAccepted); err != nil {
		return nil, err
	}
	return &job, nil
}

func (c *Client) RebuildTemplate(ctx context.Context, templateID string, extra map[string]any) (*TemplateBuildJob, error) {
	if strings.TrimSpace(templateID) == "" {
		return nil, fmt.Errorf("templateID is required")
	}
	if extra == nil {
		extra = map[string]any{}
	}
	var job TemplateBuildJob
	path := "/templates/" + url.PathEscape(templateID)
	if err := c.doJSON(ctx, http.MethodPost, path, extra, &job, http.StatusAccepted); err != nil {
		return nil, err
	}
	return &job, nil
}

func (c *Client) DeleteTemplate(ctx context.Context, templateID string) error {
	if strings.TrimSpace(templateID) == "" {
		return fmt.Errorf("templateID is required")
	}
	path := "/templates/" + url.PathEscape(templateID)
	return c.doJSON(ctx, http.MethodDelete, path, nil, nil, http.StatusNoContent)
}

func (c *Client) GetTemplateBuildStatus(ctx context.Context, templateID, buildID string) (*TemplateBuildStatus, error) {
	if strings.TrimSpace(templateID) == "" || strings.TrimSpace(buildID) == "" {
		return nil, fmt.Errorf("templateID and buildID are required")
	}
	var status TemplateBuildStatus
	path := "/templates/" + url.PathEscape(templateID) + "/builds/" + url.PathEscape(buildID) + "/status"
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &status, http.StatusOK); err != nil {
		return nil, err
	}
	return &status, nil
}

func buildTemplatePayload(opts BuildTemplateOptions) (map[string]any, error) {
	image := strings.TrimSpace(opts.Image)
	if image == "" {
		return nil, fmt.Errorf("image is required")
	}

	payload := map[string]any{
		"image": image,
	}
	if instanceType := strings.TrimSpace(opts.InstanceType); instanceType != "" {
		payload["instanceType"] = instanceType
	}
	if writableLayerSize := strings.TrimSpace(opts.WritableLayerSize); writableLayerSize != "" {
		payload["writableLayerSize"] = writableLayerSize
	}
	if len(opts.ExposedPorts) > 0 {
		payload["exposedPorts"] = append([]uint16(nil), opts.ExposedPorts...)
	}
	if opts.ProbePort != nil {
		payload["probePort"] = *opts.ProbePort
	}
	if probePath := strings.TrimSpace(opts.ProbePath); probePath != "" {
		payload["probePath"] = probePath
	}
	if opts.CPU != nil {
		payload["cpu"] = *opts.CPU
	}
	if opts.Memory != nil {
		payload["memory"] = *opts.Memory
	}
	if env := templateEnvList(opts.Env); len(env) > 0 {
		payload["env"] = env
	}
	if opts.AllowInternetAccess != nil {
		payload["allowInternetAccess"] = *opts.AllowInternetAccess
	}
	if networkType := strings.TrimSpace(opts.NetworkType); networkType != "" {
		payload["networkType"] = networkType
	}
	if len(opts.Nodes) > 0 {
		payload["nodes"] = append([]string(nil), opts.Nodes...)
	}
	if registryUsername := strings.TrimSpace(opts.RegistryUsername); registryUsername != "" {
		payload["registryUsername"] = registryUsername
	}
	if registryPassword := strings.TrimSpace(opts.RegistryPassword); registryPassword != "" {
		payload["registryPassword"] = registryPassword
	}
	if len(opts.Command) > 0 {
		payload["command"] = append([]string(nil), opts.Command...)
	}
	if len(opts.Args) > 0 {
		payload["args"] = append([]string(nil), opts.Args...)
	}
	if len(opts.DNS) > 0 {
		payload["dns"] = append([]string(nil), opts.DNS...)
	}
	if len(opts.AllowOut) > 0 {
		payload["allowOut"] = append([]string(nil), opts.AllowOut...)
	}
	if len(opts.DenyOut) > 0 {
		payload["denyOut"] = append([]string(nil), opts.DenyOut...)
	}
	for key, value := range opts.Extra {
		payload[key] = value
	}
	return payload, nil
}

func templateEnvList(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+env[key])
	}
	return out
}
