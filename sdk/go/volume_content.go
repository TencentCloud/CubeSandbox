// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// VolumeEntryStat uses the E2B volume-content entry wire format.
type VolumeEntryStat struct {
	Name   string    `json:"name"`
	Type   string    `json:"type"`
	Path   string    `json:"path"`
	Size   int64     `json:"size"`
	Mode   int64     `json:"mode"`
	UID    int64     `json:"uid"`
	GID    int64     `json:"gid"`
	ATime  time.Time `json:"atime"`
	MTime  time.Time `json:"mtime"`
	CTime  time.Time `json:"ctime"`
	Target *string   `json:"target,omitempty"`
}

// UnmarshalJSON distinguishes missing or null fields from valid zero metadata.
func (e *VolumeEntryStat) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return errors.New("invalid volume entry response")
	}
	for _, key := range []string{"name", "type", "path", "size", "mode", "uid", "gid", "atime", "mtime", "ctime"} {
		raw, ok := fields[key]
		if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return errors.New("invalid volume entry response")
		}
	}
	type entryWire VolumeEntryStat
	var wire entryWire
	if err := json.Unmarshal(data, &wire); err != nil || !validVolumeEntry(VolumeEntryStat(wire)) {
		return errors.New("invalid volume entry response")
	}
	*e = VolumeEntryStat(wire)
	return nil
}

// VolumeMetadataOptions preserves zero IDs/modes and force=false on the wire.
type VolumeMetadataOptions struct {
	UID   *int64
	GID   *int64
	Mode  *int64
	Force *bool
}

// VolumeContent is a direct client, not a helper sandbox. The server must
// implement scoped authorization, attachment leases and path confinement.
type VolumeContent struct {
	baseURL string
	token   string
	http    *http.Client
}

// String masks the capability when logging either a value or a pointer.
func (v VolumeContent) String() string {
	return fmt.Sprintf("VolumeContent{baseURL:%q, token:***}", v.baseURL)
}

// GoString also masks the capability for %#v formatting.
func (v VolumeContent) GoString() string {
	return v.String()
}

var volumeBearerRe = regexp.MustCompile(`^[A-Za-z0-9._~+/-]+=*$`)

// VolumeContent binds an existing volume and its gateway-issued capability.
// VolumeAPIURL must explicitly point to a deployed content service; the current
// management API alone cannot serve files. The caller retains Client ownership.
func (c *Client) VolumeContent(volume *VolumeInfo) (*VolumeContent, error) {
	if volume == nil {
		return nil, errors.New("volume is required")
	}
	if err := validateVolumeID(volume.VolumeID); err != nil {
		return nil, err
	}
	if c.config.VolumeAPIURL == "" {
		return nil, &APIError{Kind: "volume_content_unavailable", Message: "configure CUBE_VOLUME_API_URL for a deployed content gateway"}
	}
	u, err := url.Parse(c.config.VolumeAPIURL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || strings.ContainsAny(c.config.VolumeAPIURL, "?#") {
		return nil, errors.New("volume API URL must be HTTP(S) without credentials, query or fragment")
	}
	if port := u.Port(); port != "" {
		p, err := strconv.Atoi(port)
		if err != nil || p < 1 || p > 65535 {
			return nil, errors.New("invalid volume API port")
		}
	}
	if u.Scheme != "https" && u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" && u.Hostname() != "::1" {
		return nil, errors.New("volume content credentials require HTTPS outside loopback")
	}
	if volume.Token == "" {
		return nil, &APIError{Kind: apiErrorKindAuthentication, Message: "a gateway-issued volume content token is required"}
	}
	if !volumeBearerRe.MatchString(volume.Token) {
		return nil, errors.New("volume token must use the Bearer character set")
	}
	client := *c.controlHTTP
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	client.Timeout = c.config.RequestTimeout
	return &VolumeContent{baseURL: strings.TrimRight(u.String(), "/") + "/volumecontent/" + volume.VolumeID, token: volume.Token, http: &client}, nil
}

func validateVolumeContentPath(path string) error {
	if err := validateVolumeMountPath(path); err != nil {
		return err
	}
	if strings.ContainsAny(path, "\x00\\") {
		return errors.New("volume path cannot contain NUL or backslash")
	}
	return nil
}

func (v *VolumeContent) request(ctx context.Context, method, endpoint, path string, params url.Values, body io.Reader) (*http.Response, error) {
	if err := validateVolumeContentPath(path); err != nil {
		return nil, err
	}
	if params == nil {
		params = url.Values{}
	}
	params.Set("path", path)
	req, err := http.NewRequestWithContext(ctx, method, v.baseURL+"/"+endpoint+"?"+params.Encode(), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+v.token)
	if method == http.MethodPut {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	if method == http.MethodPatch {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := v.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024+1))
	var details struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if len(raw) <= 64*1024 {
		_ = json.Unmarshal(raw, &details)
	}
	message := fmt.Sprintf("volume content request failed (HTTP %d)", resp.StatusCode)
	if details.Message != "" {
		message = strings.ReplaceAll(details.Message, v.token, "***")
	}
	kind := apiErrorKindAPI
	switch resp.StatusCode {
	case 401, 403:
		kind = apiErrorKindAuthentication
	case 404:
		kind = "volume_path_not_found"
	case 405, 501, 503:
		kind = "volume_content_unavailable"
	}
	return nil, &APIError{StatusCode: resp.StatusCode, Kind: kind, Message: message}
}

