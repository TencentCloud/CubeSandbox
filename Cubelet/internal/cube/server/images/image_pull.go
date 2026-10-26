// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package images

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/diff"
	containerdimages "github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/containerd/v2/core/remotes/docker/config"
	snpkg "github.com/containerd/containerd/v2/pkg/snapshotters"
	"github.com/containerd/containerd/v2/pkg/tracing"
	"github.com/containerd/errdefs"
	"github.com/containerd/imgcrypt/v2"
	"github.com/containerd/imgcrypt/v2/images/encryption"
	distribution "github.com/distribution/reference"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/annotations"
	criconfig "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/config"
	crilabels "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/labels"
	"github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/util"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

func (c *GRPCCRIImageService) PullImage(ctx context.Context, r *runtime.PullImageRequest) (_ *runtime.PullImageResponse, err error) {

	imageRef := r.GetImage().GetImage()

	credentials := func(host string) (string, string, error) {
		hostauth := r.GetAuth()
		if hostauth == nil {
			config := c.config.Registry.Configs[host]
			if config.Auth != nil {
				hostauth = toRuntimeAuthConfig(*config.Auth)
			}
		}
		return ParseAuth(hostauth, host)
	}

	ref, err := c.CubeImageService.PullImage(ctx, imageRef, credentials, r.SandboxConfig)
	if err != nil {
		return nil, err
	}
	return &runtime.PullImageResponse{ImageRef: ref}, nil
}

type PullImageOption struct {
	Credentials   func(string) (string, string, error)
	SandboxConfig *runtime.PodSandboxConfig

	UpdateClientFn config.UpdateClientFunc
}

type imageLock struct {
	sync.Mutex
	refCount int32
}

