// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"text/tabwriter"

	"github.com/tencentcloud/CubeSandbox/Cubelet/cmd/cubecli/commands"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	"github.com/urfave/cli/v2"
	bolt "go.etcd.io/bbolt"
)

var (
	defaulFormat = []string{"512Mi", "others"}
)
var cleanup = &cli.Command{
	Name:  "cleanup",
	Usage: "cleanup blk files",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:    "bucket",
			Aliases: []string{"b"},
			Value:   "emptydir/v1",
			Usage:   "bucket name",
		},
		&cli.StringSliceFlag{
			Name:  "format",
			Usage: "format such as \"512Mi\", \"1Gi\",\"others\"",
		},
		&cli.BoolFlag{
			Name:  "printall",
			Usage: "print all result",
		},
	},
	Action: func(context *cli.Context) error {

		dbFiles := readDbs(context)

		poolDefaultFormatSizeList := context.StringSlice("format")
		if poolDefaultFormatSizeList == nil {
			poolDefaultFormatSizeList = defaulFormat
		}

		allformatFiles := make(map[string]map[string]struct{})
		for _, s := range poolDefaultFormatSizeList {
			baseFormatPath := filepath.Join("/data/cubelet/storage", storageDir, "emptydir", s)
			allFiles := readDir(baseFormatPath)
			allformatFiles[s] = allFiles
		}
		w := tabwriter.NewWriter(os.Stdout, 4, 8, 4, ' ', 0)
		fmt.Fprintln(w, "Format\tFile\tInDB\tID")

		needClean := []string{}
		for ft, v := range allformatFiles {
			for file, _ := range v {
				inDB := false
				sandboxID := ""
				if info, ok := dbFiles[file]; ok {
					inDB = true
					sandboxID = info.SandboxID
				}
				if context.Bool("printall") {
					if _, err := fmt.Fprintf(w, "%s\t%s\t%v\t%s\n", ft, file, inDB, sandboxID); err != nil {
						return err
					}
				} else if inDB {
					if _, err := fmt.Fprintf(w, "%s\t%s\t%v\t%s\n", ft, file, inDB, sandboxID); err != nil {
						return err
					}
				}
				if !inDB {
					needClean = append(needClean, file)
				}
			}
		}
		w.Flush()
		if !commands.AskForConfirm("init will destroy ALL of the resource above, continue only if you confirm", 3) {
			return nil
		}
		for _, file := range needClean {
			if err := os.RemoveAll(file); err != nil {
				return err
			}
		}
		return nil
	},
}

func readDbs(context *cli.Context) map[string]*storage.StorageInfo {
	basePath := filepath.Join(context.String("state"), storageDir, "db")
	opt := utils.MakeBoltDBOption()
	opt.ReadOnly = true
	log.Printf("basePath: %s", basePath)
	cs, err := utils.NewCubeStoreExt(basePath, "meta.db", 10, opt)
	if err != nil {
		if err == bolt.ErrTimeout {
			err = fmt.Errorf("should stop cubelet first,err:%v", err)
		}
		log.Printf("fail:%v", err)
		return map[string]*storage.StorageInfo{}
	}
	all, err := cs.ReadAll(context.String("bucket"))
	if err != nil {
		log.Printf("read db fail:%v", err)
		return map[string]*storage.StorageInfo{}
	}
	if err != nil {
		return map[string]*storage.StorageInfo{}
	}

	dirtyList := make(map[string]*storage.StorageInfo)
	for sandBoxID, v := range all {
		if sandBoxID == "cube_stub" {
			continue
		}
		bf := &storage.StorageInfo{}
		err = json.Unmarshal(v, bf)
		if err != nil {
			continue
		}
		for _, v := range bf.Volumes {
			if fileExist, _ := utils.DenExist(v.FilePath); fileExist {
				dirtyList[v.FilePath] = bf
			}
		}
	}
	return dirtyList
}

func readDir(baseFormatPath string) map[string]struct{} {
	log.Printf("basePath: %s", baseFormatPath)
	all := map[string]struct{}{}
	denList, err := os.ReadDir(baseFormatPath)
	if err != nil {
		return all
	}
	for _, den := range denList {
		if den.IsDir() {
			baseDirList, err := os.ReadDir(path.Clean(filepath.Join(baseFormatPath, den.Name())))
			if err != nil {
				continue
			}
			for _, bdir := range baseDirList {
				if !bdir.IsDir() {
					filePath := path.Join(path.Clean(filepath.Join(baseFormatPath, den.Name())), bdir.Name())
					all[filePath] = struct{}{}
				}
			}
			continue
		}
		filePath := path.Join(baseFormatPath, den.Name())

		all[filePath] = struct{}{}
	}
	return all
}
