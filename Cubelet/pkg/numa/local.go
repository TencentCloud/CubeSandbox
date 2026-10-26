// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package numa

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
)

var (
	l    = &NumaInfo{}
	once sync.Once
)

type NumaInfo struct {
	numaNodes     []NumaNode
	maxNumaNodeID int
	numaCount     int
}

type NumaNode struct {
	NodeId  int
	Cpulist string

	CpulistOrigin string
	Cores         map[int]bool
}

func initNumaInfo() {

	l = &NumaInfo{}
	if err := l.init(); err != nil {
		panic(fmt.Sprintf("Failed to initialize NumaInfo: %v", err))
	}
}

func GetNumaInfo() *NumaInfo {
	once.Do(initNumaInfo)
	return l
}

func (p *NumaInfo) init() error {
	nodes, err := getNumaNodes()
	if err != nil {
		return err
	}
	p.numaNodes = nodes

	for _, node := range p.numaNodes {
		if node.NodeId > p.maxNumaNodeID {
			p.maxNumaNodeID = node.NodeId
		}
	}
	if len(p.numaNodes) != p.maxNumaNodeID+1 {
		return fmt.Errorf("numa node count mismatch")
	}

	p.numaCount = len(p.numaNodes)

	if len(config.GetCommon().CgroupDisableCpusetList) > 0 {

		for i, node := range p.numaNodes {
			node.Cpulist = removeDisabledCpus(node.Cpulist, strings.Split(config.GetCommon().CgroupDisableCpusetList, ","))
			p.numaNodes[i] = node
			if node.Cpulist != node.CpulistOrigin {
				log.L.Infof("numa node %d cpulist changed from %s to %s", node.NodeId, node.CpulistOrigin, node.Cpulist)
			}
		}
	}
	return nil
}

func getNumaNodes() ([]NumaNode, error) {
	nodesDir := "/sys/devices/system/node/"
	nodes := make([]NumaNode, 0)
	entries, err := os.ReadDir(nodesDir)
	if err != nil {
		return nodes, err
	}

	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "node") {
			continue
		}

		nodeIDStr := strings.TrimPrefix(entry.Name(), "node")
		nodeID, err := strconv.Atoi(nodeIDStr)
		if err != nil {
			return nodes, fmt.Errorf("invalid node ID: %s", nodeIDStr)
		}

		cpuList, err := getNumaNodeCPUs(nodeID)
		if err != nil {
			return nodes, err
		}
		cores := numaNodeCPUsToCores(cpuList)
		nodes = append(nodes, NumaNode{
			NodeId:        nodeID,
			Cpulist:       cpuList,
			CpulistOrigin: cpuList,
			Cores:         cores,
		})

	}
	return nodes, nil
}

func GetMaxNumaNodeId() int {
	return GetNumaInfo().maxNumaNodeID
}

func GetNumaNodeCount() int {
	return GetNumaInfo().numaCount
}

func GetAllNumaNodes() []NumaNode {
	return GetNumaInfo().numaNodes
}

func getNumaNodeCPUs(nodeID int) (string, error) {
	cpuListFile := filepath.Join("/sys/devices/system/node", fmt.Sprintf("node%d", nodeID), "cpulist")
	data, err := os.ReadFile(cpuListFile)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func numaNodeCPUsToCores(cpulist string) map[int]bool {
	cores := make(map[int]bool)
	ranges := strings.Split(cpulist, ",")
	for _, r := range ranges {
		if strings.Contains(r, "-") {
			bounds := strings.Split(r, "-")
			start, err := strconv.Atoi(bounds[0])
			if err != nil {
				continue
			}
			end, err := strconv.Atoi(bounds[1])
			if err != nil {
				continue
			}
			for i := start; i <= end; i++ {
				cores[i] = true
			}
		} else {
			cpu, err := strconv.Atoi(r)
			if err != nil {
				continue
			}
			cores[cpu] = true
		}
	}
	return cores
}

func removeDisabledCpus(cpulist string, disabledCpus []string) string {
	if len(disabledCpus) == 0 {
		return cpulist
	}

	cpus := strings.Split(cpulist, ",")
	filteredCpus := make([]string, 0)

	for _, cpu := range cpus {
		if strings.Contains(cpu, "-") {

			bounds := strings.Split(cpu, "-")
			start, err := strconv.Atoi(bounds[0])
			if err != nil {
				continue
			}
			end, err := strconv.Atoi(bounds[1])
			if err != nil {
				continue
			}

			isDisabled := false
			for _, disabled := range disabledCpus {
				disabledCpu, err := strconv.Atoi(disabled)
				if err != nil {
					continue
				}
				if disabledCpu >= start && disabledCpu <= end {
					isDisabled = true
					break
				}
			}

			if !isDisabled {
				filteredCpus = append(filteredCpus, cpu)
			} else {

				newStart := start
				for _, disabled := range disabledCpus {
					disabledCpu, err := strconv.Atoi(disabled)
					if err != nil {
						continue
					}
					if disabledCpu >= start && disabledCpu <= end {
						if newStart < disabledCpu {
							filteredCpus = append(filteredCpus, fmt.Sprintf("%d-%d", newStart, disabledCpu-1))
						}
						newStart = disabledCpu + 1
					}
				}
				if newStart <= end {
					filteredCpus = append(filteredCpus, fmt.Sprintf("%d-%d", newStart, end))
				}
			}
		} else {

			cpuNum, err := strconv.Atoi(cpu)
			if err != nil {
				continue
			}

			isDisabled := false
			for _, disabled := range disabledCpus {
				disabledCpu, err := strconv.Atoi(disabled)
				if err != nil {
					continue
				}
				if cpuNum == disabledCpu {
					isDisabled = true
					break
				}
			}

			if !isDisabled {
				filteredCpus = append(filteredCpus, cpu)
			}
		}
	}

	return strings.Join(filteredCpus, ",")
}
