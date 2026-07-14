// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	listenAddress = "127.0.0.1:18080"
	policyPath    = "/run/cube-vz-egress-policy.json"
	caCertPath    = "/etc/cube-vz/egress-ca.crt"
	caKeyPath     = "/etc/cube-vz/egress-ca.key"
	proxyUID      = 8049
)

type policy struct {
	Rules []rule `json:"rules"`
}

type rule struct {
	Name   string     `json:"name"`
	Match  ruleMatch  `json:"match"`
	Action ruleAction `json:"action"`
}

type ruleMatch struct {
	SNI    *string  `json:"sni"`
	Host   *string  `json:"host"`
	Method []string `json:"method"`
	Path   *string  `json:"path"`
	Scheme *string  `json:"scheme"`
}

type ruleAction struct {
	Allow  bool        `json:"allow"`
	Audit  *string     `json:"audit"`
	Inject []injection `json:"inject"`
}

type injection struct {
	Header string  `json:"header"`
	Secret string  `json:"secret"`
	Format *string `json:"format"`
}

type requestContext struct {
	SNI, Host, Method, Path, Scheme string
}

type proxy struct {
	caCert    *x509.Certificate
	caKey     *ecdsa.PrivateKey
	transport *http.Transport
	cacheMu   sync.Mutex
	certCache map[string]*tls.Certificate
	logger    *log.Logger
}

func main() {
	caCert, caKey, err := loadCA()
	if err != nil {
		log.Fatal(err)
	}
	audit, err := os.OpenFile("/var/log/cube-vz-egress.jsonl", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		log.Fatal(err)
	}
	listener, err := net.Listen("tcp", listenAddress)
	if err != nil {
		log.Fatal(err)
	}
	p := &proxy{
		caCert: caCert,
		caKey:  caKey,
		transport: &http.Transport{
			Proxy:               nil,
			ForceAttemptHTTP2:   true,
			MaxIdleConns:        64,
			IdleConnTimeout:     60 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		},
		certCache: make(map[string]*tls.Certificate),
		logger:    log.New(audit, "", 0),
	}
	if err := syscall.Setgid(proxyUID); err != nil {
		log.Fatal(err)
	}
	if err := syscall.Setuid(proxyUID); err != nil {
		log.Fatal(err)
	}
	server := &http.Server{
		Handler:           p,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       65 * time.Second,
	}
	log.Fatal(server.Serve(listener))
}

func loadCA() (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certPEM, err := os.ReadFile(caCertPath)
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err := os.ReadFile(caKeyPath)
	if err != nil {
		return nil, nil, err
	}
	certBlock, _ := pem.Decode(certPEM)
	keyBlock, _ := pem.Decode(keyPEM)
	if certBlock == nil || keyBlock == nil {
		return nil, nil, errors.New("invalid egress CA PEM")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		parsed, parseErr := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
		if parseErr != nil {
			return nil, nil, err
		}
		var ok bool
		key, ok = parsed.(*ecdsa.PrivateKey)
		if !ok {
			return nil, nil, errors.New("egress CA key is not ECDSA")
		}
	}
	return cert, key, nil
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	host := hostname(r.Host)
	p.forward(w, r, requestContext{
		Host: host, Method: r.Method, Path: r.URL.Path, Scheme: "http",
	})
}

func (p *proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "CONNECT unsupported", http.StatusInternalServerError)
		return
	}
	client, _, err := hijacker.Hijack()
	if err != nil {
		return
	}
	host := hostname(r.Host)
	certificate, err := p.certificateFor(host)
	if err != nil {
		client.Close()
		return
	}
	if _, err = io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		client.Close()
		return
	}
	tlsClient := tls.Server(client, &tls.Config{
		Certificates: []tls.Certificate{*certificate},
		MinVersion:   tls.VersionTLS12,
	})
	if err = tlsClient.Handshake(); err != nil {
		tlsClient.Close()
		return
	}
	listener := &singleConnectionListener{connection: tlsClient}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, inner *http.Request) {
		p.forward(w, inner, requestContext{
			SNI: host, Host: hostname(inner.Host), Method: inner.Method,
			Path: inner.URL.Path, Scheme: "https",
		})
	})}
	_ = server.Serve(listener)
}

