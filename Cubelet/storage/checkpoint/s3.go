// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package checkpoint

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	mathrand "math/rand"
	"net"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
)

// ErrNotFound is returned for a key the store does not hold.
var ErrNotFound = errors.New("object not found")

// ErrExists is returned by PutIfAbsent when the key already holds an object.
var ErrExists = errors.New("object already exists")

const (
	emptyPayloadSHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	requestAttempts    = 5
	requestBackoff     = 200 * time.Millisecond
	// Each chunk request gets its own deadline so one stalled connection
	// cannot hold a worker for the whole job.
	requestTimeout = 2 * time.Minute
)

// Client is a minimal S3 client (SigV4, path-style or virtual-host) that
// covers what checkpoints need: whole-object PUT／GET, HEAD, DELETE,
// DeleteObjects and ListObjectsV2. It deliberately avoids multipart uploads: chunks are small
// enough to send in one request, and every chunk is its own object.
type Client struct {
	cfg    Config
	base   *url.URL
	http   *http.Client
	now    func() time.Time
	region string
	// noConditional is set once the store was seen to ignore
	// If-None-Match; commits then take the degraded path.
	noConditional atomic.Bool
	// noBatchDelete is set once the store refused DeleteObjects.
	noBatchDelete atomic.Bool
}

// Config returns the client's configuration.
func (c *Client) Config() Config { return c.cfg }

// ConditionalWritesOptional reports whether the configuration lets this
// store hold checkpoints although it ignores If-None-Match.
func (c *Client) ConditionalWritesOptional() bool {
	return c.cfg.ConditionalWrites == ConditionalOptional
}

// UseConditionalWrites records whether the store honours If-None-Match.
// Without it, commit objects are checked around an ordinary PUT instead.
func (c *Client) UseConditionalWrites(supported bool) {
	c.noConditional.Store(!supported)
}

// ConditionalWritesInUse reports whether commits rely on If-None-Match.
func (c *Client) ConditionalWritesInUse() bool { return !c.noConditional.Load() }

// NewClient builds a client for cfg.
func NewClient(cfg Config) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	raw := strings.TrimSpace(cfg.Endpoint)
	if !strings.Contains(raw, "://") {
		scheme := "https"
		if cfg.NoTLS {
			scheme = "http"
		}
		raw = scheme + "://" + raw
	}
	base, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse checkpoint endpoint: %w", err)
	}
	region := strings.TrimSpace(cfg.Region)
	if region == "" {
		region = "us-east-1"
	}
	transport := &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     90 * time.Second,
	}
	return &Client{
		cfg:    cfg,
		base:   base,
		http:   &http.Client{Transport: transport},
		now:    time.Now,
		region: region,
	}, nil
}

// Prefix is the key prefix all checkpoints live under.
func (c *Client) Prefix() string { return strings.Trim(c.cfg.Prefix, "/") }

// Put stores body under key. The payload hash is signed, so the store
// rejects a body that changed in transit.
func (c *Client) Put(ctx context.Context, key string, body []byte) error {
	sum := sha256.Sum256(body)
	_, err := c.do(ctx, http.MethodPut, key, nil, nil, body, hex.EncodeToString(sum[:]), nil)
	return err
}

// PutIfAbsent stores body under key only when the key holds nothing yet
// (If-None-Match: *), and returns ErrExists otherwise. A retried request
// whose first attempt did land also reports ErrExists; callers compare the
// stored object to tell that apart from a real conflict.
func (c *Client) PutIfAbsent(ctx context.Context, key string, body []byte) error {
	sum := sha256.Sum256(body)
	hdr := http.Header{}
	hdr.Set("If-None-Match", "*")
	_, err := c.do(ctx, http.MethodPut, key, nil, hdr, body, hex.EncodeToString(sum[:]), nil)
	return err
}

// ErrConditionalWriteUnsupported means the store does not honour
// If-None-Match on PUT: it overwrites an existing object, or refuses the
// header altogether. A committed session cannot be protected on such a
// store.
var ErrConditionalWriteUnsupported = errors.New("object store does not support conditional writes (If-None-Match: *)")

