// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeS3Server is a loopback S3-compatible HTTP server for contract testing
// SigV4 signing, path/virtual addressing, Put/Get/Head/ListV2/Delete/Copy,
// and multipart upload + abort flows.
type fakeS3Server struct {
	mu              sync.Mutex
	bucket          string
	objects         map[string]*fakeS3Object
	uploads         map[string]*fakeS3Multipart
	abortedUploads  []string
	listPageSize    int
	truncateNoToken bool
	expiredOnce     bool
	expiredHits     int
	redirectOnce    bool
	requests        []fakeS3RequestRecord
	requireToken    bool
	expectedAK      string
	expectedToken   string
	errorEchoSecret string
}

type fakeS3Object struct {
	data        []byte
	metadata    map[string]string
	contentType string
	modTime     time.Time
}

type fakeS3Multipart struct {
	key         string
	metadata    map[string]string
	contentType string
	parts       map[int][]byte
}

type fakeS3RequestRecord struct {
	Method       string
	Host         string
	Path         string
	RawQuery     string
	AuthHeader   string
	SessionToken string
	ContentHash  string
}

func newFakeS3Server(t *testing.T, bucket string) (*fakeS3Server, *httptest.Server) {
	t.Helper()
	fs := &fakeS3Server{
		bucket:  bucket,
		objects: make(map[string]*fakeS3Object),
		uploads: make(map[string]*fakeS3Multipart),
	}
	srv := httptest.NewServer(http.HandlerFunc(fs.serveHTTP))
	t.Cleanup(srv.Close)
	return fs, srv
}

func (f *fakeS3Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	auth := r.Header.Get("Authorization")
	sessionTok := r.Header.Get("X-Amz-Security-Token")
	contentHash := r.Header.Get("X-Amz-Content-Sha256")
	f.requests = append(f.requests, fakeS3RequestRecord{
		Method:       r.Method,
		Host:         r.Host,
		Path:         r.URL.Path,
		RawQuery:     r.URL.RawQuery,
		AuthHeader:   auth,
		SessionToken: sessionTok,
		ContentHash:  contentHash,
	})

	if f.redirectOnce {
		f.redirectOnce = false
		w.Header().Set("Location", "http://169.254.169.254/latest/meta-data/")
		w.WriteHeader(http.StatusTemporaryRedirect)
		return
	}

	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") || r.Header.Get("X-Amz-Date") == "" || contentHash == "" {
		writeS3Error(w, http.StatusForbidden, "AccessDenied", "missing SigV4 headers")
		return
	}
	if f.expectedAK != "" && !strings.Contains(auth, "Credential="+f.expectedAK+"/") {
		writeS3Error(w, http.StatusForbidden, "InvalidAccessKeyId", "bad access key "+f.errorEchoSecret)
		return
	}
	if f.requireToken && sessionTok != f.expectedToken {
		writeS3Error(w, http.StatusForbidden, "InvalidToken", "bad session token "+f.errorEchoSecret)
		return
	}
	if f.expiredOnce && f.expiredHits == 0 {
		f.expiredHits++
		writeS3Error(w, http.StatusBadRequest, "ExpiredToken", "The provided token has expired "+f.errorEchoSecret)
		return
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeS3Error(w, http.StatusInternalServerError, "InternalError", "read body failed")
		return
	}
	actualSum := sha256.Sum256(bodyBytes)
	if hex.EncodeToString(actualSum[:]) != contentHash {
		writeS3Error(w, http.StatusBadRequest, "XAmzContentSHA256Mismatch", "payload hash mismatch")
		return
	}

	key, ok := f.extractKey(r)
	if !ok {
		writeS3Error(w, http.StatusNotFound, "NoSuchBucket", "bucket mismatch")
		return
	}

	q := r.URL.Query()
	switch {
	case r.Method == http.MethodGet && q.Get("list-type") == "2":
		f.handleListV2(w, q)
	case r.Method == http.MethodPost && q.Has("uploads"):
		f.handleCreateMultipart(w, r, key)
	case r.Method == http.MethodPut && q.Get("uploadId") != "" && q.Get("partNumber") != "":
		f.handleUploadPart(w, key, q.Get("uploadId"), q.Get("partNumber"), bodyBytes)
	case r.Method == http.MethodPost && q.Get("uploadId") != "":
		f.handleCompleteMultipart(w, r, key, q.Get("uploadId"), bodyBytes)
	case r.Method == http.MethodDelete && q.Get("uploadId") != "":
		f.handleAbortMultipart(w, q.Get("uploadId"))
	case r.Method == http.MethodPut && r.Header.Get("X-Amz-Copy-Source") != "":
		f.handleCopy(w, r, key)
	case r.Method == http.MethodPut:
		f.handlePut(w, r, key, bodyBytes)
	case r.Method == http.MethodHead:
		f.handleHead(w, key)
	case r.Method == http.MethodGet:
		f.handleGet(w, key)
	case r.Method == http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		writeS3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "unsupported")
	}
}

func (f *fakeS3Server) extractKey(r *http.Request) (string, bool) {
	// Support both path-style (/<bucket>/<key>) and virtual-hosted style (<bucket>.host/<key>).
	hostOnly := strings.Split(r.Host, ":")[0]
	if strings.HasPrefix(hostOnly, f.bucket+".") {
		return strings.TrimPrefix(r.URL.Path, "/"), true
	}
	trimmed := strings.TrimPrefix(r.URL.Path, "/")
	if trimmed == f.bucket {
		return "", true
	}
	if strings.HasPrefix(trimmed, f.bucket+"/") {
		return strings.TrimPrefix(trimmed, f.bucket+"/"), true
	}
	return "", false
}

func extractMetaHeaders(h http.Header) map[string]string {
	meta := make(map[string]string)
	for k, vals := range h {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-amz-meta-") && len(vals) > 0 {
			meta[strings.TrimPrefix(lk, "x-amz-meta-")] = vals[0]
		}
	}
	return meta
}

func (f *fakeS3Server) handlePut(w http.ResponseWriter, r *http.Request, key string, body []byte) {
	if r.Header.Get("If-None-Match") == "*" {
		if _, exists := f.objects[key]; exists {
			writeS3Error(w, http.StatusPreconditionFailed, "PreconditionFailed", "exists")
			return
		}
	}
	f.objects[key] = &fakeS3Object{
		data:        append([]byte(nil), body...),
		metadata:    extractMetaHeaders(r.Header),
		contentType: r.Header.Get("Content-Type"),
		modTime:     time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC),
	}
	w.Header().Set("ETag", `"etag-put"`)
	w.WriteHeader(http.StatusOK)
}

