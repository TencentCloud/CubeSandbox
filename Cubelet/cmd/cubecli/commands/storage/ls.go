// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"text/tabwriter"
	"time"

	"encoding/json"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	"github.com/urfave/cli/v2"
)

var (
	storageDir = "io.cubelet.internal.v1.storage"
	dbHandle   *utils.CubeStore
	dbDir      = "db"
)

var lsdb = &cli.Command{
	Name:  "ls",
	Usage: "ls storage info",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:    "bucket",
			Aliases: []string{"b"},
			Value:   "emptydir/v1",
			Usage:   "bucket name",
		},
		&cli.BoolFlag{
			Name:  "raw",
			Usage: "raw json",
		},
		&cli.BoolFlag{
			Name:  "each-raw",
			Usage: "each element raw json",
		},
	},
	Action: func(context *cli.Context) error {
		basePath := filepath.Join(context.String("state"), storageDir, "db")
		clean, err := copyDb(basePath)
		if err != nil {
			myPrint("fail copy db:%v", err)
			return err
		}
		defer clean()
		all, err := dbHandle.ReadAll(context.String("bucket"))
		if err != nil {
			myPrint("read db fail:%v", err)
			return nil
		}
		if context.Bool("raw") {
			for id, v := range all {
				fmt.Printf("%s\t%s\n", id, string(v))
			}
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 4, 8, 4, ' ', 0)
		tabHeader := "NS\tID\tFile\tSize"
		if context.Bool("each-raw") {
			tabHeader += "\tRAW"
		}
		fmt.Fprintln(w, tabHeader)
		for id, v := range all {
			if id == "cube_stub" {
				continue
			}
			bf := &storage.StorageInfo{}
			err = json.Unmarshal(v, bf)
			if err != nil {
				continue
			}
			for _, v := range bf.Volumes {
				row := fmt.Sprintf("%s\t%s\t%s\t%d", bf.Namespace, bf.SandboxID, v.FilePath, v.SizeLimit)
				if context.Bool("each-raw") {
					row += fmt.Sprintf("\t%v", v)
				}
				if _, err := fmt.Fprintf(w, "%s\n", row); err != nil {
					return err
				}
			}
		}
		return w.Flush()
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
		myPrint("failed to create temp dir: %v", er)
		return nil, er
	}

	cmds := [][]string{
		{"mkdir", "-p", targedir},
		{"ls", "-l", onlineBaseDir},
		{"cp", "-r", onlineBaseDir, targedir},
	}
	myPrint("cmds:%v", cmds)
	for _, cmd := range cmds {
		if out, stderr, err := utils.ExecV(cmd, time.Minute); err == nil {
			myPrint("storage copy: %v", out)
		} else {
			myPrint("storage copy: failed to exec %v: %v", cmd, err)
			return clean, fmt.Errorf("storage failed:%s", stderr)
		}
	}

	var err error
	if dbHandle, err = utils.NewCubeStoreExt(filepath.Join(targedir, dbDir), "meta.db", 10, nil); err != nil {
		myPrint("storage: failed to open db: %v", err)
		return clean, err
	}
	return clean, nil
}
