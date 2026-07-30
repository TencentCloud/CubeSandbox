// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"golang.org/x/net/idna"
)

const secretEnvPrefix = "CUBE_WEBHOOK_SECRET_"

// DeliveryConfig controls in-memory buffering and external delivery.
type DeliveryConfig struct {
	EventQueueCapacity       int `toml:"event_queue_capacity"`
	MaxOutstandingDeliveries int `toml:"max_outstanding_deliveries"`
	MaxConcurrentRequests    int `toml:"max_concurrent_requests"`
	DefaultBatchSize         int `toml:"default_batch_size"`
	FlushIntervalSecs        int `toml:"flush_interval_secs"`
	RequestTimeoutSecs       int `toml:"request_timeout_secs"`
	MaxAttempts              int `toml:"max_attempts"`
	InitialBackoffMS         int `toml:"initial_backoff_ms"`
	MaxBackoffSecs           int `toml:"max_backoff_secs"`
}

// Endpoint is a validated webhook destination.
type Endpoint struct {
	ID        int
	Name      string
	URL       *url.URL
	BatchSize int
	Secret    string
}

// Config contains resolved delivery settings, endpoints, and immutable routes.
type Config struct {
	Delivery  DeliveryConfig
	Endpoints []*Endpoint
	Routes    map[string][]*Endpoint
}

type fileConfig struct {
	Delivery  DeliveryConfig   `toml:"delivery"`
	Endpoints []endpointConfig `toml:"endpoints"`
}

type endpointConfig struct {
	Name      string   `toml:"name"`
	URL       string   `toml:"url"`
	Events    []string `toml:"events"`
	BatchSize *int     `toml:"batch_size"`
	SecretEnv string   `toml:"secret_env"`
}

// LoadConfig parses and resolves the CubeOps-owned webhook configuration.
// An empty path disables external webhook delivery.
func LoadConfig(path string) (*Config, error) {
	resolved := &Config{
		Delivery: defaultDeliveryConfig(),
		Routes:   make(map[string][]*Endpoint),
	}
	if strings.TrimSpace(path) == "" {
		return resolved, nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read webhook config file %s: %w", path, err)
	}
	parsed := fileConfig{Delivery: defaultDeliveryConfig()}
	if err := toml.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("parse webhook config TOML: %w", err)
	}
	if err := validateDelivery(parsed.Delivery); err != nil {
		return nil, err
	}
	if len(parsed.Endpoints) == 0 {
		return nil, fmt.Errorf("webhook config must contain at least one endpoint")
	}

	resolved.Delivery = parsed.Delivery
	seenNames := make(map[string]struct{}, len(parsed.Endpoints))
	seenSubscriptions := make(map[string]struct{})
	for id, rawEndpoint := range parsed.Endpoints {
		endpoint, events, err := resolveEndpoint(id, rawEndpoint, parsed.Delivery.DefaultBatchSize)
		if err != nil {
			return nil, err
		}
		if _, exists := seenNames[endpoint.Name]; exists {
			return nil, fmt.Errorf("duplicate webhook endpoint name %q", endpoint.Name)
		}
		seenNames[endpoint.Name] = struct{}{}

		normalizedURL, err := normalizedURLKey(endpoint.URL)
		if err != nil {
			return nil, err
		}
		for _, event := range events {
			key := normalizedURL + "\x00" + event
			if _, exists := seenSubscriptions[key]; exists {
				return nil, fmt.Errorf("duplicate webhook endpoint subscription for url %s and event %s", endpoint.URL, event)
			}
			seenSubscriptions[key] = struct{}{}
			resolved.Routes[event] = append(resolved.Routes[event], endpoint)
		}
		resolved.Endpoints = append(resolved.Endpoints, endpoint)
	}
	return resolved, nil
}

