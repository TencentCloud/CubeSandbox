// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	jsoniter "github.com/json-iterator/go"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/urfave/cli"
)

var InfoCommand = cli.Command{
	Name:  "info",
	Usage: "info sandboxes",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "sandboxid,s",
			Usage: "与hostid二选一,也可同时指定,单意义不大,sandbox决定了在那个host上",
		},
		cli.StringFlag{
			Name:  "hostid,t",
			Usage: "与sandboxid二选一,也可同时指定,只查某个实例",
		},
		cli.BoolFlag{
			Name:  "old",
			Usage: "/cube/sandbox/info 旧接口用法",
		},
		cli.IntFlag{
			Name:  "containerport,p",
			Usage: "可选，查询 exposed port 时传入目标 container port",
		},
		cli.StringFlag{
			Name:  "callerhostip",
			Usage: "可选，模拟 cube proxy 所在节点 HostIP，用于选择 tap ip 或 host port",
		},
	},
	Action: func(c *cli.Context) error {
		hostID := c.String("hostid")
		sandboxID := c.String("sandboxid")
		if hostID == "" && sandboxID == "" {
			return errors.New("hostid和sandboxid不能同时为空")
		}

		serverList = getServerAddrs(c)
		if len(serverList) == 0 {
			log.Printf("no server addr\n")
			return errors.New("no server addr")
		}
		port = c.GlobalString("port")

		requestID := uuid.New().String()
		host := serverList[rand.Int()%len(serverList)]
		containerPort := c.Int("containerport")

		url := ""
		var body io.Reader
		if c.Bool("old") {

			req := &types.GetCubeSandboxReq{
				RequestID:     requestID,
				SandboxID:     sandboxID,
				HostID:        hostID,
				ContainerPort: int32(containerPort),
			}
			reqEn, _ := jsoniter.Marshal(req)
			body = bytes.NewBuffer(reqEn)
			url = fmt.Sprintf("http://%s/cube/sandbox/info", net.JoinHostPort(host, port))
		} else {
			if hostID != "" {
				url = fmt.Sprintf("http://%s/cube/sandbox/info?requestID=%s&host_id=%s", net.JoinHostPort(host, port), requestID, hostID)
			} else {
				url = fmt.Sprintf("http://%s/cube/sandbox/info?requestID=%s&sandbox_id=%s", net.JoinHostPort(host, port), requestID, sandboxID)
			}
			if containerPort > 0 {
				url = fmt.Sprintf("%s&container_port=%d", url, containerPort)
			}
		}

		rsp := &types.GetCubeSandboxRes{}
		err := doHttpReq(c, url, http.MethodGet, requestID, body, rsp)
		if err != nil {
			log.Printf("list_getBodyData err. %s. RequestId: %s\n", err.Error(), requestID)
			return err
		}
		if rsp.Ret.RetCode != 200 {
			log.Printf("rsp err. %s. RequestId: %s\n", rsp.Ret.RetMsg, requestID)
			return errors.New(rsp.Ret.RetMsg)
		}

		w := tabwriter.NewWriter(os.Stdout, 4, 8, 4, ' ', 0)
		for idx, sb := range rsp.Data {
			if idx > 0 {
				fmt.Fprintln(w)
			}
			printSandboxInfoBlock(w, sb)
		}
		return w.Flush()
	},
}

func printSandboxInfoBlock(w *tabwriter.Writer, sb *types.SandboxData) {
	fmt.Fprintf(w, "SANDBOX_ID\t%s\n", displayValue(sb.SandboxID))
	fmt.Fprintf(w, "STATUS\t%s\n", getStatus(sb.Status))
	fmt.Fprintf(w, "HOST_ID\t%s\n", displayValue(sb.HostID))
	fmt.Fprintf(w, "HOST_IP\t%s\n", displayValue(sb.HostIP))
	fmt.Fprintf(w, "SANDBOX_IP\t%s\n", displayValue(sb.SandboxIP))
	fmt.Fprintf(w, "TEMPLATE_ID\t%s\n", displayValue(sb.TemplateID))
	fmt.Fprintf(w, "NAMESPACE\t%s\n", displayValue(sb.NameSpace))
	fmt.Fprintf(w, "ANNOTATIONS\t%s\n", displayValue(utils.InterfaceToString(sb.Annotations)))
	fmt.Fprintf(w, "LABELS\t%s\n", displayValue(utils.InterfaceToString(sb.Labels)))
	if sb.ExposedPortEndpoint != "" {
		fmt.Fprintf(w, "EXPOSED_PORT_MODE\t%s\n", displayValue(sb.ExposedPortMode))
		fmt.Fprintf(w, "EXPOSED_ENDPOINT\t%s\n", displayValue(sb.ExposedPortEndpoint))
		fmt.Fprintf(w, "REQUESTED_CONTAINER_PORT\t%d\n", sb.RequestedContainerPort)
	}

	sort.Slice(sb.Containers, func(i, j int) bool {
		return sb.Containers[i].CreateAt < sb.Containers[j].CreateAt
	})
	fmt.Fprintf(w, "CONTAINERS\t%d\n", len(sb.Containers))
	fmt.Fprintln(w, "NAME\tCONTAINER\tIMAGE\tSTATUS\tCREATED\tCPU\tMEM\tCONTAINER_TYPE")
	for _, c := range sb.Containers {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			displayValue(c.Name), displayValue(c.ContainerID), displayValue(c.Image), getStatus(c.Status),
			formatTime(c.CreateAt), displayValue(c.Cpu), displayValue(c.Mem), displayValue(c.Type))
	}
}

func displayValue(v string) string {
	if v == "" || v == "null" {
		return "-"
	}
	return v
}

func formatTime(created int64) string {
	if created == 0 {
		return "-"
	}
	createdAt := time.Unix(0, created).Round(time.Second).Local()
	return createdAt.Format("2006-01-02 15:04:05")
}
