// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package image

import (
	"context"
	gocontext "context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/platforms"
	"github.com/distribution/reference"
	"github.com/ipfs/go-cid"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"
)

var Inspect = &cli.Command{
	Name:  "inspect",
	Usage: "inspect image",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:  "mode,m",
			Value: "native",
			Usage: "Inspect mode, 'dockercompat' for Docker-compatible output, 'native' for containerd-native output",
		},
		&cli.StringFlag{
			Name:  "platform",
			Value: "amd64",
			Usage: "Inspect a specific platform",
		},
	},
	Action: func(context *cli.Context) error {
		mode := context.String("mode")
		f := &imageInspector{
			mode: mode,
		}

		var clientOpts []containerd.Opt
		if context.String("platform") != "" {
			platformParsed, err := platforms.Parse(context.String("platform"))
			if err != nil {
				return err
			}
			platformM := platforms.Only(platformParsed)
			clientOpts = append(clientOpts, containerd.WithDefaultPlatform(platformM))
		}

		cntdClient, err := containerd.New(context.String("address"), clientOpts...)
		if err != nil {
			return fmt.Errorf("init containerd connect failed.%s", err)
		}
		cntCtx := namespaces.WithNamespace(gocontext.Background(), context.String("namespace"))
		cntCtx, cntCancel := gocontext.WithTimeout(cntCtx, context.Duration("timeout"))
		defer cntCancel()

		walker := &ImageWalker{
			Client: cntdClient,
			OnFound: func(ctx gocontext.Context, found Found) error {
				ctx, cancel := gocontext.WithTimeout(ctx, 5*time.Second)
				defer cancel()

				n, err := InspectImge(ctx, cntdClient, found.Image)
				if err != nil {
					return err
				}
				switch f.mode {
				case "native":
					f.entries = append(f.entries, n)
				default:
					return fmt.Errorf("unknown mode %q", f.mode)
				}
				return nil
			},
		}
		var errs []error
		for _, req := range context.Args().Slice() {
			n, err := walker.Walk(cntCtx, req)
			if err != nil {
				errs = append(errs, err)
			} else if n == 0 {
				errs = append(errs, fmt.Errorf("no such object: %s", req))
			}
		}
		if len(errs) > 0 {
			return fmt.Errorf("%d errors: %v", len(errs), errs)
		}

		b, err := json.MarshalIndent(f.entries, "", "    ")
		if err != nil {
			return err
		}

		fmt.Fprintln(os.Stdout, string(b))
		return nil
	},
}

type imageInspector struct {
	mode    string
	entries []interface{}
}

type Found struct {
	Image      images.Image
	Req        string
	MatchIndex int
	MatchCount int
}

type OnFound func(ctx gocontext.Context, found Found) error

type ImageWalker struct {
	Client  *containerd.Client
	OnFound OnFound
}

func (w *ImageWalker) Walk(ctx gocontext.Context, req string) (int, error) {
	var filters []string
	if canonicalRef, err := ParseAny(req); err == nil {
		filters = append(filters, fmt.Sprintf("name==%s", canonicalRef.String()))
	}
	filters = append(filters,
		fmt.Sprintf("target.digest~=^sha256:%s.*$", regexp.QuoteMeta(req)),
		fmt.Sprintf("target.digest~=^%s.*$", regexp.QuoteMeta(req)),
	)

	images, err := w.Client.ImageService().List(ctx, filters...)
	if err != nil {
		return -1, err
	}

	matchCount := len(images)
	for i, img := range images {
		f := Found{
			Image:      img,
			Req:        req,
			MatchIndex: i,
			MatchCount: matchCount,
		}
		if e := w.OnFound(ctx, f); e != nil {
			return -1, e
		}
	}
	return matchCount, nil
}

type Reference interface {
	String() string
}

func ParseAny(rawRef string) (Reference, error) {
	if scheme, ref, err := ParseIPFSRefWithScheme(rawRef); err == nil {
		return stringRef{scheme: scheme, s: ref}, nil
	}
	if c, err := cid.Decode(rawRef); err == nil {
		return c, nil
	}
	return ParseDockerRef(rawRef)
}

func ParseDockerRef(rawRef string) (reference.Named, error) {
	return reference.ParseDockerRef(rawRef)
}

func ParseIPFSRefWithScheme(name string) (scheme, ref string, err error) {
	if strings.HasPrefix(name, "ipfs://") || strings.HasPrefix(name, "ipns://") {
		return name[:4], name[7:], nil
	}
	return "", "", fmt.Errorf("reference is not an IPFS reference")
}

type stringRef struct {
	scheme string
	s      string
}

func (s stringRef) String() string {
	return s.s
}

type nativeImage struct {
	Image        images.Image        `json:"Image"`
	IndexDesc    *ocispec.Descriptor `json:"IndexDesc,omitempty"`
	Index        *ocispec.Index      `json:"Index,omitempty"`
	ManifestDesc *ocispec.Descriptor `json:"ManifestDesc,omitempty"`
	Manifest     *ocispec.Manifest   `json:"Manifest,omitempty"`

	ImageConfigDesc ocispec.Descriptor `json:"ImageConfigDesc"`
	ImageConfig     ocispec.Image      `json:"ImageConfig"`
}

func InspectImge(ctx context.Context, client *containerd.Client, image images.Image) (*nativeImage, error) {

	n := &nativeImage{}

	img := containerd.NewImage(client, image)
	idx, idxDesc, err := ReadIndex(ctx, img)
	if err != nil {
		logrus.WithError(err).WithField("id", image.Name).Warnf("failed to inspect index")
	} else {
		n.IndexDesc = idxDesc
		n.Index = idx
	}

	mani, maniDesc, err := ReadManifest(ctx, img)
	if err != nil {
		logrus.WithError(err).WithField("id", image.Name).Warnf("failed to inspect manifest")
	} else {
		n.ManifestDesc = maniDesc
		n.Manifest = mani
	}

	imageConfig, imageConfigDesc, err := ReadImageConfig(ctx, img)
	if err != nil {
		logrus.WithError(err).WithField("id", image.Name).Warnf("failed to inspect image config")
	} else {
		n.ImageConfigDesc = imageConfigDesc
		n.ImageConfig = imageConfig
	}
	n.Image = image

	return n, nil
}
