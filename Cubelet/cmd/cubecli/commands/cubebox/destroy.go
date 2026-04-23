// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"fmt"
	"log"
	"strings"

	"github.com/google/uuid"
	"github.com/urfave/cli/v2"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/cmd/cubecli/commands"
)

var Destroy = &cli.Command{
	Name:    "destroy",
	Usage:   "destroy cubebox",
	Aliases: []string{"rm"},
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:    "name",
			Aliases: []string{"n"},
			Usage:   "sandboxId",
		},
		&cli.StringFlag{
			Name:  "annotation",
			Usage: "k=v",
		},
		&cli.BoolFlag{
			Name: "all",
			Aliases: []string{
				"a",
			},
			Usage: "destroy all",
		},
		&cli.BoolFlag{
			Name:    "force",
			Aliases: []string{"f"},
			Usage:   "skip list,just do destroy",
		},
	},
	Action: func(context *cli.Context) error {
		conn, _, _, err := commands.NewGrpcConn(context)
		if err != nil {
			return err
		}
		defer conn.Close()
		client := cubebox.NewCubeboxMgrClient(conn)
		var ids []string
		args := context.Args().Slice()
		if len(args) > 0 {
			ids = args
		} else if context.Bool("all") {
			ctx, cancel := commands.AppContext(context)
			defer cancel()

			req := &cubebox.ListCubeSandboxRequest{}
			resp, err := client.List(ctx, req)
			if err != nil {
				return err
			}
			for _, cnt := range resp.Items {
				ids = append(ids, cnt.GetId())
			}
		} else if context.IsSet("name") {
			ids = append(ids, context.String("name"))
		}
		if context.Bool("force") {
			for _, id := range ids {
				if err = destroy(context, &cubebox.DestroyCubeSandboxRequest{
					RequestID: uuid.New().String(),
					SandboxID: strings.TrimSpace(id),
				}, client); err != nil {
					return err
				}
			}
			return nil
		}

		if len(ids) <= 0 {
			log.Printf("should provide sandboxid")
			return fmt.Errorf("should provide sandboxid")
		}
		tmpAnnation := make(map[string]string)
		if context.IsSet("annotation") {
			kv := context.String("annotation")
			m := strings.Split(kv, "=")
			tmpAnnation[m[0]] = m[1]
		}

		ctx, cancel := commands.AppContext(context)
		defer cancel()
		req := &cubebox.ListCubeSandboxRequest{}
		resp, err := client.List(ctx, req)
		if err != nil {
			log.Printf("list sandbox error:%v", err)
			return err
		}
		if len(resp.Items) == 0 {
			log.Printf("no any sandbox exits")
			return nil
		}

		for _, item := range resp.Items {
			for _, id := range ids {
				id := strings.TrimSpace(id)
				if len(item.GetContainers()) == 0 {
					log.Printf("Warning sandbox:%s has no container", item.GetId())
					continue
				}

				if strings.HasPrefix(item.GetId(), id) || strings.HasPrefix(item.GetContainers()[0].GetId(), id) {
					if err = destroy(context, &cubebox.DestroyCubeSandboxRequest{
						RequestID:   uuid.New().String(),
						SandboxID:   item.GetId(),
						Annotations: tmpAnnation,
					}, client); err == nil {
						break
					}

				}
			}
		}
		return nil
	},
}

var DestroyAll = &cli.Command{
	Name:  "destroyall",
	Usage: "destroy all cubebox",
	Flags: []cli.Flag{
		&cli.BoolFlag{
			Name:    "force",
			Aliases: []string{"f"},
			Usage:   "remove all",
		},
	},
	Action: func(context *cli.Context) error {
		log.Printf("cubecli destroyall is deprecated, please use: cubecli unsafe rm --all")
		if !commands.AskForConfirm("will destroy ALL of the container, continue only if you confirm", 3) {
			return nil
		}

		conn, ctx, cancel, err := commands.NewGrpcConn(context)
		if err != nil {
			return err
		}
		defer conn.Close()
		defer cancel()
		client := cubebox.NewCubeboxMgrClient(conn)

		req := &cubebox.ListCubeSandboxRequest{}

		resp, err := client.List(ctx, req)
		if err != nil {
			return err
		}

		sandboxes := resp.Items

		if len(sandboxes) == 0 {
			log.Printf("no any Containers")
			return nil
		}
		log.Printf("Sandboxes:%d", len(sandboxes))

		tmpAnnation := make(map[string]string)
		if context.IsSet("annotation") {
			kv := context.String("annotation")
			m := strings.Split(kv, "=")
			tmpAnnation[m[0]] = m[1]
		}

		for _, cnt := range sandboxes {
			if len(cnt.Containers) == 0 {
				continue
			}
			sb := cnt.Containers[0]
			if !context.Bool("force") {
				if sb.GetState() != cubebox.ContainerState_CONTAINER_RUNNING {
					continue
				}
			}
			destroy(context, &cubebox.DestroyCubeSandboxRequest{
				RequestID:   uuid.New().String(),
				SandboxID:   cnt.GetId(),
				Annotations: tmpAnnation,
			}, client)
		}
		return nil
	},
}

func destroy(context *cli.Context, req *cubebox.DestroyCubeSandboxRequest, client cubebox.CubeboxMgrClient) error {
	log.Printf("destroy sandbox: %s", req.GetSandboxID())
	ctx, cancel := commands.AppContext(context)
	defer cancel()
	rsp, err := client.Destroy(ctx, req)
	if err != nil {
		log.Printf("destroy failure:%v", err)
		return err
	}
	log.Printf("destroy rsp:%+v", rsp)
	return nil
}