func (c *CubeImageService) PullRegistryImage(ctx context.Context, name string, opts *PullImageOption) (_ string, err error) {
	start := time.Now()
	ctx = constants.WithStartPullImageTime(ctx, &start)
	defer func() {
		workflow.RecordCreateMetricIfGreaterThan(ctx, err, "CubeImageService.PullRegistryImage", time.Since(start), time.Millisecond)
	}()
	if opts != nil && opts.Credentials != nil {
		u, p, e := opts.Credentials(name)
		if e != nil {
			log.G(ctx).Errorf("get image credentials failed: %v", e)
			return "", e
		}
		ctx = constants.WithImageCredentials(ctx, &runtime.AuthConfig{
			Username: u,
			Password: p,
		})
	}

	lock, _ := c.pullImageLockMap.LoadOrStore(name, &imageLock{})
	ilock := lock.(*imageLock)
	atomic.AddInt32(&ilock.refCount, 1)
	ilock.Mutex.Lock()
	defer func() {
		ilock.Unlock()
		if atomic.AddInt32(&ilock.refCount, -1) <= 0 {
			c.pullImageLockMap.Delete(name)
		}
	}()

	var (
		credentials   = opts.Credentials
		sandboxConfig = opts.SandboxConfig

		updateClientFn = opts.UpdateClientFn
	)

	span := tracing.SpanFromContext(ctx)
	defer func() {

		if err != nil {
			imagePulls.WithValues("failure").Inc()
		} else {
			imagePulls.WithValues("success").Inc()
		}
	}()

	inProgressImagePulls.Inc()
	defer func() {
		inProgressImagePulls.Dec()
	}()
	startTime := time.Now()

	if credentials == nil {
		credentials = func(host string) (string, string, error) {
			var hostauth *runtime.AuthConfig

			config := c.config.Registry.Configs[host]
			if config.Auth != nil {
				hostauth = toRuntimeAuthConfig(*config.Auth)

			}

			return ParseAuth(hostauth, host)
		}
	}

	namedRef, err := distribution.ParseDockerRef(name)
	if err != nil {
		return "", fmt.Errorf("failed to parse image reference %q: %w", name, err)
	}
	ref := namedRef.String()
	if ref != name {
		log.G(ctx).Debugf("PullImage using normalized image ref: %q", ref)
	}

	imagePullProgressTimeout, err := time.ParseDuration(c.config.ImagePullProgressTimeout)
	if err != nil {
		return "", fmt.Errorf("failed to parse image_pull_progress_timeout %q: %w", c.config.ImagePullProgressTimeout, err)
	}

	var (
		pctx, pcancel = context.WithCancel(ctx)

		pullReporter = newPullProgressReporter(ref, pcancel, imagePullProgressTimeout)

		resolver = docker.NewResolver(docker.ResolverOptions{
			Headers: c.config.Registry.Headers,
			Hosts: c.registryHosts(ctx, credentials, func(client *http.Client) error {

				if updateClientFn != nil {
					err = updateClientFn(client)
					if err != nil {
						return err
					}
				}
				return pullReporter.optionUpdateClient(client)
			}),
		})
		isSchema1    bool
		imageHandler containerdimages.HandlerFunc = func(_ context.Context,
			desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
			if desc.MediaType == containerdimages.MediaTypeDockerSchema1Manifest {
				isSchema1 = true
			}
			return nil, nil
		}
	)

	defer pcancel()
	snapshotter, err := c.snapshotterFromPodSandboxConfig(ctx, ref, sandboxConfig)
	if err != nil {
		return "", fmt.Errorf("failed to get snapshotter from pod sandbox config: %w", err)
	}
	log.G(ctx).Debugf("PullImage %s with snapshotter %s", ref, snapshotter)
	span.SetAttributes(
		tracing.Attribute("image.ref", ref),
		tracing.Attribute("snapshotter.name", snapshotter),
	)

	labels := c.getLabels(ctx, ref)

	pullOpts := []containerd.RemoteOpt{
		containerd.WithResolver(resolver),
		containerd.WithPullSnapshotter(snapshotter),
		containerd.WithPullUnpack,
		containerd.WithPullLabels(labels),
		containerd.WithMaxConcurrentDownloads(c.config.MaxConcurrentDownloads),
		containerd.WithImageHandler(imageHandler),
		containerd.WithUnpackOpts([]containerd.UnpackOpt{
			containerd.WithUnpackDuplicationSuppressor(c.unpackDuplicationSuppressor),
			containerd.WithUnpackApplyOpts(diff.WithSyncFs(c.config.ImagePullWithSyncFs)),
		}),
	}

	if !c.config.DisableSnapshotAnnotations {
		pullOpts = append(pullOpts,
			containerd.WithImageHandlerWrapper(snpkg.AppendInfoHandlerWrapper(ref)))
	}

	if c.config.DiscardUnpackedLayers {

		pullOpts = append(pullOpts,
			containerd.WithChildLabelMap(containerdimages.ChildGCLabelsFilterLayers))
	}

	pullReporter.start(pctx)
	image, err := c.client.Pull(pctx, ref, pullOpts...)
	pcancel()
	if err != nil {
		return "", fmt.Errorf("failed to pull and unpack image %q: %w", ref, err)
	}
	span.AddEvent("Pull and unpack image complete")

	imageID, err := c.afterImagePulled(ctx, updateImageOpt{
		image:       image,
		isSchema1:   isSchema1,
		snapshotter: snapshotter,
	})
	if err != nil {
		return "", fmt.Errorf("failed to after pull image %q: %w", ref, err)
	}

	const mbToByte = 1024 * 1024
	size, _ := image.Size(ctx)
	imagePullingSpeed := float64(size) / mbToByte / time.Since(startTime).Seconds()
	imagePullThroughput.Observe(imagePullingSpeed)

	return imageID, err
}

type updateImageOpt struct {
	image         containerd.Image
	isSchema1     bool
	snapshotter   string
	externImageID string
}

