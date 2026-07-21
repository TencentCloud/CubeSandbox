// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
	"github.com/tencentcloud/CubeSandbox/CubeDB/dao"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/crypto"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/handler"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/store"
)

func init() { gin.SetMode(gin.TestMode) }

// testEnv wires up a real *store.Store (backed by a throwaway MySQL
// container) + a fake CubeMasterClient + a gin engine with all agenthub
// routes mounted. This lets us test the full HTTP → gin → handler → DB
// chain with real SQL, real migrations, and real crypto.
type testEnv struct {
	router   *gin.Engine
	store    *store.Store
	fakeCM   *fakeCMHandler
	teardown func()
}

// newTestEnv spins up MySQL, runs migrations, creates the store, and mounts
// the agenthub handler on a gin engine. The caller must defer env.teardown().
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()

	pool, err := dockertest.NewPool("")
	if err != nil {
		t.Skipf("dockertest not available (%v)", err)
	}
	if err := pool.Client.Ping(); err != nil {
		t.Skipf("docker daemon not reachable (%v)", err)
	}

	resource, err := pool.RunWithOptions(&dockertest.RunOptions{
		Repository: "mysql",
		Tag:        "8.0",
		Env: []string{
			"MYSQL_ROOT_PASSWORD=root",
			"MYSQL_DATABASE=cubeops_test",
		},
	}, func(hostConfig *docker.HostConfig) {
		hostConfig.AutoRemove = true
		hostConfig.RestartPolicy = docker.RestartPolicy{Name: "no"}
	})
	if err != nil {
		t.Skipf("could not start mysql container (%v)", err)
	}

	port := resource.GetPort("3306/tcp")
	cfg := dao.Config{
		Driver:       "mysql",
		User:         "root",
		Pwd:          "root",
		Addr:         fmt.Sprintf("127.0.0.1:%s", port),
		DBName:       "cubeops_test",
		MaxIdleConns: 5,
		MaxOpenConns: 10,
	}

	pool.MaxWait = 90 * time.Second
	if err := pool.Retry(func() error {
		db, err := dockertestOpenMySQL(port)
		if err != nil {
			return err
		}
		defer db.Close()
		return db.Ping()
	}); err != nil {
		_ = pool.Purge(resource)
		t.Fatalf("mysql container never became reachable: %v", err)
	}

	// Reset the global master key so each container gets its own.
	crypto.ResetMasterKeyForTest()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	s, err := store.New(ctx, cfg)
	if err != nil {
		_ = pool.Purge(resource)
		t.Fatalf("store.New: %v", err)
	}

	cm := &fakeCMHandler{}
	h := handler.NewAgentHubHandler(s, cm)

	r := gin.New()
	g := r.Group("/api/v1")
	h.Register(g)

	return &testEnv{
		router: r,
		store:  s,
		fakeCM: cm,
		teardown: func() {
			_ = s.Close()
			_ = pool.Purge(resource)
		},
	}
}

// doRequest issues an HTTP request against the test router and returns the
// response recorder.
func doRequest(t *testing.T, env *testEnv, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	return w
}
