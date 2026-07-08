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
	EndAt        *time.Time        `json:"endAt,omitempty"`
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
	// AllowPublicTraffic gates default public egress; nil leaves it to the
	// server default. AllowOut/DenyOut are L3/L4 CIDRs or hostnames; Rules are
	// L7 host/path/SNI matches with audit and credential injection.
	AllowPublicTraffic *bool
	AllowOut           []string
	DenyOut            []string
	Rules              []Rule
}

type CreateOptions struct {
	TemplateID string
	// Optional idle TTL; nil omits the field. See docs/guide/lifecycle.md.
	Timeout             *time.Duration
	EnvVars             map[string]string
	Metadata            map[string]string
	AllowInternetAccess *bool
	Network             NetworkOptions
	Extra               map[string]any
}

// DurationPtr returns a pointer to d. It is a convenience for optional
// duration fields such as CreateOptions.Timeout and Sandbox.Resume, where nil
// means "not provided; let the server decide".
func DurationPtr(d time.Duration) *time.Duration {
	return &d
}

// NeverTimeout requests a sandbox that never idle-times-out. See docs/guide/lifecycle.md.
const NeverTimeout time.Duration = -1

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
	// User authenticates the envd process call (Basic auth). Empty defaults to
	// "root" to match the Python SDK and avoid old-envd "no user specified".
	User string
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

// WriteEntry is a path + data pair for Files.WriteFiles.
type WriteEntry struct {
	Path string
	Data []byte
}

// FileEntry represents a file or directory returned by envd filesystem RPCs.
type FileEntry struct {
	Name         string `json:"name"`
	Type         string `json:"type"`
	Path         string `json:"path"`
	Size         int64  `json:"size,string"`
	Mode         int    `json:"mode"`
	Permissions  string `json:"permissions"`
	Owner        string `json:"owner"`
	Group        string `json:"group"`
	ModifiedTime string `json:"modifiedTime"`
}

func (e FileEntry) IsDir() bool {
	return e.Type == "FILE_TYPE_DIRECTORY"
}

// NotFoundError is returned when a filesystem path does not exist.
type NotFoundError struct {
	Path    string
	Message string
}

func (e *NotFoundError) Error() string {
	return e.Message
}

// WatchEvent represents a filesystem change detected by WatchDir.
type WatchEvent struct {
	Name string `json:"name"`
	Type string `json:"type"`
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
