// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	defaultS3MultipartChunkSize = 5 << 20 // 5 MiB S3 minimum non-final part size
	defaultS3DialTimeout        = 10 * time.Second
	defaultS3AbortTimeout       = 15 * time.Second
	maxS3ErrorBodyBytes         = 64 << 10
)

var (
	// s3SensitiveQueryRe redacts presigned/auth query parameters if an upstream
	// server echoes a request URL in an error message.
	s3SensitiveQueryRe = regexp.MustCompile(`(?i)([?&](?:X-Amz-Signature|X-Amz-Credential|X-Amz-Security-Token|Signature|AWSAccessKeyId|q-signature|q-ak)=)[^&\s"'<>]+`)
	// s3SensitiveHeaderRe redacts SigV4 Authorization values if echoed back.
	s3SensitiveHeaderRe = regexp.MustCompile(`(?i)(AWS4-HMAC-SHA256\s+Credential=)[^\s,"']+`)
	s3SignatureKVRe     = regexp.MustCompile(`(?i)(Signature=)[0-9a-f]{16,}`)
	s3BucketDNSLabelRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)
	// cgnatPrefix is RFC 6598 shared address space (100.64.0.0/10).
	cgnatPrefix = netip.MustParsePrefix("100.64.0.0/10")
)

type s3ObjectHead struct {
	Size         int64
	LastModified time.Time
	ContentType  string
	Metadata     map[string]string
}

type s3ListedObject struct {
	Key          string
	Size         int64
	LastModified time.Time
}

type s3ListPage struct {
	Objects               []s3ListedObject
	CommonPrefixes        []string
	IsTruncated           bool
	NextContinuationToken string
	KeyCount              int
}

type s3PutOptions struct {
	Metadata    map[string]string
	IfNoneMatch string
	ContentType string
}

type s3CopyOptions struct {
	Metadata    map[string]string
	ContentType string
}

// volumeStorage abstracts the object-storage operations used by VolumeContent
// so storage and control-plane logic can be tested independently.
type volumeStorage interface {
	PutObject(ctx context.Context, key string, data []byte, opts s3PutOptions) error
	PutObjectStream(ctx context.Context, key string, src io.Reader, opts s3PutOptions) (int64, error)
	GetObject(ctx context.Context, key string) (io.ReadCloser, error)
	HeadObject(ctx context.Context, key string) (*s3ObjectHead, error)
	ListObjectsV2(ctx context.Context, prefix, delimiter, continuationToken string, maxKeys int) (*s3ListPage, error)
	DeleteObject(ctx context.Context, key string) error
	CopyObject(ctx context.Context, srcKey, dstKey string, opts s3CopyOptions) error
	Close() error
}

type s3StoragePolicy struct {
	allowInsecureLoopback bool
	allowPrivateEndpoints bool
	allowedEndpointHosts  []string
	httpClient            *http.Client
	clock                 func() time.Time
	partSize              int
}

type s3VolumeStorage struct {
	creds      VolumeContentCredentials
	endpoint   *url.URL
	virtual    bool
	httpClient *http.Client
	ownsClient bool
	clock      func() time.Time
	partSize   int
}

func newS3VolumeStorage(creds VolumeContentCredentials, policy s3StoragePolicy) (*s3VolumeStorage, error) {
	parsed, err := validateStorageEndpoint(creds.Endpoint, policy)
	if err != nil {
		return nil, err
	}
	virtual, err := resolveS3AddressingStyle(creds.AddressingStyle, creds.Bucket, parsed.Hostname())
	if err != nil {
		return nil, err
	}

	clk := policy.clock
	if clk == nil {
		clk = time.Now
	}
	partSize := policy.partSize
	if partSize <= 0 {
		partSize = defaultS3MultipartChunkSize
	}

	var client *http.Client
	ownsClient := false
	if policy.httpClient != nil {
		clone := *policy.httpClient
		clone.CheckRedirect = disallowStorageRedirects
		client = &clone
	} else {
		client = newSafeS3HTTPClient(policy)
		ownsClient = true
	}

	return &s3VolumeStorage{
		creds:      creds,
		endpoint:   parsed,
		virtual:    virtual,
		httpClient: client,
		ownsClient: ownsClient,
		clock:      clk,
		partSize:   partSize,
	}, nil
}

