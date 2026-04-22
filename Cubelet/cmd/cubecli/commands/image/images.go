// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package image

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"io"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/progress"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	"github.com/docker/go-units"
	"github.com/opencontainers/image-spec/identity"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/tencentcloud/CubeSandbox/Cubelet/cmd/cubecli/commands"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
)

var ListImageCommand = &cli.Command{
	Name:                   "ls",
	Usage:                  "List images",
	ArgsUsage:              "[REPOSITORY[:TAG]]",
	UseShortOptionHandling: true,
	Flags: []cli.Flag{
		&cli.BoolFlag{
			Name:    "verbose",
			Aliases: []string{"v"},
			Usage:   "Show verbose info for images",
		},
		&cli.BoolFlag{
			Name:    "quiet",
			Aliases: []string{"q"},
			Usage:   "Only show image IDs",
		},
		&cli.StringSliceFlag{
			Name:    "filter",
			Aliases: []string{"f"},
			Usage:   "Filter output based on provided conditions.\nAvailable filters: \n* dangling=(boolean - true or false)\n* reference=/regular expression/\n* before=<image-name>[:<tag>]|<image id>|<image@digest>\n* since=<image-name>[:<tag>]|<image id>|<image@digest>\nMultiple filters can be combined together.",
		},
		&cli.StringFlag{
			Name:    "output",
			Aliases: []string{"o"},
			Usage:   "Output format, One of: json|yaml|table",
		},
		&cli.BoolFlag{
			Name:  "digests",
			Usage: "Show digests",
		},
		&cli.BoolFlag{
			Name:  "no-trunc",
			Usage: "Show output without truncating the ID",
		},
		&cli.BoolFlag{
			Name:  "pinned",
			Usage: "Show whether the image is pinned or not",
		},
	},
	Action: func(c *cli.Context) error {
		conn, ctx, cancel, err := commands.NewGrpcConn(c)
		if err != nil {
			return err
		}
		defer conn.Close()
		defer cancel()
		iClient := runtime.NewImageServiceClient(conn)
		r, err := ListImages(ctx, iClient, c.Args().First(), c.StringSlice("filter"))
		if err != nil {
			return fmt.Errorf("listing images: %w", err)
		}

		switch c.String("output") {
		case outputTypeJSON:
			return outputProtobufObjAsJSON(r)
		case outputTypeYAML:
			return outputProtobufObjAsYAML(r)
		}

		display := newDefaultTableDisplay()
		verbose := c.Bool("verbose")
		showDigest := c.Bool("digests")
		showPinned := c.Bool("pinned")
		quiet := c.Bool("quiet")
		noTrunc := c.Bool("no-trunc")
		if !verbose && !quiet {
			row := []string{columnImage, columnTag}
			if showDigest {
				row = append(row, columnDigest)
			}
			row = append(row, columnImageID, columnSize, columnMedia)
			if showPinned {
				row = append(row, columnPinned)
			}
			display.AddRow(row)
		}
		for _, image := range r.Images {
			if quiet {
				fmt.Printf("%s\n", image.Id)

				continue
			}
			if !verbose {
				imageName, repoDigest := utils.NormalizeRepoDigest(image.RepoDigests)
				repoTagPairs := utils.NormalizeRepoTagPair(image.RepoTags, imageName)
				size := units.HumanSizeWithPrecision(float64(image.GetSize_()), 3)
				media := "docker"
				if image.Spec != nil && image.Spec.Image != "" {
					media = image.Spec.Image
				}
				id := image.Id
				if !noTrunc {
					id = utils.GetTruncatedID(id, "sha256:")
					repoDigest = utils.GetTruncatedID(repoDigest, "sha256:")
				}
				for _, repoTagPair := range repoTagPairs {
					row := []string{repoTagPair[0], repoTagPair[1]}
					if showDigest {
						row = append(row, repoDigest)
					}
					row = append(row, id, size, media)
					if showPinned {
						row = append(row, strconv.FormatBool(image.Pinned))
					}
					display.AddRow(row)
				}

				continue
			}
			fmt.Printf("ID: %s\n", image.Id)
			for _, tag := range image.RepoTags {
				fmt.Printf("RepoTags: %s\n", tag)
			}
			for _, digest := range image.RepoDigests {
				fmt.Printf("RepoDigests: %s\n", digest)
			}
			if image.Size_ != 0 {
				fmt.Printf("Size: %d\n", image.Size_)
			}
			if image.Uid != nil {
				fmt.Printf("Uid: %v\n", image.Uid)
			}
			if image.Username != "" {
				fmt.Printf("Username: %v\n", image.Username)
			}
			if image.Pinned {
				fmt.Printf("Pinned: %v\n", image.Pinned)
			}
			fmt.Printf("\n")
		}
		display.Flush()

		return nil
	},
}

var GlobalListImageCommand = &cli.Command{
	Name:   "images",
	Usage:  "Warning: List images is deprecated, use 'cube image ls' instead.",
	Action: ListImageCommand.Action,
	Flags:  ListImageCommand.Flags,
}