func (f *fakeS3Server) handleCopy(w http.ResponseWriter, r *http.Request, dstKey string) {
	srcHeader := strings.TrimPrefix(r.Header.Get("X-Amz-Copy-Source"), "/")
	parts := strings.SplitN(srcHeader, "/", 2)
	if len(parts) != 2 || parts[0] != f.bucket {
		writeS3Error(w, http.StatusNotFound, "NoSuchKey", "bad copy source")
		return
	}
	srcKey, err := decodePercentPath(parts[1])
	if err != nil {
		writeS3Error(w, http.StatusBadRequest, "InvalidArgument", "bad copy source encoding")
		return
	}
	src, ok := f.objects[srcKey]
	if !ok {
		writeS3Error(w, http.StatusNotFound, "NoSuchKey", "missing copy source")
		return
	}
	meta := src.metadata
	if strings.EqualFold(r.Header.Get("X-Amz-Metadata-Directive"), "REPLACE") {
		meta = extractMetaHeaders(r.Header)
	}
	f.objects[dstKey] = &fakeS3Object{
		data:        append([]byte(nil), src.data...),
		metadata:    meta,
		contentType: r.Header.Get("Content-Type"),
		modTime:     time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC),
	}
	w.Header().Set("Content-Type", "application/xml")
	fmt.Fprint(w, `<CopyObjectResult><ETag>"etag-copy"</ETag></CopyObjectResult>`)
}

func decodePercentPath(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			v, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
			if err != nil {
				return "", err
			}
			b.WriteByte(byte(v))
			i += 2
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String(), nil
}

func (f *fakeS3Server) handleHead(w http.ResponseWriter, key string) {
	obj, ok := f.objects[key]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	for k, v := range obj.metadata {
		w.Header().Set("X-Amz-Meta-"+k, v)
	}
	if obj.contentType != "" {
		w.Header().Set("Content-Type", obj.contentType)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(obj.data)))
	w.Header().Set("Last-Modified", obj.modTime.UTC().Format(http.TimeFormat))
	w.WriteHeader(http.StatusOK)
}

func (f *fakeS3Server) handleGet(w http.ResponseWriter, key string) {
	obj, ok := f.objects[key]
	if !ok {
		writeS3Error(w, http.StatusNotFound, "NoSuchKey", "key not found")
		return
	}
	for k, v := range obj.metadata {
		w.Header().Set("X-Amz-Meta-"+k, v)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(obj.data)))
	w.Header().Set("Last-Modified", obj.modTime.UTC().Format(http.TimeFormat))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(obj.data)
}

func (f *fakeS3Server) handleListV2(w http.ResponseWriter, q map[string][]string) {
	prefix := ""
	if vals := q["prefix"]; len(vals) > 0 {
		prefix = vals[0]
	}
	delimiter := ""
	if vals := q["delimiter"]; len(vals) > 0 {
		delimiter = vals[0]
	}
	maxKeys := f.listPageSize
	if vals := q["max-keys"]; len(vals) > 0 {
		if parsed, err := strconv.Atoi(vals[0]); err == nil && parsed > 0 {
			if maxKeys == 0 || parsed < maxKeys {
				maxKeys = parsed
			}
		}
	}
	startIdx := 0
	if vals := q["continuation-token"]; len(vals) > 0 && vals[0] != "" {
		if parsed, err := strconv.Atoi(vals[0]); err == nil && parsed >= 0 {
			startIdx = parsed
		}
	}

	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	type entry struct {
		isPrefix bool
		key      string
		obj      *fakeS3Object
	}
	seenPrefix := make(map[string]bool)
	var all []entry
	for _, k := range keys {
		rem := strings.TrimPrefix(k, prefix)
		if delimiter != "" {
			if idx := strings.Index(rem, delimiter); idx >= 0 {
				cp := prefix + rem[:idx+len(delimiter)]
				if !seenPrefix[cp] {
					seenPrefix[cp] = true
					all = append(all, entry{isPrefix: true, key: cp})
				}
				continue
			}
		}
		all = append(all, entry{key: k, obj: f.objects[k]})
	}

	if startIdx > len(all) {
		startIdx = len(all)
	}
	endIdx := len(all)
	truncated := false
	nextToken := ""
	if maxKeys > 0 && startIdx+maxKeys < len(all) {
		endIdx = startIdx + maxKeys
		truncated = true
		if !f.truncateNoToken {
			nextToken = strconv.Itoa(endIdx)
		}
	}

	var buf strings.Builder
	buf.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult>`)
	fmt.Fprintf(&buf, `<IsTruncated>%t</IsTruncated>`, truncated)
	fmt.Fprintf(&buf, `<KeyCount>%d</KeyCount>`, endIdx-startIdx)
	if nextToken != "" {
		fmt.Fprintf(&buf, `<NextContinuationToken>%s</NextContinuationToken>`, nextToken)
	}
	for _, item := range all[startIdx:endIdx] {
		if item.isPrefix {
			buf.WriteString(`<CommonPrefixes><Prefix>`)
			_ = xml.EscapeText(&buf, []byte(item.key))
			buf.WriteString(`</Prefix></CommonPrefixes>`)
		} else {
			buf.WriteString(`<Contents><Key>`)
			_ = xml.EscapeText(&buf, []byte(item.key))
			fmt.Fprintf(&buf, `</Key><Size>%d</Size><LastModified>%s</LastModified></Contents>`,
				len(item.obj.data), item.obj.modTime.UTC().Format(time.RFC3339))
		}
	}
	buf.WriteString(`</ListBucketResult>`)
	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write([]byte(buf.String()))
}

func (f *fakeS3Server) handleCreateMultipart(w http.ResponseWriter, r *http.Request, key string) {
	id := fmt.Sprintf("upload-%d", len(f.uploads)+1)
	f.uploads[id] = &fakeS3Multipart{
		key:         key,
		metadata:    extractMetaHeaders(r.Header),
		contentType: r.Header.Get("Content-Type"),
		parts:       make(map[int][]byte),
	}
	w.Header().Set("Content-Type", "application/xml")
	fmt.Fprintf(w, `<InitiateMultipartUploadResult><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, id)
}

