// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package artifactticket

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDerivedArtifactKeyAndTemplateCenterDownload(t *testing.T) {
	serviceSecret := []byte(strings.Repeat("control-plane-token-", 2))
	key, err := DeriveSigningKey(serviceSecret)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	token, err := Sign(key, Claims{ArtifactID: "artifact-1", NodeID: "node-1", Generation: 1,
		Purpose: PurposeDownload, ExpiresAt: now.Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(serviceSecret, token, now); err == nil {
		t.Fatal("service key was not domain-separated")
	}
	ticket := Ticket{Source: SourceTemplateCenter, URL: "https://tc.example/internal/artifacts/artifact-1/ext4", Token: token}
	_, headers, err := ticket.DownloadRequest("node-1")
	if err != nil || headers.Get(DownloadNodeIDHeader) != "node-1" || headers.Get(DownloadTokenHeader) != token {
		t.Fatalf("TemplateCenter download headers: %v", err)
	}
}

// TestDownloadRequestRejectsMasterProxySource pins the direct-only contract:
// a ticket asking Cubelet to stream bytes through CubeMaster is refused.
func TestDownloadRequestRejectsMasterProxySource(t *testing.T) {
	ticket := Ticket{Source: "master", URL: "https://master.example/internal/template/artifacts/artifact-1/ext4", Token: "token"}
	if _, _, err := ticket.DownloadRequest("node-1"); err == nil {
		t.Fatal("Master-proxied download source unexpectedly accepted")
	}
}

func TestSignVerifyRoundTrip(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	want := Claims{
		ArtifactID: "artifact-1",
		NodeID:     "node-1",
		ExpiresAt:  now.Add(time.Hour).Unix(),
		Generation: 3,
		Purpose:    PurposeDownload,
	}
	token, err := Sign([]byte(strings.Repeat("k", 32)), want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Verify([]byte(strings.Repeat("k", 32)), token, now)
	if err != nil {
		t.Fatal(err)
	}
	want.Version = tokenVersion
	if got != want {
		t.Fatalf("claims = %+v, want %+v", got, want)
	}
}

func TestVerifyRejectsTamperingAndExpiry(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	secret := []byte(strings.Repeat("s", 32))
	token, err := Sign(secret, Claims{
		ArtifactID: "artifact-1",
		NodeID:     "node-1",
		ExpiresAt:  now.Add(time.Minute).Unix(),
		Generation: 1,
		Purpose:    PurposeDownload,
	})
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")
	replacement := byte('A')
	if parts[0][len(parts[0])-1] == replacement {
		replacement = 'B'
	}
	tampered := parts[0][:len(parts[0])-1] + string(replacement) + "." + parts[1]
	if _, err := Verify(secret, tampered, now); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("tampered token error = %v, want ErrInvalidToken", err)
	}
	if _, err := Verify(secret, token, now.Add(time.Minute)); !errors.Is(err, ErrExpiredToken) {
		t.Fatalf("expired token error = %v, want ErrExpiredToken", err)
	}
	if _, err := Verify([]byte(strings.Repeat("x", 32)), token, now); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("wrong-key error = %v, want ErrInvalidToken", err)
	}
}

func TestSignRejectsWeakSecretAndIncompleteClaims(t *testing.T) {
	valid := Claims{
		ArtifactID: "artifact-1",
		NodeID:     "node-1",
		ExpiresAt:  time.Now().Add(time.Hour).Unix(),
		Generation: 1,
		Purpose:    PurposeDownload,
	}
	if _, err := Sign([]byte("short"), valid); err == nil {
		t.Fatal("weak secret unexpectedly accepted")
	}
	valid.NodeID = ""
	if _, err := Sign([]byte(strings.Repeat("k", 32)), valid); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("incomplete claims error = %v, want ErrInvalidToken", err)
	}
}

func TestDownloadRequestEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ticket  Ticket
		wantErr bool
	}{
		{"legacy TC", Ticket{Source: SourceTemplateCenter, URL: "https://tc.example/file", Token: "token"}, false},
		{"header TC", Ticket{Source: SourceTemplateCenter, URL: "https://tc.example/file", Method: "GET", Headers: map[string]string{DownloadTokenHeader: "token", DownloadNodeIDHeader: "node"}}, false},
		{"S3", Ticket{Source: SourceS3, URL: "https://objects.example/file?signature=demo", Method: "GET"}, false},
		{"invalid method", Ticket{Source: SourceS3, URL: "https://objects.example/file", Method: "POST"}, true},
		{"S3 token leak", Ticket{Source: SourceS3, URL: "https://objects.example/file", Token: "internal"}, true},
		{"S3 header leak", Ticket{Source: SourceS3, URL: "https://objects.example/file", Headers: map[string]string{DownloadTokenHeader: "internal"}}, true},
		{"conflicting token", Ticket{Source: SourceTemplateCenter, URL: "https://tc.example/file", Token: "old", Headers: map[string]string{DownloadTokenHeader: "new"}}, true},
		{"wrong node", Ticket{Source: SourceTemplateCenter, URL: "https://tc.example/file", Token: "token", Headers: map[string]string{DownloadNodeIDHeader: "other"}}, true},
		{"duplicate case", Ticket{Source: SourceTemplateCenter, URL: "https://tc.example/file", Headers: map[string]string{DownloadTokenHeader: "token", "x-cube-artifact-token": "token"}}, true},
		{"control character", Ticket{Source: SourceTemplateCenter, URL: "https://tc.example/file", Token: "token\r\ninjected"}, true},
		{"oversized token", Ticket{Source: SourceTemplateCenter, URL: "https://tc.example/file", Token: strings.Repeat("t", maxTokenBytes+1)}, true},
		{"range injection", Ticket{Source: SourceTemplateCenter, URL: "https://tc.example/file", Token: "token", Headers: map[string]string{"Range": "bytes=99-"}}, true},
		{"auth injection", Ticket{Source: SourceTemplateCenter, URL: "https://tc.example/file", Token: "token", Headers: map[string]string{"Authorization": "secret"}}, true},
		{"missing token", Ticket{Source: SourceTemplateCenter, URL: "https://tc.example/file"}, true},
		{"URL userinfo", Ticket{Source: SourceTemplateCenter, URL: "https://user@tc.example/file", Token: "token"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			method, header, err := tc.ticket.DownloadRequest("node")
			if (err != nil) != tc.wantErr {
				t.Fatalf("DownloadRequest error = %v; want error=%v", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if method != "GET" {
				t.Fatalf("method = %s", method)
			}
			if tc.ticket.Source == SourceS3 && len(header) != 0 {
				t.Fatal("S3 received internal headers")
			}
			if tc.ticket.Source == SourceTemplateCenter && (header.Get(DownloadTokenHeader) != "token" || header.Get(DownloadNodeIDHeader) != "node") {
				t.Fatal("TC credentials are incomplete")
			}
		})
	}
}

func TestURLOrigin(t *testing.T) {
	got, err := URLOrigin("HTTPS://S3.Example.COM:443/bucket/key?X-Amz-Signature=secret")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://s3.example.com:443" {
		t.Fatalf("origin = %q", got)
	}
	for _, raw := range []string{"", "file:///tmp/rootfs", "https://user@example.com/file"} {
		if _, err := URLOrigin(raw); err == nil {
			t.Fatalf("URLOrigin(%q) unexpectedly succeeded", raw)
		}
	}
}
