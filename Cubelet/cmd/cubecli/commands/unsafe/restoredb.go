// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package unsafe

import (
	gocontext "context"
	"fmt"
	"path/filepath"

	"github.com/containerd/containerd/v2/plugins"
	"github.com/urfave/cli/v2"

	"github.com/tencentcloud/CubeSandbox/Cubelet/cmd/cubecli/commands"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	srvconfig "github.com/tencentcloud/CubeSandbox/Cubelet/services/server/config"
)

var RestoreDB = &cli.Command{
	Name:  "restoredb",
	Usage: "restore metadata from backup db",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:    "config",
			Aliases: []string{"c"},
			Usage:   "path to the configuration file",
			Value:   "/usr/local/services/cubetoolbox/Cubelet/config/config.toml",
		},
	},
	Action: func(context *cli.Context) error {
		if !commands.AskForConfirm("restore will forcibly replace the containerd metadata db, continue only if you confirm", 3) {
			return nil
		}

		config := &srvconfig.Config{}
		if err := srvconfig.LoadConfig(gocontext.Background(), context.String("config"), config); err != nil {
			return err
		}

		backupPluginURI := fmt.Sprintf("%s.%s", constants.InternalPlugin, constants.BackupID.ID())
		boltPluginURI := fmt.Sprintf("%v.%v", plugins.MetadataPlugin, "bolt")

		backupFile := filepath.Join(config.Root, backupPluginURI, boltPluginURI, "meta.db")
		targetFile := filepath.Join(config.State, boltPluginURI, "meta.db")

		if exist, _ := utils.DenExist(backupFile); !exist {
			fmt.Printf("SKIP: backup file %s not exist\n", backupFile)
			return nil
		}

		return utils.SafeCopyFile(targetFile, backupFile)
	},
}
