// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
package config

import (
	"bytes"
	"errors"
	"net/netip"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// WebhookConf is a startup snapshot. Invalid YAML inside this block disables
// only webhooks; diagnostic text never contains user supplied values.
type WebhookConf struct {
	Enabled         bool                  `yaml:"enabled"`
	IngressCapacity int                   `yaml:"ingress_capacity"`
	Workers         int                   `yaml:"workers"`
	RequestTimeout  time.Duration         `yaml:"request_timeout"`
	MaxAttempts     int                   `yaml:"max_attempts"`
	Lease           time.Duration         `yaml:"lease"`
	AllowHTTP       bool                  `yaml:"allow_http"`
	AllowedCIDRs    []string              `yaml:"allowed_cidrs,omitempty"`
	Store           WebhookStoreConf      `yaml:"store"`
	Subscriptions   []WebhookSubscription `yaml:"subscriptions,omitempty"`
	raw             *yaml.Node
	Invalid         bool `yaml:"-"`
}
type WebhookStoreConf struct {
	Backend           string        `yaml:"backend"`
	Redis             *RedisConf    `yaml:"redis,omitempty"`
	OperationTimeout  time.Duration `yaml:"operation_timeout"`
	MaxRecords        int           `yaml:"max_records"`
	MaxActive         int           `yaml:"max_active"`
	MaxIdle           int           `yaml:"max_idle"`
	TerminalRetention time.Duration `yaml:"terminal_retention"`
}
type WebhookSubscription struct {
	ID      string   `yaml:"id"`
	Enabled bool     `yaml:"enabled"`
	URL     string   `yaml:"url"`
	Events  []string `yaml:"events"`
}

func DefaultWebhookConf() WebhookConf {
	return WebhookConf{IngressCapacity: 1024, Workers: 4, RequestTimeout: 5 * time.Second, MaxAttempts: 3, Lease: 15 * time.Second,
		Store: WebhookStoreConf{Backend: "redis", OperationTimeout: 2 * time.Second, MaxRecords: 10000, MaxActive: 8, MaxIdle: 4, TerminalRetention: 24 * time.Hour}}
}
func (c *WebhookConf) UnmarshalYAML(n *yaml.Node) error {
	type wire WebhookConf
	v := wire(DefaultWebhookConf())
	raw, err := yaml.Marshal(n)
	if err == nil {
		d := yaml.NewDecoder(bytes.NewReader(raw))
		d.KnownFields(true)
		err = d.Decode(&v)
	}
	*c = WebhookConf(v)
	if err != nil || !webhookNodeTypes(n, reflect.TypeOf(v), 0) || c.Validate() != nil {
		*c = DefaultWebhookConf()
		c.Invalid = true
	}
	c.raw = n
	return nil
}
func (c WebhookConf) MarshalYAML() (interface{}, error) {
	if c.raw != nil {
		return c.raw, nil
	}
	type wire WebhookConf
	return wire(c), nil
}
func (c WebhookConf) String() string { return "[webhooks redacted; restart required]" }
func (c WebhookConf) MarshalJSON() ([]byte, error) {
	return []byte(`"[webhooks redacted; restart required]"`), nil
}

var ErrWebhookConfig = errors.New("webhooks disabled: invalid configuration (values redacted)")

func (c WebhookConf) Validate() error {
	s := c.Store
	if c.Invalid || c.IngressCapacity < 1 || c.IngressCapacity > 4096 || c.Workers < 1 || c.Workers > 32 || c.MaxAttempts < 1 || c.MaxAttempts > 1000 ||
		c.RequestTimeout < time.Millisecond || c.RequestTimeout > time.Minute || s.OperationTimeout < time.Millisecond || s.OperationTimeout > 10*time.Second ||
		c.Lease <= 2*s.OperationTimeout+c.RequestTimeout+2*time.Second || c.Lease > 5*time.Minute || s.Backend != "redis" ||
		s.MaxRecords < 1 || s.MaxRecords > 1000000 || s.MaxActive < 1 || s.MaxActive > 64 || s.MaxIdle < 0 || s.MaxIdle > s.MaxActive ||
		s.TerminalRetention < 32*time.Second || s.TerminalRetention > 30*24*time.Hour || s.TerminalRetention%time.Millisecond != 0 || len(c.Subscriptions) > 64 || len(c.AllowedCIDRs) > 64 {
		return ErrWebhookConfig
	}
	for _, cidr := range c.AllowedCIDRs {
		if _, err := netip.ParsePrefix(cidr); err != nil {
			return ErrWebhookConfig
		}
	}
	seen := map[string]bool{}
	for _, sub := range c.Subscriptions {
		if strings.TrimSpace(sub.ID) == "" || len(sub.ID) > 1024 || !utf8.ValidString(sub.ID) || strings.IndexFunc(sub.ID, unicode.IsControl) >= 0 || seen[sub.ID] || len(sub.Events) == 0 || len(sub.Events) > 6 {
			return ErrWebhookConfig
		}
		seen[sub.ID] = true
		if _, err := ValidateWebhookURL(sub.URL, c.AllowHTTP); err != nil {
			return ErrWebhookConfig
		}
		events := map[string]bool{}
		for _, e := range sub.Events {
			switch e {
			case "sandbox.lifecycle.created", "sandbox.lifecycle.killed", "sandbox.lifecycle.paused", "sandbox.lifecycle.resumed", "sandbox.lifecycle.updated", "sandbox.lifecycle.checkpointed":
			default:
				return ErrWebhookConfig
			}
			if events[e] {
				return ErrWebhookConfig
			}
			events[e] = true
		}
	}
	if s.Redis != nil && (s.Redis.DbNo < 0 || (s.Redis.MasterName == "" && strings.TrimSpace(s.Redis.Nodes) == "") || (s.Redis.MasterName != "" && strings.TrimSpace(s.Redis.SentinelNodes) == "")) {
		return ErrWebhookConfig
	}
	return nil
}
func ValidateWebhookURL(raw string, allowHTTP bool) (*url.URL, error) {
	if len(raw) > 4096 || !utf8.ValidString(raw) || strings.IndexFunc(raw, unicode.IsControl) >= 0 {
		return nil, ErrWebhookConfig
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" || strings.Contains(raw, "#") || strings.Contains(u.Hostname(), "%") || (u.Scheme != "https" && !(allowHTTP && u.Scheme == "http")) {
		return nil, ErrWebhookConfig
	}
	host := u.Hostname()
	numeric := strings.IndexFunc(host, func(r rune) bool { return (r < '0' || r > '9') && r != '.' }) < 0
	if numeric || strings.Contains(host, ":") || strings.HasPrefix(strings.ToLower(host), "0x") {
		if _, err := netip.ParseAddr(host); err != nil {
			return nil, ErrWebhookConfig
		}
	}
	if u.Port() != "" {
		p, e := strconv.Atoi(u.Port())
		if e != nil || p < 1 || p > 65535 {
			return nil, ErrWebhookConfig
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return nil, ErrWebhookConfig
	}
	return u, nil
}

// yaml.v3 otherwise coerces numbers into string fields. This local boundary
// checks types before accepting values, including explicit null and duration.
func webhookNodeTypes(n *yaml.Node, t reflect.Type, depth int) bool {
	if depth > 16 {
		return false
	}
	if n.Kind == yaml.AliasNode {
		return webhookNodeTypes(n.Alias, t, depth+1)
	}
	if t.Kind() == reflect.Pointer {
		return webhookNodeTypes(n, t.Elem(), depth+1)
	}
	if t == reflect.TypeOf(time.Duration(0)) {
		return n.Kind == yaml.ScalarNode && n.Tag == "!!str"
	}
	switch t.Kind() {
	case reflect.Struct:
		if n.Kind != yaml.MappingNode {
			return false
		}
		fields := map[string]reflect.Type{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.PkgPath == "" {
				fields[strings.Split(f.Tag.Get("yaml"), ",")[0]] = f.Type
			}
		}
		for i := 0; i < len(n.Content); i += 2 {
			ft, ok := fields[n.Content[i].Value]
			if !ok || !webhookNodeTypes(n.Content[i+1], ft, depth+1) {
				return false
			}
		}
		return true
	case reflect.Slice:
		if n.Kind != yaml.SequenceNode {
			return false
		}
		for _, v := range n.Content {
			if !webhookNodeTypes(v, t.Elem(), depth+1) {
				return false
			}
		}
		return true
	case reflect.Bool:
		return n.Kind == yaml.ScalarNode && n.Tag == "!!bool"
	case reflect.String:
		return n.Kind == yaml.ScalarNode && n.Tag == "!!str"
	case reflect.Int:
		return n.Kind == yaml.ScalarNode && n.Tag == "!!int"
	}
	return false
}