func (p *proxy) forward(w http.ResponseWriter, r *http.Request, ctx requestContext) {
	matched, action := matchPolicy(ctx)
	if !matched || !action.Allow {
		p.audit(ctx, false, "no_rule_match")
		http.Error(w, "egress denied", http.StatusForbidden)
		return
	}
	for _, inject := range action.Inject {
		r.Header.Del(inject.Header)
	}
	if ctx.Scheme != "https" || strings.EqualFold(ctx.SNI, ctx.Host) {
		for _, inject := range action.Inject {
			format := "${SECRET}"
			if inject.Format != nil && *inject.Format != "" {
				format = *inject.Format
			}
			r.Header.Set(inject.Header, strings.Replace(format, "${SECRET}", inject.Secret, 1))
		}
	}
	out := r.Clone(context.Background())
	out.RequestURI = ""
	out.URL.Scheme = ctx.Scheme
	out.URL.Host = r.Host
	if out.URL.Host == "" {
		out.URL.Host = ctx.Host
	}
	out.Host = r.Host
	out.Header.Del("Proxy-Connection")
	out.Header.Del("Proxy-Authorization")
	response, err := p.transport.RoundTrip(out)
	if err != nil {
		p.audit(ctx, false, "upstream_error")
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	copyHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
	p.audit(ctx, true, "rule_allow")
}

func matchPolicy(ctx requestContext) (bool, ruleAction) {
	data, err := os.ReadFile(policyPath)
	if err != nil {
		return false, ruleAction{}
	}
	var current policy
	if json.Unmarshal(data, &current) != nil {
		return false, ruleAction{}
	}
	for _, candidate := range current.Rules {
		if matches(candidate.Match, ctx) {
			return true, candidate.Action
		}
	}
	return false, ruleAction{}
}

func matches(m ruleMatch, ctx requestContext) bool {
	if m.SNI != nil && !domainMatches(*m.SNI, ctx.SNI) {
		return false
	}
	if m.Host != nil && !domainMatches(*m.Host, ctx.Host) {
		return false
	}
	if len(m.Method) > 0 {
		found := false
		for _, method := range m.Method {
			found = found || strings.EqualFold(method, ctx.Method)
		}
		if !found {
			return false
		}
	}
	if m.Path != nil {
		if strings.HasSuffix(*m.Path, "*") {
			if !strings.HasPrefix(ctx.Path, strings.TrimSuffix(*m.Path, "*")) {
				return false
			}
		} else if *m.Path != ctx.Path {
			return false
		}
	}
	return m.Scheme == nil || strings.EqualFold(*m.Scheme, ctx.Scheme)
}

func domainMatches(pattern, value string) bool {
	pattern, value = strings.ToLower(pattern), strings.ToLower(value)
	if strings.HasPrefix(pattern, "*.") {
		return value != strings.TrimPrefix(pattern, "*.") && strings.HasSuffix(value, pattern[1:])
	}
	return pattern == value
}

func hostname(hostport string) string {
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		return host
	}
	return strings.Trim(hostport, "[]")
}

func (p *proxy) certificateFor(host string) (*tls.Certificate, error) {
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	if cached := p.certCache[host]; cached != nil {
		return cached, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(7 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, p.caCert, &key.PublicKey, p.caKey)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	certificate, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
	)
	if err != nil {
		return nil, err
	}
	p.certCache[host] = &certificate
	return &certificate, nil
}

func (p *proxy) audit(ctx requestContext, allowed bool, reason string) {
	entry, _ := json.Marshal(map[string]any{
		"ts": time.Now().UTC().Format(time.RFC3339Nano), "allow": allowed,
		"reason": reason, "scheme": ctx.Scheme, "sni": ctx.SNI,
		"host": ctx.Host, "method": ctx.Method, "path": ctx.Path,
	})
	p.logger.Print(string(entry))
}

func copyHeaders(destination, source http.Header) {
	for key, values := range source {
		for _, value := range values {
			destination.Add(key, value)
		}
	}
}

type singleConnectionListener struct {
	connection net.Conn
	once       sync.Once
}

func (l *singleConnectionListener) Accept() (net.Conn, error) {
	var connection net.Conn
	l.once.Do(func() { connection = l.connection })
	if connection == nil {
		return nil, io.EOF
	}
	return connection, nil
}

func (l *singleConnectionListener) Close() error   { return nil }
func (l *singleConnectionListener) Addr() net.Addr { return l.connection.LocalAddr() }