func (f *fakeS3Server) handleUploadPart(w http.ResponseWriter, key, uploadID, partNumStr string, body []byte) {
	up, ok := f.uploads[uploadID]
	if !ok || up.key != key {
		writeS3Error(w, http.StatusNotFound, "NoSuchUpload", "missing upload")
		return
	}
	partNum, err := strconv.Atoi(partNumStr)
	if err != nil || partNum <= 0 {
		writeS3Error(w, http.StatusBadRequest, "InvalidArgument", "bad part number")
		return
	}
	up.parts[partNum] = append([]byte(nil), body...)
	w.Header().Set("ETag", fmt.Sprintf(`"part-%d"`, partNum))
	w.WriteHeader(http.StatusOK)
}

func (f *fakeS3Server) handleCompleteMultipart(w http.ResponseWriter, r *http.Request, key, uploadID string, body []byte) {
	up, ok := f.uploads[uploadID]
	if !ok || up.key != key {
		writeS3Error(w, http.StatusNotFound, "NoSuchUpload", "missing upload")
		return
	}
	if r.Header.Get("If-None-Match") == "*" {
		if _, exists := f.objects[key]; exists {
			writeS3Error(w, http.StatusPreconditionFailed, "PreconditionFailed", "exists")
			return
		}
	}
	var req s3CompleteMultipartRequest
	if err := xml.Unmarshal(body, &req); err != nil {
		writeS3Error(w, http.StatusBadRequest, "MalformedXML", "bad xml")
		return
	}
	var combined bytes.Buffer
	for _, p := range req.Parts {
		chunk, exists := up.parts[p.PartNumber]
		if !exists {
			writeS3Error(w, http.StatusBadRequest, "InvalidPart", "missing part")
			return
		}
		combined.Write(chunk)
	}
	f.objects[key] = &fakeS3Object{
		data:        combined.Bytes(),
		metadata:    up.metadata,
		contentType: up.contentType,
		modTime:     time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC),
	}
	delete(f.uploads, uploadID)
	w.Header().Set("Content-Type", "application/xml")
	fmt.Fprint(w, `<CompleteMultipartUploadResult><ETag>"etag-mp"</ETag></CompleteMultipartUploadResult>`)
}

func (f *fakeS3Server) handleAbortMultipart(w http.ResponseWriter, uploadID string) {
	delete(f.uploads, uploadID)
	f.abortedUploads = append(f.abortedUploads, uploadID)
	w.WriteHeader(http.StatusNoContent)
}

func writeS3Error(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	var b strings.Builder
	b.WriteString(`<Error><Code>`)
	_ = xml.EscapeText(&b, []byte(code))
	b.WriteString(`</Code><Message>`)
	_ = xml.EscapeText(&b, []byte(msg))
	b.WriteString(`</Message></Error>`)
	_, _ = w.Write([]byte(b.String()))
}

func newControlServerForCreds(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(srv.Close)
	return srv
}

