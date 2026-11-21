// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/checkpoint"
)

// retireStore is a path-style bucket without DeleteObjects that can be told
// to fail deletes.
type retireStore struct {
	mu      sync.Mutex
	objects map[string]bool
	failDel bool
}

func (s *retireStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/b"), "/")
	switch {
	case r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2":
		type content struct {
			Key string `xml:"Key"`
		}
		res := struct {
			XMLName  xml.Name  `xml:"ListBucketResult"`
			Contents []content `xml:"Contents"`
		}{}
		var keys []string
		for k := range s.objects {
			if strings.HasPrefix(k, r.URL.Query().Get("prefix")) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			res.Contents = append(res.Contents, content{Key: k})
		}
		_ = xml.NewEncoder(w).Encode(res)
	case r.Method == http.MethodPost && r.URL.Query().Has("delete"):
		// No batch delete: the client falls back to single deletes.
		http.Error(w, "not implemented", http.StatusNotImplemented)
	case r.Method == http.MethodDelete:
		if s.failDel {
			http.Error(w, "unavailable", http.StatusBadRequest)
			return
		}
		delete(s.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "unexpected", http.StatusBadRequest)
	}
}

func TestXFSS3RetirementSurvivesAFailedDelete(t *testing.T) {
	const snap = "snap-retire000000000000000001"
	store := &retireStore{objects: map[string]bool{
		"ckpt/" + snap + "/candidate.json": true,
		"ckpt/" + snap + "/s-a/c/0123":     true,
		"ckpt/snap-other/candidate.json":   true,
	}, failDel: true}
	srv := httptest.NewServer(store)
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	prevDir, prevNew, prevVal := xfss3RetireDir, xfss3NewClient, xfss3ClientVal
	t.Cleanup(func() { xfss3RetireDir, xfss3NewClient, xfss3ClientVal = prevDir, prevNew, prevVal })
	xfss3RetireDir = func() string { return dir }
	xfss3ClientVal = nil
	xfss3NewClient = func() (*checkpoint.Client, error) {
		return checkpoint.NewClient(checkpoint.Config{Endpoint: srv.URL, Bucket: "b", AccessKey: "a", SecretKey: "s",
			PathStyle: true, Prefix: "ckpt"})
	}

	if err := RetireXFSS3Checkpoint(context.Background(), snap); err == nil {
		t.Fatal("a failed remote delete must be reported")
	}
	if _, err := os.Stat(filepath.Join(dir, snap)); err != nil {
		t.Fatalf("the retirement intent must survive a failed delete: %v", err)
	}

	// The store comes back; the retry loop finishes the job.
	store.mu.Lock()
	store.failDel = false
	store.mu.Unlock()
	RetryXFSS3Retirements(context.Background())
	if _, err := os.Stat(filepath.Join(dir, snap)); !os.IsNotExist(err) {
		t.Fatalf("intent left after a successful delete: %v", err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	for k := range store.objects {
		if strings.Contains(k, snap) {
			t.Fatalf("object %s survived the retirement", k)
		}
	}
	if !store.objects["ckpt/snap-other/candidate.json"] {
		t.Fatal("another checkpoint was deleted")
	}
}