type imageByRef []*runtime.Image

func (a imageByRef) Len() int      { return len(a) }
func (a imageByRef) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a imageByRef) Less(i, j int) bool {
	if len(a[i].RepoTags) > 0 && len(a[j].RepoTags) > 0 {
		return a[i].RepoTags[0] < a[j].RepoTags[0]
	}

	if len(a[i].RepoDigests) > 0 && len(a[j].RepoDigests) > 0 {
		return a[i].RepoDigests[0] < a[j].RepoDigests[0]
	}

	return a[i].Id < a[j].Id
}

func ListImages(ctx context.Context, client runtime.ImageServiceClient, nameFilter string, conditionFilters []string) (*runtime.ListImagesResponse, error) {
	request := &runtime.ListImagesRequest{Filter: &runtime.ImageFilter{Image: &runtime.ImageSpec{Image: nameFilter}}}
	logrus.Debugf("ListImagesRequest: %v", request)

	resp, err := client.ListImages(ctx, request)
	if err != nil {
		return nil, err
	}
	logrus.Debugf("ListImagesResponse: %v", resp)

	sort.Sort(imageByRef(resp.Images))

	if len(conditionFilters) > 0 && len(resp.Images) > 0 {
		resp.Images, err = filterImagesList(resp.Images, conditionFilters)
		if err != nil {
			return nil, fmt.Errorf("filter images: %w", err)
		}
	}

	return resp, nil
}

func filterImagesList(imageList []*runtime.Image, filters []string) ([]*runtime.Image, error) {
	filtered := []*runtime.Image{}
	filtered = append(filtered, imageList...)

	for _, filter := range filters {
		switch {
		case strings.HasPrefix(filter, "before="):
			reversedList := filtered
			slices.Reverse(reversedList)
			filtered = filterByBeforeSince(strings.TrimPrefix(filter, "before="), reversedList)
			slices.Reverse(filtered)
		case strings.HasPrefix(filter, "dangling="):
			filtered = filterByDangling(strings.TrimPrefix(filter, "dangling="), filtered)
		case strings.HasPrefix(filter, "reference="):
			var err error
			if filtered, err = filterByReference(strings.TrimPrefix(filter, "reference="), filtered); err != nil {
				return []*runtime.Image{}, err
			}
		case strings.HasPrefix(filter, "since="):
			filtered = filterByBeforeSince(strings.TrimPrefix(filter, "since="), filtered)
		default:
			return []*runtime.Image{}, fmt.Errorf("unknown filter flag: %s", filter)
		}
	}

	return filtered, nil
}

func filterByBeforeSince(filterValue string, imageList []*runtime.Image) []*runtime.Image {
	filtered := []*runtime.Image{}

	for _, img := range imageList {

		if strings.Contains(filterValue, ":") && !strings.Contains(filterValue, "@") {
			imageName, _ := utils.NormalizeRepoDigest(img.RepoDigests)

			repoTagPairs := utils.NormalizeRepoTagPair(img.RepoTags, imageName)
			if strings.Join(repoTagPairs[0], ":") == filterValue {
				break
			}

			filtered = append(filtered, img)
		}

		if !strings.Contains(filterValue, ":") && !strings.Contains(filterValue, "@") {
			if strings.HasPrefix(img.Id, filterValue) {
				break
			}

			filtered = append(filtered, img)
		}

		if strings.Contains(filterValue, ":") && strings.Contains(filterValue, "@") {
			if len(img.RepoDigests) > 0 {
				if strings.HasPrefix(img.RepoDigests[0], filterValue) {
					break
				}

				filtered = append(filtered, img)
			}
		}
	}

	return filtered
}

func filterByReference(filterValue string, imageList []*runtime.Image) ([]*runtime.Image, error) {
	filtered := []*runtime.Image{}

	re, err := regexp.Compile(filterValue)
	if err != nil {
		return filtered, err
	}

	for _, img := range imageList {
		imgName, _ := utils.NormalizeRepoDigest(img.RepoDigests)
		if re.MatchString(imgName) || imgName == filterValue {
			filtered = append(filtered, img)
		}
	}

	return filtered, nil
}

func filterByDangling(filterValue string, imageList []*runtime.Image) []*runtime.Image {
	filtered := []*runtime.Image{}

	for _, img := range imageList {
		if filterValue == "true" && len(img.RepoTags) == 0 {
			filtered = append(filtered, img)
		}

		if filterValue == "false" && len(img.RepoTags) > 0 {
			filtered = append(filtered, img)
		}
	}

	return filtered
}

type imagePrintable struct {
	CreatedAt    string
	CreatedSince string
	Digest       string
	ID           string
	Repository   string
	Tag          string
	Size         string
	BlobSize     string

	Platform string
}

type imagePrinter struct {
	w                           io.Writer
	quiet, noTrunc, digestsFlag bool
	tmpl                        *template.Template
	client                      *containerd.Client
	contentStore                content.Store
	snapshotter                 snapshots.Snapshotter
}