func (s *s3VolumeStorage) Close() error {
	if s == nil {
		return nil
	}
	if s.ownsClient && s.httpClient != nil {
		s.httpClient.CloseIdleConnections()
	}
	return nil
}

func disallowStorageRedirects(*http.Request, []*http.Request) error {
	return errors.New("redirects are not allowed for volume storage requests")
}

func newSafeS3HTTPClient(policy s3StoragePolicy) *http.Client {
	dialer := &net.Dialer{
		Timeout:   defaultS3DialTimeout,
		KeepAlive: 30 * time.Second,
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("invalid storage dial address: %w", err)
		}
		if ip, err := netip.ParseAddr(host); err == nil {
			if err := checkStorageIP(ip, policy); err != nil {
				return nil, err
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(ip.Unmap().String(), port))
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("storage host %q resolved to no addresses", host)
		}
		var allowed []netip.Addr
		for _, item := range ips {
			addrIP, ok := netip.AddrFromSlice(item.IP)
			if !ok {
				continue
			}
			if err := checkStorageIP(addrIP, policy); err != nil {
				return nil, err
			}
			allowed = append(allowed, addrIP.Unmap())
		}
		if len(allowed) == 0 {
			return nil, fmt.Errorf("storage host %q resolved to no usable addresses", host)
		}
		var lastErr error
		for _, ip := range allowed {
			conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if dialErr == nil {
				return conn, nil
			}
			lastErr = dialErr
		}
		return nil, lastErr
	}
	return &http.Client{
		Transport:     transport,
		CheckRedirect: disallowStorageRedirects,
	}
}

func checkStorageIP(raw netip.Addr, policy s3StoragePolicy) error {
	ip := raw.Unmap()
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() {
		return fmt.Errorf("storage endpoint resolves to disallowed address")
	}
	if ip.IsLoopback() {
		if policy.allowInsecureLoopback || policy.allowPrivateEndpoints {
			return nil
		}
		return fmt.Errorf("storage endpoint resolves to loopback address; enable loopback endpoints explicitly for local tests")
	}
	if ip.IsPrivate() || cgnatPrefix.Contains(ip) {
		if policy.allowPrivateEndpoints {
			return nil
		}
		return fmt.Errorf("storage endpoint resolves to private address; enable private endpoints explicitly for VPC storage")
	}
	return nil
}

func isLoopbackHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(h)
	return err == nil && ip.Unmap().IsLoopback()
}

func validateStorageEndpoint(raw string, policy s3StoragePolicy) (*url.URL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("storage endpoint must be non-empty")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("invalid storage endpoint URL")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "https" && scheme != "http" {
		return nil, fmt.Errorf("storage endpoint scheme must be https (or http for explicitly allowed loopback)")
	}
	host := parsed.Hostname()
	if host == "" || parsed.Host == "" {
		return nil, fmt.Errorf("storage endpoint must include a hostname")
	}
	if parsed.Port() == "0" {
		return nil, fmt.Errorf("storage endpoint port must be non-zero")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || strings.ContainsAny(trimmed, "?#") {
		return nil, fmt.Errorf("storage endpoint must not contain credentials, query, or fragment")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return nil, fmt.Errorf("storage endpoint must not contain a path prefix")
	}
	loopback := isLoopbackHost(host)
	if scheme != "https" {
		if !policy.allowInsecureLoopback || !loopback {
			return nil, fmt.Errorf("storage endpoint must use HTTPS outside explicitly permitted loopback tests")
		}
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if err := checkStorageIP(ip, policy); err != nil {
			return nil, err
		}
	} else if loopback && !policy.allowInsecureLoopback && !policy.allowPrivateEndpoints {
		return nil, fmt.Errorf("storage endpoint targets loopback host; enable loopback endpoints explicitly for local tests")
	}
	if len(policy.allowedEndpointHosts) > 0 && !hostAllowed(host, policy.allowedEndpointHosts) {
		return nil, fmt.Errorf("storage endpoint host %q is not in the allowed endpoint host list", host)
	}
	parsed.Scheme = scheme
	parsed.Path = ""
	parsed.RawPath = ""
	return parsed, nil
}

func hostAllowed(host string, allowlist []string) bool {
	h := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	for _, entry := range allowlist {
		pattern := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(entry), "."))
		if pattern == "" {
			continue
		}
		if strings.HasPrefix(pattern, ".") {
			if strings.HasSuffix(h, pattern) && len(h) > len(pattern) {
				return true
			}
			continue
		}
		if h == pattern {
			return true
		}
	}
	return false
}