// ProbeConditionalWrite checks that the store refuses to overwrite an
// existing object under If-None-Match: *, by writing a probe object twice.
// It returns nil when the second write is refused, an error wrapping
// ErrConditionalWriteUnsupported when the store overwrote it or rejected the
// header, and any other error while the store cannot be asked.
func (c *Client) ProbeConditionalWrite(ctx context.Context) error {
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	key := strings.TrimPrefix(c.Prefix()+"/.probe/conditional-write-"+hex.EncodeToString(nonce[:]), "/")
	body := []byte("conditional write probe")
	unsupported := func(err error) error {
		var se *StatusError
		if errors.As(err, &se) && (se.Code == http.StatusNotImplemented || se.Code == http.StatusBadRequest ||
			se.Code == http.StatusForbidden || se.Code == http.StatusMethodNotAllowed) {
			return fmt.Errorf("%w: %v", ErrConditionalWriteUnsupported, err)
		}
		return err
	}
	if err := c.PutIfAbsent(ctx, key, body); err != nil {
		return unsupported(err)
	}
	defer func() { _ = c.Delete(context.WithoutCancel(ctx), key) }()
	switch err := c.PutIfAbsent(ctx, key, body); {
	case errors.Is(err, ErrExists):
		return nil
	case err == nil:
		return fmt.Errorf("%w: a second write of %s was accepted", ErrConditionalWriteUnsupported, key)
	default:
		return unsupported(err)
	}
}

// Get returns the whole object.
func (c *Client) Get(ctx context.Context, key string) ([]byte, error) {
	return c.do(ctx, http.MethodGet, key, nil, nil, nil, emptyPayloadSHA256, nil)
}

// Head reports the size of key, or ErrNotFound.
func (c *Client) Head(ctx context.Context, key string) (int64, error) {
	var size int64 = -1
	_, err := c.do(ctx, http.MethodHead, key, nil, nil, nil, emptyPayloadSHA256, func(rsp *http.Response) {
		size = rsp.ContentLength
	})
	return size, err
}