func (c *CubeImageService) afterImagePulled(ctx context.Context, opt updateImageOpt) (string, error) {
	startTime := time.Now()
	defer func() {
		workflow.RecordCreateMetricIfGreaterThan(ctx, nil, "CubeImageService.afterImagePulled", time.Since(startTime), time.Millisecond)
	}()
	namedRef, err := distribution.ParseNormalizedNamed(opt.image.Name())
	if err != nil {
		return "", fmt.Errorf("failed to parse image reference %q: %w", opt.image.Name(), err)
	}
	ref := namedRef.String()
	if ref != opt.image.Name() {
		log.G(ctx).Debugf("PullImage using normalized image ref: %q", ref)
	}

	labels := c.getLabels(ctx, ref)

	maps.Copy(labels, opt.image.Metadata().Labels)

	if err := c.createOrUpdateImageReference(ctx, opt.image.Name(), opt.image.Target(), labels, opt.snapshotter); err != nil {
		return "", fmt.Errorf("failed to update image reference %q after image pulled: %w", opt.image.Name(), err)
	}
	i, err := c.images.Get(ctx, opt.image.Name())
	if err != nil {
		return "", fmt.Errorf("failed to get image %q: %w", opt.image.Name(), err)
	}
	labels = i.Labels

	configDesc, err := opt.image.Config(ctx)
	if err != nil {
		return "", fmt.Errorf("get image config descriptor: %w", err)
	}
	imageID := configDesc.Digest.String()
	repoDigest, repoTag := util.GetRepoDigestAndTag(namedRef, opt.image.Target().Digest)
	for _, r := range []string{opt.externImageID, imageID, repoTag, repoDigest} {
		if r == "" {
			continue
		}
		if err := c.createOrUpdateImageReference(ctx, r, opt.image.Target(), labels, opt.snapshotter); err != nil {
			return "", fmt.Errorf("failed to create image reference %q: %w", r, err)
		}

		if err := c.imageStore.Update(ctx, r); err != nil {
			return "", fmt.Errorf("failed to update image store %q: %w", r, err)
		}
	}
	return imageID, nil
}

func ParseAuth(auth *runtime.AuthConfig, host string) (string, string, error) {
	if auth == nil {
		return "", "", nil
	}
	if auth.ServerAddress != "" {

		u, err := url.Parse(auth.ServerAddress)
		if err != nil {
			return "", "", fmt.Errorf("parse server address: %w", err)
		}
		if host != u.Host {
			return "", "", nil
		}
	}
	if auth.Username != "" {
		return auth.Username, auth.Password, nil
	}
	if auth.IdentityToken != "" {
		return "", auth.IdentityToken, nil
	}
	if auth.Auth != "" {
		decLen := base64.StdEncoding.DecodedLen(len(auth.Auth))
		decoded := make([]byte, decLen)
		_, err := base64.StdEncoding.Decode(decoded, []byte(auth.Auth))
		if err != nil {
			return "", "", err
		}
		user, passwd, ok := strings.Cut(string(decoded), ":")
		if !ok {
			return "", "", fmt.Errorf("invalid decoded auth: %q", decoded)
		}
		return user, strings.Trim(passwd, "\x00"), nil
	}

	return "", "", nil
}

func (c *CubeImageService) createOrUpdateImageReference(ctx context.Context, name string, desc ocispec.Descriptor, labels map[string]string, snapshotter string) error {
	startTime := time.Now()
	defer func() {
		workflow.RecordCreateMetricIfGreaterThan(ctx, nil, "CubeImageService.createOrUpdateImageReference", time.Since(startTime), time.Millisecond)
	}()
	img := containerdimages.Image{
		Name:   name,
		Target: desc,

		Labels: labels,
	}

	_, err := c.images.Create(ctx, img)
	if err != nil && !errdefs.IsAlreadyExists(err) {
		return err
	}

	oldImg, err := c.images.Get(ctx, name)
	if err != nil {
		return err
	}
	fieldpaths := []string{"target"}
	if oldImg.Labels[crilabels.ImageLabelKey] != labels[crilabels.ImageLabelKey] {
		fieldpaths = append(fieldpaths, "labels."+crilabels.ImageLabelKey)
	}
	if oldImg.Labels[crilabels.PinnedImageLabelKey] != labels[crilabels.PinnedImageLabelKey] &&
		labels[crilabels.PinnedImageLabelKey] == crilabels.PinnedImageLabelValue {
		fieldpaths = append(fieldpaths, "labels."+crilabels.PinnedImageLabelKey)
	}

	paths, err := c.GenImageExtraAttributes(ctx, oldImg, img, snapshotter)
	if err != nil {
		log.G(ctx).WithError(err).Errorf("failed to gen image extra attributes: %v", err)
		return fmt.Errorf("failed to gen image extra attributes: %w", err)
	}
	fieldpaths = append(fieldpaths, paths...)

	if oldImg.Target.Digest == img.Target.Digest && len(fieldpaths) < 2 {
		return nil
	}

	_, err = c.images.Update(ctx, img, fieldpaths...)
	log.G(ctx).WithError(err).Debugf("update image %s with fieldpaths %v", name, fieldpaths)
	return err
}

