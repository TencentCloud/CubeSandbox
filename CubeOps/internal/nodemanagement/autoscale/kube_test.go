// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package autoscale

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/config"
)

func TestKubeClientPlaceholderLifecycle(t *testing.T) {
	var created map[string]any
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer tok", r.Header.Get("Authorization"))
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes":
			assert.Equal(t, "nodepool=cube", r.URL.Query().Get("labelSelector"))
			_, _ = io.WriteString(w, `{"items":[{"metadata":{"name":"n1"}},{"metadata":{"name":"n2","deletionTimestamp":"2026-11-10T00:00:00Z"}}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/cube-system/pods":
			assert.Equal(t, PlaceholderLabelKey+"="+PlaceholderLabelValue, r.URL.Query().Get("labelSelector"))
			_, _ = io.WriteString(w, `{"items":[{"metadata":{"name":"cube-capacity-x","creationTimestamp":"2026-11-10T00:00:00Z"},"spec":{"nodeName":"n1"}},{"metadata":{"name":"cube-capacity-y","creationTimestamp":"2026-11-10T00:01:00Z"},"spec":{}}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/namespaces/cube-system/pods":
			require.NoError(t, json.NewDecoder(r.Body).Decode(&created))
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{}`)
		case r.Method == http.MethodDelete:
			deleted = append(deleted, r.URL.Path)
			w.WriteHeader(http.StatusNotFound) // already gone is fine
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	tok := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tok, []byte("tok\n"), 0o600))
	cfg := config.AutoscaleConfig{
		NodeSelector:     map[string]string{"nodepool": "cube"},
		Tolerations:      []config.AutoscaleToleration{{Key: "cube.tencent.com/compute", Operator: "Equal", Value: "true", Effect: "NoSchedule"}},
		PlaceholderImage: "pause:test",
	}
	k := newKubeClientForTest(srv.URL, "cube-system", tok, cfg, srv.Client())
	ctx := context.Background()

	nodes, err := k.PoolNodes(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"n1"}, nodes, "nodes being deleted are excluded")

	phs, err := k.Placeholders(ctx)
	require.NoError(t, err)
	require.Len(t, phs, 2)
	assert.Equal(t, "n1", phs[0].NodeName)
	assert.Equal(t, "", phs[1].NodeName)

	require.NoError(t, k.CreatePlaceholder(ctx, []string{"n9", "n3"}))
	spec := created["spec"].(map[string]any)
	assert.Equal(t, map[string]any{"nodepool": "cube"}, spec["nodeSelector"])
	aff := spec["affinity"].(map[string]any)
	assert.Contains(t, aff, "podAntiAffinity")
	expr := aff["nodeAffinity"].(map[string]any)["requiredDuringSchedulingIgnoredDuringExecution"].(map[string]any)["nodeSelectorTerms"].([]any)[0].(map[string]any)["matchExpressions"].([]any)[0].(map[string]any)
	assert.Equal(t, "NotIn", expr["operator"])
	assert.Equal(t, []any{"n3", "n9"}, expr["values"])
	assert.Equal(t, "cube-capacity-", created["metadata"].(map[string]any)["generateName"])

	require.NoError(t, k.DeletePlaceholder(ctx, "cube-capacity-x"))
	assert.Equal(t, []string{"/api/v1/namespaces/cube-system/pods/cube-capacity-x"}, deleted)
	assert.Error(t, k.DeletePlaceholder(ctx, "cube-master-0"), "only placeholder pods may be deleted")
}
