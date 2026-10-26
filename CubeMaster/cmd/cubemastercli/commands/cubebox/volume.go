// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/cmd/cubemastercli/commands"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/urfave/cli"
)

// volumeWireItem mirrors CubeMaster VolumeItem JSON. Token may be present in
// the HTTP response but is never shown by this CLI (same for private_data,
// which the API does not return).
type volumeWireItem struct {
	VolumeID  string `json:"volumeID"`
	Name      string `json:"name"`
	Driver    string `json:"driver"`
	Token     string `json:"token,omitempty"`
	RefCount  int64  `json:"refCount"`
	CreatedAt int64  `json:"createdAt"`
}

// volumeView is the CLI-facing volume record without sensitive fields.
type volumeView struct {
	VolumeID  string `json:"volumeID"`
	Name      string `json:"name"`
	Driver    string `json:"driver"`
	RefCount  int64  `json:"refCount"`
	CreatedAt int64  `json:"createdAt"`
}

type volumeListResponse struct {
	Ret     *types.Ret       `json:"ret,omitempty"`
	Volumes []volumeWireItem `json:"volumes"`
}

type volumeGetResponse struct {
	Ret    *types.Ret      `json:"ret,omitempty"`
	Volume *volumeWireItem `json:"volume,omitempty"`
}

type volumeDeleteResponse struct {
	Ret      *types.Ret `json:"ret,omitempty"`
	VolumeID string     `json:"volumeID,omitempty"`
}

type volumeListViewResponse struct {
	Ret     *types.Ret   `json:"ret,omitempty"`
	Volumes []volumeView `json:"volumes"`
}

type volumeGetViewResponse struct {
	Ret    *types.Ret  `json:"ret,omitempty"`
	Volume *volumeView `json:"volume,omitempty"`
}

var VolumeCommand = cli.Command{
	Name:    "volume",
	Aliases: []string{"vol"},
	Usage:   "manage volumes (CubeMaster /cube/volume)",
	Subcommands: cli.Commands{
		VolumeListCommand,
		VolumeGetCommand,
		VolumeDeleteCommand,
	},
}

var VolumeListCommand = cli.Command{
	Name:    "list",
	Aliases: []string{"ls"},
	Usage:   "list volumes (omits token and private_data)",
	Flags: []cli.Flag{
		cli.BoolFlag{Name: "json", Usage: "print json (without sensitive fields)"},
	},
	Action: func(c *cli.Context) error {
		rsp := &volumeListResponse{}
		if err := doVolumeReq(c, http.MethodGet, "/cube/volume", uuid.NewString(), nil, rsp); err != nil {
			return err
		}
		if err := ensureVolumeSuccessRet(rsp.Ret); err != nil {
			return err
		}
		view := volumeListViewResponse{
			Ret:     rsp.Ret,
			Volumes: make([]volumeView, 0, len(rsp.Volumes)),
		}
		for i := range rsp.Volumes {
			view.Volumes = append(view.Volumes, toVolumeView(&rsp.Volumes[i]))
		}
		if c.Bool("json") {
			commands.PrintAsJSON(view)
			return nil
		}
		printVolumeList(view.Volumes)
		return nil
	},
}

var VolumeGetCommand = cli.Command{
	Name:    "get",
	Aliases: []string{"info", "describe"},
	Usage:   "get a volume by id (omits token and private_data)",
	Flags: []cli.Flag{
		cli.StringFlag{Name: "volume-id", Usage: "volume id to query"},
		cli.BoolFlag{Name: "json", Usage: "print json (without sensitive fields)"},
	},
	Action: func(c *cli.Context) error {
		volumeID := resolveVolumeID(c)
		if volumeID == "" {
			return errors.New("volume id is required (positional arg or --volume-id)")
		}
		endpoint := "/cube/volume/" + url.PathEscape(volumeID)
		rsp := &volumeGetResponse{}
		if err := doVolumeReq(c, http.MethodGet, endpoint, uuid.NewString(), nil, rsp); err != nil {
			return err
		}
		if err := ensureVolumeSuccessRet(rsp.Ret); err != nil {
			return err
		}
		view := volumeGetViewResponse{Ret: rsp.Ret}
		if rsp.Volume != nil {
			v := toVolumeView(rsp.Volume)
			view.Volume = &v
		}
		if c.Bool("json") {
			commands.PrintAsJSON(view)
			return nil
		}
		if view.Volume == nil {
			return errors.New("empty volume in response")
		}
		printVolumeInfo(view.Volume)
		return nil
	},
}

