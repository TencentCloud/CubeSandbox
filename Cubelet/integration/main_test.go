// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package integration

import (
	"context"
	"flag"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/nbi/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/version/v1"
	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/images/v1"
)

var (
	cubeClient    cubebox.CubeboxMgrClient
	imageClient   images.ImagesClient
	nbiClient     nbi.CubeLetClient
	versionClient version.VersionClient
)

var cubeletEndpoint = flag.String("endpoint", "0.0.0.0:9999", "The endpoint of cubelet")
var initBeforeTest = flag.Bool("init", false, "cube init before test")

func TestMain(m *testing.M) {
	flag.Parse()
	if err := ConnectDaemon(*cubeletEndpoint, 3*time.Second); err != nil {
		CubeLog.Fatalf("Failed to connect daemons: %v", err)
		os.Exit(1)
	}

	if *initBeforeTest {
		initReq := &nbi.InitRequest{
			RequestID: uuid.New().String(),
		}
		initCtx, initCancel := context.WithTimeout(context.Background(), 1*time.Minute)
		defer initCancel()
		resp, err := nbiClient.InitHost(initCtx, initReq)
		if err != nil {
			CubeLog.Fatalf("Failed to init host: %v, requestID %v", err, initReq.RequestID)
			os.Exit(1)
		}

		var extInfo string
		for k, v := range resp.ExtInfo {
			extInfo += fmt.Sprintf("%v: %vms ", k, string(v))
		}
		CubeLog.Infof("Init host success, requestID %v, extInfo: %v", initReq.RequestID, extInfo)
	}

	os.Exit(m.Run())
}

func ConnectDaemon(addr string, connTimeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), connTimeout)
	defer cancel()

	backoffConfig := backoff.DefaultConfig
	backoffConfig.MaxDelay = 3 * time.Second
	connParams := grpc.ConnectParams{
		Backoff: backoffConfig,
	}
	conn, err := grpc.DialContext(ctx, addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.FailOnNonTempDialError(true),
		grpc.WithConnectParams(connParams),
		grpc.WithReturnConnectionError(),
	)
	if err != nil {
		return fmt.Errorf("dial cubelet %v: %w", addr, err)
	}

	cubeClient = cubebox.NewCubeboxMgrClient(conn)
	imageClient = images.NewImagesClient(conn)
	nbiClient = nbi.NewCubeLetClient(conn)
	versionClient = version.NewVersionClient(conn)
	return nil
}
