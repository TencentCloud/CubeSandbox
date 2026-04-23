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
	"strings"
	"text/tabwriter"

	"github.com/google/uuid"
	jsoniter "github.com/json-iterator/go"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/urfave/cli"
)

type listSummary struct {
	NodeScope    string
	NodesScanned int
	NodesTotal   int
	SandboxCount int
}

var ListCommand = cli.Command{
	Name:    "list",
	Aliases: []string{"ls"},
	Usage:   "list sandboxes",
	Flags: []cli.Flag{
		cli.IntFlag{
			Name:  "index,i",
			Value: 1,
			Usage: "与size组合填写,cube物理机列表(以db中的主键id作为排序)起始位置,从1开始,与hostid互斥关系",
		},
		cli.IntFlag{
			Name:  "size,s",
			Value: 1,
			Usage: "与index组合填写,本次请求遍历的主机列表个数,与hostid互斥关系",
		},
		cli.StringFlag{
			Name:  "hostid,t",
			Usage: "与(index,size)必填一种，互斥关系",
		},
		cli.BoolFlag{
			Name:  "old",
			Usage: "/cube/sandbox/info 旧接口用法",
		},
		cli.StringSliceFlag{
			Name:  "filter",
			Usage: "过滤条件,支持多个,格式:key=value,key=value,key=value",
		},
		cli.StringFlag{
			Name:  "type",
			Value: "cubebox",
			Usage: "instancetype,cubebox",
		},
		cli.BoolFlag{
			Name:  "delete",
			Usage: "是否删除,必须与hostid配合使用",
		},
		cli.BoolFlag{
			Name:  "quiet, q",
			Usage: "print only the container id",
		},
		cli.BoolFlag{
			Name:  "wide,w",
			Usage: "display more detailed info",
		},
		cli.BoolFlag{
			Name:  "all",
			Usage: "scan all healthy nodes across the cluster",
		},
		cli.StringFlag{
			Name:  "sandboxid",
			Usage: "sandbox id",
		},
	},
	Action: func(c *cli.Context) error {
		hostID := c.String("hostid")
		startIdx := c.Int("index")
		size := c.Int("size")
		all := c.Bool("all")

		if all && hostID != "" {
			return errors.New("all flag cannot be used with hostid")
		}

		if hostID == "" && (startIdx == 0 || size == 0) {
			return errors.New("hostid和(start_idx、size)至少填写一种")
		}

		serverList = getServerAddrs(c)
		if len(serverList) == 0 {
			log.Printf("no server addr\n")
			return errors.New("no server addr")
		}
		port = c.GlobalString("port")

		requestID := uuid.New().String()
		host := serverList[rand.Int()%len(serverList)]

		quiet := c.Bool("quiet")
		delete := c.Bool("delete")
		sandboxID := c.String("sandboxid")
		if delete && (hostID == "" || !quiet) {
			return errors.New("delete flag must be used with hostid and quiet flag")
		}
		if delete && all {
			return errors.New("delete flag cannot be used with all flag")
		}

		filters, filterList := parseListFilters(c.StringSlice("filter"))
		req := &types.ListCubeSandboxReq{
			RequestID:    requestID,
			HostID:       hostID,
			StartIdx:     startIdx,
			Size:         size,
			InstanceType: c.String("type"),
		}
		if len(filters) > 0 {
			req.Filter = &types.CubeSandboxFilter{
				LabelSelector: filters,
			}
		}

		rsp, summary, err := runListQuery(c, host, req, filterList, all)
		if err != nil {
			log.Printf("list_getBodyData err. %s. RequestId: %s\n", err.Error(), requestID)
			return err
		}
		if rsp.Ret == nil {
			return errors.New("empty response")
		}
		if rsp.Ret.RetCode != 200 {
			log.Printf("rsp err. %s. RequestId: %s\n", rsp.Ret.RetMsg, requestID)
			return errors.New(rsp.Ret.RetMsg)
		}

		if quiet {
			for _, sandbox := range rsp.Data {
				if delete {
					if sandboxID != "" && sandbox.SandboxID != sandboxID {
						continue
					}
					err = doInnerDestroySandbox(c, sandbox.SandboxID, sandbox.Labels, c.String("type"))
					if err != nil {
						log.Printf("doDestroySandbox err. %s. RequestId: %s\n", err.Error(), requestID)
					}
					log.Printf("doDestroySandbox success: %s\n", sandbox.SandboxID)
				} else {
					fmt.Println(sandbox.SandboxID)
				}
			}
			return nil
		}

		w := tabwriter.NewWriter(os.Stdout, 4, 8, 4, ' ', 0)
		fmt.Fprintf(w, "NODE_SCOPE\t%s\n", summary.NodeScope)
		fmt.Fprintf(w, "NODES_SCANNED\t%d/%d\n", summary.NodesScanned, summary.NodesTotal)
		fmt.Fprintf(w, "SANDBOX_COUNT\t%d\n", summary.SandboxCount)
		fmt.Fprintln(w)
		tabHeader := "sandbox_id\tstatus\thost_id\tcreate_at\tpause_at"
		if c.Bool("wide") {
			tabHeader += "\ttemplate_id\tnamespace\thost_ip\tlabels"
		}
		sort.Slice(rsp.Data, func(i, j int) bool {
			return rsp.Data[i].CreateAt < rsp.Data[j].CreateAt
		})
		fmt.Fprintln(w, tabHeader)
		for _, sandbox := range rsp.Data {
			row := fmt.Sprintf("%s\t%s\t%s\t%s\t%s", sandbox.SandboxID,
				getStatus(sandbox.Status), sandbox.HostID, formatTime(sandbox.CreateAt), formatTime(sandbox.PauseAt))
			if c.Bool("wide") {
				row += fmt.Sprintf("\t%s\t%s\t%s\t%s", sandbox.TemplateID, sandbox.NameSpace, sandbox.HostIP,
					utils.InterfaceToString(sandbox.Labels))
			}
			if _, err := fmt.Fprintln(w, row); err != nil {
				return err
			}
		}
		return w.Flush()
	},
}