func TestVolumeContentContractFullLifecycle(t *testing.T) {
	s3Srv, s3HTTP := newFakeS3Server(t, "tenant-bucket")
	s3Srv.expectedAK = "AKID-TEMP-1"
	s3Srv.requireToken = true
	s3Srv.expectedToken = "STS-TOK-1"

	var tokenCalls atomic.Int32
	controlSrv := newControlServerForCreds(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/volumes/vol-direct/content-token" {
			t.Fatalf("unexpected control request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer api-secret-key" {
			t.Fatalf("Authorization=%q", got)
		}
		body, _ := io.ReadAll(r.Body)
		if len(body) != 0 {
			t.Fatalf("content-token request must send no body, got %q", string(body))
		}
		tokenCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(VolumeContentCredentials{
			Provider:        "cos",
			Endpoint:        s3HTTP.URL,
			Bucket:          "tenant-bucket",
			Region:          "ap-guangzhou",
			Prefix:          "custom-tenant/subscope/", // intentionally NOT volumes/<volumeID>/
			AddressingStyle: "path",
			AccessKeyID:     "AKID-TEMP-1",
			SecretAccessKey: "SECRET-TEMP-1",
			SessionToken:    "STS-TOK-1",
			ExpiresAt:       time.Now().Add(10 * time.Minute).Unix(),
		})
	})

	client := NewClient(Config{APIURL: controlSrv.URL, APIKey: "api-secret-key"})
	vc, err := client.VolumeContent("vol-direct", WithVolumeAllowInsecureLoopback(true))
	if err != nil {
		t.Fatalf("VolumeContent: %v", err)
	}
	defer vc.Close()

	ctx := context.Background()

	// Write + special characters + metadata
	written, err := vc.Write(ctx, "/folder/a 空格/#中文.txt", []byte("hello volume"), VolumeWriteOptions{
		UID:  IntPtr(1000),
		GID:  IntPtr(2000),
		Mode: IntPtr(0o600),
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if written.Type != VolumeFileTypeFile || written.Size != 12 || written.Mode != 0o600 || written.UID != 1000 || written.GID != 2000 {
		t.Fatalf("written entry mismatch: %#v", written)
	}
	if _, ok := s3Srv.objects["custom-tenant/subscope/folder/a 空格/#中文.txt"]; !ok {
		t.Fatalf("object not stored under plugin-chosen prefix; keys=%v", s3Srv.objects)
	}

	// Read / ReadBytes / ReadStream
	text, err := vc.Read(ctx, "/folder/a 空格/#中文.txt")
	if err != nil || text != "hello volume" {
		t.Fatalf("Read=(%q, %v)", text, err)
	}
	raw, err := vc.ReadBytes(ctx, "/folder/a 空格/#中文.txt")
	if err != nil || string(raw) != "hello volume" {
		t.Fatalf("ReadBytes=(%q, %v)", string(raw), err)
	}
	rc, err := vc.ReadStream(ctx, "/folder/a 空格/#中文.txt")
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}
	streamed, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || string(streamed) != "hello volume" {
		t.Fatalf("ReadStream data=(%q, %v)", string(streamed), err)
	}

	// Force=false rejects overwriting existing file
	if _, err := vc.Write(ctx, "/folder/a 空格/#中文.txt", []byte("overwrite"), VolumeWriteOptions{Force: BoolPtr(false)}); !errors.Is(err, ErrVolumePathExists) {
		t.Fatalf("Write(force=false) err=%v, want ErrVolumePathExists", err)
	}

	// Stat & Exists on file, implicit dir, root, missing
	st, err := vc.Stat(ctx, "/folder/a 空格/#中文.txt")
	if err != nil || st.Size != 12 || st.Mode != 0o600 || st.UID != 1000 || st.GID != 2000 {
		t.Fatalf("Stat file=(%#v, %v)", st, err)
	}
	dirStat, err := vc.Stat(ctx, "/folder")
	if err != nil || !dirStat.IsDir() || dirStat.Path != "/folder" {
		t.Fatalf("Stat implicit dir=(%#v, %v)", dirStat, err)
	}
	rootStat, err := vc.Stat(ctx, "/")
	if err != nil || !rootStat.IsDir() || rootStat.Path != "/" {
		t.Fatalf("Stat root=(%#v, %v)", rootStat, err)
	}
	exists, err := vc.Exists(ctx, "/folder/a 空格/#中文.txt")
	if err != nil || !exists {
		t.Fatalf("Exists(file)=(%v, %v)", exists, err)
	}
	exists, err = vc.Exists(ctx, "/does-not-exist")
	if err != nil || exists {
		t.Fatalf("Exists(missing)=(%v, %v)", exists, err)
	}

	// MakeDir + Force=false + file collision
	mk, err := vc.MakeDir(ctx, "/empty-dir", VolumeMakeDirOptions{UID: IntPtr(500), Mode: IntPtr(0o700)})
	if err != nil || !mk.IsDir() || mk.Mode != 0o700 || mk.UID != 500 {
		t.Fatalf("MakeDir=(%#v, %v)", mk, err)
	}
	if _, err := vc.MakeDir(ctx, "/empty-dir", VolumeMakeDirOptions{Force: BoolPtr(false)}); !errors.Is(err, ErrVolumePathExists) {
		t.Fatalf("MakeDir existing with force=false err=%v, want ErrVolumePathExists", err)
	}
	if _, err := vc.MakeDir(ctx, "/folder/a 空格/#中文.txt", VolumeMakeDirOptions{}); !errors.Is(err, ErrVolumePathExists) {
		t.Fatalf("MakeDir over file err=%v, want ErrVolumePathExists", err)
	}

	// UpdateMetadata on file, explicit dir, implicit dir, missing
	upd, err := vc.UpdateMetadata(ctx, "/folder/a 空格/#中文.txt", VolumeUpdateMetadataOptions{Mode: IntPtr(0o640)})
	if err != nil || upd.Mode != 0o640 || upd.UID != 1000 || upd.GID != 2000 {
		t.Fatalf("UpdateMetadata file=(%#v, %v)", upd, err)
	}
	stAfter, err := vc.Stat(ctx, "/folder/a 空格/#中文.txt")
	if err != nil || stAfter.Mode != 0o640 || stAfter.UID != 1000 {
		t.Fatalf("Stat after UpdateMetadata=(%#v, %v)", stAfter, err)
	}
	updDir, err := vc.UpdateMetadata(ctx, "/folder", VolumeUpdateMetadataOptions{UID: IntPtr(42), Mode: IntPtr(0o750)})
	if err != nil || !updDir.IsDir() || updDir.UID != 42 || updDir.Mode != 0o750 {
		t.Fatalf("UpdateMetadata implicit dir=(%#v, %v)", updDir, err)
	}
	if _, err := vc.UpdateMetadata(ctx, "/missing", VolumeUpdateMetadataOptions{Mode: IntPtr(0o600)}); !errors.Is(err, ErrVolumePathNotFound) {
		t.Fatalf("UpdateMetadata missing err=%v, want ErrVolumePathNotFound", err)
	}

	// List with depth=0, depth=1, depth=2, and pagination
	s3Srv.listPageSize = 1
	list0, err := vc.List(ctx, "/", VolumeListOptions{Depth: IntPtr(0)})
	if err != nil || len(list0) != 0 {
		t.Fatalf("List(depth=0)=(%#v, %v)", list0, err)
	}
	list1, err := vc.List(ctx, "/", VolumeListOptions{Depth: IntPtr(1)})
	if err != nil {
		t.Fatalf("List(depth=1): %v", err)
	}
	if len(list1) != 2 || list1[0].Path != "/empty-dir" || list1[1].Path != "/folder" {
		t.Fatalf("List(depth=1)=%#v", list1)
	}
	list2, err := vc.List(ctx, "/", VolumeListOptions{Depth: IntPtr(2)})
	if err != nil {
		t.Fatalf("List(depth=2): %v", err)
	}
	if len(list2) != 3 || list2[2].Path != "/folder/a 空格" || !list2[2].IsDir() {
		t.Fatalf("List(depth=2)=%#v", list2)
	}
	list3, err := vc.List(ctx, "/", VolumeListOptions{Depth: IntPtr(3)})
	if err != nil {
		t.Fatalf("List(depth=3): %v", err)
	}
	if len(list3) != 4 || list3[3].Path != "/folder/a 空格/#中文.txt" || list3[3].IsDir() {
		t.Fatalf("List(depth=3)=%#v", list3)
	}
	s3Srv.listPageSize = 0

	// Remove semantics: non-empty dir -> ErrVolumeDirNotEmpty, empty dir -> deleted, missing -> ErrVolumePathNotFound
	if err := vc.Remove(ctx, "/folder"); !errors.Is(err, ErrVolumeDirNotEmpty) {
		t.Fatalf("Remove non-empty dir err=%v, want ErrVolumeDirNotEmpty", err)
	}
	if err := vc.Remove(ctx, "/empty-dir"); err != nil {
		t.Fatalf("Remove empty dir: %v", err)
	}
	if exists, err := vc.Exists(ctx, "/empty-dir"); err != nil || exists {
		t.Fatalf("empty-dir still exists after Remove: (%v, %v)", exists, err)
	}
	if err := vc.Remove(ctx, "/never-existed"); !errors.Is(err, ErrVolumePathNotFound) {
		t.Fatalf("Remove missing err=%v, want ErrVolumePathNotFound", err)
	}

	// RemoveAll recursively removes non-empty directory
	if err := vc.RemoveAll(ctx, "/folder"); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if exists, err := vc.Exists(ctx, "/folder"); err != nil || exists {
		t.Fatalf("/folder still exists after RemoveAll: (%v, %v)", exists, err)
	}
	if err := vc.RemoveAll(ctx, "/folder"); !errors.Is(err, ErrVolumePathNotFound) {
		t.Fatalf("RemoveAll already-deleted err=%v, want ErrVolumePathNotFound", err)
	}

	// Credentials were cached and fetched only once across all operations.
	if got := tokenCalls.Load(); got != 1 {
		t.Fatalf("expected 1 credential issuance call, got %d", got)
	}
}

func TestVolumeContentEmptyPrefixAndOptionalSessionToken(t *testing.T) {
	s3Srv, s3HTTP := newFakeS3Server(t, "root-bucket")
	s3Srv.expectedAK = "AK-NO-STS"
	s3Srv.requireToken = false

	controlSrv := newControlServerForCreds(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(VolumeContentCredentials{
			Provider:        "s3",
			Endpoint:        s3HTTP.URL,
			Bucket:          "root-bucket",
			Region:          "us-east-1",
			Prefix:          "", // empty prefix allowed
			AddressingStyle: "path",
			AccessKeyID:     "AK-NO-STS",
			SecretAccessKey: "SK-NO-STS",
			SessionToken:    "", // optional session token allowed
			ExpiresAt:       time.Now().Add(5 * time.Minute).Unix(),
		})
	})

	client := NewClient(Config{APIURL: controlSrv.URL})
	vc, err := client.VolumeContent("vol-1", WithVolumeAllowInsecureLoopback(true))
	if err != nil {
		t.Fatalf("VolumeContent: %v", err)
	}
	defer vc.Close()

	if _, err := vc.Write(context.Background(), "/hello.txt", []byte("ok"), VolumeWriteOptions{}); err != nil {
		t.Fatalf("Write with empty prefix and no session token: %v", err)
	}
	if _, ok := s3Srv.objects["hello.txt"]; !ok {
		t.Fatalf("expected object key 'hello.txt', got %v", s3Srv.objects)
	}
	if len(s3Srv.requests) == 0 || s3Srv.requests[0].SessionToken != "" {
		t.Fatalf("expected no X-Amz-Security-Token header when session_token is empty")
	}
}

