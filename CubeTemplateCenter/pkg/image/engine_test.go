// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package image

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
)

type fakeEngineClient struct {
	inspect func(context.Context, string) (*dockerInspectImage, error)
	pull    func(context.Context, string, string) (io.ReadCloser, error)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func (c *fakeEngineClient) ImageInspect(ctx context.Context, imageRef string) (*dockerInspectImage, error) {
	return c.inspect(ctx, imageRef)
}

func (c *fakeEngineClient) ImagePull(ctx context.Context, imageRef, registryAuth string) (io.ReadCloser, error) {
	return c.pull(ctx, imageRef, registryAuth)
}

func withEngineClient(t *testing.T, cli engineClient, err error) {
	t.Helper()
	orig := newEngineClient
	newEngineClient = func() (engineClient, error) {
		return cli, err
	}
	t.Cleanup(func() {
		newEngineClient = orig
	})
}

func TestEnginePullParserAggregatesBytesAndLayers(t *testing.T) {
	parser := newEnginePullParser()
	if _, ok := parser.feed(enginePullEvent{ID: "layer-a", Status: "Downloading", ProgressDetail: enginePullProgressDetail{Current: 25, Total: 100}}); !ok {
		t.Fatal("expected first layer update")
	}
	got, ok := parser.feed(enginePullEvent{ID: "layer-b", Status: "Download complete", ProgressDetail: enginePullProgressDetail{Current: 40, Total: 40}})
	if !ok {
		t.Fatal("expected second layer update")
	}
	if got.TotalBytes != 140 || got.DownloadedBytes != 65 {
		t.Fatalf("bytes mismatch: %+v", got)
	}
	if got.TotalLayers != 2 || got.CompletedLayers != 1 {
		t.Fatalf("layers mismatch: %+v", got)
	}
	if got.Percent <= 46 || got.Percent >= 47 {
		t.Fatalf("percent=%f want about 46.4", got.Percent)
	}
}

func TestEnginePullParserFallsBackToLayerPercentWithoutTotals(t *testing.T) {
	parser := newEnginePullParser()
	_, _ = parser.feed(enginePullEvent{ID: "layer-a", Status: "Downloading"})
	got, _ := parser.feed(enginePullEvent{ID: "layer-b", Status: "Already exists"})
	if got.TotalBytes != 0 || got.DownloadedBytes != 0 {
		t.Fatalf("unexpected byte counters without totals: %+v", got)
	}
	if got.TotalLayers != 2 || got.CompletedLayers != 1 || got.Percent != 50 {
		t.Fatalf("layer fallback mismatch: %+v", got)
	}
}

func TestStreamEnginePullProgressReportsStreamError(t *testing.T) {
	stream := strings.NewReader(`{"errorDetail":{"message":"pull access denied"}}` + "\n")
	var updates []PullProgress
	err := streamEnginePullProgress(stream, func(p PullProgress) { updates = append(updates, p) })
	if err == nil || !strings.Contains(err.Error(), "pull access denied") {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(updates) != 0 {
		t.Fatalf("error stream should not emit progress: %+v", updates)
	}
}

func TestEngineRegistryAuthEncodesCredentialsWithoutPlaintext(t *testing.T) {
	auth, err := engineRegistryAuth(SourceSpec{
		ImageRef:         "registry.example.com/ns/app:tag",
		RegistryUsername: "alice",
		RegistryPassword: "s3cret",
	})
	if err != nil {
		t.Fatalf("engineRegistryAuth: %v", err)
	}
	if strings.Contains(auth, "s3cret") || strings.Contains(auth, "alice") {
		t.Fatalf("encoded auth leaked plaintext: %s", auth)
	}
	raw, err := base64.URLEncoding.DecodeString(auth)
	if err != nil {
		t.Fatalf("decode auth: %v", err)
	}
	var payload map[string]string
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal auth: %v", err)
	}
	if payload["username"] != "alice" || payload["password"] != "s3cret" || payload["serveraddress"] != "registry.example.com" {
		t.Fatalf("auth payload mismatch: %+v", payload)
	}
}

func TestHTTPDockerEngineClientImageInspectEscapesImageRefPath(t *testing.T) {
	var gotPath string
	cli := &httpDockerEngineClient{
		baseURL: "http://docker",
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			gotPath = req.URL.EscapedPath()
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"RepoDigests":["registry.example.com/ns/app@sha256:test"],"Config":{"Cmd":["run"]}}`)),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		})},
	}

	inspect, err := cli.ImageInspect(context.Background(), "registry.example.com/ns/app:tag")
	if err != nil {
		t.Fatalf("ImageInspect: %v", err)
	}
	if gotPath != "/images/registry.example.com%2Fns%2Fapp:tag/json" {
		t.Fatalf("escaped path=%q", gotPath)
	}
	if len(inspect.RepoDigests) != 1 || len(inspect.Config.Cmd) != 1 {
		t.Fatalf("unexpected inspect result: %+v", inspect)
	}
}

func TestNewHTTPDockerEngineClientUsesDedicatedHTTPClient(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:2375")

	cli, err := newHTTPDockerEngineClient()
	if err != nil {
		t.Fatalf("newHTTPDockerEngineClient: %v", err)
	}
	httpCli, ok := cli.(*httpDockerEngineClient)
	if !ok {
		t.Fatalf("client type=%T want *httpDockerEngineClient", cli)
	}
	if httpCli.client == http.DefaultClient {
		t.Fatalf("engine client must not share http.DefaultClient")
	}
	if httpCli.client.Timeout != 0 {
		t.Fatalf("client timeout=%v want no whole-request timeout", httpCli.client.Timeout)
	}
	if httpCli.client.Transport == nil {
		t.Fatalf("engine client should still configure transport-level timeouts")
	}
}

func TestHTTPDockerEngineClientImageInspectMapsNotFound(t *testing.T) {
	cli := &httpDockerEngineClient{
		baseURL: "http://docker",
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Body:       io.NopCloser(strings.NewReader("not found")),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		})},
	}

	_, err := cli.ImageInspect(context.Background(), "registry.example.com/ns/missing:tag")
	if !errors.Is(err, errEngineImageNotFound) {
		t.Fatalf("ImageInspect error=%v, want errEngineImageNotFound", err)
	}
}

func TestPrepareDockerSourceWithEngineReusesClientForPullFlow(t *testing.T) {
	withExecutableLookPath(t, func(file string) (string, error) {
		return "", errors.New("not found")
	})

	orig := newEngineClient
	defer func() {
		newEngineClient = orig
	}()

	constructCalls := 0
	inspectCalls := 0
	pullCalled := false
	newEngineClient = func() (engineClient, error) {
		constructCalls++
		return &fakeEngineClient{
			inspect: func(_ context.Context, imageRef string) (*dockerInspectImage, error) {
				inspectCalls++
				if inspectCalls == 1 {
					return nil, errEngineImageNotFound
				}
				return &dockerInspectImage{RepoDigests: []string{imageRef + "@sha256:pulled"}}, nil
			},
			pull: func(context.Context, string, string) (io.ReadCloser, error) {
				pullCalled = true
				return io.NopCloser(strings.NewReader(`{"id":"a","status":"Pull complete","progressDetail":{"current":10,"total":10}}` + "\n")), nil
			},
		}, nil
	}

	got, err := prepareDockerSource(context.Background(), SourceSpec{ImageRef: "docker.io/library/nginx:latest"})
	if err != nil {
		t.Fatalf("prepareDockerSource: %v", err)
	}
	if constructCalls != 1 {
		t.Fatalf("newEngineClient calls=%d want 1", constructCalls)
	}
	if !pullCalled || inspectCalls != 2 {
		t.Fatalf("pullCalled=%v inspectCalls=%d", pullCalled, inspectCalls)
	}
	if got.Digest != "sha256:pulled" {
		t.Fatalf("unexpected digest: %s", got.Digest)
	}
}

func TestPrepareDockerSourceUsesEngineWhenImageExists(t *testing.T) {
	withExecutableLookPath(t, func(file string) (string, error) {
		return "", errors.New("not found")
	})
	withEngineClient(t, &fakeEngineClient{
		inspect: func(_ context.Context, imageRef string) (*dockerInspectImage, error) {
			return &dockerInspectImage{
				RepoDigests: []string{imageRef + "@sha256:engine"},
				Config:      DockerImageConfig{Cmd: []string{"run"}},
			}, nil
		},
		pull: func(context.Context, string, string) (io.ReadCloser, error) {
			t.Fatal("pull should be skipped for existing engine image")
			return nil, nil
		},
	}, nil)

	got, err := prepareDockerSource(context.Background(), SourceSpec{ImageRef: "docker.io/library/nginx:latest"})
	if err != nil {
		t.Fatalf("prepareDockerSource: %v", err)
	}
	if got.Digest != "sha256:engine" || len(got.Config.Cmd) != 1 {
		t.Fatalf("unexpected source: %+v", got)
	}
}

func TestPrepareDockerSourceUsesEnginePullAfterMiss(t *testing.T) {
	withExecutableLookPath(t, func(file string) (string, error) {
		return "", errors.New("not found")
	})
	inspectCalls := 0
	pullCalled := false
	withEngineClient(t, &fakeEngineClient{
		inspect: func(_ context.Context, imageRef string) (*dockerInspectImage, error) {
			inspectCalls++
			if inspectCalls == 1 {
				return nil, errEngineImageNotFound
			}
			return &dockerInspectImage{RepoDigests: []string{imageRef + "@sha256:pulled"}}, nil
		},
		pull: func(context.Context, string, string) (io.ReadCloser, error) {
			pullCalled = true
			return io.NopCloser(strings.NewReader(`{"id":"a","status":"Pull complete","progressDetail":{"current":10,"total":10}}` + "\n")), nil
		},
	}, nil)

	got, err := prepareDockerSource(context.Background(), SourceSpec{ImageRef: "docker.io/library/nginx:latest"})
	if err != nil {
		t.Fatalf("prepareDockerSource: %v", err)
	}
	if !pullCalled || inspectCalls != 2 {
		t.Fatalf("pullCalled=%v inspectCalls=%d", pullCalled, inspectCalls)
	}
	if got.Digest != "sha256:pulled" {
		t.Fatalf("unexpected digest: %s", got.Digest)
	}
}

func TestPrepareDockerSourceFallsBackToCLIWhenEnginePullFails(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	withExecutableLookPath(t, func(file string) (string, error) {
		return "", errors.New("not found")
	})
	withEngineClient(t, &fakeEngineClient{
		inspect: func(context.Context, string) (*dockerInspectImage, error) {
			return nil, errEngineImageNotFound
		},
		pull: func(context.Context, string, string) (io.ReadCloser, error) {
			return nil, errors.New("compat api pull failed")
		},
	}, nil)

	inspectCalls := 0
	pullCalled := false
	inspectPayload := `[{"RepoDigests":["docker.io/library/nginx@sha256:cli"],"Config":{"Cmd":["nginx"]}}]`
	patches.ApplyFunc(dockerOutput, func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
		if len(args) == 4 && args[0] == "image" && args[1] == "inspect" && args[2] == "--" && args[3] == "docker.io/library/nginx:latest" {
			inspectCalls++
			if inspectCalls == 1 {
				return nil, errors.New("No such image")
			}
			return []byte(inspectPayload), nil
		}
		if len(args) == 3 && args[0] == "pull" && args[1] == "--" && args[2] == "docker.io/library/nginx:latest" {
			pullCalled = true
			return nil, nil
		}
		t.Fatalf("unexpected dockerOutput args=%v", args)
		return nil, nil
	})

	got, err := prepareDockerSource(context.Background(), SourceSpec{ImageRef: "docker.io/library/nginx:latest"})
	if err != nil {
		t.Fatalf("prepareDockerSource: %v", err)
	}
	if !pullCalled || inspectCalls != 2 {
		t.Fatalf("pullCalled=%v inspectCalls=%d", pullCalled, inspectCalls)
	}
	if got.Digest != "sha256:cli" {
		t.Fatalf("unexpected digest: %s", got.Digest)
	}
}
