// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"fmt"
	"log"
	"strings"

	"context"
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/platforms"
	"github.com/urfave/cli/v2"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/cmd/cubecli/commands"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cubes"
)

var (
	cubeboxDir = "io.cubelet.internal.v1.cubebox"
)

var (
	dbDir    = "db"
	dbHandle *utils.CubeStore
)

type sandBoxInfo struct {
	SandboxID  string
	IP         string
	PID        int
	Namespace  string
	Containers map[string]*containerInfo
}

type containerInfo struct {
	ID  string
	PID int
}

func copyDb(onlineBaseDir string) (func(), error) {
	targedir := filepath.Join(os.TempDir(), dbDir)
	if err := os.MkdirAll(path.Clean(targedir), os.ModeDir|0755); err != nil {
		return nil, fmt.Errorf("init dir failed %s", err.Error())
	}
	clean := func() {
		os.RemoveAll(path.Clean(targedir))
	}

	exist, er := utils.DenExist(targedir)
	if er != nil || !exist {
		log.Printf("failed to create temp dir: %v", er)
		return nil, er
	}

	cmds := [][]string{
		{"mkdir", "-p", targedir},
		{"ls", "-l", onlineBaseDir},
		{"cp", "-r", onlineBaseDir, targedir},
	}
	log.Printf("cmds:%v", cmds)
	for _, cmd := range cmds {
		if out, stderr, err := utils.ExecV(cmd, time.Minute); err == nil {
			log.Printf("metadata: %v", out)
		} else {
			log.Printf("metadata: failed to exec %v: %v", cmd, err)
			return clean, fmt.Errorf("metadata failed:%s", stderr)
		}
	}

	var err error
	if dbHandle, err = utils.NewCubeStoreExt(filepath.Join(targedir, dbDir), "meta.db", 10, nil); err != nil {
		log.Printf("metadata: failed to open db: %v", err)
		return clean, err
	}
	return clean, nil
}

var inspecMetaData = cli.Command{
	Name:      "inspect",
	Aliases:   []string{"i", "info"},
	Usage:     "stat metadata of cubebox.",
	ArgsUsage: "CUBEBOX-ID [CUBEBOX-ID ...]",
	Action: func(context *cli.Context) error {
		var ids []string
		if context.Args().Len() > 0 {
			ids = context.Args().Slice()
		}
		if len(ids) == 0 {
			return fmt.Errorf("cubebox id is required")
		}

		conn, ctx, cancel, err := commands.NewGrpcConn(context)
		if err != nil {
			return err
		}
		defer conn.Close()
		defer cancel()
		client := cubebox.NewCubeboxMgrClient(conn)

		var boxIDs []string
		req := &cubebox.ListCubeSandboxRequest{}
		resp, err := client.List(ctx, req)
		if err != nil {
			return err
		}
		for _, id := range ids {
			found := false
			for _, item := range resp.Items {
				if strings.HasPrefix(item.GetId(), id) {
					boxIDs = append(boxIDs, item.GetId())
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("cubebox %s not found", id)
			}
		}

		for _, id := range boxIDs {
			req := &cubebox.ListCubeSandboxRequest{
				Id: &id,
				Option: &cubebox.ListCubeSandboxOption{
					PrivateWithCubeboxStore: true,
				},
			}
			resp, err := client.List(ctx, req)
			if err != nil {
				return err
			}
			for _, item := range resp.Items {
				if len(item.GetPrivateCubeboxStorageData()) == 0 {
					continue
				}
				fmt.Println(string(item.GetPrivateCubeboxStorageData()))
			}
		}
		return nil
	},
}

var listMetaData = cli.Command{
	Name:    "listmd",
	Aliases: []string{"lsmd"},
	Usage:   "list metadata",
	Action: func(clictx *cli.Context) error {
		cntdClient, err := containerd.New(clictx.String("address"),
			containerd.WithDefaultPlatform(platforms.Default()),
		)
		if err != nil {
			return fmt.Errorf("init containerd connect failed.%s", err)
		}

		clean, err := copyDb(filepath.Join(clictx.String("state"), cubeboxDir, dbDir))
		if err != nil {
			log.Printf("fail copy db:%v", err)
			return err
		}
		defer clean()

		all, err := dbHandle.ReadAll("sandbox/v1")
		if err != nil {
			log.Printf("fail:%v", err)
			return nil
		}
		for id, sandboxBytes := range all {
			var cb = new(cubeboxstore.CubeBox)
<<<<<<< HEAD
			if err := json.Unmarshal(sandboxBytes, cb); err != nil {
				myPrint("failed to unmarshal to cubebox %s from meta: %v", id, err)
=======
			if err := jsoniter.Unmarshal(sandboxBytes, cb); err != nil {
				log.Printf("failed to unmarshal to cubebox %s from meta: %v", id, err)
>>>>>>> upstream/master
				continue
			}

			old := utils.InterfaceToString(cb)

			if cb.Namespace == "" {
				cb.Namespace = namespaces.Default
			}
			podCtx := namespaces.WithNamespace(context.TODO(), cb.Namespace)
			podCtx = context.WithValue(podCtx, constants.CubeboxID, cb.ID)
			if err := cubes.RecoverPod(podCtx, cntdClient, cb); err != nil {
				log.Printf("failed to recover pod: %v", err)
				continue
			}
			if cb.GetStatus().IsTerminated() {
				log.Printf("sandbox %s is terminating, skip", cb.ID)
				log.Printf("load sandbox %v", old)
			}
			log.Printf("Loaded sandbox %v", utils.InterfaceToString(&cb))
		}

		return nil
	},
}