func (x *imagePrinter) printImage(ctx context.Context, img images.Image) error {
	ociPlatforms, err := images.Platforms(ctx, x.contentStore, img.Target)
	if err != nil {
		logrus.WithError(err).Warnf("failed to get the platform list of image %q", img.Name)
		return x.printImageSinglePlatform(ctx, img, platforms.DefaultSpec())
	}
	for _, ociPlatform := range ociPlatforms {
		if err := x.printImageSinglePlatform(ctx, img, ociPlatform); err != nil {
			logrus.WithError(err).Warnf("failed to get platform %q of image %q", platforms.Format(ociPlatform), img.Name)
		}
	}
	return nil
}

func (x *imagePrinter) printImageSinglePlatform(ctx context.Context, img images.Image, ociPlatform v1.Platform) error {
	platMC := platforms.OnlyStrict(ociPlatform)
	if avail, _, _, _, availErr := images.Check(ctx, x.contentStore, img.Target, platMC); !avail {
		logrus.WithError(availErr).Debugf("skipping printing image %q for platform %q", img.Name, platforms.Format(ociPlatform))
		return nil
	}

	blobSize, err := img.Size(ctx, x.contentStore, platMC)
	if err != nil {
		logrus.WithError(err).Warnf("failed to get blob size of image %q for platform %q", img.Name, platforms.Format(ociPlatform))
	}

	size, err := unpackedImageSize(ctx, x.client, x.snapshotter, img, platMC)
	if err != nil {
		logrus.WithError(err).Warnf("failed to get unpacked size of image %q for platform %q", img.Name, platforms.Format(ociPlatform))
	}

	repository, tag := ParseRepoTag(img.Name)

	p := imagePrintable{
		CreatedAt:    img.CreatedAt.Round(time.Second).Local().String(),
		CreatedSince: TimeSinceInHuman(img.CreatedAt),
		Digest:       img.Target.Digest.String(),
		ID:           img.Target.Digest.String(),
		Repository:   repository,
		Tag:          tag,
		Size:         progress.Bytes(size).String(),
		BlobSize:     progress.Bytes(blobSize).String(),
		Platform:     platforms.Format(ociPlatform),
	}
	if p.Tag == "" {
		p.Tag = "<none>"
	}
	if !x.noTrunc {

		p.ID = strings.Split(p.ID, ":")[1][:12]
	}
	if x.tmpl != nil {
		var b bytes.Buffer
		if err := x.tmpl.Execute(&b, p); err != nil {
			return err
		}
		if _, err = fmt.Fprintf(x.w, b.String()+"\n"); err != nil {
			return err
		}
	} else if x.quiet {
		if _, err := fmt.Fprintf(x.w, "%s\n", p.ID); err != nil {
			return err
		}
	} else {
		if x.digestsFlag {
			if _, err := fmt.Fprintf(x.w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				p.Repository,
				p.Tag,
				p.Digest,
				p.ID,
				p.CreatedSince,
				p.Platform,
				p.Size,
				p.BlobSize,
			); err != nil {
				return err
			}
		} else {
			if _, err := fmt.Fprintf(x.w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				p.Repository,
				p.Tag,
				p.ID,
				p.CreatedSince,
				p.Platform,
				p.Size,
				p.BlobSize,
			); err != nil {
				return err
			}
		}
	}
	return nil
}

type snapshotKey string

func (key snapshotKey) add(ctx context.Context, s snapshots.Snapshotter, usage *snapshots.Usage) error {
	if key == "" {
		return nil
	}
	u, err := s.Usage(ctx, string(key))
	if err != nil {
		return err
	}

	usage.Add(u)

	info, err := s.Stat(ctx, string(key))
	if err != nil {
		return err
	}

	key = snapshotKey(info.Parent)
	return key.add(ctx, s, usage)
}

func unpackedImageSize(ctx context.Context, client *containerd.Client, s snapshots.Snapshotter, i images.Image, platMC platforms.MatchComparer) (int64, error) {
	img := containerd.NewImageWithPlatform(client, i, platMC)

	diffIDs, err := img.RootFS(ctx)
	if err != nil {
		return 0, err
	}

	chainID := identity.ChainID(diffIDs).String()
	usage, err := s.Usage(ctx, chainID)
	if err != nil {
		if errdefs.IsNotFound(err) {
			logrus.WithError(err).Debugf("image %q seems not unpacked", i.Name)
			return 0, nil
		}
		return 0, err
	}

	info, err := s.Stat(ctx, chainID)
	if err != nil {
		return 0, err
	}

	if err := snapshotKey(info.Parent).add(ctx, s, &usage); err != nil {
		return 0, err
	}
	return usage.Size, nil
}

func TimeSinceInHuman(since time.Time) string {
	return fmt.Sprintf("%s ago", units.HumanDuration(time.Since(since)))
}