var VolumeDeleteCommand = cli.Command{
	Name:    "delete",
	Aliases: []string{"rm", "remove"},
	Usage:   "delete a volume by id",
	Flags: []cli.Flag{
		cli.StringFlag{Name: "volume-id", Usage: "volume id to delete"},
		cli.BoolFlag{Name: "json", Usage: "print json response"},
	},
	Action: func(c *cli.Context) error {
		volumeID := resolveVolumeID(c)
		if volumeID == "" {
			return errors.New("volume id is required (positional arg or --volume-id)")
		}
		endpoint := "/cube/volume/" + url.PathEscape(volumeID)
		rsp := &volumeDeleteResponse{}
		if err := doVolumeReq(c, http.MethodDelete, endpoint, uuid.NewString(), nil, rsp); err != nil {
			return err
		}
		if err := ensureVolumeSuccessRet(rsp.Ret); err != nil {
			return err
		}
		if c.Bool("json") {
			commands.PrintAsJSON(rsp)
			return nil
		}
		fmt.Printf("deleted volume: %s\n", rsp.VolumeID)
		return nil
	},
}

func resolveVolumeID(c *cli.Context) string {
	if id := strings.TrimSpace(c.String("volume-id")); id != "" {
		return id
	}
	if c.NArg() > 0 {
		return strings.TrimSpace(c.Args().First())
	}
	return ""
}

func toVolumeView(item *volumeWireItem) volumeView {
	if item == nil {
		return volumeView{}
	}
	return volumeView{
		VolumeID:  item.VolumeID,
		Name:      item.Name,
		Driver:    item.Driver,
		RefCount:  item.RefCount,
		CreatedAt: item.CreatedAt,
	}
}

func ensureVolumeSuccessRet(ret *types.Ret) error {
	if ret == nil {
		return errors.New("empty response")
	}
	// Volume handlers use ret_code 0; other CubeMaster APIs often use 200.
	if ret.RetCode != 0 && ret.RetCode != 200 {
		if ret.RetMsg != "" {
			return errors.New(ret.RetMsg)
		}
		return fmt.Errorf("request failed with ret_code %d", ret.RetCode)
	}
	return nil
}

func doVolumeReq(c *cli.Context, method, endpoint, requestID string, body io.Reader, rsp interface{}) error {
	serverList = getServerAddrs(c)
	if len(serverList) == 0 {
		return errors.New("no server addr")
	}
	port = c.GlobalString("port")
	host := serverList[rand.Int()%len(serverList)]
	urlStr := fmt.Sprintf("http://%s%s", net.JoinHostPort(host, port), endpoint)
	return doHttpReq(c, urlStr, method, requestID, body, rsp)
}

func printVolumeList(items []volumeView) {
	w := tabwriter.NewWriter(os.Stdout, 4, 8, 4, ' ', 0)
	fmt.Fprintln(w, "VOLUME_ID\tNAME\tDRIVER\tREFCOUNT\tCREATED_AT")
	for _, item := range items {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\n",
			item.VolumeID,
			item.Name,
			item.Driver,
			item.RefCount,
			formatVolumeCreatedAt(item.CreatedAt),
		)
	}
	_ = w.Flush()
}

func printVolumeInfo(item *volumeView) {
	fmt.Printf("volume_id:  %s\n", item.VolumeID)
	fmt.Printf("name:       %s\n", item.Name)
	fmt.Printf("driver:     %s\n", item.Driver)
	fmt.Printf("ref_count:  %d\n", item.RefCount)
	fmt.Printf("created_at: %s\n", formatVolumeCreatedAt(item.CreatedAt))
}

func formatVolumeCreatedAt(unixSec int64) string {
	if unixSec <= 0 {
		return "-"
	}
	return time.Unix(unixSec, 0).UTC().Format(time.RFC3339)
}
