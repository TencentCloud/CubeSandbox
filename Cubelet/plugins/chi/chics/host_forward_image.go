// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package chics

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/containerd/errdefs"
	"github.com/containerd/typeurl/v2"
	"encoding/json"
	"encoding/json"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/identity"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubehost/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/types/v1"
	cubeimages "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/server/images"
	imagestore "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/store/image"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/rootfs"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/transfer/remote"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

func init() {
	typeurl.Register(&ocispec.Descriptor{}, "opencontainers/oci-spec", "Descriptor")
}

type DownloadLock struct {
	mu    sync.Mutex
	count int32
}

var downloadLocks = &sync.Map{}

func (s *cubeHostImageReverseClient) handleForwardImage(ctx context.Context, client cubehost.CubeHostImageService_ReverseStreamForwardImageClient, vmReq *cubehost.ServerMessage) bool {
	var (
		code  = cubehost.ResponseStatusCode_OK
		msg   = ""
		start = time.Now()
	)

	s.hostMonitor.add()

	imageID := vmReq.GetForwardImageRequest().GetImage().GetImage()
	logEntry := log.G(ctx).WithFields(CubeLog.Fields{
		"imageID":   imageID,
		"requestID": vmReq.Id,
		"step":      "handle image forward",
	})
	logEntry.Info("start handle image forward request")

	lockObj, _ := downloadLocks.LoadOrStore(imageID, &DownloadLock{})
	lock := lockObj.(*DownloadLock)
	atomic.AddInt32(&lock.count, 1)
	lock.mu.Lock()

	defer func() {
		lock.mu.Unlock()

		if atomic.AddInt32(&lock.count, -1) <= 0 {
			downloadLocks.Delete(imageID)
		}
	}()

	resp, err := s.sharedHostSnapshotToVm(ctx, client, vmReq, logEntry)
	duration := time.Since(start)
	logEntry = logEntry.WithFields(CubeLog.Fields{
		"duration": duration.String(),
	})
	if err != nil {
		s.hostMonitor.complete(statusFail, 0, 0)
		code = cubehost.ResponseStatusCode_ERROR
		msg = fmt.Sprintf("failed to handle image forward request: %v", err)
		logEntry.Errorf(msg)
	} else {
		s.hostMonitor.complete(statusSuccess, float64(duration.Milliseconds()), resp.GetImage().Size)
		logEntry.WithField("response", log.WithJsonValue(resp)).Info("image forward request handled success")
	}

	err = client.Send(&cubehost.ClientMessage{
		Id:   vmReq.Id,
		Type: cubehost.MessageType_IMAGE_FORWARD,
		Status: &cubehost.ResponseStatus{
			Code:    code,
			Message: msg,
		},
		Content: &cubehost.ClientMessage_ForwardImageResponse{
			ForwardImageResponse: resp,
		},
	})
	if err != nil {
		s.sendError(fmt.Errorf("failed to send image forward response: %w", err))
		return true
	}
	return false
}