// Delete removes key. A missing key is not an error.
func (c *Client) Delete(ctx context.Context, key string) error {
	_, err := c.do(ctx, http.MethodDelete, key, nil, nil, nil, emptyPayloadSHA256, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// ErrBatchDeleteUnsupported means the store does not implement
// DeleteObjects; the caller deletes the keys one by one.
var ErrBatchDeleteUnsupported = errors.New("object store does not support DeleteObjects")

// MaxBatchDelete is the most keys one DeleteObjects request may carry.
const MaxBatchDelete = 1000

// BatchDeleteSupported is false once the store refused DeleteObjects.
func (c *Client) BatchDeleteSupported() bool { return !c.noBatchDelete.Load() }

type deleteRequest struct {
	XMLName xml.Name `xml:"Delete"`
	Quiet   bool     `xml:"Quiet"`
	Objects []struct {
		Key string `xml:"Key"`
	} `xml:"Object"`
}

type deleteResult struct {
	Errors []struct {
		Key     string `xml:"Key"`
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	} `xml:"Error"`
}

// DeleteObjects removes up to MaxBatchDelete keys in one request and returns
// the keys the store could not delete, each with its reason; a missing key
// counts as deleted. A store without DeleteObjects is ErrBatchDeleteUnsupported
// from then on, without asking it again.
func (c *Client) DeleteObjects(ctx context.Context, keys []string) (map[string]error, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	if len(keys) > MaxBatchDelete {
		return nil, fmt.Errorf("%d keys in one DeleteObjects, the most is %d", len(keys), MaxBatchDelete)
	}
	if c.noBatchDelete.Load() {
		return nil, ErrBatchDeleteUnsupported
	}
	req := deleteRequest{Quiet: true}
	req.Objects = make([]struct {
		Key string `xml:"Key"`
	}, len(keys))
	for i, k := range keys {
		req.Objects[i].Key = k
	}
	body, err := xml.Marshal(req)
	if err != nil {
		return nil, err
	}
	// S3 requires Content-MD5 on DeleteObjects; it is an integrity check
	// the protocol mandates, not a security hash.
	digest := md5.Sum(body) // NOCC:Weak Crypto()
	hdr := http.Header{}
	hdr.Set("Content-MD5", base64.StdEncoding.EncodeToString(digest[:]))
	hdr.Set("Content-Type", "application/xml")
	sum := sha256.Sum256(body)
	out, err := c.do(ctx, http.MethodPost, "", url.Values{"delete": {""}}, hdr, body, hex.EncodeToString(sum[:]), nil)
	if err != nil {
		var se *StatusError
		if errors.As(err, &se) && (se.Code == http.StatusNotImplemented || se.Code == http.StatusMethodNotAllowed) {
			c.noBatchDelete.Store(true)
			return nil, fmt.Errorf("%w: %v", ErrBatchDeleteUnsupported, err)
		}
		return nil, err
	}
	var res deleteResult
	if err := xml.Unmarshal(out, &res); err != nil {
		return nil, fmt.Errorf("decode DeleteObjects response: %w", err)
	}
	failed := map[string]error{}
	for _, e := range res.Errors {
		if e.Code == "NoSuchKey" {
			continue
		}
		failed[e.Key] = fmt.Errorf("s3 DeleteObjects %q: %s: %s", e.Key, e.Code, e.Message)
	}
	return failed, nil
}

type listResult struct {
	Contents []struct {
		Key          string    `xml:"Key"`
		Size         int64     `xml:"Size"`
		LastModified time.Time `xml:"LastModified"`
	} `xml:"Contents"`
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
}

// Object is one listed key.
type Object struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// List returns every key under prefix.
func (c *Client) List(ctx context.Context, prefix string) ([]string, error) {
	objs, err := c.ListObjects(ctx, prefix)
	if err != nil {
		return nil, err
	}
	keys := make([]string, len(objs))
	for i, o := range objs {
		keys[i] = o.Key
	}
	return keys, nil
}

// ListObjects returns every object under prefix with its size and time.
func (c *Client) ListObjects(ctx context.Context, prefix string) ([]Object, error) {
	var objs []Object
	token := ""
	for {
		q := url.Values{}
		q.Set("list-type", "2")
		q.Set("prefix", prefix)
		if token != "" {
			q.Set("continuation-token", token)
		}
		body, err := c.do(ctx, http.MethodGet, "", q, nil, nil, emptyPayloadSHA256, nil)
		if err != nil {
			return nil, err
		}
		var res listResult
		if err := xml.Unmarshal(body, &res); err != nil {
			return nil, fmt.Errorf("decode list response: %w", err)
		}
		for _, item := range res.Contents {
			objs = append(objs, Object{Key: item.Key, Size: item.Size, LastModified: item.LastModified})
		}
		if !res.IsTruncated || res.NextContinuationToken == "" {
			return objs, nil
		}
		token = res.NextContinuationToken
	}
}

// DeletePrefix deletes every object under prefix, which must lie below the
// client's prefix, and returns how many it deleted. Retiring a checkpoint
// deletes thousands of objects beside exports and imports that share the
// store's request quota, so it goes like a sweep: DeleteObjects batches when
// the store has it, else single deletes, opt.Workers requests at a time and
// at most opt.RequestsPerSecond requests, the listing included.
func (c *Client) DeletePrefix(ctx context.Context, prefix string, opt SweepOptions) (int, error) {
	if p := strings.Trim(prefix, "/"); p == "" || p == c.Prefix() || !strings.HasPrefix(p, c.Prefix()+"/") {
		return 0, fmt.Errorf("refusing to delete prefix %q", prefix)
	}
	opt = opt.withDefaults()
	ctx, _ = withRequestHooks(ctx, float64(opt.RequestsPerSecond))
	keys, err := c.List(ctx, prefix)
	if err != nil {
		return 0, err
	}
	deleted, failed, _, firstErr := deleteKeys(ctx, c, keys, opt)
	if failed > 0 {
		return deleted, fmt.Errorf("%d of %d objects under %s were not deleted: %w", failed, len(keys), prefix, firstErr)
	}
	if deleted < len(keys) {
		if err := ctx.Err(); err != nil {
			return deleted, err
		}
		return deleted, fmt.Errorf("%d of %d objects under %s were not deleted", len(keys)-deleted, len(keys), prefix)
	}
	return deleted, nil
}

// StatusError is a non-2xx response.
type StatusError struct {
	Method string
	Key    string
	Code   int
	Body   string
	// RetryAfter is the store's Retry-After, when it sent one.
	RetryAfter time.Duration
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("s3 %s %q: http %d: %s", e.Method, e.Key, e.Code, e.Body)
}

func retryable(err error) bool {
	var se *StatusError
	if errors.As(err, &se) {
		if se.Code == http.StatusNotImplemented {
			// The store will never do what was asked.
			return false
		}
		return se.Code >= 500 || se.Code == http.StatusTooManyRequests || se.Code == http.StatusRequestTimeout
	}
	return !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrExists) && !errors.Is(err, context.Canceled)
}

// A store that throttles (429, or 503 SlowDown) is asked again far more
// patiently than one that failed: a bandwidth or request quota frees up
// within seconds to tens of seconds, and giving up there would fail a
// whole checkpoint transfer. Vars for tests.
var (
	throttleAttempts    = 10
	throttleBackoffBase = time.Second
	throttleBackoffMax  = 30 * time.Second
	retryAfterMax       = time.Minute
)

// throttled reports whether err is the store asking to slow down, and how
// long it asked to wait.
func throttled(err error) (bool, time.Duration) {
	var se *StatusError
	if errors.As(err, &se) && (se.Code == http.StatusTooManyRequests || se.Code == http.StatusServiceUnavailable) {
		return true, se.RetryAfter
	}
	return false, 0
}

// retryWait is how long to wait before attempt+1, and whether to try at all.
func retryWait(attempt int, err error) (time.Duration, bool) {
	slow, after := throttled(err)
	if !slow {
		return requestBackoff * time.Duration(1<<(attempt-1)), attempt < requestAttempts
	}
	wait := min(throttleBackoffBase<<(attempt-1), throttleBackoffMax)
	if after > wait {
		wait = min(after, retryAfterMax)
	}
	// Spread the clients a quota throttles together.
	wait += time.Duration(mathrand.Int63n(int64(wait)/4 + 1))
	return wait, attempt < throttleAttempts
}

// requestRetries counts requests of this process that were sent again after
// a retryable failure (429, 503, a dropped connection).
var requestRetries atomic.Int64

// RequestRetries is the total number of request retries so far.
func RequestRetries() int64 { return requestRetries.Load() }

// ThrottleStats counts the responses in which the store asked this process
// to slow down, and the backoff the retries that followed waited.
type ThrottleStats struct {
	Status429 int64
	Status503 int64
	Backoff   time.Duration
}

// Sub is what happened between an earlier reading o and s.
func (s ThrottleStats) Sub(o ThrottleStats) ThrottleStats {
	return ThrottleStats{Status429: s.Status429 - o.Status429, Status503: s.Status503 - o.Status503, Backoff: s.Backoff - o.Backoff}
}

var throttled429, throttled503, throttleBackoffNS atomic.Int64

// Throttles is this process's throttling so far; callers diff two readings.
func Throttles() ThrottleStats {
	return ThrottleStats{Status429: throttled429.Load(), Status503: throttled503.Load(), Backoff: time.Duration(throttleBackoffNS.Load())}
}

// requestHooks rate-limits and counts the requests of one operation: every
// request made with a context that carries them, retries included. A sweep
// uses them to stay under its own request rate and to report its own 429s
// apart from the exports and imports that run beside it.
type requestHooks struct {
	limit     *rate.Limiter
	requests  atomic.Int64
	retries   atomic.Int64
	s429      atomic.Int64
	s503      atomic.Int64
	backoffNS atomic.Int64
}

type requestHooksKey struct{}

// withRequestHooks makes every request under ctx go through h. A limit of
// zero or less sends without one.
func withRequestHooks(ctx context.Context, perSecond float64) (context.Context, *requestHooks) {
	h := &requestHooks{}
	if perSecond > 0 {
		h.limit = rate.NewLimiter(rate.Limit(perSecond), 1)
	}
	return context.WithValue(ctx, requestHooksKey{}, h), h
}

func hooksOf(ctx context.Context) *requestHooks {
	h, _ := ctx.Value(requestHooksKey{}).(*requestHooks)
	return h
}

// throttles is what the requests under h were throttled.
func (h *requestHooks) throttles() ThrottleStats {
	return ThrottleStats{Status429: h.s429.Load(), Status503: h.s503.Load(), Backoff: time.Duration(h.backoffNS.Load())}
}

// keyCategory names the kind of object behind key without its ids, for logs.
func keyCategory(key string) string {
	switch {
	case strings.Contains(key, "/lineage/") && strings.Contains(key, "/c/"):
		return "lineage-chunk"
	case strings.Contains(key, "/template/") && strings.Contains(key, "/c/"):
		return "template-chunk"
	case strings.Contains(key, "/c/"):
		return "session-chunk"
	case key == "":
		return "bucket"
	}
	return path.Base(key)
}

// noteThrottle counts a 429/503 and logs the retry it causes, if any.
func noteThrottle(ctx context.Context, method, key string, err error, attempt int, wait time.Duration, again bool) {
	var se *StatusError
	if !errors.As(err, &se) {
		return
	}
	h := hooksOf(ctx)
	if se.Code == http.StatusTooManyRequests {
		throttled429.Add(1)
		if h != nil {
			h.s429.Add(1)
		}
	} else {
		throttled503.Add(1)
		if h != nil {
			h.s503.Add(1)
		}
	}
	what := keyCategory(key)
	if method == http.MethodPost && key == "" {
		what = "batch-delete"
	}
	if !again {
		log.G(ctx).Warnf("xfs-s3: store throttled %s %s (HTTP %d) on attempt %d; giving up", method, what, se.Code, attempt)
		return
	}
	throttleBackoffNS.Add(int64(wait))
	if h != nil {
		h.backoffNS.Add(int64(wait))
	}
	log.G(ctx).Warnf("xfs-s3: store throttled %s %s (HTTP %d) on attempt %d; retrying in %s", method, what, se.Code, attempt, wait.Round(time.Millisecond))
}

func (c *Client) do(ctx context.Context, method, key string, query url.Values, hdr http.Header, body []byte, payloadHash string, onRsp func(*http.Response)) ([]byte, error) {
	var lastErr error
	h := hooksOf(ctx)
	for attempt := 1; ; attempt++ {
		if h != nil {
			if h.limit != nil {
				if err := h.limit.Wait(ctx); err != nil {
					return nil, err
				}
			}
			h.requests.Add(1)
		}
		out, err := c.once(ctx, method, key, query, hdr, body, payloadHash, onRsp)
		if err == nil {
			return out, nil
		}
		lastErr = err
		if ctx.Err() != nil || !retryable(err) {
			break
		}
		wait, again := retryWait(attempt, err)
		if slow, _ := throttled(err); slow {
			noteThrottle(ctx, method, key, err, attempt, wait, again)
		}
		if !again {
			break
		}
		requestRetries.Add(1)
		if h != nil {
			h.retries.Add(1)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	return nil, lastErr
}

// parseRetryAfter reads a Retry-After header: delay seconds or an HTTP date.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}

func (c *Client) once(ctx context.Context, method, key string, query url.Values, hdr http.Header, body []byte, payloadHash string, onRsp func(*http.Response)) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	u := *c.base
	host := u.Host
	path := "/"
	if c.cfg.PathStyle {
		path += c.cfg.Bucket
		if key != "" {
			path += "/" + key
		} else {
			path += "/"
		}
	} else {
		host = c.cfg.Bucket + "." + u.Host
		path += key
	}
	u.Host = host
	u.Path = path
	u.RawPath = escapePath(path)
	u.RawQuery = canonicalQuery(query)
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	c.sign(req, host, u.RawPath, u.RawQuery, payloadHash)
	if err := waitBandwidth(ctx, len(body)); err != nil {
		return nil, err
	}
	rsp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer rsp.Body.Close()
	if rsp.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, rsp.Body)
		return nil, fmt.Errorf("s3 %s %q: %w", method, key, ErrNotFound)
	}
	if rsp.StatusCode == http.StatusPreconditionFailed && hdr.Get("If-None-Match") != "" {
		_, _ = io.Copy(io.Discard, rsp.Body)
		return nil, fmt.Errorf("s3 %s %q: %w", method, key, ErrExists)
	}
	if rsp.StatusCode < 200 || rsp.StatusCode > 299 {
		msg, _ := io.ReadAll(io.LimitReader(rsp.Body, 2048))
		return nil, &StatusError{Method: method, Key: key, Code: rsp.StatusCode, Body: strings.TrimSpace(string(msg)),
			RetryAfter: parseRetryAfter(rsp.Header.Get("Retry-After"), c.now())}
	}
	if onRsp != nil {
		onRsp(rsp)
	}
	if method == http.MethodHead {
		return nil, nil
	}
	if rsp.ContentLength > 0 {
		if err := waitBandwidth(ctx, int(rsp.ContentLength)); err != nil {
			return nil, err
		}
	}
	data, err := io.ReadAll(rsp.Body)
	if err != nil {
		return nil, err
	}
	if method == http.MethodGet && rsp.ContentLength >= 0 && int64(len(data)) != rsp.ContentLength {
		return nil, fmt.Errorf("s3 GET %q: short body %d of %d", key, len(data), rsp.ContentLength)
	}
	return data, nil
}