func TestVolumeContentWriteFromStreamingAndMultipartAbort(t *testing.T) {
	s3Srv, s3HTTP := newFakeS3Server(t, "stream-bucket")
	controlSrv := newControlServerForCreds(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(VolumeContentCredentials{
			Provider:        "s3",
			Endpoint:        s3HTTP.URL,
			Bucket:          "stream-bucket",
			Region:          "us-east-1",
			Prefix:          "v/",
			AddressingStyle: "path",
			AccessKeyID:     "AK-STREAM",
			SecretAccessKey: "SK-STREAM",
			SessionToken:    "TOK-STREAM",
			ExpiresAt:       time.Now().Add(10 * time.Minute).Unix(),
		})
	})

	client := NewClient(Config{APIURL: controlSrv.URL})
	vc, err := client.VolumeContent("vol-stream",
		WithVolumeAllowInsecureLoopback(true),
		withVolumeMultipartChunkSize(16),
	)
	if err != nil {
		t.Fatalf("VolumeContent: %v", err)
	}
	defer vc.Close()

	ctx := context.Background()

	// 1. Small stream (< chunk size) -> single PutObject, reader not closed.
	smallData := []byte("small-payload")
	smallReader := &trackingReader{r: bytes.NewReader(smallData)}
	entry, err := vc.WriteFrom(ctx, "/small.bin", smallReader, VolumeWriteOptions{Force: BoolPtr(false)})
	if err != nil || entry.Size != int64(len(smallData)) {
		t.Fatalf("WriteFrom small=(%#v, %v)", entry, err)
	}
	if smallReader.closed {
		t.Fatal("WriteFrom must not close caller reader")
	}
	// Duplicate small with Force=false -> ErrVolumePathExists
	if _, err := vc.WriteFrom(ctx, "/small.bin", bytes.NewReader(smallData), VolumeWriteOptions{Force: BoolPtr(false)}); !errors.Is(err, ErrVolumePathExists) {
		t.Fatalf("WriteFrom duplicate force=false err=%v, want ErrVolumePathExists", err)
	}

	// 2. Large stream (> chunk size) -> Multipart upload across 3 parts.
	largeData := []byte("0123456789abcdef-part2-6789abcdef-tail")
	entry, err = vc.WriteFrom(ctx, "/large.bin", bytes.NewReader(largeData), VolumeWriteOptions{Mode: IntPtr(0o640)})
	if err != nil || entry.Size != int64(len(largeData)) {
		t.Fatalf("WriteFrom large=(%#v, %v)", entry, err)
	}
	got, err := vc.ReadBytes(ctx, "/large.bin")
	if err != nil || !bytes.Equal(got, largeData) {
		t.Fatalf("ReadBytes large mismatch: got=%q err=%v", string(got), err)
	}
	st, err := vc.Stat(ctx, "/large.bin")
	if err != nil || st.Mode != 0o640 {
		t.Fatalf("Stat large metadata=(%#v, %v)", st, err)
	}

	// 3. Large stream failing mid-way -> aborts multipart upload cleanly.
	boom := errors.New("disk read error")
	failingReader := io.MultiReader(bytes.NewReader(bytes.Repeat([]byte("a"), 20)), failingChunkReader{err: boom})
	if _, err := vc.WriteFrom(ctx, "/broken.bin", failingReader, VolumeWriteOptions{}); !errors.Is(err, boom) {
		t.Fatalf("WriteFrom failing reader err=%v, want %v", err, boom)
	}
	if len(s3Srv.abortedUploads) != 1 {
		t.Fatalf("expected 1 aborted multipart upload, got %v", s3Srv.abortedUploads)
	}
	if exists, err := vc.Exists(ctx, "/broken.bin"); err != nil || exists {
		t.Fatalf("broken upload should not be visible: (%v, %v)", exists, err)
	}
}

type trackingReader struct {
	r      io.Reader
	closed bool
}

func (tr *trackingReader) Read(p []byte) (int, error) { return tr.r.Read(p) }
func (tr *trackingReader) Close() error               { tr.closed = true; return nil }

type failingChunkReader struct{ err error }

func (f failingChunkReader) Read([]byte) (int, error) { return 0, f.err }

