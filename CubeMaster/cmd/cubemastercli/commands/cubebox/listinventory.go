// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/google/uuid"
	jsoniter "github.com/json-iterator/go"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/urfave/cli"
)

var ListInventoryCommand = cli.Command{
	Name:    "listinventory",
	Usage:   "list inventory",
	Aliases: []string{"li"},
	Flags: []cli.Flag{
		cli.StringSliceFlag{
			Name:  "filter",
			Usage: "过滤条件,支持多个,格式:key=value,key=value,key=value",
		},
		cli.StringFlag{
			Name:  "type",
			Value: "cubebox",
			Usage: "instancetype,cubebox",
		},
	},
	Action: func(c *cli.Context) error {
		serverList = getServerAddrs(c)
		if len(serverList) == 0 {
			log.Printf("no server addr\n")
			return errors.New("no server addr")
		}
		port = c.GlobalString("port")

		requestID := uuid.New().String()
		host := serverList[rand.Int()%len(serverList)]

		filterlist := c.StringSlice("filter")
		filters := make(map[string][]string)
		for _, filter := range filterlist {
			labels := strings.TrimSpace(filter)
			if labels == "" {
				continue
			}
			kv := strings.Split(labels, "=")
			if len(kv) >= 2 {
				filters[kv[0]] = append(filters[kv[0]], kv[1])
			}
		}

		req := &types.ListInventoryReq{
			RequestID:    requestID,
			InstanceType: c.String("type"),
		}
		if len(filters) > 0 {
			for k, v := range filters {
				req.Filters = append(req.Filters, &types.FilterItem{
					Name:   k,
					Values: v,
				})
			}
		}
		reqEn, _ := jsoniter.Marshal(req)
		url := fmt.Sprintf("http://%s/cube/listinventory", net.JoinHostPort(host, port))
		rsp := &types.ListInventoryRes{}
		err := doHttpReq(c, url, http.MethodPost, requestID, bytes.NewBuffer(reqEn), rsp)
		if err != nil {
			log.Printf("list_getBodyData err. %s. RequestId: %s\n", err.Error(), requestID)
			return err
		}
		if rsp.Ret.RetCode != 200 {
			log.Printf("rsp err. %s. RequestId: %s\n", rsp.Ret.RetMsg, requestID)
			return errors.New(rsp.Ret.RetMsg)
		}

		w := tabwriter.NewWriter(os.Stdout, 4, 8, 4, ' ', 0)
		fmt.Fprintln(w, "Zone\tCpuType\tCPU\tMemory")
		for _, res := range rsp.Data {
			fmt.Fprintf(w, "%s\t%s\t%d\t%d\n", res.Zone, res.CPUType, res.CPU, res.Memory)
		}

		return w.Flush()
	},
}