func (c *Client) sign(req *http.Request, host, rawPath, rawQuery, payloadHash string) {
	now := c.now().UTC()
	amzDate := now.Format("20060102T150405Z")
	day := now.Format("20060102")
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	req.Host = host
	signed := "host;x-amz-content-sha256;x-amz-date"
	canonical := strings.Join([]string{
		req.Method,
		rawPath,
		rawQuery,
		"host:" + host + "\n" + "x-amz-content-sha256:" + payloadHash + "\n" + "x-amz-date:" + amzDate + "\n",
		signed,
		payloadHash,
	}, "\n")
	scope := day + "/" + c.region + "/s3/aws4_request"
	sum := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	key := hmacSHA256([]byte("AWS4"+c.cfg.SecretKey), day)
	key = hmacSHA256(key, c.region)
	key = hmacSHA256(key, "s3")
	key = hmacSHA256(key, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(key, toSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+c.cfg.AccessKey+"/"+scope+
		", SignedHeaders="+signed+", Signature="+sig)
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// escapePath applies SigV4 URI encoding to every segment, keeping '/'.
func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, part := range parts {
		parts[i] = uriEncode(part)
	}
	return strings.Join(parts, "/")
}

func canonicalQuery(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vals := append([]string(nil), q[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			parts = append(parts, uriEncode(k)+"="+uriEncode(v))
		}
	}
	return strings.Join(parts, "&")
}

func uriEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') ||
			ch == '-' || ch == '_' || ch == '.' || ch == '~' {
			b.WriteByte(ch)
			continue
		}
		b.WriteString("%" + strings.ToUpper(strconv.FormatUint(uint64(ch)|0x100, 16)[1:]))
	}
	return b.String()
}
