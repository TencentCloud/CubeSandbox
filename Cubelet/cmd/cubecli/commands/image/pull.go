// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package image

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"syscall"

	"github.com/sirupsen/logrus"
	"github.com/tencentcloud/CubeSandbox/Cubelet/cmd/cubecli/commands"
	"github.com/urfave/cli/v2"
	"golang.org/x/term"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

var pullImageCommand = &cli.Command{
	Name:                   "pull",
	Usage:                  "Pull an image from a registry",
	UseShortOptionHandling: true,
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:    "creds",
			Usage:   "Use `USERNAME[:PASSWORD]` for accessing the registry",
			EnvVars: []string{"CRICTL_CREDS"},
		},
		&cli.StringFlag{
			Name:    "auth",
			Usage:   "Use `AUTH_STRING` for accessing the registry. AUTH_STRING is a base64 encoded 'USERNAME[:PASSWORD]'",
			EnvVars: []string{"CRICTL_AUTH"},
		},
		&cli.StringFlag{
			Name:    "username",
			Aliases: []string{"u"},
			Usage:   "Use `USERNAME` for accessing the registry. The password will be requested on the command line",
		},
		&cli.StringFlag{
			Name:      "pod-config",
			Usage:     "Use `pod-config.[json|yaml]` to override the pull config",
			TakesFile: true,
		},
		&cli.StringSliceFlag{
			Name:    "annotation",
			Aliases: []string{"a"},
			Usage:   "Annotation to be set on the pulled image",
		},
		&cli.DurationFlag{
			Name:    "pull-timeout",
			Aliases: []string{"pt"},
			Usage:   "Maximum time to be used for pulling the image, disabled if set to 0s",
			EnvVars: []string{"CRICTL_PULL_TIMEOUT"},
		},
	},
	Subcommands: []*cli.Command{{
		Name:      "jsonschema",
		Aliases:   []string{"js"},
		Usage:     "Display the JSON schema for the pod-config.json, ",
		UsageText: "The schema will be generated from the PodSandboxConfig of the CRI API compiled with this version of crictl",
		Action: func(*cli.Context) error {
			return printJSONSchema(&runtime.PodSandboxConfig{})
		},
	}},
	ArgsUsage: "NAME[:TAG|@DIGEST]",
	Action: func(c *cli.Context) error {
		imageName := c.Args().First()
		if imageName == "" {
			return errors.New("image name cannot be empty")
		}

		if c.NArg() > 1 {
			return cli.ShowSubcommandHelp(c)
		}

		conn, ctx, cancel, err := commands.NewGrpcConn(c)
		if err != nil {
			return err
		}
		defer conn.Close()
		defer cancel()
		imageClient := runtime.NewImageServiceClient(conn)

		auth, err := getAuth(c.String("creds"), c.String("auth"), c.String("username"))
		if err != nil {
			return err
		}
		var sandbox *runtime.PodSandboxConfig
		if c.IsSet("pod-config") {
			sandbox, err = loadPodSandboxConfig(c.String("pod-config"))
			if err != nil {
				return fmt.Errorf("load podSandboxConfig: %w", err)
			}
		}
		var ann map[string]string
		if c.IsSet("annotation") {
			annotationFlags := c.StringSlice("annotation")
			ann, err = parseLabelStringSlice(annotationFlags)
			if err != nil {
				return err
			}
		}
		timeout := c.Duration("pull-timeout")
		if timeout != 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}
		r, err := PullImageWithSandbox(ctx, imageClient, imageName, auth, sandbox, ann)
		if err != nil {
			return fmt.Errorf("pulling image: %w", err)
		}
		fmt.Printf("Image is up to date for %s\n", r.ImageRef)

		return nil
	},
}

func PullImageWithSandbox(ctx context.Context, client runtime.ImageServiceClient, image string, auth *runtime.AuthConfig, sandbox *runtime.PodSandboxConfig, ann map[string]string) (*runtime.PullImageResponse, error) {
	request := &runtime.PullImageRequest{
		Image: &runtime.ImageSpec{
			Image:       image,
			Annotations: ann,
		},
		Auth:          auth,
		SandboxConfig: sandbox,
	}
	logrus.Debugf("PullImageRequest: %v", request)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	resp, err := client.PullImage(ctx, request)
	if err != nil {
		return nil, err
	}
	logrus.Debugf("PullImageResponse: %v", resp)

	return resp, nil
}

func getAuth(creds, auth, username string) (*runtime.AuthConfig, error) {
	if username != "" {
		fmt.Print("Enter Password:")

		bytePassword, err := term.ReadPassword(int(syscall.Stdin))

		fmt.Print("\n")

		if err != nil {
			return nil, err
		}

		password := string(bytePassword)

		return &runtime.AuthConfig{
			Username: username,
			Password: password,
		}, nil
	}

	if creds != "" && auth != "" {
		return nil, errors.New("both `--creds` and `--auth` are specified")
	}

	if creds != "" {
		username, password, err := parseCreds(creds)
		if err != nil {
			return nil, err
		}

		return &runtime.AuthConfig{
			Username: username,
			Password: password,
		}, nil
	}

	if auth != "" {
		return &runtime.AuthConfig{
			Auth: auth,
		}, nil
	}

	return nil, nil
}

func parseCreds(creds string) (username, password string, err error) {
	if creds == "" {
		return "", "", errors.New("credentials can't be empty")
	}

	up := strings.SplitN(creds, ":", 2)
	if len(up) == 1 {
		return up[0], "", nil
	}

	if up[0] == "" {
		return "", "", errors.New("username can't be empty")
	}

	return up[0], up[1], nil
}