func resolveS3AddressingStyle(style, bucket, hostname string) (bool, error) {
	switch style {
	case "", "auto":
		if _, err := netip.ParseAddr(hostname); err == nil || isLoopbackHost(hostname) {
			return false, nil
		}
		return s3BucketDNSLabelRe.MatchString(bucket), nil
	case "path":
		return false, nil
	case "virtual":
		if !s3BucketDNSLabelRe.MatchString(bucket) {
			return false, fmt.Errorf("bucket %q is not compatible with virtual-hosted addressing style", bucket)
		}
		return true, nil
	default:
		return false, fmt.Errorf("addressing_style must be 'auto', 'path', or 'virtual', got %q", style)
	}
}

func (s *s3VolumeStorage) buildRequestURL(key string, query url.Values) (hostHeader string, targetURL *url.URL, canonicalURI string) {
	u := *s.endpoint
	if s.virtual {
		host := s.creds.Bucket + "." + u.Hostname()
		if port := u.Port(); port != "" {
			u.Host = net.JoinHostPort(host, port)
		} else {
			u.Host = host
		}
		if key == "" {
			canonicalURI = "/"
			u.Path = "/"
			u.RawPath = "/"
		} else {
			canonicalURI = "/" + awsURIEncodePath(key)
			u.Path = "/" + key
			u.RawPath = canonicalURI
		}
	} else {
		if key == "" {
			canonicalURI = "/" + awsURIEncodeSegment(s.creds.Bucket)
			u.Path = "/" + s.creds.Bucket
			u.RawPath = canonicalURI
		} else {
			canonicalURI = "/" + awsURIEncodeSegment(s.creds.Bucket) + "/" + awsURIEncodePath(key)
			u.Path = "/" + s.creds.Bucket + "/" + key
			u.RawPath = canonicalURI
		}
	}
	if len(query) > 0 {
		u.RawQuery = awsCanonicalQueryString(query)
	}
	return u.Host, &u, canonicalURI
}

func (s *s3VolumeStorage) newSignedRequest(ctx context.Context, method, key string, query url.Values, body []byte, extraHeaders map[string]string) (*http.Request, error) {
	hostHeader, targetURL, canonicalURI := s.buildRequestURL(key, query)
	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, targetURL.String(), reader)
	if err != nil {
		return nil, s.scrubErr("request", "", err)
	}
	req.URL = targetURL
	req.Host = hostHeader
	if body != nil {
		req.ContentLength = int64(len(body))
	}

	payloadSum := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(payloadSum[:])

	now := s.clock().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if s.creds.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", s.creds.SessionToken)
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}

	signSigV4(req, hostHeader, canonicalURI, awsCanonicalQueryString(query), payloadHash, amzDate, dateStamp, s.creds.Region, s.creds.AccessKeyID, s.creds.SecretAccessKey)
	return req, nil
}

func signSigV4(req *http.Request, hostHeader, canonicalURI, canonicalQuery, payloadHash, amzDate, dateStamp, region, accessKeyID, secretAccessKey string) {
	headerMap := map[string]string{
		"host": hostHeader,
	}
	for k, vals := range req.Header {
		lk := strings.ToLower(strings.TrimSpace(k))
		if lk == "host" || lk == "authorization" || lk == "user-agent" {
			continue
		}
		if lk == "content-type" || lk == "if-none-match" || strings.HasPrefix(lk, "x-amz-") {
			trimmed := make([]string, len(vals))
			for i, v := range vals {
				trimmed[i] = collapseSpaces(strings.TrimSpace(v))
			}
			headerMap[lk] = strings.Join(trimmed, ",")
		}
	}

	keys := make([]string, 0, len(headerMap))
	for k := range headerMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var canonicalHeaders strings.Builder
	for _, k := range keys {
		canonicalHeaders.WriteString(k)
		canonicalHeaders.WriteByte(':')
		canonicalHeaders.WriteString(headerMap[k])
		canonicalHeaders.WriteByte('\n')
	}
	signedHeaders := strings.Join(keys, ";")

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI,
		canonicalQuery,
		canonicalHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	crSum := sha256.Sum256([]byte(canonicalRequest))
	scope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, region)
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hex.EncodeToString(crSum[:]),
	}, "\n")

	kDate := hmacSHA256([]byte("AWS4"+secretAccessKey), dateStamp)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, "s3")
	kSigning := hmacSHA256(kService, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))

	auth := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		accessKeyID, scope, signedHeaders, signature)
	req.Header.Set("Authorization", auth)
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

