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

package commands

import (
	"bufio"
	gocontext "context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/defaults"
	"github.com/containerd/containerd/v2/pkg/dialer"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/platforms"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/urfave/cli/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	DefaultCubeletConfigPath = "/usr/local/services/cubetoolbox/Cubelet/config/config.toml"
)

var (
	SnapshotterFlags = []cli.Flag{
		&cli.StringFlag{
			Name:    "snapshotter",
			Usage:   "snapshotter name. Empty value stands for the default value.",
			EnvVars: []string{"CONTAINERD_SNAPSHOTTER"},
			Value:   "overlayfs",
		},
	}
)

func AppContext(context *cli.Context) (gocontext.Context, gocontext.CancelFunc) {
	var (
		ctx       = gocontext.Background()
		timeout   = context.Duration("timeout")
		namespace = context.String("namespace")
		cancel    gocontext.CancelFunc
	)
	ctx = namespaces.WithNamespace(ctx, namespace)
	if timeout > 0 {
		ctx, cancel = gocontext.WithTimeout(ctx, timeout)
	} else {
		ctx, cancel = gocontext.WithCancel(ctx)
	}
	return ctx, cancel
}

func NewGrpcConn(context *cli.Context) (*grpc.ClientConn, gocontext.Context, gocontext.CancelFunc, error) {
	conTimeout := context.Duration("connect-timeout")

	address := context.String("address")
	if address == "" {
		address = context.String("tcpaddress")
		if address == "" {
			return nil, nil, nil, fmt.Errorf("address is not set")
		}
	} else {
		address = dialer.DialAddress(address)
	}
	backoffConfig := backoff.DefaultConfig
	backoffConfig.MaxDelay = 10 * time.Second
	connParams := grpc.ConnectParams{
		Backoff:           backoffConfig,
		MinConnectTimeout: conTimeout,
	}
	gopts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithConnectParams(connParams),
		grpc.WithContextDialer(dialer.ContextDialer),
		grpc.WithUserAgent(constants.UserAgentCubecli),
	}

	gopts = append(gopts, grpc.WithDefaultCallOptions(
		grpc.MaxCallRecvMsgSize(defaults.DefaultMaxRecvMsgSize),
		grpc.MaxCallSendMsgSize(defaults.DefaultMaxSendMsgSize)))

	connector := func() (*grpc.ClientConn, error) {
		conn, err := grpc.NewClient(address, gopts...)
		if err != nil {
			return nil, fmt.Errorf("failed to dial %q: %w", address, err)
		}
		return conn, nil
	}
	conn, err := connector()
	if err != nil {
		return nil, nil, nil, err
	}

	ctx, cancel := AppContext(context)
	return conn, ctx, cancel, nil
}

func PrintAsJSON(x interface{}) {
	b, err := json.MarshalIndent(x, "", "    ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "can't marshal %+v as a JSON string: %v\n", x, err)
	}
	fmt.Println(string(b))
}

func AskForConfirm(s string, tries int) bool {
	r := bufio.NewReader(os.Stdin)

	for ; tries > 0; tries-- {
		fmt.Printf("%s [y/n]: ", s)

		res, err := r.ReadString('\n')
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			return false
		}

		if len(res) < 2 {
			continue
		}

		res = strings.ToLower(strings.TrimSpace(res))

		if res == "y" || res == "yes" {
			return true
		} else if res == "n" || res == "no" {
			return false
		}
	}

	return false
}

func CompleteShortId(ctx gocontext.Context, ctr *containerd.Client, shortId string) (string, error) {
	if len(shortId) >= 64 {
		return shortId, nil
	}
	filters := []string{
		fmt.Sprintf("id~=^%s.*$", regexp.QuoteMeta(shortId)),
	}

	containers, err := ctr.Containers(ctx, filters...)
	if err != nil {
		return "", err
	}

	if len(containers) > 1 {
		return "", fmt.Errorf("ambiguous ID %q", shortId)
	}

	if len(containers) == 0 {
		return "", fmt.Errorf("no such container %s", shortId)
	}
	return containers[0].ID(), nil
}

func NewDefaultContainerdClient(context *cli.Context) (*containerd.Client, gocontext.Context, error) {
	cntdClient, err := containerd.New(context.String("address"),
		containerd.WithDefaultPlatform(platforms.Default()),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("init containerd connect failed.%s", err)
	}
	cntCtx := namespaces.WithNamespace(gocontext.Background(), context.String("namespace"))
	return cntdClient, cntCtx, nil
}
