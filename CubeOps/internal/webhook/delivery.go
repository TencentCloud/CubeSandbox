// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/config"
)

const (
	signatureHeader = "X-Cube-Webhook-Signature"
	timestampHeader = "X-Cube-Webhook-Timestamp"
)

type endpoint struct {
	name       string
	url        string
	events     map[string]struct{}
	secret     string
	timeout    time.Duration
	maxRetries int
}

type dispatcher struct {
	client         *http.Client
	endpoints      []endpoint
	initialBackoff time.Duration
	maxBackoff     time.Duration
}

// newDispatcher 根据 webhook 配置创建并初始化一个 dispatcher 实例。
//
// 该函数会校验每个 endpoint 的配置：URL 必须是合法的 http/https 地址，
// 且至少订阅一个受支持的事件；对未命名的 endpoint 会以索引生成默认名称，
// 并按 endpoint 级、全局默认、内置默认（3）的优先级确定最大重试次数。
//
// 参数 cfg 为 webhook 配置，包含 endpoints 列表、默认重试次数及退避设置。
//
// 返回初始化完成的 dispatcher；若任一 endpoint 配置非法则返回错误。
func newDispatcher(cfg config.WebhookConfig) (*dispatcher, error) {
	endpoints := make([]endpoint, 0, len(cfg.Endpoints))
	for index, item := range cfg.Endpoints {
		parsed, err := url.Parse(strings.TrimSpace(item.URL))
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return nil, fmt.Errorf("webhook endpoint %d has invalid url %q", index, item.URL)
		}
		if len(item.Events) == 0 {
			return nil, fmt.Errorf("webhook endpoint %q has no subscribed events", item.Name)
		}
		events := make(map[string]struct{}, len(item.Events))
		for _, name := range item.Events {
			name = strings.TrimSpace(name)
			if _, ok := supportedEvents[name]; !ok {
				return nil, fmt.Errorf("webhook endpoint %q subscribes to unsupported event %q", item.Name, name)
			}
			events[name] = struct{}{}
		}
		name := strings.TrimSpace(item.Name)
		if name == "" {
			name = fmt.Sprintf("endpoint-%d", index)
		}
		maxRetries := 3
		if cfg.DefaultMaxRetries != nil {
			maxRetries = *cfg.DefaultMaxRetries
		}
		if item.MaxRetries != nil {
			maxRetries = *item.MaxRetries
		}
		endpoints = append(endpoints, endpoint{
			name:       name,
			url:        parsed.String(),
			events:     events,
			secret:     item.Secret,
			timeout:    item.Timeout,
			maxRetries: maxRetries,
		})
	}
	return &dispatcher{
		client:         &http.Client{},
		endpoints:      endpoints,
		initialBackoff: cfg.InitialBackoff,
		maxBackoff:     cfg.MaxBackoff,
	}, nil
}

func (d *dispatcher) Deliver(ctx context.Context, event LifecycleEvent) error {
	payload, ok := buildPayload(event)
	if !ok {
		return nil
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal webhook event %s: %w", event.EventID, err)
	}

	var targets []endpoint
	for _, endpoint := range d.endpoints {
		if _, subscribed := endpoint.events[payload.Event]; subscribed {
			targets = append(targets, endpoint)
		}
	}
	if len(targets) == 0 {
		return nil
	}

	errs := make(chan error, len(targets))
	var wg sync.WaitGroup
	for _, target := range targets {
		target := target
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := d.deliverWithRetry(ctx, target, payload, body); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)

	var joined error
	for err := range errs {
		joined = errors.Join(joined, err)
	}
	return joined
}

func (d *dispatcher) deliverWithRetry(
	ctx context.Context,
	target endpoint,
	payload Payload,
	body []byte,
) error {
	attempts := target.maxRetries + 1
	for attempt := 1; attempt <= attempts; attempt++ {
		err := d.deliverOnce(ctx, target, body)
		if err == nil {
			slog.Info("webhook delivered",
				"endpoint", target.name,
				"event", payload.Event,
				"event_id", payload.EventID,
				"attempt", attempt)
			return nil
		}
		if attempt == attempts {
			return fmt.Errorf("endpoint %s failed after %d attempts: %w", target.name, attempts, err)
		}
		delay := retryDelay(attempt, d.initialBackoff, d.maxBackoff)
		slog.Warn("webhook delivery retry",
			"endpoint", target.name,
			"event", payload.Event,
			"event_id", payload.EventID,
			"attempt", attempt,
			"retry_in", delay,
			"error", err)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}

func (d *dispatcher) deliverOnce(ctx context.Context, target endpoint, body []byte) error {
	requestCtx, cancel := context.WithTimeout(ctx, target.timeout)
	defer cancel()

	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, target.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	if target.secret != "" {
		timestamp := strconv.FormatInt(time.Now().Unix(), 10)
		request.Header.Set(timestampHeader, timestamp)
		request.Header.Set(signatureHeader, sign(target.secret, timestamp, body))
	}

	response, err := d.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		responseBody, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		return fmt.Errorf("unexpected status %d: %s", response.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	_, _ = io.Copy(io.Discard, response.Body)
	return nil
}

func sign(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(timestamp))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func retryDelay(attempt int, initial, maximum time.Duration) time.Duration {
	delay := initial
	for i := 1; i < attempt; i++ {
		if delay >= maximum/2 {
			return maximum
		}
		delay *= 2
	}
	if delay > maximum {
		return maximum
	}
	return delay
}
