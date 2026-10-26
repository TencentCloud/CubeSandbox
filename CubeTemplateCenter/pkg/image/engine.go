// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package image

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

var errEngineImageNotFound = errors.New("engine image not found")

type engineClient interface {
	ImageInspect(context.Context, string) (*dockerInspectImage, error)
	ImagePull(context.Context, string, string) (io.ReadCloser, error)
}

var newEngineClient = newHTTPDockerEngineClient

func engineImageInspect(ctx context.Context, imageRef string) (*dockerInspectImage, error) {
	cli, err := newEngineClient()
	if err != nil {
		return nil, err
	}
	return engineImageInspectWithClient(ctx, cli, imageRef)
}

func engineImagePull(ctx context.Context, spec SourceSpec) error {
	cli, err := newEngineClient()
	if err != nil {
		return err
	}
	return engineImagePullWithClient(ctx, cli, spec)
}

func engineImageInspectWithClient(ctx context.Context, cli engineClient, imageRef string) (*dockerInspectImage, error) {
	return cli.ImageInspect(ctx, imageRef)
}

func engineImagePullWithClient(ctx context.Context, cli engineClient, spec SourceSpec) error {
	registryAuth := ""
	if spec.RegistryUsername != "" || spec.RegistryPassword != "" {
		var err error
		registryAuth, err = engineRegistryAuth(spec)
		if err != nil {
			return err
		}
	}
	body, err := cli.ImagePull(ctx, spec.ImageRef, registryAuth)
	if err != nil {
		return err
	}
	defer body.Close()
	return streamEnginePullProgress(body, spec.OnPullProgress)
}

type httpDockerEngineClient struct {
	client  *http.Client
	baseURL string
}

func newHTTPDockerEngineClient() (engineClient, error) {
	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}
	u, err := url.Parse(host)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "unix":
		socketPath := u.Path
		if socketPath == "" {
			socketPath = strings.TrimPrefix(host, "unix://")
		}
		if socketPath == "" {
			return nil, errors.New("empty docker unix socket path")
		}
		dialer := &net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}
		transport := &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.DialContext(ctx, "unix", socketPath)
			},
			IdleConnTimeout: 90 * time.Second,
		}
		return &httpDockerEngineClient{
			client:  &http.Client{Transport: transport},
			baseURL: "http://docker",
		}, nil
	case "tcp":
		return &httpDockerEngineClient{
			client:  newDockerEngineHTTPClient(),
			baseURL: "http://" + u.Host,
		}, nil
	case "http", "https":
		return &httpDockerEngineClient{
			client:  newDockerEngineHTTPClient(),
			baseURL: strings.TrimRight(host, "/"),
		}, nil
	default:
		return nil, fmt.Errorf("unsupported DOCKER_HOST scheme %q", u.Scheme)
	}
}

func newDockerEngineHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}

func (c *httpDockerEngineClient) ImageInspect(ctx context.Context, imageRef string) (*dockerInspectImage, error) {
	resp, err := c.do(ctx, http.MethodGet, "/images/"+url.PathEscape(imageRef)+"/json", nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errEngineImageNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, engineHTTPError("inspect", resp)
	}
	inspect := &dockerInspectImage{}
	if err := json.NewDecoder(resp.Body).Decode(inspect); err != nil {
		return nil, err
	}
	return inspect, nil
}

func (c *httpDockerEngineClient) ImagePull(ctx context.Context, imageRef, registryAuth string) (io.ReadCloser, error) {
	query := engineImagePullQuery(imageRef)
	resp, err := c.do(ctx, http.MethodPost, "/images/create?"+query.Encode(), nil, registryAuth)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, engineHTTPError("pull", resp)
	}
	return resp.Body, nil
}

func (c *httpDockerEngineClient) do(ctx context.Context, method, path string, body io.Reader, registryAuth string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	if registryAuth != "" {
		req.Header.Set("X-Registry-Auth", registryAuth)
	}
	return c.client.Do(req)
}

func engineHTTPError(action string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = resp.Status
	}
	return fmt.Errorf("engine %s failed: status=%d body=%s", action, resp.StatusCode, msg)
}

func engineImagePullQuery(imageRef string) url.Values {
	ref := strings.TrimPrefix(imageRef, "docker://")
	lastSlash := strings.LastIndex(ref, "/")
	query := url.Values{}
	if at := strings.LastIndex(ref, "@"); at > lastSlash {
		query.Set("fromImage", ref[:at])
		query.Set("tag", ref[at+1:])
		return query
	}
	if colon := strings.LastIndex(ref, ":"); colon > lastSlash {
		query.Set("fromImage", ref[:colon])
		query.Set("tag", ref[colon+1:])
		return query
	}
	query.Set("fromImage", ref)
	query.Set("tag", "latest")
	return query
}

func engineRegistryAuth(spec SourceSpec) (string, error) {
	payload := map[string]string{
		"username":      spec.RegistryUsername,
		"password":      spec.RegistryPassword,
		"serveraddress": registryHostFromImageRef(spec.ImageRef),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(data), nil
}

type enginePullEvent struct {
	ID             string                   `json:"id"`
	Status         string                   `json:"status"`
	ProgressDetail enginePullProgressDetail `json:"progressDetail"`
	Error          string                   `json:"error"`
	ErrorDetail    *enginePullErrorDetail   `json:"errorDetail"`
}

type enginePullProgressDetail struct {
	Current int64 `json:"current"`
	Total   int64 `json:"total"`
}

type enginePullErrorDetail struct {
	Message string `json:"message"`
}

func streamEnginePullProgress(r io.Reader, onProgress ProgressFunc) error {
	dec := json.NewDecoder(r)
	parser := newEnginePullParser()
	var (
		lastEmit    time.Time
		latest      PullProgress
		haveLatest  bool
		latestDirty bool
	)
	emit := func(p PullProgress) {
		if onProgress == nil {
			return
		}
		latest = p
		haveLatest = true
		now := time.Now()
		if now.Sub(lastEmit) < progressEmitInterval {
			latestDirty = true
			return
		}
		lastEmit = now
		latestDirty = false
		onProgress(p)
	}
	for {
		var event enginePullEvent
		if err := dec.Decode(&event); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("decode engine pull progress: %w", err)
		}
		if event.Error != "" || event.ErrorDetail != nil {
			msg := event.Error
			if event.ErrorDetail != nil && event.ErrorDetail.Message != "" {
				msg = event.ErrorDetail.Message
			}
			return fmt.Errorf("engine pull failed: %s", strings.TrimSpace(msg))
		}
		if p, ok := parser.feed(event); ok {
			emit(p)
		}
	}
	if onProgress != nil && haveLatest && latestDirty {
		onProgress(latest)
	}
	return nil
}