func normalizedURLKey(endpointURL *url.URL) (string, error) {
	normalized := *endpointURL
	normalized.Scheme = strings.ToLower(normalized.Scheme)
	normalized.Fragment = ""

	host, err := idna.Lookup.ToASCII(normalized.Hostname())
	if err != nil {
		return "", fmt.Errorf("normalize webhook endpoint host: %w", err)
	}
	host = strings.ToLower(host)
	port := normalized.Port()
	if (normalized.Scheme == "http" && port == "80") || (normalized.Scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		normalized.Host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		normalized.Host = "[" + host + "]"
	} else {
		normalized.Host = host
	}
	return normalized.String(), nil
}

func defaultDeliveryConfig() DeliveryConfig {
	return DeliveryConfig{
		EventQueueCapacity:       10_000,
		MaxOutstandingDeliveries: 1_000,
		MaxConcurrentRequests:    100,
		DefaultBatchSize:         1,
		FlushIntervalSecs:        5,
		RequestTimeoutSecs:       5,
		MaxAttempts:              3,
		InitialBackoffMS:         500,
		MaxBackoffSecs:           10,
	}
}

func validateDelivery(delivery DeliveryConfig) error {
	values := []struct {
		name  string
		value int
	}{
		{"event_queue_capacity", delivery.EventQueueCapacity},
		{"max_outstanding_deliveries", delivery.MaxOutstandingDeliveries},
		{"max_concurrent_requests", delivery.MaxConcurrentRequests},
		{"default_batch_size", delivery.DefaultBatchSize},
		{"flush_interval_secs", delivery.FlushIntervalSecs},
		{"request_timeout_secs", delivery.RequestTimeoutSecs},
		{"max_attempts", delivery.MaxAttempts},
	}
	for _, value := range values {
		if value.value <= 0 {
			return fmt.Errorf("webhook delivery %s must be greater than 0", value.name)
		}
	}
	if delivery.InitialBackoffMS < 0 || delivery.MaxBackoffSecs < 0 {
		return fmt.Errorf("webhook delivery backoff values must not be negative")
	}
	return nil
}

func resolveEndpoint(id int, raw endpointConfig, defaultBatchSize int) (*Endpoint, []string, error) {
	name := strings.TrimSpace(raw.Name)
	if name == "" {
		return nil, nil, fmt.Errorf("webhook endpoint name must not be empty")
	}
	parsedURL, err := url.Parse(strings.TrimSpace(raw.URL))
	if err != nil {
		return nil, nil, fmt.Errorf("parse webhook endpoint %s URL: %w", name, err)
	}
	if (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") || parsedURL.Host == "" {
		return nil, nil, fmt.Errorf("webhook endpoint %s URL must be an absolute http or https URL", name)
	}

	batchSize := defaultBatchSize
	if raw.BatchSize != nil {
		batchSize = *raw.BatchSize
	}
	if batchSize <= 0 {
		return nil, nil, fmt.Errorf("webhook endpoint %s batch_size must be greater than 0", name)
	}

	eventSet := make(map[string]struct{}, len(raw.Events))
	events := make([]string, 0, len(raw.Events))
	for _, rawEvent := range raw.Events {
		event := strings.TrimSpace(rawEvent)
		if event == "" {
			continue
		}
		if _, exists := eventSet[event]; exists {
			continue
		}
		eventSet[event] = struct{}{}
		events = append(events, event)
	}
	if len(events) == 0 {
		return nil, nil, fmt.Errorf("webhook endpoint %s must subscribe to at least one event", name)
	}

	var secret string
	if raw.SecretEnv != "" {
		secretEnv := strings.TrimSpace(raw.SecretEnv)
		if !strings.HasPrefix(secretEnv, secretEnvPrefix) {
			return nil, nil, fmt.Errorf("webhook endpoint %s secret_env must use the %s prefix", name, secretEnvPrefix)
		}
		secret, err = lookupSecret(secretEnv, name)
		if err != nil {
			return nil, nil, err
		}
	}

	return &Endpoint{ID: id, Name: name, URL: parsedURL, BatchSize: batchSize, Secret: secret}, events, nil
}

func lookupSecret(envName, endpointName string) (string, error) {
	secret, exists := os.LookupEnv(envName)
	if !exists || secret == "" {
		return "", fmt.Errorf("read webhook secret env %s for endpoint %s: value is missing or empty", envName, endpointName)
	}
	return secret, nil
}
