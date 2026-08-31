// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package inner

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rcrowley/go-metrics"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

func handleWebsocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	go func() {
		for {
			jData, err := json.Marshal(getNodeStat())
			if err != nil {
				break
			}
			if err := conn.WriteMessage(websocket.TextMessage, jData); err != nil {
				CubeLog.Errorf("handleWebsocket:%v", err)
				break
			}
			time.Sleep(2 * time.Second)
		}
		conn.Close()
	}()
}

func handleQuery(w http.ResponseWriter, r *http.Request) {
	insID := r.URL.Query().Get("nodeId")
	result := GetStatPercents()
	if insID != "" {
		n, ok := localcache.GetNode(insID)
		if ok {
			data := map[string]string{
				"cpuQuota":   getAbsScore(n.QuotaCpu, 1000),
				"memQuota":   getAbsScore(n.QuotaMem, 1024),
				"cpuUsage":   getAbsScore(n.QuotaCpuUsage, 1000),
				"memUsage":   getAbsScore(n.QuotaMemUsage, 1024),
				"mvmNum":     getAbsScore(n.MvmNum, 1),
				"score":      strconv.FormatFloat(n.Score, 'f', 2, 64),
				"cpuPercent": getAbsScore(n.QuotaCpuUsage*100, n.QuotaCpu) + "%",
				"memPercent": getAbsScore(n.QuotaMemUsage*100, n.QuotaMem) + "%",
				"numPercent": getAbsScore(n.MvmNum*100, n.MaxMvmLimit) + "%",
			}
			for k, v := range data {
				result[k] = v
			}

		}
	}
	jData, err := utils.JSONTool.Marshal(result)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(jData)
}

type nodeData struct {
	CPU int64 `json:"cpu"`
	Mem int64 `json:"mem"`
	Num int64 `json:"num"`

	CpuPercent int64 `json:"cpuPercent"`

	MemPercent int64 `json:"memPercent"`

	NumPercent int64 `json:"numPercent"`
}

type chartData struct {
	Node map[string]nodeData `json:"node"`
}

type percentMetric struct {
	histogram metrics.Histogram
}

func addPercentMetric(tmpMetric map[string]*percentMetric, id string, v int64) {
	m, ok := tmpMetric[id]
	if !ok {
		tmpMetric[id] = &percentMetric{
			histogram: metrics.NewHistogram(metrics.NewUniformSample(1000)),
		}
		m = tmpMetric[id]
	}
	m.histogram.Update(v)
}

func printPercentiles(id string, h metrics.Histogram) string {
	var buff strings.Builder
	ps := h.Percentiles(config.GetConfig().Common.MockPercents)
	if strings.Contains(id, "Percent") {
		buff.WriteString(fmt.Sprintf("min:%d%%\t", h.Min()))
		buff.WriteString(fmt.Sprintf("max:%d%%\t", h.Max()))
	} else {
		if id == "pscore" {
			base := 1e6 * 1.0
			buff.WriteString(fmt.Sprintf("min:%f\t", float64(h.Min())/base))
			buff.WriteString(fmt.Sprintf("max:%f\t", float64(h.Max())/base))
		} else {
			buff.WriteString(fmt.Sprintf("min:%d\t", h.Min()))
			buff.WriteString(fmt.Sprintf("max:%d\t", h.Max()))
		}
	}
	for i, d := range config.GetConfig().Common.MockPercents {
		if strings.Contains(id, "Percent") {
			buff.WriteString(fmt.Sprintf("p%d:%d%%\t", int(d*100), int(ps[i]*1000)/1000))
		} else {
			if id == "pscore" {
				base := 1e6 * 1.0
				buff.WriteString(fmt.Sprintf("p%d:%f\t", int(d*100), float64(ps[i]*1000)/1000/base))
			} else {
				buff.WriteString(fmt.Sprintf("p%d:%d\t", int(d*100), int(ps[i]*1000)/1000))
			}
		}
	}
	if id == "pscore" {
		buff.WriteString(fmt.Sprintf("stddev:%f", h.StdDev()/(1e6*1.0)))
	} else {
		buff.WriteString(fmt.Sprintf("stddev:%f", h.StdDev()))
	}
	return fmt.Sprintf("[%v]", buff.String())
}

func GetStatPercents() map[string]string {
	nodes := localcache.GetHealthyNodes(-1)
	percentMap := make(map[string]*percentMetric)
	for _, n := range nodes {
		addPercentMetric(percentMap, "pcpuUsage", n.QuotaCpuUsage/1000)
		addPercentMetric(percentMap, "pmemUsage", n.QuotaMemUsage/1024)
		addPercentMetric(percentMap, "pmvmNum", n.MvmNum)
		addPercentMetric(percentMap, "pscore", int64(n.Score*1e6))
		addPercentMetric(percentMap, "pcpuPercent", n.QuotaCpuUsage*100/n.QuotaCpu)
		addPercentMetric(percentMap, "pmemPercent", n.QuotaMemUsage*100/n.QuotaMem)
		addPercentMetric(percentMap, "pmvmPercent", n.MvmNum*100/n.MaxMvmLimit)
	}

	result := make(map[string]string)
	for k, v := range percentMap {
		result[k] = printPercentiles(k, v.histogram)
	}
	return result
}

func getNodeStat() *chartData {
	data := &chartData{
		Node: make(map[string]nodeData),
	}
	nodes := localcache.GetHealthyNodes(-1)
	for _, node := range nodes {
		data.Node[node.ID()] = nodeData{
			CPU:        node.QuotaCpuUsage / 1000,
			Mem:        node.QuotaMemUsage / 1024,
			Num:        node.MvmNum,
			CpuPercent: node.QuotaCpuUsage * 100 / node.QuotaCpu,
			MemPercent: node.QuotaMemUsage * 100 / node.QuotaMem,
			NumPercent: node.MvmNum * 100 / node.MaxMvmLimit,
		}
	}
	return data
}

func getAbsScore(v int64, base int64) string {
	if base == 0 {
		return "0"
	}
	f := float64(v) * 1.0 / float64(base)
	return strconv.FormatFloat(f, 'f', 2, 64)
}
