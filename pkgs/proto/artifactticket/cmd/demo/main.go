// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"time"

	"github.com/tencentcloud/CubeSandbox/pkgs/proto/artifactticket"
)

func main() {
	ttl := flag.Duration("ttl", time.Hour, "download ticket validity (1h or 2h)")
	flag.Parse()
	if err := runDemo(*ttl, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runDemo(ttl time.Duration, out io.Writer) error {
	if ttl != time.Hour && ttl != 2*time.Hour {
		return fmt.Errorf("demo ttl must be 1h or 2h")
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return err
	}
	const id, node = "rfs-demo", "node-demo"
	body := []byte("demo-rootfs-bytes: downloaded directly from the artifact owner\n")
	digest := sha256.Sum256(body)
	sha := hex.EncodeToString(digest[:])
	now := time.Now()
	validToken := func(token, requestedNode string, purpose string, at time.Time) bool {
		claims, err := artifactticket.Verify(secret, token, at)
		return err == nil && claims.ArtifactID == id && claims.NodeID == requestedNode && requestedNode == node && claims.Generation == 1 && claims.Purpose == purpose
	}
	tc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/internal/artifacts/"+id+"/ext4" || !validToken(r.Header.Get(artifactticket.DownloadTokenHeader), r.Header.Get(artifactticket.DownloadNodeIDHeader), artifactticket.PurposeDownload, time.Now()) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write(body)
	}))
	defer tc.Close()
	// This local object server demonstrates a signed-query capability, not AWS
	// SigV4. Production Master uses its existing S3/MinIO/COS presigner.
	objects := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/bucket/"+id+"-"+sha+".ext4" || r.Header.Get(artifactticket.DownloadTokenHeader) != "" || r.Header.Get(artifactticket.DownloadNodeIDHeader) != "" || !validToken(r.URL.Query().Get("demo-signature"), node, artifactticket.PurposeDownload, time.Now()) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		_, _ = w.Write(body)
	}))
	defer objects.Close()

	// In production these stable locators are read from RootfsArtifact in DB.
	locators := map[string]string{
		artifactticket.SourceTemplateCenter: tc.URL + "/internal/artifacts/" + id + "/ext4",
		artifactticket.SourceS3:             objects.URL + "/bucket/" + id + "-" + sha + ".ext4",
	}
	master := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/ticket" || !validToken(r.Header.Get("X-Cube-Artifact-Refresh-Token"), r.Header.Get(artifactticket.DownloadNodeIDHeader), artifactticket.PurposeRefresh, time.Now()) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		source := r.URL.Query().Get("source")
		locator, ok := locators[source]
		if !ok {
			http.Error(w, "unknown source", http.StatusBadRequest)
			return
		}
		issued := time.Now()
		token, err := artifactticket.Sign(secret, artifactticket.Claims{ArtifactID: id, NodeID: node, Generation: 1, Purpose: artifactticket.PurposeDownload, ExpiresAt: issued.Add(ttl).Unix()})
		if err != nil {
			http.Error(w, "sign failed", http.StatusInternalServerError)
			return
		}
		ticket := artifactticket.Ticket{Source: source, URL: locator, Method: http.MethodGet, ArtifactID: id, Generation: 1, SizeBytes: int64(len(body)), SHA256: sha, ServerTime: issued, ExpiresAt: issued.Add(ttl)}
		if source == artifactticket.SourceTemplateCenter {
			ticket.Token = token
			ticket.Headers = map[string]string{artifactticket.DownloadTokenHeader: token, artifactticket.DownloadNodeIDHeader: node}
		} else {
			u, _ := url.Parse(locator)
			q := u.Query()
			q.Set("demo-signature", token)
			u.RawQuery = q.Encode()
			ticket.URL = u.String()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ticket)
	}))
	defer master.Close()
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		return fmt.Errorf("demo does not allow redirects")
	}}
	refresh, err := artifactticket.Sign(secret, artifactticket.Claims{ArtifactID: id, NodeID: node, Generation: 1, Purpose: artifactticket.PurposeRefresh, ExpiresAt: now.Add(24 * time.Hour).Unix()})
	if err != nil {
		return err
	}
	for _, source := range []string{artifactticket.SourceTemplateCenter, artifactticket.SourceS3} {
		req, err := http.NewRequest(http.MethodGet, master.URL+"/ticket?source="+source, nil)
		if err != nil {
			return err
		}
		req.Header.Set("X-Cube-Artifact-Refresh-Token", refresh)
		req.Header.Set(artifactticket.DownloadNodeIDHeader, node)
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("demo ticket request failed")
		}
		var ticket artifactticket.Ticket
		err = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&ticket)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || err != nil {
			return fmt.Errorf("demo ticket status=%d decode=%v", resp.StatusCode, err)
		}
		origin, err := artifactticket.URLOrigin(ticket.URL)
		wantOrigin, _ := artifactticket.URLOrigin(locators[source])
		if err != nil || origin != wantOrigin || ticket.ArtifactID != id || ticket.Generation != 1 || ticket.SHA256 != sha || ticket.SizeBytes != int64(len(body)) || !ticket.ExpiresAt.After(ticket.ServerTime) {
			return fmt.Errorf("demo ticket identity, origin or expiry mismatch")
		}
		method, headers, err := ticket.DownloadRequest(node)
		if err != nil {
			return err
		}
		req, err = http.NewRequest(method, ticket.URL, nil)
		if err != nil {
			return err
		}
		req.Header = headers
		resp, err = client.Do(req)
		if err != nil {
			return fmt.Errorf("demo direct download failed")
		}
		got, readErr := io.ReadAll(io.LimitReader(resp.Body, ticket.SizeBytes+1))
		_ = resp.Body.Close()
		actualDigest := sha256.Sum256(got)
		if readErr != nil || resp.StatusCode != http.StatusOK || int64(len(got)) != ticket.SizeBytes || hex.EncodeToString(actualDigest[:]) != ticket.SHA256 || !bytes.Equal(got, body) {
			return fmt.Errorf("demo direct download integrity failed")
		}
		token := headers.Get(artifactticket.DownloadTokenHeader)
		if source == artifactticket.SourceS3 {
			u, _ := url.Parse(ticket.URL)
			token = u.Query().Get("demo-signature")
		}
		if _, err := artifactticket.Verify(secret, token, ticket.ExpiresAt.Add(time.Second)); err != artifactticket.ErrExpiredToken {
			return fmt.Errorf("demo expiry rejection failed")
		}
		redacted := ticket
		redacted.URL = locators[source]
		redacted.Token = ""
		if source == artifactticket.SourceTemplateCenter {
			redacted.Headers = map[string]string{artifactticket.DownloadTokenHeader: "[redacted]", artifactticket.DownloadNodeIDHeader: node}
		} else {
			redacted.URL += "?demo-signature=[redacted]"
		}
		if err := json.NewEncoder(out).Encode(redacted); err != nil {
			return err
		}
		fmt.Fprintf(out, "PASS source=%s ttl=%s direct_bytes=%d sha256=%s expired_rejected=true\n", source, ttl, len(got), sha)
	}
	fmt.Fprintln(out, "Loopback-only simulation; no DB, cloud bucket, live node or service deployment was used.")
	return nil
}
