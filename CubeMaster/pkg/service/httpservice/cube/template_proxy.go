// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
)

// proxyToTemplateCenter reverse-proxies /cube/template/* requests to the
// standalone CubeTemplateCenter process. CubeMaster keeps only the internal
// status-callback route; every public template API is served by TC.
//
// The proxy preserves the original path and query string so TC sees the same
// URL the client called. CUBE_TEMPLATE_CENTER_ADDR must be set; when it is
// empty the proxy returns 503 with a clear message instead of panicking.
func proxyToTemplateCenter(c *gin.Context) {
	endpoint := strings.TrimRight(strings.TrimSpace(config.GetConfig().TemplateCenterAddr()), "/")
	if endpoint == "" {
		log.G(c.Request.Context()).Errorf("template proxy: %s is empty", config.EnvTemplateCenterAddr)
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "CubeTemplateCenter address is not configured (CUBE_TEMPLATE_CENTER_ADDR)",
		})
		return
	}

	target, err := url.Parse(endpoint)
	if err != nil {
		log.G(c.Request.Context()).Errorf("template proxy: parse %s=%q fail: %v", config.EnvTemplateCenterAddr, endpoint, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "invalid CubeTemplateCenter address"})
		return
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	// Preserve the original path and query so TC's routes match exactly what
	// the client called (e.g. /cube/template/from-image?job_id=...).
	proxy.Director = func(req *http.Request) {
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.Host = target.Host
		// Keep the original path and query untouched: TC registers the same
		// /cube/template/* routes as CubeMaster used to.
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.G(r.Context()).Errorf("template proxy: %s %s -> %s fail: %v", r.Method, r.URL.String(), endpoint, err)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"template center unreachable"}`))
	}
	proxy.ServeHTTP(c.Writer, c.Request)
}
