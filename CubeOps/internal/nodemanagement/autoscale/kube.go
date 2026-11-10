// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package autoscale

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/config"
)

// Placeholder Pod labels.
const (
	PlaceholderLabelKey   = "cube.cloud.tencentcloud.com/capacity-placeholder"
	PlaceholderLabelValue = "true"
	placeholderPrefix     = "cube-capacity-"
	hostnameTopologyKey   = "kubernetes.io/hostname"
)

// Cluster is the Kubernetes surface the reconciler needs.
type Cluster interface {
	// PoolNodes lists the names of Kubernetes nodes in the compute pool.
	PoolNodes(ctx context.Context) ([]string, error)
	// Placeholders lists placeholder Pods (bound and Pending).
	Placeholders(ctx context.Context) ([]Placeholder, error)
	// CreatePlaceholder adds one placeholder that must not land on avoid.
	CreatePlaceholder(ctx context.Context, avoid []string) error
	// DeletePlaceholder removes one placeholder Pod by name.
	DeletePlaceholder(ctx context.Context, name string) error
}

const (
	saDir           = "/var/run/secrets/kubernetes.io/serviceaccount"
	saTokenFile     = saDir + "/token"
	saCAFile        = saDir + "/ca.crt"
	saNamespaceFile = saDir + "/namespace"
)

// KubeClient talks to the Kubernetes API with the Pod's ServiceAccount. It
// uses plain REST so CubeOps does not depend on client-go.
type KubeClient struct {
	base      string
	namespace string
	cfg       config.AutoscaleConfig
	http      *http.Client
	tokenFile string
}

// NewInClusterClient builds a client from the in-cluster environment.
func NewInClusterClient(cfg config.AutoscaleConfig) (*KubeClient, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("autoscale: not running in a Kubernetes Pod (KUBERNETES_SERVICE_HOST/PORT unset)")
	}
	caPEM, err := os.ReadFile(saCAFile)
	if err != nil {
		return nil, fmt.Errorf("autoscale: read service account CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("autoscale: service account CA has no certificates")
	}
	ns := cfg.Namespace
	if ns == "" {
		b, err := os.ReadFile(saNamespaceFile)
		if err != nil {
			return nil, fmt.Errorf("autoscale: read namespace: %w", err)
		}
		ns = strings.TrimSpace(string(b))
	}
	return &KubeClient{
		base:      "https://" + net.JoinHostPort(host, port),
		namespace: ns,
		cfg:       cfg,
		tokenFile: saTokenFile,
		http: &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
			},
		},
	}, nil
}

// newKubeClientForTest points the client at a test server.
func newKubeClientForTest(base, namespace, tokenFile string, cfg config.AutoscaleConfig, hc *http.Client) *KubeClient {
	return &KubeClient{base: base, namespace: namespace, cfg: cfg, http: hc, tokenFile: tokenFile}
}

func (k *KubeClient) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, k.base+path, rd)
	if err != nil {
		return err
	}
	// Projected tokens rotate; read on every call.
	if tok, err := os.ReadFile(k.tokenFile); err == nil {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := k.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode == http.StatusNotFound && method == http.MethodDelete {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := string(raw)
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return fmt.Errorf("kubernetes %s %s: HTTP %d: %s", method, path, resp.StatusCode, msg)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func selectorString(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, ",")
}

// PoolNodes implements Cluster.
func (k *KubeClient) PoolNodes(ctx context.Context) ([]string, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name              string     `json:"name"`
				DeletionTimestamp *time.Time `json:"deletionTimestamp"`
			} `json:"metadata"`
		} `json:"items"`
	}
	q := url.Values{"labelSelector": {selectorString(k.cfg.NodeSelector)}}
	if err := k.do(ctx, http.MethodGet, "/api/v1/nodes?"+q.Encode(), nil, &list); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(list.Items))
	for _, it := range list.Items {
		if it.Metadata.DeletionTimestamp == nil {
			out = append(out, it.Metadata.Name)
		}
	}
	return out, nil
}

// Placeholders implements Cluster.
func (k *KubeClient) Placeholders(ctx context.Context) ([]Placeholder, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name              string     `json:"name"`
				CreationTimestamp time.Time  `json:"creationTimestamp"`
				DeletionTimestamp *time.Time `json:"deletionTimestamp"`
			} `json:"metadata"`
			Spec struct {
				NodeName string `json:"nodeName"`
			} `json:"spec"`
		} `json:"items"`
	}
	q := url.Values{"labelSelector": {PlaceholderLabelKey + "=" + PlaceholderLabelValue}}
	path := "/api/v1/namespaces/" + url.PathEscape(k.namespace) + "/pods?" + q.Encode()
	if err := k.do(ctx, http.MethodGet, path, nil, &list); err != nil {
		return nil, err
	}
	out := make([]Placeholder, 0, len(list.Items))
	for _, it := range list.Items {
		if it.Metadata.DeletionTimestamp != nil {
			continue
		}
		out = append(out, Placeholder{Name: it.Metadata.Name, NodeName: it.Spec.NodeName, CreatedAt: it.Metadata.CreationTimestamp})
	}
	return out, nil
}

