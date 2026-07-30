// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig_EmptyPathReturnsEmptyRoutes(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Endpoints) != 0 || len(cfg.Routes) != 0 {
		t.Fatalf("empty config routes = %#v, endpoints = %#v", cfg.Routes, cfg.Endpoints)
	}
}

func TestLoadConfig_MissingConfiguredFileFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.toml")
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("LoadConfig missing configured file = nil error")
	}
}

func TestLoadConfig_ParsesExistingSchemaAndDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "webhooks.toml")
	content := []byte(`[delivery]
event_queue_capacity = 8
max_outstanding_deliveries = 4
max_concurrent_requests = 2
default_batch_size = 2
flush_interval_secs = 1
request_timeout_secs = 2
max_attempts = 3
initial_backoff_ms = 1
max_backoff_secs = 2

[[endpoints]]
name = "audit"
url = "http://127.0.0.1:8088/webhook"
events = ["sandbox.created"]
batch_size = 1
`)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Delivery.EventQueueCapacity != 8 || cfg.Delivery.DefaultBatchSize != 2 {
		t.Fatalf("delivery config = %#v", cfg.Delivery)
	}
	if len(cfg.Routes["sandbox.created"]) != 1 {
		t.Fatalf("routes = %#v", cfg.Routes)
	}
}

func TestLoadConfig_FillsMissingDeliveryDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "webhooks.toml")
	content := []byte(`[[endpoints]]
name = "audit"
url = "http://127.0.0.1:8088/webhook"
events = ["sandbox.created"]
`)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Delivery.EventQueueCapacity != 10_000 || cfg.Delivery.DefaultBatchSize != 1 {
		t.Fatalf("delivery defaults = %#v", cfg.Delivery)
	}
}

func TestLoadConfig_RejectsDuplicateURLEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "webhooks.toml")
	content := []byte(`[[endpoints]]
name = "one"
url = "http://127.0.0.1:8088/webhook"
events = ["sandbox.created"]

[[endpoints]]
name = "two"
url = "http://127.0.0.1:8088/webhook"
events = ["sandbox.created"]
`)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("LoadConfig duplicate URL/event = nil error")
	}
}

func TestLoadConfig_RejectsDuplicateEventForNormalizedURL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "webhooks.toml")
	content := []byte(`[[endpoints]]
name = "one"
url = "HTTP://EXAMPLE.COM:80/webhook#ignored"
events = ["sandbox.created"]

[[endpoints]]
name = "two"
url = "http://example.com/webhook"
events = ["sandbox.created"]
`)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("LoadConfig duplicate normalized URL/event = nil error")
	}
}

func TestLoadConfig_RejectsDuplicateEventForIDNAEquivalentURL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "webhooks.toml")
	content := []byte(`[[endpoints]]
name = "unicode"
url = "http://bücher.example/webhook"
events = ["sandbox.created"]

[[endpoints]]
name = "punycode"
url = "http://xn--bcher-kva.example/webhook"
events = ["sandbox.created"]
`)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("LoadConfig duplicate IDNA-equivalent URL/event = nil error")
	}
}

func TestLoadConfig_ResolvesSecretEnvironmentVariable(t *testing.T) {
	t.Setenv("CUBE_WEBHOOK_SECRET_0", "secret-value")
	path := filepath.Join(t.TempDir(), "webhooks.toml")
	content := []byte(`[[endpoints]]
name = "audit"
url = "http://127.0.0.1:8088/webhook"
events = ["sandbox.created"]
secret_env = "CUBE_WEBHOOK_SECRET_0"
`)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := cfg.Endpoints[0].Secret; got != "secret-value" {
		t.Fatalf("secret = %q, want secret-value", got)
	}
}

func TestLoadConfig_RejectsSecretEnvironmentVariableWithoutPrefix(t *testing.T) {
	t.Setenv("WEBHOOK_SECRET", "secret-value")
	path := filepath.Join(t.TempDir(), "webhooks.toml")
	content := []byte(`[[endpoints]]
name = "audit"
url = "http://127.0.0.1:8088/webhook"
events = ["sandbox.created"]
secret_env = "WEBHOOK_SECRET"
`)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("LoadConfig secret env without CUBE_WEBHOOK_SECRET_ prefix = nil error")
	}
}

func TestRouter_FansOutSubscribedEvent(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Routes["sandbox.created"] = []*Endpoint{{Name: "audit"}}

	event := Event{"event": []byte(`"sandbox.created"`)}
	if got := cfg.Routes[event.Name()]; len(got) != 1 || got[0].Name != "audit" {
		t.Fatalf("route = %#v", got)
	}
}