func collapseSpaces(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inSpace := false
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' || s[i] == '\t' {
			if !inSpace {
				b.WriteByte(' ')
				inSpace = true
			}
			continue
		}
		inSpace = false
		b.WriteByte(s[i])
	}
	return b.String()
}

func awsURIEncodeSegment(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isAWSUnreserved(c) {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func awsURIEncodePath(path string) string {
	parts := strings.Split(path, "/")
	for i, part := range parts {
		parts[i] = awsURIEncodeSegment(part)
	}
	return strings.Join(parts, "/")
}

func isAWSUnreserved(c byte) bool {
	return (c >= 'A' && c <= 'Z') ||
		(c >= 'a' && c <= 'z') ||
		(c >= '0' && c <= '9') ||
		c == '-' || c == '_' || c == '.' || c == '~'
}

func awsCanonicalQueryString(query url.Values) string {
	if len(query) == 0 {
		return ""
	}
	type kv struct {
		k string
		v string
	}
	var pairs []kv
	for k, vals := range query {
		ek := awsURIEncodeSegment(k)
		if len(vals) == 0 {
			pairs = append(pairs, kv{k: ek, v: ""})
			continue
		}
		for _, v := range vals {
			pairs = append(pairs, kv{k: ek, v: awsURIEncodeSegment(v)})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	var b strings.Builder
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(p.k)
		b.WriteByte('=')
		b.WriteString(p.v)
	}
	return b.String()
}

func (s *s3VolumeStorage) PutObject(ctx context.Context, key string, data []byte, opts s3PutOptions) error {
	headers := buildMetadataHeaders(opts.Metadata)
	if opts.ContentType != "" {
		headers["Content-Type"] = opts.ContentType
	} else {
		headers["Content-Type"] = "application/octet-stream"
	}
	if opts.IfNoneMatch != "" {
		headers["If-None-Match"] = opts.IfNoneMatch
	}
	req, err := s.newSignedRequest(ctx, http.MethodPut, key, nil, data, headers)
	if err != nil {
		return err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return s.scrubErr("write", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return s.parseErrorResponse("write", key, resp)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func (s *s3VolumeStorage) PutObjectStream(ctx context.Context, key string, src io.Reader, opts s3PutOptions) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	buf := make([]byte, s.partSize)
	n, firstErr := readChunk(ctx, src, buf)
	if firstErr != nil && !errors.Is(firstErr, io.EOF) {
		return 0, firstErr
	}
	if errors.Is(firstErr, io.EOF) {
		if err := s.PutObject(ctx, key, buf[:n], opts); err != nil {
			return 0, err
		}
		return int64(n), nil
	}

	// Stream exceeds a single part: check If-None-Match upfront when Force=false
	// so we avoid uploading parts for a guaranteed conflict.
	if opts.IfNoneMatch == "*" {
		if _, err := s.HeadObject(ctx, key); err == nil {
			return 0, &VolumeStorageError{
				Op:         "write",
				Path:       key,
				StatusCode: http.StatusPreconditionFailed,
				Code:       "PreconditionFailed",
				Message:    "object already exists",
				Err:        ErrVolumePathExists,
			}
		} else if !errors.Is(err, ErrVolumePathNotFound) {
			return 0, err
		}
	}

	uploadID, err := s.createMultipartUpload(ctx, key, opts)
	if err != nil {
		return 0, err
	}
	completed := false
	defer func() {
		if !completed {
			abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultS3AbortTimeout)
			defer cancel()
			_ = s.abortMultipartUpload(abortCtx, key, uploadID)
		}
	}()

	var total int64
	var parts []s3CompletedPart
	partNum := 1

	for {
		if n > 0 {
			etag, upErr := s.uploadPart(ctx, key, uploadID, partNum, buf[:n])
			if upErr != nil {
				return total, upErr
			}
			parts = append(parts, s3CompletedPart{PartNumber: partNum, ETag: etag})
			total += int64(n)
			partNum++
		}
		if errors.Is(firstErr, io.EOF) {
			break
		}
		n, firstErr = readChunk(ctx, src, buf)
		if firstErr != nil && !errors.Is(firstErr, io.EOF) {
			return total, firstErr
		}
	}

	if err := s.completeMultipartUpload(ctx, key, uploadID, parts, opts.IfNoneMatch); err != nil {
		return total, err
	}
	completed = true
	return total, nil
}

func readChunk(ctx context.Context, src io.Reader, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, err := src.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

type s3InitiateMultipartResult struct {
	UploadID string `xml:"UploadId"`
}

type s3CompletedPart struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

type s3CompleteMultipartRequest struct {
	XMLName xml.Name          `xml:"CompleteMultipartUpload"`
	Parts   []s3CompletedPart `xml:"Part"`
}

func (s *s3VolumeStorage) createMultipartUpload(ctx context.Context, key string, opts s3PutOptions) (string, error) {
	headers := buildMetadataHeaders(opts.Metadata)
	if opts.ContentType != "" {
		headers["Content-Type"] = opts.ContentType
	} else {
		headers["Content-Type"] = "application/octet-stream"
	}
	query := url.Values{"uploads": {""}}
	req, err := s.newSignedRequest(ctx, http.MethodPost, key, query, nil, headers)
	if err != nil {
		return "", err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", s.scrubErr("multipart-init", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", s.parseErrorResponse("multipart-init", key, resp)
	}
	var out s3InitiateMultipartResult
	if err := xml.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", &VolumeStorageError{Op: "multipart-init", Path: key, Message: "invalid multipart upload response"}
	}
	if strings.TrimSpace(out.UploadID) == "" {
		return "", &VolumeStorageError{Op: "multipart-init", Path: key, Message: "missing upload ID in multipart response"}
	}
	return out.UploadID, nil
}

func (s *s3VolumeStorage) uploadPart(ctx context.Context, key, uploadID string, partNumber int, data []byte) (string, error) {
	query := url.Values{
		"partNumber": {strconv.Itoa(partNumber)},
		"uploadId":   {uploadID},
	}
	req, err := s.newSignedRequest(ctx, http.MethodPut, key, query, data, nil)
	if err != nil {
		return "", err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", s.scrubErr("multipart-upload", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", s.parseErrorResponse("multipart-upload", key, resp)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	etag := strings.TrimSpace(resp.Header.Get("ETag"))
	if etag == "" {
		return "", &VolumeStorageError{Op: "multipart-upload", Path: key, Message: "missing ETag in upload part response"}
	}
	return etag, nil
}

func (s *s3VolumeStorage) completeMultipartUpload(ctx context.Context, key, uploadID string, parts []s3CompletedPart, ifNoneMatch string) error {
	payload, err := xml.Marshal(s3CompleteMultipartRequest{Parts: parts})
	if err != nil {
		return err
	}
	headers := map[string]string{"Content-Type": "application/xml"}
	if ifNoneMatch != "" {
		headers["If-None-Match"] = ifNoneMatch
	}
	query := url.Values{"uploadId": {uploadID}}
	req, err := s.newSignedRequest(ctx, http.MethodPost, key, query, payload, headers)
	if err != nil {
		return err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return s.scrubErr("multipart-complete", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return s.parseErrorResponse("multipart-complete", key, resp)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxS3ErrorBodyBytes))
	if err == nil && bytes.Contains(raw, []byte("<Error>")) {
		return s.parseErrorXML("multipart-complete", key, http.StatusInternalServerError, raw)
	}
	return nil
}

func (s *s3VolumeStorage) abortMultipartUpload(ctx context.Context, key, uploadID string) error {
	query := url.Values{"uploadId": {uploadID}}
	req, err := s.newSignedRequest(ctx, http.MethodDelete, key, query, nil, nil)
	if err != nil {
		return err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return s.scrubErr("multipart-abort", key, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func (s *s3VolumeStorage) GetObject(ctx context.Context, key string) (io.ReadCloser, error) {
	req, err := s.newSignedRequest(ctx, http.MethodGet, key, nil, nil, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, s.scrubErr("read", key, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, s.parseErrorResponse("read", key, resp)
	}
	return resp.Body, nil
}

func (s *s3VolumeStorage) HeadObject(ctx context.Context, key string) (*s3ObjectHead, error) {
	req, err := s.newSignedRequest(ctx, http.MethodHead, key, nil, nil, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, s.scrubErr("stat", key, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, s.parseErrorResponse("stat", key, resp)
	}
	size := resp.ContentLength
	if size < 0 {
		if parsed, parseErr := strconv.ParseInt(strings.TrimSpace(resp.Header.Get("Content-Length")), 10, 64); parseErr == nil && parsed >= 0 {
			size = parsed
		} else {
			size = 0
		}
	}
	var lastMod time.Time
	if lm := strings.TrimSpace(resp.Header.Get("Last-Modified")); lm != "" {
		if parsed, parseErr := http.ParseTime(lm); parseErr == nil {
			lastMod = parsed.UTC()
		}
	}
	meta := make(map[string]string)
	for k, vals := range resp.Header {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-amz-meta-") && len(vals) > 0 {
			meta[strings.TrimPrefix(lk, "x-amz-meta-")] = vals[0]
		}
	}
	return &s3ObjectHead{
		Size:         size,
		LastModified: lastMod,
		ContentType:  resp.Header.Get("Content-Type"),
		Metadata:     meta,
	}, nil
}

type s3ListBucketResultXML struct {
	IsTruncated           bool   `xml:"IsTruncated"`
	KeyCount              int    `xml:"KeyCount"`
	NextContinuationToken string `xml:"NextContinuationToken"`
	Contents              []struct {
		Key          string `xml:"Key"`
		Size         int64  `xml:"Size"`
		LastModified string `xml:"LastModified"`
	} `xml:"Contents"`
	CommonPrefixes []struct {
		Prefix string `xml:"Prefix"`
	} `xml:"CommonPrefixes"`
}

func (s *s3VolumeStorage) ListObjectsV2(ctx context.Context, prefix, delimiter, continuationToken string, maxKeys int) (*s3ListPage, error) {
	query := url.Values{
		"list-type": {"2"},
	}
	if prefix != "" {
		query.Set("prefix", prefix)
	}
	if delimiter != "" {
		query.Set("delimiter", delimiter)
	}
	if continuationToken != "" {
		query.Set("continuation-token", continuationToken)
	}
	if maxKeys > 0 {
		query.Set("max-keys", strconv.Itoa(maxKeys))
	}
	req, err := s.newSignedRequest(ctx, http.MethodGet, "", query, nil, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, s.scrubErr("list", prefix, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, s.parseErrorResponse("list", prefix, resp)
	}
	var parsed s3ListBucketResultXML
	if err := xml.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, &VolumeStorageError{Op: "list", Path: prefix, Message: "invalid S3 list XML response"}
	}
	page := &s3ListPage{
		IsTruncated:           parsed.IsTruncated,
		NextContinuationToken: parsed.NextContinuationToken,
		KeyCount:              parsed.KeyCount,
	}
	for _, item := range parsed.Contents {
		var lm time.Time
		if item.LastModified != "" {
			if t, parseErr := time.Parse(time.RFC3339Nano, item.LastModified); parseErr == nil {
				lm = t.UTC()
			} else if t, parseErr := time.Parse(time.RFC3339, item.LastModified); parseErr == nil {
				lm = t.UTC()
			}
		}
		page.Objects = append(page.Objects, s3ListedObject{
			Key:          item.Key,
			Size:         item.Size,
			LastModified: lm,
		})
	}
	for _, cp := range parsed.CommonPrefixes {
		page.CommonPrefixes = append(page.CommonPrefixes, cp.Prefix)
	}
	if page.KeyCount == 0 {
		page.KeyCount = len(page.Objects) + len(page.CommonPrefixes)
	}
	return page, nil
}

func (s *s3VolumeStorage) DeleteObject(ctx context.Context, key string) error {
	req, err := s.newSignedRequest(ctx, http.MethodDelete, key, nil, nil, nil)
	if err != nil {
		return err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return s.scrubErr("remove", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return s.parseErrorResponse("remove", key, resp)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func (s *s3VolumeStorage) CopyObject(ctx context.Context, srcKey, dstKey string, opts s3CopyOptions) error {
	headers := buildMetadataHeaders(opts.Metadata)
	headers["X-Amz-Copy-Source"] = "/" + awsURIEncodeSegment(s.creds.Bucket) + "/" + awsURIEncodePath(srcKey)
	headers["X-Amz-Metadata-Directive"] = "REPLACE"
	if opts.ContentType != "" {
		headers["Content-Type"] = opts.ContentType
	} else {
		headers["Content-Type"] = "application/octet-stream"
	}
	req, err := s.newSignedRequest(ctx, http.MethodPut, dstKey, nil, nil, headers)
	if err != nil {
		return err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return s.scrubErr("update-metadata", dstKey, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return s.parseErrorResponse("update-metadata", dstKey, resp)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxS3ErrorBodyBytes))
	if err == nil && bytes.Contains(raw, []byte("<Error>")) {
		return s.parseErrorXML("update-metadata", dstKey, http.StatusInternalServerError, raw)
	}
	return nil
}

func buildMetadataHeaders(metadata map[string]string) map[string]string {
	headers := make(map[string]string, len(metadata)+2)
	for k, v := range metadata {
		if k != "" {
			headers["X-Amz-Meta-"+strings.ToLower(k)] = v
		}
	}
	return headers
}

type s3ErrorXML struct {
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

func (s *s3VolumeStorage) parseErrorResponse(op, key string, resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxS3ErrorBodyBytes))
	return s.parseErrorXML(op, key, resp.StatusCode, raw)
}

func (s *s3VolumeStorage) parseErrorXML(op, key string, statusCode int, raw []byte) error {
	var parsed s3ErrorXML
	_ = xml.Unmarshal(raw, &parsed)
	code := strings.TrimSpace(parsed.Code)
	msg := s.redact(strings.TrimSpace(parsed.Message))
	if msg == "" {
		if code != "" {
			msg = code
		} else {
			msg = http.StatusText(statusCode)
		}
	}
	var sentinel error
	switch {
	case statusCode == http.StatusNotFound || code == "NoSuchKey" || code == "NotFound" || code == "404":
		sentinel = ErrVolumePathNotFound
	case statusCode == http.StatusPreconditionFailed || code == "PreconditionFailed":
		sentinel = ErrVolumePathExists
	case isExpiredS3Code(code, msg):
		sentinel = ErrVolumeCredentialsExpired
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden ||
		code == "AccessDenied" || code == "InvalidAccessKeyId" || code == "SignatureDoesNotMatch":
		sentinel = ErrAuthentication
	}
	return &VolumeStorageError{
		Op:         op,
		Path:       key,
		StatusCode: statusCode,
		Code:       code,
		Message:    msg,
		Err:        sentinel,
	}
}

func isExpiredS3Code(code, message string) bool {
	switch code {
	case "ExpiredToken", "ExpiredTokenException", "InvalidToken", "RequestExpired", "TokenRefreshRequired":
		return true
	}
	lower := strings.ToLower(message)
	return strings.Contains(lower, "token has expired") || strings.Contains(lower, "expired token") || strings.Contains(lower, "security token included in the request is expired")
}

func (s *s3VolumeStorage) scrubErr(op, key string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return &VolumeStorageError{
		Op:      op,
		Path:    key,
		Message: s.redact(err.Error()),
	}
}

func (s *s3VolumeStorage) redact(text string) string {
	return redactVolumeSecrets(text, s.creds.AccessKeyID, s.creds.SecretAccessKey, s.creds.SessionToken)
}

func redactVolumeSecrets(text string, secrets ...string) string {
	out := text
	for _, secret := range secrets {
		if secret != "" {
			out = strings.ReplaceAll(out, secret, "***")
		}
	}
	out = s3SensitiveQueryRe.ReplaceAllString(out, "${1}***")
	out = s3SensitiveHeaderRe.ReplaceAllString(out, "${1}***")
	out = s3SignatureKVRe.ReplaceAllString(out, "${1}***")
	return out
}