// CreatePlaceholder implements Cluster.
func (k *KubeClient) CreatePlaceholder(ctx context.Context, avoid []string) error {
	path := "/api/v1/namespaces/" + url.PathEscape(k.namespace) + "/pods"
	return k.do(ctx, http.MethodPost, path, PlaceholderPod(k.cfg, avoid), nil)
}

// DeletePlaceholder implements Cluster.
func (k *KubeClient) DeletePlaceholder(ctx context.Context, name string) error {
	if !strings.HasPrefix(name, placeholderPrefix) {
		return fmt.Errorf("refusing to delete non-placeholder pod %q", name)
	}
	path := "/api/v1/namespaces/" + url.PathEscape(k.namespace) + "/pods/" + url.PathEscape(name) + "?gracePeriodSeconds=0"
	return k.do(ctx, http.MethodDelete, path, nil, nil)
}

// PlaceholderPod renders the placeholder Pod manifest. One per node
// (required anti-affinity on hostname), tiny requests, tolerates the pool
// taints, and avoids nodes that are being retired. It is a bare Pod on
// purpose: the Cluster Autoscaler does not remove a node that runs a Pod
// without a controller, which is what pins kept nodes.
func PlaceholderPod(cfg config.AutoscaleConfig, avoid []string) map[string]any {
	labels := map[string]string{
		PlaceholderLabelKey:            PlaceholderLabelValue,
		"app.kubernetes.io/managed-by": "cubeops-autoscaler",
	}
	tolerations := make([]map[string]any, 0, len(cfg.Tolerations))
	for _, t := range cfg.Tolerations {
		m := map[string]any{}
		if t.Key != "" {
			m["key"] = t.Key
		}
		if t.Operator != "" {
			m["operator"] = t.Operator
		}
		if t.Value != "" {
			m["value"] = t.Value
		}
		if t.Effect != "" {
			m["effect"] = t.Effect
		}
		tolerations = append(tolerations, m)
	}
	affinity := map[string]any{
		"podAntiAffinity": map[string]any{
			"requiredDuringSchedulingIgnoredDuringExecution": []any{map[string]any{
				"labelSelector": map[string]any{"matchLabels": map[string]string{PlaceholderLabelKey: PlaceholderLabelValue}},
				"topologyKey":   hostnameTopologyKey,
			}},
		},
	}
	if len(avoid) > 0 {
		sorted := append([]string(nil), avoid...)
		sort.Strings(sorted)
		affinity["nodeAffinity"] = map[string]any{
			"requiredDuringSchedulingIgnoredDuringExecution": map[string]any{
				"nodeSelectorTerms": []any{map[string]any{
					"matchExpressions": []any{map[string]any{
						"key": hostnameTopologyKey, "operator": "NotIn", "values": sorted,
					}},
				}},
			},
		}
	}
	spec := map[string]any{
		"nodeSelector":                  cfg.NodeSelector,
		"tolerations":                   tolerations,
		"affinity":                      affinity,
		"terminationGracePeriodSeconds": 0,
		"automountServiceAccountToken":  false,
		"enableServiceLinks":            false,
		"containers": []any{map[string]any{
			"name":  "placeholder",
			"image": cfg.PlaceholderImage,
			"resources": map[string]any{
				"requests": map[string]string{"cpu": "10m", "memory": "16Mi"},
				"limits":   map[string]string{"cpu": "10m", "memory": "16Mi"},
			},
			"securityContext": map[string]any{
				"allowPrivilegeEscalation": false,
				"readOnlyRootFilesystem":   true,
				"runAsNonRoot":             true,
				"runAsUser":                65535,
				"capabilities":             map[string]any{"drop": []string{"ALL"}},
			},
		}},
	}
	if cfg.PriorityClassName != "" {
		spec["priorityClassName"] = cfg.PriorityClassName
	}
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"generateName": placeholderPrefix,
			"labels":       labels,
			"annotations": map[string]string{
				"cube.cloud.tencentcloud.com/purpose": "keeps this compute node; managed by CubeOps autoscale",
			},
		},
		"spec": spec,
	}
}
