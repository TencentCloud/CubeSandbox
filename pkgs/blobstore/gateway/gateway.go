// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package gateway serves HMAC-signed GET URLs minted by the fs backend.
package gateway

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/tencentcloud/CubeSandbox/pkgs/blobstore"
	"github.com/tencentcloud/CubeSandbox/pkgs/blobstore/signer"
)

// Handler verifies a signed GET and streams the object. Range requests are
// honoured when the backend Body is an io.ReadSeeker (fs and memory are).
type Handler struct {
	Store  blobstore.Store
	Signer *signer.Signer
	Now    func() time.Time
}

func (h *Handler) now() time.Time {
	if h != nil && h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h == nil || h.Store == nil || h.Signer == nil {
		http.Error(w, "object store unavailable", http.StatusServiceUnavailable)
		return
	}
	key, exp, sig, err := signer.ParseGET(r.URL)
	if err != nil {
		http.Error(w, "invalid signature params", http.StatusBadRequest)
		return
	}
	if err := blobstore.ValidateKey(key); err != nil {
		http.Error(w, "invalid object key", http.StatusBadRequest)
		return
	}
	if err := h.Signer.VerifyGET(key, sig, exp, h.now()); err != nil {
		slog.Warn("blobstore gateway signature rejected",
			"key", key, "fp", h.Signer.Fingerprint(), "error", err)
		if errors.Is(err, signer.ErrExpired) {
			http.Error(w, "url expired", http.StatusForbidden)
			return
		}
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	obj, err := h.Store.Get(r.Context(), key, blobstore.GetOptions{})
	if err != nil {
		if blobstore.IsNotExist(err) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "object store unavailable", http.StatusBadGateway)
		return
	}
	defer obj.Body.Close()

	name := blobstore.BaseName(key)
	mod := obj.LastModified
	if rs, ok := obj.Body.(io.ReadSeeker); ok {
		http.ServeContent(w, r, name, mod, rs)
		return
	}
	if obj.ContentType != "" {
		w.Header().Set("Content-Type", obj.ContentType)
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = io.Copy(w, obj.Body)
}
