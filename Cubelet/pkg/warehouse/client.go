// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package warehouse

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
	"strings"
	"time"
)

const nodeIDHeader = "X-Cube-Node-ID"

var (
	// ErrNotFound means CubeOps has no copy of this version.
	ErrNotFound = errors.New("component version not in warehouse")
	// ErrDownloadFailed is a transport / 5xx / corrupt blob failure.
	ErrDownloadFailed = errors.New("component version download failed")

	s3XMLCodeRe = regexp.MustCompile(`(?s)<Code>([^<]+)</Code>`)
)

// Client talks to CubeOps /internal/warehouse with no cluster token.
type Client struct {
	base   string
	nodeID string
	arch   string
	http   *http.Client
}

func NewClient(base, nodeID, arch string, timeout time.Duration) *Client {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	return &Client{
		base:   base,
		nodeID: nodeID,
		arch:   arch,
		http:   &http.Client{Timeout: timeout},
	}
}

func (c *Client) enabled() bool {
	return c != nil && c.base != ""
}

func (c *Client) downloadTimeout() time.Duration {
	if c != nil && c.http != nil && c.http.Timeout > 0 {
		return c.http.Timeout
	}
	return 30 * time.Minute
}

// BlobRef is a time-limited download ticket issued by CubeOps.
type BlobRef struct {
	URL       string `json:"url"`
	ExpiresIn int    `json:"expiresIn"`
	SizeBytes int64  `json:"sizeBytes"`
	Checksum  string `json:"checksum"`
	Component string `json:"-"`
	Version   string `json:"-"`
}

// ResolveBlob asks CubeOps for a presigned GET URL. Error classes match the
// previous DownloadBlob contract (ErrNotFound vs ErrDownloadFailed).
func (c *Client) ResolveBlob(ctx context.Context, component, version string) (*BlobRef, error) {
	if !c.enabled() {
		return nil, fmt.Errorf("%w: cubeops_addr is not configured", ErrNotFound)
	}
	q := url.Values{}
	q.Set("arch", c.arch)
	q.Set("component", component)
	q.Set("version", version)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/internal/warehouse/blob?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	c.decorate(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDownloadFailed, sanitizeHTTPErr(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		code, msg := readAPIError(resp)
		if code == "warehouse_not_found" {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, msg)
		}
		return nil, fmt.Errorf("%w: HTTP 404 %s", ErrDownloadFailed, sanitizeMsg(msg))
	}
	if resp.StatusCode == http.StatusNotImplemented {
		_, msg := readAPIError(resp)
		return nil, fmt.Errorf("%w: warehouse disabled: %s", ErrNotFound, sanitizeMsg(msg))
	}
	if resp.StatusCode >= 400 {
		_, msg := readAPIError(resp)
		return nil, fmt.Errorf("%w: HTTP %d %s", ErrDownloadFailed, resp.StatusCode, sanitizeMsg(msg))
	}
	var ref BlobRef
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&ref); err != nil {
		return nil, fmt.Errorf("%w: decode blob ref: %v", ErrDownloadFailed, err)
	}
	if strings.TrimSpace(ref.URL) == "" {
		return nil, fmt.Errorf("%w: empty presigned url", ErrDownloadFailed)
	}
	ref.Component = component
	ref.Version = version
	return &ref, nil
}

// OpenBlob GETs the presigned URL with a dedicated client (explicit Timeout).
func (c *Client) OpenBlob(ctx context.Context, ref *BlobRef) (io.ReadCloser, error) {
	if ref == nil || strings.TrimSpace(ref.URL) == "" {
		return nil, fmt.Errorf("%w: empty blob ref", ErrDownloadFailed)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDownloadFailed, sanitizeHTTPErr(err))
	}
	origHost := req.URL.Host
	cli := &http.Client{
		Timeout:       c.downloadTimeout(),
		CheckRedirect: sameHostRedirects(origHost),
	}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDownloadFailed, sanitizeHTTPErr(err))
	}
	if resp.StatusCode == http.StatusNotFound {
		defer resp.Body.Close()
		return nil, fmt.Errorf("%w: object missing", ErrNotFound)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		code := readS3ErrorCode(resp)
		return nil, fmt.Errorf("%w: object store HTTP %d %s", ErrDownloadFailed, resp.StatusCode, code)
	}
	return resp.Body, nil
}