func runListQuery(c *cli.Context, host string, req *types.ListCubeSandboxReq, filterList []string, all bool) (
	*types.ListCubeSandboxRes, *listSummary, error) {
	if all {
		return runListQueryAllPages(c, host, req, filterList)
	}

	rsp, err := doListRequest(c, host, req, filterList)
	if err != nil {
		return nil, nil, err
	}
	return rsp, buildListSummary(req, rsp, false), nil
}

func runListQueryAllPages(c *cli.Context, host string, req *types.ListCubeSandboxReq, filterList []string) (
	*types.ListCubeSandboxRes, *listSummary, error) {
	pageReq := *req
	pageReq.HostID = ""
	if pageReq.StartIdx <= 0 {
		pageReq.StartIdx = 1
	}
	if pageReq.Size <= 0 {
		pageReq.Size = 1
	}

	aggregated := &types.ListCubeSandboxRes{
		RequestID: req.RequestID,
		Ret:       &types.Ret{RetCode: 200, RetMsg: "OK"},
	}
	for {
		rsp, err := doListRequest(c, host, &pageReq, filterList)
		if err != nil {
			return nil, nil, err
		}
		if rsp.Ret == nil {
			return nil, nil, errors.New("empty response")
		}
		if rsp.Ret.RetCode != 200 {
			return rsp, nil, nil
		}

		aggregated.Total = rsp.Total
		aggregated.EndIdx = rsp.EndIdx
		aggregated.Size += rsp.Size
		aggregated.Data = append(aggregated.Data, rsp.Data...)

		if rsp.Total == 0 || rsp.Size == 0 || rsp.EndIdx == 0 || rsp.EndIdx >= rsp.Total {
			break
		}
		pageReq.StartIdx = rsp.EndIdx + 1
	}
	return aggregated, buildListSummary(&pageReq, aggregated, true), nil
}

func doListRequest(c *cli.Context, host string, req *types.ListCubeSandboxReq, filterList []string) (
	*types.ListCubeSandboxRes, error) {
	url, body, err := buildListRequest(c, host, req, filterList)
	if err != nil {
		return nil, err
	}
	rsp := &types.ListCubeSandboxRes{}
	if err := doHttpReq(c, url, http.MethodGet, req.RequestID, body, rsp); err != nil {
		return nil, err
	}
	return rsp, nil
}

func buildListRequest(c *cli.Context, host string, req *types.ListCubeSandboxReq, filterList []string) (string, io.Reader, error) {
	if !c.Bool("old") {

		reqEn, err := jsoniter.Marshal(req)
		if err != nil {
			return "", nil, err
		}
		url := fmt.Sprintf("http://%s/cube/sandbox/list", net.JoinHostPort(host, port))
		return url, bytes.NewBuffer(reqEn), nil
	}

	url := fmt.Sprintf("http://%s/cube/sandbox/list?requestID=%s", net.JoinHostPort(host, port), req.RequestID)
	if req.HostID != "" {
		url += "&host_id=" + req.HostID
	} else {
		url += fmt.Sprintf("&start_idx=%d&size=%d", req.StartIdx, req.Size)
	}
	if len(filterList) > 0 {
		url += "&filter.label_selector=" + strings.Join(filterList, ",")
	}
	if req.InstanceType != "" {
		url += "&instance_type=" + req.InstanceType
	}
	return url, nil, nil
}

func parseListFilters(filters []string) (map[string]string, []string) {
	parsed := make(map[string]string)
	normalized := make([]string, 0, len(filters))
	for _, filter := range filters {
		labels := strings.TrimSpace(filter)
		if labels == "" {
			continue
		}
		kv := strings.Split(labels, "=")
		if len(kv) >= 2 {
			parsed[kv[0]] = kv[1]
			normalized = append(normalized, labels)
		}
	}
	return parsed, normalized
}

func buildListSummary(req *types.ListCubeSandboxReq, rsp *types.ListCubeSandboxRes, all bool) *listSummary {
	summary := &listSummary{
		NodesScanned: rsp.Size,
		NodesTotal:   rsp.Total,
		SandboxCount: len(rsp.Data),
	}
	switch {
	case req.HostID != "":
		summary.NodeScope = "host:" + req.HostID
	case all:
		summary.NodeScope = "all"
	default:
		start := req.StartIdx
		if start <= 0 {
			start = 1
		}
		end := rsp.EndIdx
		if end <= 0 && rsp.Size > 0 {
			end = start + rsp.Size - 1
		}
		if rsp.Size == 0 || end < start {
			summary.NodeScope = fmt.Sprintf("%d-empty", start)
			return summary
		}
		summary.NodeScope = fmt.Sprintf("%d-%d", start, end)
	}
	return summary
}

func getStatus(s int32) string {
	switch int(s) {
	case 0:
		return "created"
	case 1:
		return "running"
	case 2:
		return "exited"
	case 3:
		return "unknow"
	case 4:
		return "pausing"
	case 5:
		return "paused"
	default:
		return "unknow"
	}
}