func TestVolumeContentValidationRejectsTraversalBadPrefixesAndBadOptions(t *testing.T) {
	client := newNoRequestClient(t)

	// Invalid volume ID rejected upfront.
	if _, err := client.VolumeContent("../escape"); err == nil {
		t.Fatal("expected error for invalid volumeID")
	}

	vc, err := client.VolumeContent("vol-valid", withVolumeStorageFactory(func(VolumeContentCredentials) (volumeStorage, error) {
		t.Fatal("storage factory must not be called for invalid inputs")
		return nil, nil
	}))
	if err != nil {
		t.Fatalf("VolumeContent: %v", err)
	}

	ctx := context.Background()
	badPaths := []string{"", "relative", "../escape", "/a/../b", "/a/./b", "/nul\x00", `/a\b`, "/line\nbreak"}
	for _, p := range badPaths {
		if _, err := vc.ReadBytes(ctx, p); err == nil {
			t.Fatalf("ReadBytes(%q) expected validation error", p)
		}
		if _, err := vc.Write(ctx, p, []byte("x"), VolumeWriteOptions{}); err == nil {
			t.Fatalf("Write(%q) expected validation error", p)
		}
		if _, err := vc.List(ctx, p, VolumeListOptions{}); err == nil {
			t.Fatalf("List(%q) expected validation error", p)
		}
		if _, err := vc.Stat(ctx, p); err == nil {
			t.Fatalf("Stat(%q) expected validation error", p)
		}
		if err := vc.Remove(ctx, p); err == nil {
			t.Fatalf("Remove(%q) expected validation error", p)
		}
	}

	// Root rejections and negative metadata/depth rejections.
	if _, err := vc.Read(ctx, "/"); err == nil {
		t.Fatal("Read(/) expected error")
	}
	if _, err := vc.Write(ctx, "/", []byte("x"), VolumeWriteOptions{}); err == nil {
		t.Fatal("Write(/) expected error")
	}
	if err := vc.Remove(ctx, "/"); err == nil {
		t.Fatal("Remove(/) expected error")
	}
	if err := vc.Remove(ctx, "///"); err == nil {
		t.Fatal("Remove(///) expected error")
	}
	if err := vc.RemoveAll(ctx, "/"); err == nil {
		t.Fatal("RemoveAll(/) expected error")
	}
	if _, err := vc.List(ctx, "/", VolumeListOptions{Depth: IntPtr(-1)}); err == nil {
		t.Fatal("List(depth=-1) expected error")
	}
	if _, err := vc.Write(ctx, "/a", []byte("x"), VolumeWriteOptions{UID: IntPtr(-1)}); err == nil {
		t.Fatal("Write(uid=-1) expected error")
	}
	if _, err := vc.Write(ctx, "/a", []byte("x"), VolumeWriteOptions{GID: IntPtr(-1)}); err == nil {
		t.Fatal("Write(gid=-1) expected error")
	}
	if _, err := vc.Write(ctx, "/a", []byte("x"), VolumeWriteOptions{Mode: IntPtr(-1)}); err == nil {
		t.Fatal("Write(mode=-1) expected error")
	}
	if _, err := vc.MakeDir(ctx, "/d", VolumeMakeDirOptions{Mode: IntPtr(-1)}); err == nil {
		t.Fatal("MakeDir(mode=-1) expected error")
	}
	if _, err := vc.UpdateMetadata(ctx, "/a", VolumeUpdateMetadataOptions{}); err == nil {
		t.Fatal("UpdateMetadata(empty) expected error")
	}
	if _, err := vc.WriteFrom(ctx, "/a", nil, VolumeWriteOptions{}); err == nil {
		t.Fatal("WriteFrom(nil) expected error")
	}
}

func TestValidateVolumeContentCredentialsAndSSRFPolicy(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	base := func() VolumeContentCredentials {
		return VolumeContentCredentials{
			Provider:        "s3",
			Endpoint:        "https://cos.ap-guangzhou.myqcloud.com",
			Bucket:          "my-bucket",
			Region:          "ap-guangzhou",
			Prefix:          "tenant/vol-a/",
			AddressingStyle: "auto",
			AccessKeyID:     "AKID",
			SecretAccessKey: "SECRET",
			SessionToken:    "TOKEN",
			ExpiresAt:       now.Add(5 * time.Minute).Unix(),
		}
	}

	// Valid baseline passes.
	c := base()
	if err := validateVolumeContentCredentials(&c, now, s3StoragePolicy{}); err != nil {
		t.Fatalf("valid creds rejected: %v", err)
	}

	badPrefixes := []string{"/leading/", "no-slash", "a/../b/", "a/./b/", "a//b/", `a\b/`, "a\x00/"}
	for _, p := range badPrefixes {
		c := base()
		c.Prefix = p
		if err := validateVolumeContentCredentials(&c, now, s3StoragePolicy{}); err == nil {
			t.Fatalf("expected prefix %q to be rejected", p)
		}
	}

	badProviders := []string{"", "gcs", "S3", "azure"}
	for _, prov := range badProviders {
		c := base()
		c.Provider = prov
		if err := validateVolumeContentCredentials(&c, now, s3StoragePolicy{}); err == nil {
			t.Fatalf("expected provider %q to be rejected", prov)
		}
	}

	badEndpoints := []string{
		"",
		"http://cos.ap-guangzhou.myqcloud.com",
		"https://user:pass@cos.ap-guangzhou.myqcloud.com",
		"https://cos.ap-guangzhou.myqcloud.com?foo=bar",
		"https://cos.ap-guangzhou.myqcloud.com#frag",
		"https://cos.ap-guangzhou.myqcloud.com/extra-path",
		"https://169.254.169.254",
		"https://127.0.0.1:9000",
		"https://10.0.0.5:9000",
		"https://localhost:9000",
		"ftp://cos.ap-guangzhou.myqcloud.com",
	}
	for _, ep := range badEndpoints {
		c := base()
		c.Endpoint = ep
		if err := validateVolumeContentCredentials(&c, now, s3StoragePolicy{}); err == nil {
			t.Fatalf("expected endpoint %q to be rejected under default policy", ep)
		}
	}

	// Endpoint allowlist enforces trusted destinations.
	c = base()
	if err := validateVolumeContentCredentials(&c, now, s3StoragePolicy{allowedEndpointHosts: []string{".amazonaws.com"}}); err == nil {
		t.Fatal("expected non-allowlisted endpoint to be rejected")
	}
	if err := validateVolumeContentCredentials(&c, now, s3StoragePolicy{allowedEndpointHosts: []string{".myqcloud.com"}}); err != nil {
		t.Fatalf("allowlisted suffix rejected: %v", err)
	}

	// Expired credentials rejected with ErrVolumeCredentialsExpired.
	c = base()
	c.ExpiresAt = now.Add(-1 * time.Second).Unix()
	if err := validateVolumeContentCredentials(&c, now, s3StoragePolicy{}); !errors.Is(err, ErrVolumeCredentialsExpired) {
		t.Fatalf("expired creds err=%v, want ErrVolumeCredentialsExpired", err)
	}

	// Addressing style validation.
	c = base()
	c.AddressingStyle = "bogus"
	if err := validateVolumeContentCredentials(&c, now, s3StoragePolicy{}); err == nil {
		t.Fatal("expected invalid addressing_style to be rejected")
	}

	// IP-level SSRF checks including IPv4-mapped IPv6 metadata address.
	for _, rawIP := range []string{"169.254.169.254", "::ffff:169.254.169.254", "10.0.0.1", "172.16.0.1", "192.168.1.1", "100.64.0.1", "127.0.0.1", "::1", "0.0.0.0"} {
		ip := netip.MustParseAddr(rawIP)
		if err := checkStorageIP(ip, s3StoragePolicy{}); err == nil {
			t.Fatalf("checkStorageIP(%s) allowed under default policy", rawIP)
		}
	}
	// Link-local metadata stays blocked even when private endpoints are enabled.
	if err := checkStorageIP(netip.MustParseAddr("169.254.169.254"), s3StoragePolicy{allowPrivateEndpoints: true, allowInsecureLoopback: true}); err == nil {
		t.Fatal("link-local metadata IP must always be blocked")
	}
}