// ReadFileStream returns a live response body. Close it on early exit; context
// cancellation and Config.RequestTimeout bound the entire request lifetime.
func (v *VolumeContent) ReadFileStream(ctx context.Context, path string) (io.ReadCloser, error) {
	resp, err := v.request(ctx, http.MethodGet, "file", path, nil, nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (v *VolumeContent) ReadFile(ctx context.Context, path string) ([]byte, error) {
	stream, err := v.ReadFileStream(ctx, path)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	return io.ReadAll(stream)
}

func volumeMetadataParams(opts VolumeMetadataOptions) (url.Values, error) {
	params := url.Values{}
	for name, value := range map[string]*int64{"uid": opts.UID, "gid": opts.GID, "mode": opts.Mode} {
		if value != nil {
			if *value < 0 {
				return nil, fmt.Errorf("%s must be non-negative", name)
			}
			params.Set(name, strconv.FormatInt(*value, 10))
		}
	}
	if opts.Force != nil {
		params.Set("force", strconv.FormatBool(*opts.Force))
	}
	return params, nil
}

func validVolumeEntry(e VolumeEntryStat) bool {
	typeOK := e.Type == "file" || e.Type == "directory" || e.Type == "symlink" || e.Type == "unknown"
	return typeOK && strings.HasPrefix(e.Path, "/") && e.Size >= 0 && e.Mode >= 0 && e.UID >= 0 && e.GID >= 0 && !e.ATime.IsZero() && !e.MTime.IsZero() && !e.CTime.IsZero()
}

func decodeVolumeEntry(resp *http.Response) (*VolumeEntryStat, error) {
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024+1))
	if err != nil || len(raw) > 1024*1024 {
		return nil, errors.New("invalid volume entry response")
	}
	var entry VolumeEntryStat
	if err := json.Unmarshal(raw, &entry); err != nil {
		return nil, errors.New("invalid volume entry response")
	}
	return &entry, nil
}

// WriteFile streams caller-owned data without closing it or retrying mutations.
func (v *VolumeContent) WriteFile(ctx context.Context, path string, data io.Reader, opts VolumeMetadataOptions) (*VolumeEntryStat, error) {
	if data == nil {
		return nil, errors.New("file reader is required")
	}
	params, err := volumeMetadataParams(opts)
	if err != nil {
		return nil, err
	}
	if _, ok := data.(io.Closer); ok {
		data = io.NopCloser(data)
	}
	resp, err := v.request(ctx, http.MethodPut, "file", path, params, data)
	if err != nil {
		return nil, err
	}
	return decodeVolumeEntry(resp)
}

func (v *VolumeContent) List(ctx context.Context, path string, depth *int) ([]VolumeEntryStat, error) {
	params := url.Values{}
	if depth != nil {
		if *depth < 0 {
			return nil, errors.New("depth must be non-negative")
		}
		params.Set("depth", strconv.Itoa(*depth))
	}
	resp, err := v.request(ctx, http.MethodGet, "dir", path, params, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var entries []VolumeEntryStat
	decoder := json.NewDecoder(resp.Body)
	if err := decoder.Decode(&entries); err != nil || entries == nil {
		return nil, errors.New("invalid volume directory response")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("invalid volume directory response")
	}
	return entries, nil
}

func (v *VolumeContent) GetInfo(ctx context.Context, path string) (*VolumeEntryStat, error) {
	resp, err := v.request(ctx, http.MethodGet, "path", path, nil, nil)
	if err != nil {
		return nil, err
	}
	return decodeVolumeEntry(resp)
}

func (v *VolumeContent) Exists(ctx context.Context, path string) (bool, error) {
	_, err := v.GetInfo(ctx, path)
	if errors.Is(err, ErrVolumePathNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (v *VolumeContent) MakeDir(ctx context.Context, path string, opts VolumeMetadataOptions) (*VolumeEntryStat, error) {
	params, err := volumeMetadataParams(opts)
	if err != nil {
		return nil, err
	}
	resp, err := v.request(ctx, http.MethodPost, "dir", path, params, nil)
	if err != nil {
		return nil, err
	}
	return decodeVolumeEntry(resp)
}

func (v *VolumeContent) UpdateMetadata(ctx context.Context, path string, opts VolumeMetadataOptions) (*VolumeEntryStat, error) {
	if opts.Force != nil {
		return nil, errors.New("force is not a metadata update option")
	}
	params, err := volumeMetadataParams(opts)
	if err != nil {
		return nil, err
	}
	payload := map[string]int64{}
	for key := range params {
		value, _ := strconv.ParseInt(params.Get(key), 10, 64)
		payload[key] = value
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	resp, err := v.request(ctx, http.MethodPatch, "path", path, nil, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	return decodeVolumeEntry(resp)
}

func (v *VolumeContent) Remove(ctx context.Context, path string) error {
	if strings.Trim(path, "/") == "" {
		return errors.New("removing volume root is not allowed")
	}
	resp, err := v.request(ctx, http.MethodDelete, "path", path, nil, nil)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}