func (s *cubeHostImageReverseClient) sharedHostSnapshotToVm(ctx context.Context, client cubehost.CubeHostImageService_ReverseStreamForwardImageClient, vmReq *cubehost.ServerMessage, logEntry *log.CubeWrapperLogEntry) (*cubehost.ForwardImageResponse, error) {
	forwardRequest := vmReq.GetForwardImageRequest()
	imageID := forwardRequest.GetImage().GetImage()
	image, err := s.updater.GetImageService().LocalResolve(ctx, imageID)
	if err != nil {
		if errdefs.IsNotFound(err) {
			s.hostMonitor.addCache(false)
			if forwardRequest.GetAuth() != nil {
				ctx = constants.WithImageCredentials(ctx, forwardRequest.GetAuth().ToCRI())
			}

			credentials := func(host string) (string, string, error) {
				if forwardRequest.GetAuth() == nil {
					return "", "", nil
				}
				return cubeimages.ParseAuth(forwardRequest.GetAuth().ToCRI(), host)
			}

			sandbox := s.updater.GetSandboxer()
			ctx = cubeboxstore.WithCubeBox(ctx, sandbox)

			_, err = s.updater.GetImageService().PullRegistryImage(ctx, imageID, &cubeimages.PullImageOption{
				Credentials:    credentials,
				UpdateClientFn: remote.WithTunnelHttpTransport(s.factory.CreateHttpProxyConn, defaultProxyUrl),
			})
			if err != nil {
				return nil, fmt.Errorf("failed to pull image %s by cube host: %w", imageID, err)
			}
			image, err = s.updater.GetImageService().LocalResolve(ctx, imageID)
			if err != nil {
				return nil, fmt.Errorf("failed to resolve image after pull image by cube host %s: %w", imageID, err)
			}
		} else {
			return nil, err
		}
	} else {
		s.hostMonitor.addCache(true)
	}

	response := &cubehost.ForwardImageResponse{
		Image: &cubehost.HostImage{
			Id:          image.ID,
			RepoDigests: image.References,
			Spec: &types.ImageSpec{
				Image:       imageID,
				Annotations: image.ImageSpec.Config.Labels,
			},
			Size: uint64(image.Size),
		},
	}

	err = appendImageDescriptors(image, response)
	if err != nil {
		return response, err
	}

	var (
		diffIDs     = image.ImageSpec.RootFS.DiffIDs
		parent      = identity.ChainID(diffIDs).String()
		i           = len(diffIDs) - 1
		layerMounts []*cubehost.LayerMount
	)
	sn, err := s.updater.GetSnapShotter()
	if err != nil {
		return nil, fmt.Errorf("failed to get snapshotter: %w", err)
	}

	for {
		if parent == "" {
			break
		}
		info, err := sn.Stat(ctx, parent)
		if err != nil {
			return nil, fmt.Errorf("failed to stat snapshot %s: %w", parent, err)
		}
		usage, err := sn.Usage(ctx, parent)
		if err != nil {
			return nil, fmt.Errorf("failed to get usage for snapshot %s: %w", parent, err)
		}

		hostPaths, err := rootfs.SnapshotRefFs(ctx, sn, info.Name)
		if err != nil {
			return nil, fmt.Errorf("failed to get host ref path for snapshot %s: %w", info.Name, err)
		}
		if len(hostPaths) == 0 {
			return nil, fmt.Errorf("empty host ref path for snapshot %s", info.Name)
		}
		layerDigestString := diffIDs[i].String()
		layerMount := &cubehost.LayerMount{
			Name:      parent,
			HostPath:  hostPaths[0],
			Parent:    info.Parent,
			Digest:    layerDigestString,
			Usage:     log.WithJsonValue(usage),
			LayerType: cubehost.LayerType_LAYER_TYPE_FS,
		}
		if layerMount.HostPath == "" {
			return nil, fmt.Errorf("failed to get host ref path for snapshot %s", parent)
		}
		mapping, err := rootfs.HostToShareDir(layerMount.HostPath)
		if err != nil {
			return nil, fmt.Errorf("failed to get share dir for snapshot %s: %w", parent, err)
		}
		layerMount.VmPath = mapping.MountPath
		layerMount.HostPath = mapping.SharePath

		i -= 1
		layerMounts = append(layerMounts, layerMount)
		parent = info.Parent
	}

	slices.Reverse(layerMounts)
	response.Image.LayerMounts = layerMounts

	return response, s.updater.AppendSharedImageFs(ctx, response.Image)
}

func appendImageDescriptors(image imagestore.Image, response *cubehost.ForwardImageResponse) error {

	imageSpec := image.ImageSpec
	configBytes, err := json.Marshal(imageSpec)
	if err != nil {
		return fmt.Errorf("failed to marshal image config: %w", err)
	}
	ocidesc := &ocispec.Descriptor{
		MediaType:   ocispec.MediaTypeImageConfig,
		Annotations: image.ImageSpec.Config.Labels,
		Data:        configBytes,
		Digest:      digest.FromBytes(configBytes),
		Size:        int64(len(configBytes)),
	}
	dsc, err := typeurl.MarshalAnyToProto(ocidesc)
	if err != nil {
		return fmt.Errorf("failed to marshal image config descriptor: %w", err)
	}
	response.Image.Descriptors = append(response.Image.Descriptors, dsc)
	return nil
}
