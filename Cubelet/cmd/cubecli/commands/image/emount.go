// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package image

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"slices"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/defaults"
	"github.com/containerd/platforms"
	"github.com/opencontainers/image-spec/identity"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/urfave/cli/v2"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/tencentcloud/CubeSandbox/Cubelet/cmd/cubecli/commands"
	internalimages "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/server/images"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/rootfs"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
)

const (
	uidsFileDirName = "uids_file"
)

type imageLayer struct {
	ID     string      `json:"id"`
	Source string      `json:"source"`
	Dir    string      `json:"dir"`
	Usage  interface{} `json:"usage,omitempty"`
}

type layerIndex struct {
	SchemaVersion int                  `json:"schemaVersion"`
	Image         *runtime.Image       `json:"image"`
	Target        ocispec.Descriptor   `json:"target"`
	Descriptors   []ocispec.Descriptor `json:"descriptors,omitempty"`
	Layers        []*imageLayer        `json:"layers,omitempty"`
	UidsFileDir   string               `json:"uidsFileDir,omitempty"`
}

var erofsMountImageCommand = &cli.Command{
	Name:                   "emount",
	Usage:                  "mount images and layer to dest",
	ArgsUsage:              "emount [REPOSITORY[:TAG]] dest",
	UseShortOptionHandling: true,
	Action: func(c *cli.Context) error {
		conn, ctx, cancel, err := commands.NewGrpcConn(c)
		if err != nil {
			return err
		}
		defer conn.Close()
		defer cancel()
		if c.Args().Len() < 2 {
			return fmt.Errorf("image and dest is required, use cubelli image emount [REPOSITORY[:TAG]] dest")
		}

		id := c.Args().First()
		dest := c.Args().Get(1)
		err = os.MkdirAll(dest, 0755)
		if err != nil {
			return fmt.Errorf("failed to create dest dir: %w", err)
		}

		iClient := runtime.NewImageServiceClient(conn)
		cClient, err := containerd.NewWithConn(conn)
		if err != nil {
			return fmt.Errorf("failed to create containerd client: %w", err)
		}

		r, err := ImageStatus(ctx, iClient, id, true)
		if err != nil {
			return fmt.Errorf("image status for %q request: %w", id, err)
		}
		if r.Image == nil {
			return fmt.Errorf("failed to get image info for %q", id)
		}

		var (
			platform = platforms.Default()
			imageID  = r.Image.Id
			provider = cClient.ContentStore()
			sn       = cClient.SnapshotService(defaults.DefaultSnapshotter)
		)
		fmt.Printf("resolved image id: %s", imageID)
		img, err := cClient.ImageService().Get(ctx, imageID)
		if err != nil {
			return fmt.Errorf("failed to get containerd image: %w", err)
		}

		mb, err := content.ReadBlob(ctx, provider, img.Target)
		if err != nil {
			return fmt.Errorf("failed to read containerd image blob: %w", err)
		}
		img.Target.Data = mb

		var validChildren []ocispec.Descriptor
		images.Walk(ctx, images.HandlerFunc(func(ctx context.Context, desc ocispec.Descriptor) (subdescs []ocispec.Descriptor, err error) {
			var localDescs []ocispec.Descriptor
			childs, err := images.Children(ctx, provider, desc)
			if err != nil {
				fmt.Printf("failed to get children for %s: %v", desc.Digest.String(), err)
				return localDescs, nil
			}
			for _, child := range childs {
				if !images.IsConfigType(child.MediaType) && !images.IsManifestType(child.MediaType) {
					continue
				}
				b, err := content.ReadBlob(ctx, provider, child)
				if err != nil {

					continue
				}
				child.Data = b
				validChildren = append(validChildren, child)
				localDescs = append(localDescs, child)

			}
			return localDescs, nil
		}), img.Target)

		i := containerd.NewImageWithPlatform(cClient, img, platform)
		diffIDs, err := i.RootFS(ctx)
		if err != nil {
			return fmt.Errorf("failed to get containerd image diffIDs: %w", err)
		}

		lowerDirs, err := rootfs.SnapshotRefFs(ctx, cClient.SnapshotService(defaults.DefaultSnapshotter), identity.ChainID(diffIDs).String())
		if err != nil {
			return fmt.Errorf("failed to get containerd image lowerDirs: %w", err)
		}
		slices.Reverse(lowerDirs)
		var layers []*imageLayer
		for i, dir := range lowerDirs {
			id := identity.ChainID(diffIDs[:i+1]).String()
			usage, err := sn.Usage(ctx, id)
			if err != nil {
				return fmt.Errorf("failed to get usage for snapshot %s: %w", id, err)
			}
			layers = append(layers, &imageLayer{
				ID:     id,
				Source: dir,
				Dir:    fmt.Sprintf("%d", i),
				Usage:  log.WithJsonValue(usage),
			})
		}

		var uidsDir string
		uidsFilePath := path.Join(dest, uidsFileDirName)
		if err := internalimages.CopyImageUidsFile(ctx, cClient, defaults.DefaultSnapshotter, i, uidsFilePath); err != nil {
			log.G(ctx).WithError(err).Errorf("failed to copy uids file, will not use uids file: %v", err)
			uidsFilePath = ""
		} else {
			uidsDir = uidsFileDirName
		}

		lindex := &layerIndex{
			SchemaVersion: 1,
			Image:         r.Image,
			Target:        img.Target,
			Descriptors:   validChildren,
			Layers:        layers,
			UidsFileDir:   uidsDir,
		}

		lindexPath := path.Join(dest, "layer-index.json")
		exist, err := utils.DenExist(lindexPath)
		if err != nil {
			err = fmt.Errorf("failed to check if layer index file exists: %w", err)
			return err
		}
		if !exist {
			b, _ := json.Marshal(lindex)
			err = os.WriteFile(lindexPath, b, 0644)
			if err != nil {
				err = fmt.Errorf("failed to write layer index file: %w", err)
				return err
			}
		}

		for _, layer := range layers {
			layerTarget := path.Join(dest, layer.Dir)
			err := os.MkdirAll(layerTarget, 0755)
			if err != nil {
				return fmt.Errorf("failed to create layer target %s: %w", layerTarget, err)
			}

			bind := &mount.Mount{
				Type:    "bind",
				Source:  layer.Source,
				Options: []string{"bind", "ro"},
			}
			err = bind.Mount(layerTarget)
			if err != nil {
				return fmt.Errorf("bind mount %s to %s failed: %w", bind.Source, layerTarget, err)
			}
			fmt.Printf("bind mount %s to %s success\n", bind.Source, layerTarget)
		}
		fmt.Printf("success to bind mount all layers to %s\n", dest)
		return nil
	},
}