func TestVolumeContentCredentialRefreshAndExpiredRetry(t *testing.T) {
	s3Srv, s3HTTP := newFakeS3Server(t, "refresh-bucket")
	s3Srv.expiredOnce = true

	var mu sync.Mutex
	currentTime := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	nowFn := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return currentTime
	}
	advance := func(d time.Duration) {
		mu.Lock()
		currentTime = currentTime.Add(d)
		mu.Unlock()
	}

	var issueCount atomic.Int32
	controlSrv := newControlServerForCreds(t, func(w http.ResponseWriter, r *http.Request) {
		n := issueCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(VolumeContentCredentials{
			Provider:        "s3",
			Endpoint:        s3HTTP.URL,
			Bucket:          "refresh-bucket",
			Region:          "us-east-1",
			Prefix:          "p/",
			AddressingStyle: "path",
			AccessKeyID:     fmt.Sprintf("AK-%d", n),
			SecretAccessKey: fmt.Sprintf("SK-%d", n),
			SessionToken:    fmt.Sprintf("TOK-%d", n),
			ExpiresAt:       nowFn().Add(10 * time.Minute).Unix(),
		})
	})

	client := NewClient(Config{APIURL: controlSrv.URL})
	vc, err := client.VolumeContent("vol-refresh",
		WithVolumeAllowInsecureLoopback(true),
		WithVolumeRefreshMargin(2*time.Minute),
		withVolumeClock(nowFn),
	)
	if err != nil {
		t.Fatalf("VolumeContent: %v", err)
	}
	defer vc.Close()

	// First Write hits ExpiredToken once from S3 -> automatically refreshes and succeeds.
	if _, err := vc.Write(context.Background(), "/a.txt", []byte("v1"), VolumeWriteOptions{}); err != nil {
		t.Fatalf("Write with ExpiredToken retry failed: %v", err)
	}
	if got := issueCount.Load(); got != 2 {
		t.Fatalf("expected 2 issuances (initial + retry), got %d", got)
	}

	// Within validity window (5m elapsed, 5m remaining > 2m margin) -> reuses cached credentials.
	advance(5 * time.Minute)
	if _, err := vc.Read(context.Background(), "/a.txt"); err != nil {
		t.Fatalf("Read cached: %v", err)
	}
	if got := issueCount.Load(); got != 2 {
		t.Fatalf("expected cached credentials to be reused, got %d issuances", got)
	}

	// Enter refresh window (8m30s elapsed, 1m30s remaining < 2m margin) -> proactively refreshes.
	advance(3*time.Minute + 30*time.Second)
	if _, err := vc.Read(context.Background(), "/a.txt"); err != nil {
		t.Fatalf("Read after proactive refresh: %v", err)
	}
	if got := issueCount.Load(); got != 3 {
		t.Fatalf("expected proactive refresh (3 issuances), got %d", got)
	}

	// Concurrent reads under -race must stay safe.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := vc.Read(context.Background(), "/a.txt"); err != nil {
				t.Errorf("concurrent Read failed: %v", err)
			}
		}()
	}
	wg.Wait()
}

func TestVolumeContentControlPlaneErrorsAndNoFallback(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		wantErr    error
	}{
		{
			name:       "unauthorized",
			statusCode: http.StatusUnauthorized,
			body:       `{"message":"invalid api key"}`,
			wantErr:    ErrAuthentication,
		},
		{
			name:       "volume not found",
			statusCode: http.StatusNotFound,
			body:       `{"message":"volume not found: vol-x"}`,
			wantErr:    ErrVolumeNotFound,
		},
		{
			name:       "unsupported driver hook",
			statusCode: http.StatusBadRequest,
			body:       `{"message":"volume driver has no direct-access credential hook: nfs"}`,
			wantErr:    ErrVolumeContentUnsupported,
		},
		{
			name:       "server without content-token endpoint",
			statusCode: http.StatusNotFound,
			body:       `{"message":"404 page not found"}`,
			wantErr:    ErrVolumeContentUnsupported,
		},
		{
			name:       "not implemented",
			statusCode: http.StatusNotImplemented,
			body:       `{"message":"not implemented"}`,
			wantErr:    ErrVolumeContentUnsupported,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var hits atomic.Int32
			srv := newControlServerForCreds(t, func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				if r.URL.Path != "/volumes/vol-x/content-token" {
					t.Fatalf("unexpected fallback request to %s", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.statusCode)
				fmt.Fprint(w, tt.body)
			})
			client := NewClient(Config{APIURL: srv.URL, APIKey: "secret-key"})
			vc, err := client.VolumeContent("vol-x")
			if err != nil {
				t.Fatalf("VolumeContent: %v", err)
			}
			_, err = vc.List(context.Background(), "/", VolumeListOptions{})
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err=%v, want %v", err, tt.wantErr)
			}
			if hits.Load() != 1 {
				t.Fatalf("expected 1 request with no silent fallback, got %d", hits.Load())
			}
		})
	}
}