func (c *CubeImageService) getLabels(ctx context.Context, name string) map[string]string {
	labels := map[string]string{crilabels.ImageLabelKey: crilabels.ImageLabelValue}
	for _, pinned := range c.config.PinnedImages {
		if pinned == name {
			labels[crilabels.PinnedImageLabelKey] = crilabels.PinnedImageLabelValue
		}
	}
	return labels
}

func (c *CubeImageService) UpdateImage(ctx context.Context, r string) error {

	img, err := c.client.GetImage(ctx, r)
	if err != nil {
		if !errdefs.IsNotFound(err) {
			return fmt.Errorf("get image by reference: %w", err)
		}

		if err := c.imageStore.Update(ctx, r); err != nil {
			return fmt.Errorf("update image store for %q: %w", r, err)
		}
		return nil
	}

	labels := img.Labels()
	if labels == nil {
		labels = map[string]string{}
	}
	criLabels := c.getLabels(ctx, r)

	createImageId := false
	for key, value := range criLabels {
		if labels[key] != value {
			labels[key] = value
			createImageId = true
			break
		}
	}
	if _, ok := labels[constants.LabelImageUidFiles]; !ok {
		createImageId = true
	}
	if labels[constants.LabelImageUidFiles] != "" {
		_, err = os.Stat(labels[constants.LabelImageUidFiles])
		if err != nil {

			delete(labels, constants.LabelImageUidFiles)
			createImageId = true
		}
	}
	if createImageId {

		configDesc, err := img.Config(ctx)
		if err != nil {
			return fmt.Errorf("get image id: %w", err)
		}
		id := configDesc.Digest.String()
		if err := c.createOrUpdateImageReference(ctx, id, img.Target(), labels, ""); err != nil {
			return fmt.Errorf("create image id reference %q: %w", id, err)
		}
		if err := c.imageStore.Update(ctx, id); err != nil {
			return fmt.Errorf("update image store for %q: %w", id, err)
		}
	}

	if err := c.imageStore.Update(ctx, r); err != nil {
		return fmt.Errorf("update image store for %q: %w", r, err)
	}
	return nil
}

func hostDirFromRoots(roots []string) func(string) (string, error) {
	rootfn := make([]func(string) (string, error), len(roots))
	for i := range roots {
		rootfn[i] = config.HostDirFromRoot(roots[i])
	}
	return func(host string) (dir string, err error) {
		for _, fn := range rootfn {
			dir, err = fn(host)
			if (err != nil && !errdefs.IsNotFound(err)) || (dir != "") {
				break
			}
		}
		return
	}
}

func (c *CubeImageService) registryHosts(ctx context.Context, credentials func(host string) (string, string, error), updateClientFn config.UpdateClientFunc) docker.RegistryHosts {
	paths := filepath.SplitList(c.config.Registry.ConfigPath)
	if len(paths) > 0 {
		hostOptions := config.HostOptions{
			UpdateClient: updateClientFn,
		}
		hostOptions.Credentials = credentials
		hostOptions.HostDir = hostDirFromRoots(paths)

		return config.ConfigureHosts(ctx, hostOptions)
	}

	return func(host string) ([]docker.RegistryHost, error) {
		var registries []docker.RegistryHost

		endpoints, err := c.registryEndpoints(host)
		if err != nil {
			return nil, fmt.Errorf("get registry endpoints: %w", err)
		}
		for _, e := range endpoints {
			u, err := url.Parse(e)
			if err != nil {
				return nil, fmt.Errorf("parse registry endpoint %q from mirrors: %w", e, err)
			}

			var (
				transport = newTransport()
				client    = &http.Client{Transport: transport}
				config    = c.config.Registry.Configs[u.Host]
			)

			if docker.IsLocalhost(host) && u.Scheme == "http" {

				transport.TLSClientConfig = &tls.Config{
					InsecureSkipVerify: true,
				}
			}

			credentials := credentials
			if credentials == nil && config.Auth != nil {
				auth := toRuntimeAuthConfig(*config.Auth)
				credentials = func(host string) (string, string, error) {
					return ParseAuth(auth, host)
				}

			}

			if updateClientFn != nil {
				if err := updateClientFn(client); err != nil {
					return nil, fmt.Errorf("failed to update http client: %w", err)
				}
			}

			authorizer := docker.NewDockerAuthorizer(
				docker.WithAuthClient(client),
				docker.WithAuthCreds(credentials))

			if u.Path == "" {
				u.Path = "/v2"
			}

			registries = append(registries, docker.RegistryHost{
				Client:       client,
				Authorizer:   authorizer,
				Host:         u.Host,
				Scheme:       u.Scheme,
				Path:         u.Path,
				Capabilities: docker.HostCapabilityResolve | docker.HostCapabilityPull,
			})
		}
		return registries, nil
	}
}

