// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/transfer"
	"github.com/containerd/containerd/v2/core/unpack"
	"github.com/containerd/containerd/v2/pkg/tracing"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	"github.com/opencontainers/image-spec/identity"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/transfer/transferi"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

func (ts *localTransferService) prepareCfs(ctx context.Context, erf transferi.ExternalRootfs, is transfer.ImageStorer, tops *transfer.Config) error {
	ctx, layerSpan := tracing.StartSpan(ctx, tracing.Name("transfer", "prepareCfs"))
	defer layerSpan.End()

	manifestDesc, err := erf.PrepareContent(ctx, ts.content)
	if err != nil {
		return err
	}
	unpackStart := time.Now()
	var manifest ocispec.Manifest
	if err := json.Unmarshal(manifestDesc.Data, &manifest); err != nil {
		return fmt.Errorf("unmarshal image config: %w", err)
	}

	p, err := content.ReadBlob(ctx, ts.content, manifest.Config)
	if err != nil {
		return err
	}
	var config ocispec.Image
	if err := json.Unmarshal(p, &config); err != nil {
		return fmt.Errorf("unmarshal image config: %w", err)
	}
	diffIDs := config.RootFS.DiffIDs
	if len(manifest.Layers) != len(diffIDs) {
		return fmt.Errorf("number of layers and diffIDs don't match: %d != %d", len(manifest.Layers), len(diffIDs))
	}

	var platform *unpack.Platform

	imgPlatform := platforms.Normalize(config.Platform)
	for _, up := range ts.config.UnpackPlatforms {
		if up.Platform.Match(imgPlatform) {
			platform = &up
			break
		}
	}
	if platform == nil {
		return fmt.Errorf("no matching unpack platform for %v", imgPlatform)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		sn = platform.Snapshotter
		cs = ts.content
	)

	for i, desc := range manifest.Layers {
		_, layerSpan := tracing.StartSpan(ctx, tracing.Name("cfs-image", "unpackLayer"))
		unpackLayerStart := time.Now()
		layerSpan.SetAttributes(
			tracing.Attribute("layer.media.type", desc.MediaType),
			tracing.Attribute("layer.media.size", desc.Size),
			tracing.Attribute("layer.media.digest", desc.Digest.String()),
		)
		if err := transferi.MountExternalSnapshot(ctx, sn, diffIDs[:1+i], desc.Annotations); err != nil {
			layerSpan.SetStatus(err)
			layerSpan.End()
			return err
		}
		layerSpan.End()
		log.G(ctx).WithFields(CubeLog.Fields{
			"layer":    desc.Digest,
			"duration": time.Since(unpackLayerStart),
		}).Debug("layer unpacked")
	}
	chainID := identity.ChainID(diffIDs).String()
	cinfo := content.Info{
		Digest: manifest.Config.Digest,
		Labels: map[string]string{
			fmt.Sprintf("containerd.io/gc.ref.snapshot.%s", platform.SnapshotterKey): chainID,
		},
	}
	_, err = cs.Update(ctx, cinfo, fmt.Sprintf("labels.containerd.io/gc.ref.snapshot.%s", platform.SnapshotterKey))
	if err != nil {
		return err
	}

	log.G(ctx).WithFields(CubeLog.Fields{
		"config":   manifest.Config.Digest,
		"chainID":  chainID,
		"duration": time.Since(unpackStart),
	}).Debug("image unpacked")

	imgs, err := is.Store(ctx, manifestDesc, ts.images)
	if err != nil {
		if errdefs.IsNotFound(err) {
			log.G(ctx).Infof("No images store for %s", manifestDesc.Digest)
		}
		return err
	}
	if tops.Progress != nil {
		for _, img := range imgs {
			tops.Progress(transfer.Progress{
				Event: "completed prepareCfs",
				Name:  img.Name,
				Desc:  &manifestDesc,
			})
		}
	}

	cinfo, err = cs.Info(ctx, manifest.Config.Digest)
	if err != nil {
		return fmt.Errorf("failed to get image config info: %w", err)
	}
	log.G(ctx).WithFields(CubeLog.Fields{
		"config": cinfo.Digest.String(),
		"labels": log.WithJsonValue(cinfo),
	}).Info("create image config info")
	return nil
}
