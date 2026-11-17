//go:build integration

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cube_egress_ca

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Run with -tags=integration on a host with node, python3 (+ requests), and
// curl. These real clients use their default verification, never insecure flags.
func TestRuntimeCATLSClients(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{
		Subject:      pkix.Name{CommonName: "runtime-ca-test"},
		SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage:    x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("trusted")) }))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	server.StartTLS()
	defer server.Close()
	baseEnv := []string{}
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		switch strings.ToUpper(key) {
		case "NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE", "SSL_CERT_DIR", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE", "NODE_TLS_REJECT_UNAUTHORIZED", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY":
			continue
		}
		baseEnv = append(baseEnv, value)
	}
	baseEnv = append(baseEnv, "NO_PROXY=*")
	for _, client := range []struct {
		name, bin string
		args      []string
	}{
		{"node", "node", []string{"-e", `require('https').get(process.argv[1], r => {r.resume();r.on('end',()=>process.exit(r.statusCode===200?0:1))}).on('error',()=>process.exit(1))`}},
		{"python-ssl", "python3", []string{"-c", `import sys,urllib.request; assert urllib.request.urlopen(sys.argv[1]).read()==b'trusted'`}},
		{"python-requests", "python3", []string{"-c", `import sys,requests; assert requests.get(sys.argv[1]).text=='trusted'`}},
		{"curl", "curl", []string{"--silent", "--show-error", "--fail", "--max-time", "5"}},
	} {
		t.Run(client.name, func(t *testing.T) {
			bin, err := exec.LookPath(client.bin)
			if err != nil {
				t.Skipf("install %s to run this client", client.bin)
			}
			if client.name == "python-requests" {
				if err := exec.Command(bin, "-c", "import requests").Run(); err != nil {
					t.Skip("install Python requests")
				}
			}
			for _, layout := range append(append([]string{}, bundleFiles...), "seed") {
				t.Run(filepath.Base(layout), func(t *testing.T) {
					root := t.TempDir()
					if layout != "seed" {
						mustWrite(t, root, layout, "")
					}
					result, err := Bake(root, ca)
					if err != nil {
						t.Fatal(err)
					}
					bundle := filepath.Join(root, result.BundlePath)
					trustedEnv := append(append([]string{}, baseEnv...), "NODE_EXTRA_CA_CERTS="+bundle, "SSL_CERT_FILE="+bundle, "REQUESTS_CA_BUNDLE="+bundle, "CURL_CA_BUNDLE="+bundle)
					for _, scenario := range []struct {
						name, url string
						env       []string
						success   bool
					}{
						{"without-defaults", server.URL, baseEnv, false},
						{"with-defaults", server.URL, trustedEnv, true},
						{"wrong-hostname", strings.Replace(server.URL, "127.0.0.1", "localhost", 1), trustedEnv, false},
					} {
						t.Run(scenario.name, func(t *testing.T) {
							ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
							defer cancel()
							cmd := exec.CommandContext(ctx, bin, append(append([]string{}, client.args...), scenario.url)...)
							cmd.Env = scenario.env
							output, err := cmd.CombinedOutput()
							if (err == nil) != scenario.success {
								t.Fatalf("success=%v err=%v output=%s", scenario.success, err, output)
							}
							if ctx.Err() != nil {
								t.Fatal(ctx.Err())
							}
						})
					}
				})
			}
		})
	}
}
