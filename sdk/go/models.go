// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import "time"

// Sandbox is a connected CubeSandbox instance returned by create/connect.
type Sandbox struct {
	client *Client `json:"-"`

	TemplateID         string `json:"templateID"`
	SandboxID          string `json:"sandboxID"`
	Alias              string `json:"alias,omitempty"`
	ClientID           string `json:"clientID"`
	EnvdVersion        string `json:"envdVersion"`
	EnvdAccessToken    string `json:"envdAccessToken,omitempty"`
	TrafficAccessToken string `json:"trafficAccessToken,omitempty"`
	Domain             string `json:"domain,omitempty"`
}

// SandboxInfo is returned by list and get-info endpoints.
type SandboxInfo struct {
	TemplateID   string            `json:"templateID"`
	Alias        string            `json:"alias,omitempty"`
	SandboxID    string            `json:"sandboxID"`
	ClientID     string            `json:"clientID"`
	StartedAt    time.Time         `json:"startedAt"`
	EndAt        time.Time         `json:"endAt"`
	EnvdVersion  string            `json:"envdVersion"`
	Domain       string            `json:"domain,omitempty"`
	CPUCount     int               `json:"cpuCount"`
	MemoryMB     int               `json:"memoryMB"`
	DiskSizeMB   *int              `json:"diskSizeMB,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
	State        string            `json:"state"`
	VolumeMounts []VolumeMount     `json:"volumeMounts,omitempty"`
}

type VolumeMount struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type NetworkOptions struct {
	AllowOut []string
	DenyOut  []string
}

type CreateOptions struct {
	TemplateID          string
	Timeout             time.Duration
	EnvVars             map[string]string
	Metadata            map[string]string
	AllowInternetAccess *bool
	Network             NetworkOptions
	Extra               map[string]any
}

type PauseOptions struct {
	Wait     *bool
	Timeout  time.Duration
	Interval time.Duration
}

type RunCodeOptions struct {
	Language string
	Envs     map[string]string
	Timeout  time.Duration

	OnStdout func(OutputMessage)
	OnStderr func(OutputMessage)
	OnResult func(Result)
	OnError  func(ExecutionError)
}

type CommandOptions struct {
	Timeout time.Duration
	Envs    map[string]string
	Cwd     string
}

type CommandResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

type Logs struct {
	Stdout []string
	Stderr []string
}

type ExecutionError struct {
	Name      string   `json:"name"`
	Value     string   `json:"value"`
	Traceback []string `json:"traceback"`
}

type Result struct {
	Text         string         `json:"text,omitempty"`
	HTML         string         `json:"html,omitempty"`
	Markdown     string         `json:"markdown,omitempty"`
	SVG          string         `json:"svg,omitempty"`
	PNG          string         `json:"png,omitempty"`
	JPEG         string         `json:"jpeg,omitempty"`
	PDF          string         `json:"pdf,omitempty"`
	Latex        string         `json:"latex,omitempty"`
	JSONData     map[string]any `json:"json_data,omitempty"`
	JavaScript   string         `json:"javascript,omitempty"`
	IsMainResult bool           `json:"is_main_result,omitempty"`
	Extra        map[string]any `json:"extra,omitempty"`
}

type Execution struct {
	Results        []Result
	Logs           Logs
	Error          *ExecutionError
	ExecutionCount *int
	Text           string
}

type OutputMessage struct {
	Text      string
	Timestamp string
	IsStderr  bool
}

func (e *Execution) mainText() string {
	if e == nil {
		return ""
	}
	if e.Text != "" {
		return e.Text
	}
	for _, result := range e.Results {
		if result.IsMainResult {
			return result.Text
		}
	}
	return ""
}
