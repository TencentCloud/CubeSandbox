// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package metadata

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/google/uuid"
	"github.com/urfave/cli/v2"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/multimetadb/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/cmd/cubecli/commands"
)

var ShowDbCommand = cli.Command{
	Name:  "dbs",
	Usage: "show all bucket name of db",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:  "o",
			Usage: "output format",
			Value: "json",
		},
	},
	Action: func(context *cli.Context) error {
		conn, ctx, cancel, err := commands.NewGrpcConn(context)
		if err != nil {
			return err
		}
		defer conn.Close()
		defer cancel()
		client := multimetadb.NewMultiMetaDBServerClient(conn)
		resp, err := client.GetBucketDefines(ctx, &multimetadb.CommonRequestHeader{
			RequestID: uuid.New().String(),
		})
		if err != nil {
			return err
		}

		switch context.String("o") {
		case "json":
			data, err := json.MarshalIndent(resp.BucketDefines, "", "  ")
			if err != nil {
				return fmt.Errorf("marshal json failed %s", err.Error())
			}
			fmt.Println(string(data))
		default:
			return fmt.Errorf("unsupported output format %s", context.String("o"))
		}
		return nil
	},
}

var ListDbCommand = cli.Command{
	Name:  "view",
	Usage: "view db data",
	ArgsUsage: "[flags] [<bucket>, ...]\n" +
		"-key abc erofsimage ziyan \n" +
		"-db cgroup -key sandboxidxxx1 sandbox \n",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:  "db",
			Usage: "db name, this is required to access standlan db",
		},
		&cli.StringFlag{
			Name:  "key",
			Usage: "key of data, if empty, list all data",
		},
		&cli.StringFlag{
			Name:  "o",
			Usage: "output format",
			Value: "json",
		},
	},
	Action: func(context *cli.Context) error {
		var (
			args = context.Args()
		)
		conn, ctx, cancel, err := commands.NewGrpcConn(context)
		if err != nil {
			return err
		}
		defer conn.Close()
		defer cancel()
		client := multimetadb.NewMultiMetaDBServerClient(conn)
		req := &multimetadb.GetDataRequest{
			Header: &multimetadb.CommonRequestHeader{
				RequestID: uuid.New().String(),
			},
			Buckets: args.Slice(),
			Key:     context.String("key"),
			DbName:  context.String("db"),
		}
		listStream, err := client.GetStreamData(ctx, req)
		if err != nil {
			return fmt.Errorf("get stream data failed %s", err.Error())
		}
		for {
			var resp = &multimetadb.DbData{}
			err = listStream.RecvMsg(resp)
			if errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				return fmt.Errorf("recv msg failed %s", err.Error())
			}

			switch context.String("o") {
			case "json":
				dataWriter := map[string]interface{}{
					"db":      req.DbName,
					"buckets": resp.Buckets,
					"key":     resp.Key,
					"value":   string(resp.Value),
				}

				data, err := json.MarshalIndent(dataWriter, "", "  ")
				if err != nil {
					return fmt.Errorf("marshal json failed %s", err.Error())
				}
				fmt.Println(string(data))
			default:
				return fmt.Errorf("unsupported output format %s", context.String("o"))
			}
		}

		return nil
	},
}
