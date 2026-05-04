// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package unsafe

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
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/volumefile"
	srvconfig "github.com/tencentcloud/CubeSandbox/Cubelet/services/server/config"
	"github.com/urfave/cli/v2"
)

var (
	volumeDbDir    = "volumedb"
	bucketName     = "createInfo"
	volumedbHandle *utils.CubeStore
)

type volumeInfo struct {
	FileType   volumefile.FileType
	UserID     string
	FileSha256 string
}

type createInfo struct {
	VolumeInfos []volumeInfo
	Timestamp   int64
}

func (v *createInfo) String() string {
	str := utils.InterfaceToString(v.VolumeInfos)
	if v.Timestamp > 10000 {
		str += "|" + fmt.Sprintf("Timestamp: %v", time.Unix(v.Timestamp, 0))
	}
	return str
}

var volumedb = &cli.Command{
	Name:  "volumedb",
	Usage: "scan volumedb db ",
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
		volumePluginURI := fmt.Sprintf("%s.%s", constants.InternalPlugin, constants.VolumeSourceID.ID())
		baseDBDir := filepath.Join(config.State, volumePluginURI, volumeDbDir)
		if !commands.AskForConfirm(
			fmt.Sprintf("volumedb will read all dbs from %s, continue only if you confirm", baseDBDir), 3) {
			return nil
		}
		clean, err := copyDb(baseDBDir)
		if err != nil {
			log.Printf("volumedb: failed to copy dbs: %v", err)
			return err
		}
		if clean != nil {
			defer clean()
		}

		all, err := volumedbHandle.ReadAll(bucketName)
		if err != nil {
			log.Printf("ReadAll[%s]  fail:%v", bucketName, err)
			return err
		}

		allSandboxVolumes := map[string]*createInfo{}
		for k, v := range all {
			bf := &createInfo{}
			err = json.Unmarshal(v, bf)
			if err != nil {
				log.Printf("decode[%s]  fail:%v", k, err)
				continue
			}
			allSandboxVolumes[k] = bf
		}

		checkSandboxDirty(clictx, allSandboxVolumes)
		return nil
	},
}

func checkSandboxDirty(clictx *cli.Context, all map[string]*createInfo) {
	start := time.Now()
	conn, ctx, cancel, err := commands.NewGrpcConn(clictx)
	if err != nil {
		log.Printf("Failed to NewGrpcConn: %v", err)
		return
	}
	defer conn.Close()
	defer cancel()
	client := cubebox.NewCubeboxMgrClient(conn)

	cnt := 0
	for k, v := range all {
		req := &cubebox.ListCubeSandboxRequest{}
		req.Id = &k
		resp, err := client.List(ctx, req)
		if err != nil {
			log.Printf("Failed to List: %v", err)
			return
		}
		if len(resp.Items) == 0 {
			cnt++
			log.Printf("volumedb_db_dirty: %s:%s", k, v)
		}
	}
	log.Printf("volumedb scan done,total:%d,dirty:%d,cost:%v",
		len(all), cnt, time.Since(start))
}

func copyDb(onlineBaseDir string) (func(), error) {
	targedir := filepath.Join(os.TempDir(), volumeDbDir)
	if err := os.MkdirAll(path.Clean(targedir), os.ModeDir|0755); err != nil {
		return nil, fmt.Errorf("init dir failed %s", err.Error())
	}
	clean := func() {
		os.RemoveAll(path.Clean(targedir))
	}

	exist, er := utils.DenExist(targedir)
	if er != nil || !exist {
		log.Printf("volumedb: failed to create temp dir: %v", er)
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
			log.Printf("volumedb: %v", out)
		} else {
			log.Printf("volumedb: failed to exec %v: %v", cmd, err)
			return clean, fmt.Errorf("volumedb failed:%s", stderr)
		}
	}

	var err error
	if volumedbHandle, err = utils.NewCubeStoreExt(filepath.Join(targedir, volumeDbDir), "meta.db", 10, nil); err != nil {
		log.Printf("volumedb: failed to open lifetime db: %v", err)
		return clean, err
	}
	return clean, nil
}