func TestVolumeContentSecretFreeDiagnosticsAndRedirectRejection(t *testing.T) {
	creds := VolumeContentCredentials{
		Provider:        "s3",
		Endpoint:        "https://s3.example.test",
		Bucket:          "b",
		Region:          "us-east-1",
		Prefix:          "p/",
		AddressingStyle: "path",
		AccessKeyID:     "SUPER-SECRET-AK",
		SecretAccessKey: "SUPER-SECRET-SK",
		SessionToken:    "SUPER-SECRET-TOKEN",
		ExpiresAt:       1900000000,
	}
	for _, formatted := range []string{creds.String(), fmt.Sprintf("%v", creds), fmt.Sprintf("%+v", creds), fmt.Sprintf("%#v", creds)} {
		for _, secret := range []string{"SUPER-SECRET-AK", "SUPER-SECRET-SK", "SUPER-SECRET-TOKEN"} {
			if strings.Contains(formatted, secret) {
				t.Fatalf("credential secret %q leaked in %s", secret, formatted)
			}
		}
	}

	// Hostile S3 server echoing credentials & signed query params in error message.
	s3Srv, s3HTTP := newFakeS3Server(t, "sec-bucket")
	s3Srv.expectedAK = "OTHER-AK"
	s3Srv.errorEchoSecret = "SUPER-SECRET-AK SUPER-SECRET-SK SUPER-SECRET-TOKEN https://s3.example.test/k?X-Amz-Signature=deadbeef12345678&X-Amz-Security-Token=SUPER-SECRET-TOKEN"

	controlSrv := newControlServerForCreds(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(VolumeContentCredentials{
			Provider:        "s3",
			Endpoint:        s3HTTP.URL,
			Bucket:          "sec-bucket",
			Region:          "us-east-1",
			Prefix:          "p/",
			AddressingStyle: "path",
			AccessKeyID:     "SUPER-SECRET-AK",
			SecretAccessKey: "SUPER-SECRET-SK",
			SessionToken:    "SUPER-SECRET-TOKEN",
			ExpiresAt:       time.Now().Add(5 * time.Minute).Unix(),
		})
	})

	client := NewClient(Config{APIURL: controlSrv.URL})
	vc, err := client.VolumeContent("vol-sec", WithVolumeAllowInsecureLoopback(true))
	if err != nil {
		t.Fatalf("VolumeContent: %v", err)
	}
	defer vc.Close()

	_, err = vc.Read(context.Background(), "/secret.txt")
	if !errors.Is(err, ErrAuthentication) {
		t.Fatalf("err=%v, want ErrAuthentication", err)
	}
	msg := err.Error()
	for _, leaked := range []string{"SUPER-SECRET-AK", "SUPER-SECRET-SK", "SUPER-SECRET-TOKEN", "deadbeef12345678"} {
		if strings.Contains(msg, leaked) {
			t.Fatalf("secret %q leaked into error message: %s", leaked, msg)
		}
	}

	// Redirect from storage endpoint must be rejected, never followed.
	s3Srv.expectedAK = "SUPER-SECRET-AK"
	s3Srv.redirectOnce = true
	_, err = vc.Read(context.Background(), "/secret.txt")
	if err == nil || !strings.Contains(err.Error(), "redirects are not allowed") {
		t.Fatalf("expected redirect rejection error, got %v", err)
	}
}

func TestVolumeContentCancellationAndTruncatedListError(t *testing.T) {
	s3Srv, s3HTTP := newFakeS3Server(t, "cancel-bucket")
	controlSrv := newControlServerForCreds(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(VolumeContentCredentials{
			Provider:        "s3",
			Endpoint:        s3HTTP.URL,
			Bucket:          "cancel-bucket",
			Region:          "us-east-1",
			Prefix:          "p/",
			AddressingStyle: "path",
			AccessKeyID:     "AK",
			SecretAccessKey: "SK",
			ExpiresAt:       time.Now().Add(5 * time.Minute).Unix(),
		})
	})

	client := NewClient(Config{APIURL: controlSrv.URL})
	vc, err := client.VolumeContent("vol-cancel", WithVolumeAllowInsecureLoopback(true))
	if err != nil {
		t.Fatalf("VolumeContent: %v", err)
	}
	defer vc.Close()

	// Pre-cancelled context short-circuits immediately.
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := vc.List(cancelledCtx, "/", VolumeListOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("List with cancelled ctx err=%v, want context.Canceled", err)
	}

	// Truncated ListObjectsV2 without continuation token fails explicitly instead of looping.
	_, _ = vc.Write(context.Background(), "/a.txt", []byte("a"), VolumeWriteOptions{})
	_, _ = vc.Write(context.Background(), "/b.txt", []byte("b"), VolumeWriteOptions{})
	s3Srv.listPageSize = 1
	s3Srv.truncateNoToken = true
	if _, err := vc.List(context.Background(), "/", VolumeListOptions{}); err == nil || !strings.Contains(err.Error(), "continuation token") {
		t.Fatalf("expected missing continuation token error, got %v", err)
	}
}

func TestVolumeContentVirtualHostedAddressingStyle(t *testing.T) {
	var capturedHost, capturedPath, capturedAuth string
	storageHTTP := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			capturedHost = req.Host
			capturedPath = req.URL.EscapedPath()
			capturedAuth = req.Header.Get("Authorization")
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"ETag": []string{`"ok"`}},
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    req,
			}, nil
		}),
	}
	controlSrv := newControlServerForCreds(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(VolumeContentCredentials{
			Provider:        "cos",
			Endpoint:        "https://cos.ap-guangzhou.myqcloud.com",
			Bucket:          "my-bucket-1250000000",
			Region:          "ap-guangzhou",
			Prefix:          "volumes/v1/",
			AddressingStyle: "virtual",
			AccessKeyID:     "AK-V",
			SecretAccessKey: "SK-V",
			SessionToken:    "TOK-V",
			ExpiresAt:       time.Now().Add(5 * time.Minute).Unix(),
		})
	})

	client := NewClient(Config{APIURL: controlSrv.URL})
	vc, err := client.VolumeContent("v1", WithVolumeStorageHTTPClient(storageHTTP))
	if err != nil {
		t.Fatalf("VolumeContent: %v", err)
	}
	defer vc.Close()

	if _, err := vc.Write(context.Background(), "/dir/file 1.txt", []byte("hi"), VolumeWriteOptions{}); err != nil {
		t.Fatalf("Write virtual-hosted: %v", err)
	}
	if capturedHost != "my-bucket-1250000000.cos.ap-guangzhou.myqcloud.com" {
		t.Fatalf("capturedHost=%q", capturedHost)
	}
	if capturedPath != "/volumes/v1/dir/file%201.txt" {
		t.Fatalf("capturedPath=%q", capturedPath)
	}
	if !strings.HasPrefix(capturedAuth, "AWS4-HMAC-SHA256 Credential=AK-V/") {
		t.Fatalf("capturedAuth=%q", capturedAuth)
	}
}