// DownloadBlob resolves a ticket then opens the object. Tests and callers that
// only need a stream can keep using this helper.
func (c *Client) DownloadBlob(ctx context.Context, component, version string) (io.ReadCloser, error) {
	ref, err := c.ResolveBlob(ctx, component, version)
	if err != nil {
		return nil, err
	}
	return c.OpenBlob(ctx, ref)
}

type preinstallJob struct {
	ID        string `json:"id"`
	NodeID    string `json:"nodeId"`
	Arch      string `json:"arch"`
	Component string `json:"component"`
	Version   string `json:"version"`
	Status    string `json:"status"`
}

func (c *Client) ListJobs(ctx context.Context) ([]preinstallJob, error) {
	if !c.enabled() {
		return nil, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/internal/warehouse/jobs", nil)
	if err != nil {
		return nil, err
	}
	c.decorate(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, sanitizeHTTPErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		_, msg := readAPIError(resp)
		return nil, fmt.Errorf("list warehouse jobs: HTTP %d %s", resp.StatusCode, sanitizeMsg(msg))
	}
	var wrap struct {
		Jobs []preinstallJob `json:"jobs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wrap); err != nil {
		return nil, err
	}
	return wrap.Jobs, nil
}

func (c *Client) AckJob(ctx context.Context, id, status, errMsg string) error {
	if !c.enabled() {
		return nil
	}
	body, _ := json.Marshal(map[string]string{"status": status, "error": errMsg})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/internal/warehouse/jobs/"+url.PathEscape(id)+"/ack", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.decorate(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return sanitizeHTTPErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		_, msg := readAPIError(resp)
		return fmt.Errorf("ack job: HTTP %d %s", resp.StatusCode, sanitizeMsg(msg))
	}
	return nil
}

// InventoryItem is one locally inventoried component version.
type InventoryItem struct {
	Component string `json:"component"`
	Version   string `json:"version"`
}

func (c *Client) PutInventory(ctx context.Context, items []InventoryItem) error {
	if !c.enabled() {
		return nil
	}
	if items == nil {
		items = []InventoryItem{}
	}
	body, err := json.Marshal(map[string]any{
		"arch":  c.arch,
		"items": items,
	})
	if err != nil {
		return fmt.Errorf("report inventory: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.base+"/internal/warehouse/inventory", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.decorate(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("report inventory: %v", sanitizeHTTPErr(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		_, msg := readAPIError(resp)
		return fmt.Errorf("report inventory: HTTP %d %s", resp.StatusCode, sanitizeMsg(msg))
	}
	return nil
}

func (c *Client) decorate(req *http.Request) {
	if c.nodeID != "" {
		req.Header.Set(nodeIDHeader, c.nodeID)
	}
}

func readAPIError(resp *http.Response) (code, msg string) {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var wrap struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if json.Unmarshal(raw, &wrap) == nil {
		return wrap.Code, wrap.Error
	}
	return "", strings.TrimSpace(string(raw))
}

func readS3ErrorCode(resp *http.Response) string {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if m := s3XMLCodeRe.FindSubmatch(raw); len(m) == 2 {
		return string(m[1])
	}
	return fmt.Sprintf("HTTP %d", resp.StatusCode)
}

func sanitizeHTTPErr(err error) error {
	if err == nil {
		return nil
	}
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err
	}
	return errors.New(sanitizeMsg(err.Error()))
}

func sameHostRedirects(origHost string) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= 2 {
			return fmt.Errorf("too many redirects")
		}
		if !strings.EqualFold(req.URL.Host, origHost) {
			return fmt.Errorf("redirect to disallowed host %s", req.URL.Host)
		}
		return nil
	}
}

func sanitizeMsg(msg string) string {
	if i := strings.Index(msg, "X-Amz-"); i >= 0 {
		return strings.TrimSpace(msg[:i]) + "[redacted]"
	}
	return msg
}
