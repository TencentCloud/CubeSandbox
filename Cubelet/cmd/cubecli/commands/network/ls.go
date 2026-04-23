// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package network

import (
	gocontext "context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/cmd/cubecli/commands"
	"github.com/tencentcloud/CubeSandbox/Cubelet/network/proto"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	networkstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/network"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow/provider"
	srvconfig "github.com/tencentcloud/CubeSandbox/Cubelet/services/server/config"
	"github.com/urfave/cli/v2"
)

var (
	dbDir      = "db"
	bucketName = "network/v1"
	dbHandle   *utils.CubeStore
)

const cmdTimeout = time.Second * 3

var list = &cli.Command{
	Name:  "ls",
	Usage: "ls all network(tap)",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:    "config",
			Aliases: []string{"c"},
			Usage:   "path to the configuration file",
			Value:   "/usr/local/services/cubetoolbox/Cubelet/config/config.toml",
		},
	},
	Action: func(clictx *cli.Context) error {
		config := &srvconfig.Config{}
		if err := srvconfig.LoadConfig(gocontext.Background(), clictx.String("config"), config); err != nil {
			return err
		}
		networkPluginURI := fmt.Sprintf("%s.%s", constants.InternalPlugin, constants.NetworkID.ID())
		baseDBDir := filepath.Join(config.State, networkPluginURI, dbDir)
		if !commands.AskForConfirm(
			fmt.Sprintf("will read all dbs from %s, continue only if you confirm", baseDBDir), 3) {
			return nil
		}
		clean, err := copyDb(baseDBDir)
		if err != nil {
			log.Printf("network: failed to copy dbs: %v", err)
			return err
		}
		if clean != nil {
			defer clean()
		}

		all, err := dbHandle.ReadAll(bucketName)
		if err != nil {
			return err
		}

		for id, netBytes := range all {
			var net networkstore.NetworkAllocation
			if err := json.Unmarshal(netBytes, &net); err != nil {
				return err
			}
			if id != net.SandboxID {
				log.Printf("[fatal]: id not match, %s, %s", id, net.SandboxID)
				continue
			}
			var metadata provider.NetworkProvider
			switch net.NetworkType {
			case cubebox.NetworkType_tap.String():
				metadata = &proto.ShimNetReq{}
			default:
				return fmt.Errorf("unknown instance type %s", net.NetworkType)
			}
			metadata.FromPersistMetadata(net.PersistentMetadata)
			net.Metadata = metadata

			fmt.Printf("%s|%d|%s|%s|%v\n", net.SandboxID, net.AppID, net.NetworkType, string(net.PersistentMetadata), time.Unix(net.Timestamp, 0).Format(time.DateTime))
		}

		return nil
	},
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
		if out, stderr, err := utils.ExecV(cmd, cmdTimeout); err == nil {
			log.Printf("network: %v", out)
		} else {
			log.Printf("network: failed to exec %v: %v", cmd, err)
			return clean, fmt.Errorf("network failed:%s", stderr)
		}
	}

	var err error
	if dbHandle, err = utils.NewCubeStoreExt(filepath.Join(targedir, dbDir), "meta.db", 10, nil); err != nil {
		log.Printf("network: failed to open db: %v", err)
		return clean, err
	}
	return clean, nil
}