func toRuntimeAuthConfig(a criconfig.AuthConfig) *runtime.AuthConfig {
	return &runtime.AuthConfig{
		Username:      a.Username,
		Password:      a.Password,
		Auth:          a.Auth,
		IdentityToken: a.IdentityToken,
	}
}

func defaultScheme(host string) string {
	if docker.IsLocalhost(host) {
		return "http"
	}
	return "https"
}

func addDefaultScheme(endpoint string) (string, error) {
	if strings.Contains(endpoint, "://") {
		return endpoint, nil
	}
	ue := "dummy://" + endpoint
	u, err := url.Parse(ue)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s://%s", defaultScheme(u.Host), endpoint), nil
}

func (c *CubeImageService) registryEndpoints(host string) ([]string, error) {
	var endpoints []string
	_, ok := c.config.Registry.Mirrors[host]
	if ok {
		endpoints = c.config.Registry.Mirrors[host].Endpoints
	} else {
		endpoints = c.config.Registry.Mirrors["*"].Endpoints
	}
	defaultHost, err := docker.DefaultHost(host)
	if err != nil {
		return nil, fmt.Errorf("get default host: %w", err)
	}
	for i := range endpoints {
		en, err := addDefaultScheme(endpoints[i])
		if err != nil {
			return nil, fmt.Errorf("parse endpoint url: %w", err)
		}
		endpoints[i] = en
	}
	for _, e := range endpoints {
		u, err := url.Parse(e)
		if err != nil {
			return nil, fmt.Errorf("parse endpoint url: %w", err)
		}
		if u.Host == host {

			return endpoints, nil
		}
	}
	return append(endpoints, defaultScheme(defaultHost)+"://"+defaultHost), nil
}

func newTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:       30 * time.Second,
			KeepAlive:     30 * time.Second,
			FallbackDelay: 300 * time.Millisecond,
		}).DialContext,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 5 * time.Second,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
	}
}

func (c *CubeImageService) encryptedImagesPullOpts() []containerd.RemoteOpt {
	if c.config.ImageDecryption.KeyModel == criconfig.KeyModelNode {
		ltdd := imgcrypt.Payload{}
		decUnpackOpt := encryption.WithUnpackConfigApplyOpts(encryption.WithDecryptedUnpack(&ltdd))
		opt := containerd.WithUnpackOpts([]containerd.UnpackOpt{decUnpackOpt})
		return []containerd.RemoteOpt{opt}
	}
	return nil
}

const (
	defaultPullProgressReportInterval = 10 * time.Second
)

type pullProgressReporter struct {
	ref         string
	cancel      context.CancelFunc
	reqReporter pullRequestReporter
	timeout     time.Duration
}

func newPullProgressReporter(ref string, cancel context.CancelFunc, timeout time.Duration) *pullProgressReporter {
	return &pullProgressReporter{
		ref:         ref,
		cancel:      cancel,
		reqReporter: pullRequestReporter{},
		timeout:     timeout,
	}
}

func (reporter *pullProgressReporter) optionUpdateClient(client *http.Client) error {
	client.Transport = &pullRequestReporterRoundTripper{
		rt:          client.Transport,
		reqReporter: &reporter.reqReporter,
	}
	return nil
}

