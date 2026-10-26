// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package version

import (
	"fmt"
	"strings"

	api "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/version/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/cmd/cubecli/commands"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/version"
	"github.com/urfave/cli/v2"
	"google.golang.org/protobuf/types/known/emptypb"
)

var Command = &cli.Command{
	Name:  "version",
	Usage: "print the client and server versions",
	Flags: []cli.Flag{
		&cli.BoolFlag{
			Name:    "versiononly",
			Aliases: []string{"v"},
			Usage:   "print server version only",
		},
		&cli.BoolFlag{
			Name:    "withclient",
			Aliases: []string{"c"},
			Usage:   "print client version",
		},
	},
	Action: func(context *cli.Context) error {
		var buf strings.Builder
		if context.Bool("withclient") {
			buf.WriteString(version.ShowVersion() + "\n")
			buf.WriteString("Client:" + "\n")
			buf.WriteString("  Version: " + version.Version + "\n")
			buf.WriteString("  Revision :" + version.Revision + "\n")
			buf.WriteString("  Go version: " + version.GoVersion + "\n")
		}

		conn, ctx, cancel, err := commands.NewGrpcConn(context)
		if err != nil {
			return err
		}
		defer conn.Close()
		defer cancel()
		client := api.NewVersionClient(conn)
		v, err := client.Version(ctx, &emptypb.Empty{})
		if err != nil {
			return err
		}
		if context.Bool("versiononly") {
			vvs := strings.Split(v.Version, "-")
			buf.WriteString(vvs[0])
		} else {
			buf.WriteString("Server:" + "\n")
			buf.WriteString("  Version: " + v.Version + "\n")
			buf.WriteString("  Revision: " + v.Revision + "\n")
		}
		fmt.Println(buf.String())
		return nil
	},
}
