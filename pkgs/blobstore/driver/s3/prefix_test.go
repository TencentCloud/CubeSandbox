// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"encoding/xml"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/tencentcloud/CubeSandbox/pkgs/blobstore"
)

type prefixObject struct {
	Key          string
	ETag         string
	Size         int64
	LastModified string
}

type prefixUpload struct {
	Key       string
	UploadID  string `xml:"UploadId"`
	Initiated string
}

func prefixTestStore(t *testing.T, namespace, wantPrefix string, keys []string) (*store, <-chan string) {
	t.Helper()
	aborted := make(chan string, len(keys))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if r.Method == http.MethodDelete && query.Get("uploadId") != "" {
			aborted <- strings.TrimPrefix(r.URL.Path, "/test-bucket/")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodGet || (query.Get("list-type") != "2" && !query.Has("uploads")) {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		prefix := query.Get("prefix")
		if prefix != wantPrefix {
			t.Errorf("S3 request prefix = %q, want %q", prefix, wantPrefix)
		}
		w.Header().Set("Content-Type", "application/xml")
		if query.Has("uploads") {
			result := struct {
				XMLName     xml.Name       `xml:"ListMultipartUploadsResult"`
				Bucket      string         `xml:"Bucket"`
				IsTruncated bool           `xml:"IsTruncated"`
				Uploads     []prefixUpload `xml:"Upload"`
			}{Bucket: "test-bucket"}
			for _, key := range keys {
				if strings.HasPrefix(key, prefix) {
					result.Uploads = append(result.Uploads, prefixUpload{Key: key, UploadID: "stale-upload", Initiated: "2000-01-01T00:00:00Z"})
				}
			}
			if err := xml.NewEncoder(w).Encode(result); err != nil {
				t.Error(err)
			}
			return
		}
		result := struct {
			XMLName     xml.Name       `xml:"ListBucketResult"`
			Name        string         `xml:"Name"`
			IsTruncated bool           `xml:"IsTruncated"`
			Contents    []prefixObject `xml:"Contents"`
		}{Name: "test-bucket"}
		for _, key := range keys {
			if strings.HasPrefix(key, prefix) {
				result.Contents = append(result.Contents, prefixObject{Key: key, ETag: "test-etag", Size: 1, LastModified: "2000-01-01T00:00:00Z"})
			}
		}
		if err := xml.NewEncoder(w).Encode(result); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	client, err := newMinioClient(server.URL, "test-access-key", "test-secret-key", "us-east-1", minio.BucketLookupPath, "false")
	if err != nil {
		t.Fatal(err)
	}
	return &store{client: client, core: &minio.Core{Client: client}, bucket: "test-bucket", prefix: namespace}, aborted
}

func TestListAndGCPreserveDirectoryBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, namespace, prefix, requestPrefix string
		keys, want                             []string
	}{
		{"namespace", "tenant", "", "tenant/", []string{"tenant/item", "tenant-old/item"}, []string{"tenant/item"}},
		{"directory", "tenant", "uploads/", "tenant/uploads/", []string{"tenant/uploads/item", "tenant/uploads-old/item"}, []string{"tenant/uploads/item"}},
		{"root directory", "", "uploads/", "uploads/", []string{"uploads/item", "uploads-old/item"}, []string{"uploads/item"}},
		{"text prefix", "tenant", "uploads", "tenant/uploads", []string{"tenant/uploads/item", "tenant/uploads-old/item"}, []string{"tenant/uploads/item", "tenant/uploads-old/item"}},
		{"whole bucket", "", "", "", []string{"tenant/item", "tenant-old/item"}, []string{"tenant/item", "tenant-old/item"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, aborted := prefixTestStore(t, tc.namespace, tc.requestPrefix, tc.keys)
			var listed []string
			if err := s.List(t.Context(), tc.prefix, func(info blobstore.ObjectInfo) error {
				key := info.Key
				if tc.namespace != "" {
					key = tc.namespace + "/" + key
				}
				listed = append(listed, key)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(listed, tc.want) {
				t.Fatalf("listed keys = %v, want %v", listed, tc.want)
			}
			result, err := s.GC(t.Context(), blobstore.GCOptions{Prefix: tc.prefix, Now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
			if err != nil {
				t.Fatal(err)
			}
			var removed []string
			for len(aborted) > 0 {
				removed = append(removed, <-aborted)
			}
			if !slices.Equal(removed, tc.want) || result.IncompleteAborted != len(tc.want) {
				t.Fatalf("aborted keys = %v, count = %d, want %v", removed, result.IncompleteAborted, tc.want)
			}
		})
	}
}

func TestGCRejectsInvalidPrefixBeforeListing(t *testing.T) {
	s := &store{}
	for _, prefix := range []string{"/", "../outside"} {
		if _, err := s.GC(t.Context(), blobstore.GCOptions{Prefix: prefix}); !errors.Is(err, blobstore.ErrInvalidKey) {
			t.Errorf("GC(%q): got %v, want ErrInvalidKey", prefix, err)
		}
	}
}