func (reporter *pullProgressReporter) start(ctx context.Context) {
	if reporter.timeout == 0 {
		log.G(ctx).Infof("no timeout and will not start pulling image %s reporter", reporter.ref)
		return
	}

	go func() {
		var (
			reportInterval = defaultPullProgressReportInterval

			lastSeenBytesRead = uint64(0)
			lastSeenTimestamp = time.Now()
		)

		if reporter.timeout < reportInterval {
			reportInterval = reporter.timeout / 2
		}

		var ticker = time.NewTicker(reportInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				activeReqs, bytesRead := reporter.reqReporter.status()

				log.G(ctx).WithFields(CubeLog.Fields{
					"activeReqs":        activeReqs,
					"totalBytesRead":    bytesRead,
					"lastSeenBytesRead": lastSeenBytesRead,
					"lastSeenTimestamp": lastSeenTimestamp.Format(time.RFC3339),
					"reportInterval":    reportInterval,
					"ref":               reporter.ref,
				}).Debugf("progress for image pull")

				if activeReqs == 0 || bytesRead > lastSeenBytesRead {
					lastSeenBytesRead = bytesRead
					lastSeenTimestamp = time.Now()
					continue
				}

				if time.Since(lastSeenTimestamp) > reporter.timeout {
					log.G(ctx).Errorf("cancel pulling image %s because of no progress in %v", reporter.ref, reporter.timeout)
					reporter.cancel()
					return
				}
			case <-ctx.Done():
				activeReqs, bytesRead := reporter.reqReporter.status()
				log.G(ctx).Infof("stop pulling image %s: active requests=%v, bytes read=%v", reporter.ref, activeReqs, bytesRead)
				return
			}
		}
	}()
}

type countingReadCloser struct {
	once sync.Once

	rc          io.ReadCloser
	reqReporter *pullRequestReporter
}

func (r *countingReadCloser) Read(p []byte) (int, error) {
	n, err := r.rc.Read(p)
	r.reqReporter.incByteRead(uint64(n))
	return n, err
}

func (r *countingReadCloser) Close() error {
	err := r.rc.Close()
	r.once.Do(r.reqReporter.decRequest)
	return err
}

type pullRequestReporter struct {
	activeReqs int32

	totalBytesRead uint64
}

func (reporter *pullRequestReporter) incRequest() {
	atomic.AddInt32(&reporter.activeReqs, 1)
}

func (reporter *pullRequestReporter) decRequest() {
	atomic.AddInt32(&reporter.activeReqs, -1)
}

func (reporter *pullRequestReporter) incByteRead(nr uint64) {
	atomic.AddUint64(&reporter.totalBytesRead, nr)
}

func (reporter *pullRequestReporter) status() (currentReqs int32, totalBytesRead uint64) {
	currentReqs = atomic.LoadInt32(&reporter.activeReqs)
	totalBytesRead = atomic.LoadUint64(&reporter.totalBytesRead)
	return currentReqs, totalBytesRead
}

type pullRequestReporterRoundTripper struct {
	rt http.RoundTripper

	reqReporter *pullRequestReporter
}

func (rt *pullRequestReporterRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.reqReporter.incRequest()

	resp, err := rt.rt.RoundTrip(req)
	if err != nil {
		rt.reqReporter.decRequest()
		log.L.WithError(err).Errorf("reporter: failed to pull image")
		return nil, err
	}

	resp.Body = &countingReadCloser{
		rc:          resp.Body,
		reqReporter: rt.reqReporter,
	}
	return resp, err
}

func (c *CubeImageService) snapshotterFromPodSandboxConfig(ctx context.Context, imageRef string,
	s *runtime.PodSandboxConfig) (string, error) {
	snapshotter := c.config.Snapshotter
	if s == nil || s.Annotations == nil {
		return snapshotter, nil
	}

	runtimeHandler, ok := s.Annotations[annotations.RuntimeHandler]
	if !ok {
		return snapshotter, nil
	}

	if c.runtimePlatforms != nil {
		if p, ok := c.runtimePlatforms[runtimeHandler]; ok && p.Snapshotter != snapshotter {
			snapshotter = p.Snapshotter
			log.G(ctx).Infof("experimental: PullImage %q for runtime %s, using snapshotter %s", imageRef, runtimeHandler, snapshotter)
		}
	}

	return snapshotter, nil
}
