// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rcrowley/go-metrics"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/urfave/cli"
)

func httpClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext:       (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			DisableKeepAlives: true},
	}

}

func initConcurrentSync(tmpWg *wrapWg) error {
	if !tmpWg.cliContext.Bool("same") {
		return nil
	}
	initCon := make(chan int)
	go func() {
		initCon <- 0
		for k := int64(0); k < tmpWg.cnt; k++ {
			select {
			case <-tmpWg.doneCtx.Done():
				return
			default:
			}

			tmpWg.getConcurrentWg(k).conwg.Wait()

			tmpWg.getConcurrentWg(k).conCancel()
			if k > 0 {
				tmpWg.delConcurrentWg(k - 1)
			}
		}
	}()
	<-initCon
	return nil
}

type conWg struct {
	conCtx    context.Context
	conCancel context.CancelFunc
	conwg     *sync.WaitGroup
}

func (w *wrapWg) getConcurrentWg(i int64) *conWg {
	newWg := &conWg{
		conwg: &sync.WaitGroup{},
	}
	newWg.conwg.Add(w.concurrentTotal)
	newWg.conCtx, newWg.conCancel = context.WithCancel(context.Background())
	wg, ok := w.conMap.LoadOrStore(i, newWg)
	if !ok {
		return newWg
	}
	return wg.(*conWg)
}
func (w *wrapWg) delConcurrentWg(i int64) {
	w.conMap.Delete(i)
}

func (w *wrapWg) waitConccurrent(i int64) {
	if w.cliContext.Bool("same") {

		w.getConcurrentWg(i).conwg.Done()
		select {
		case <-w.getConcurrentWg(i).conCtx.Done():
		case <-w.doneCtx.Done():
		}
	}
}

type costType []int64

func (c costType) Len() int {
	return len(c)
}
func (c costType) Less(i, j int) bool {
	return c[i] < c[j]
}
func (c costType) Swap(i, j int) {
	tmp := c[i]
	c[i] = c[j]
	c[j] = tmp
}

const (
	req_cost_in_ms = "clientReqCost"
)

type costMetric struct {
	histogram metrics.Histogram
	data      costType
}

func getBodyData(rsp *http.Response, object interface{}) error {
	if rsp.Body == nil {
		return errors.New("response body is nil")
	}
	data, err := io.ReadAll(rsp.Body)
	if err != nil {
		return err
	}
	err = json.Unmarshal(data, object)
	if err != nil {
		return err
	}
	return nil
}

func getParams(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	content, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	req := &types.CreateCubeSandboxReq{}
	if err := json.Unmarshal(content, &req); err != nil {
		return nil, err
	}
	return content, nil
}

func getServerAddrs(global *cli.Context) []string {
	var addrs []string
	iplistStr := global.GlobalString("address")
	iplist := strings.Split(iplistStr, ",")
	addrs = append(addrs, iplist...)
	return addrs
}

func printRunResult(c *cli.Context) {
	log.Printf("totalRunSuccCnt:%v\n", totalRunSuccCnt)
	log.Printf("totalRunErr:%v\n", totalRunErr)
	if totalRunSuccCnt > 0 {
		log.Printf("runMetric:\n")
		for k, v := range runMetric {
			printPercentiles(k, v.histogram)
			if c.Bool("printall") {
				sort.Sort(v.data)
				log.Printf("%v:%v\n", k, v.data)
			}
		}
	}
}

func addRunCost(id string, cost int64) {
	lock.RLock()
	m, ok := runMetric[id]
	lock.RUnlock()
	if !ok {
		lock.Lock()
		runMetric[id] = &costMetric{
			histogram: metrics.NewHistogram(metrics.NewUniformSample(100000)),
		}
		m = runMetric[id]
		lock.Unlock()
	}
	if printAll {
		lock.Lock()
		m.data = append(m.data, cost)
		lock.Unlock()
	}
	m.histogram.Update(cost)
}
func setpercents(c *cli.Context) {
	if c.IsSet("percents") {
		pstr := c.String("percents")
		ps := strings.Split(pstr, ",")
		var pertentistmp []float64
		for _, p := range ps {
			f, err := strconv.ParseFloat(p, 64)
			if err == nil {
				pertentistmp = append(pertentistmp, f)
			}
		}
		if len(pertentistmp) > 0 {
			pertentis = pertentistmp
		}
		log.Printf("pertentis:%v\n", pertentis)
	}
}
func printPercentiles(id string, h metrics.Histogram) {
	var buff strings.Builder
	ps := h.Percentiles(pertentis)
	buff.WriteString(fmt.Sprintf("min:%d\t", h.Min()))
	buff.WriteString(fmt.Sprintf("max:%d\t", h.Max()))
	for i, d := range pertentis {
		buff.WriteString(fmt.Sprintf("p%d:%d\t", int(d*100), int(ps[i]*1000)/1000))
	}
	log.Printf("%v:[%v]\n", id, buff.String())
}

func addDestroyCost(id string, cost int64) {
	lockRemove.RLock()
	m, ok := removeMetric[id]
	lockRemove.RUnlock()
	if !ok {
		lockRemove.Lock()
		removeMetric[id] = &costMetric{
			histogram: metrics.NewHistogram(metrics.NewUniformSample(100000)),
		}
		m = removeMetric[id]
		lockRemove.Unlock()
	}
	if printAll {
		lockRemove.Lock()
		m.data = append(m.data, cost)
		lockRemove.Unlock()
	}
	m.histogram.Update(cost)
}
func printDestroyResult(c *cli.Context) {
	log.Printf("totalDelSuccCnt:%v\n", totalDelSuccCnt)
	log.Printf("totalDelErr:%v\n", totalDelErr)
	if totalDelSuccCnt > 0 {
		log.Printf("removeMetric:\n")
		for k, v := range removeMetric {
			printPercentiles(k, v.histogram)
			if c.Bool("printall") {
				sort.Sort(v.data)
				log.Printf("%v:%v\n", k, v.data)
			}
		}
	}
}

func printBenchStdDevResult(c *cli.Context) {
	serverList = getServerAddrs(c)
	if len(serverList) == 0 {
		log.Printf("no server addr\n")
		return
	}
	port = c.GlobalString("port")
	host := serverList[rand.Int()%len(serverList)]
	url := fmt.Sprintf("http://%s/internal/query?nodeId=%s", net.JoinHostPort(host, port), "ins_1")
	var body io.Reader
	requestID := uuid.New().String()
	rsp := make(map[string]string)
	err := doHttpReq(c, url, http.MethodGet, requestID, body, &rsp)
	if err != nil {
		log.Printf("printBenchStdDevResult err. %s. RequestId: %s\n", err.Error(), requestID)
		return
	}
	log.Printf("BenchStdDevResult:\n")
	for _, k := range []string{"pcpuUsage", "pcpuPercent", "pmemUsage", "pmemPercent", "pmvmNum", "pmvmPercent"} {
		log.Printf("%s:%s\n", k, rsp[k])
	}
}

func GoWithWaitGroup(wg *sync.WaitGroup, handler func(), panicHandlers ...func(panicError interface{})) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		WithRecover(handler, panicHandlers...)
	}()
}

func WithRecover(handler func(), panicHandlers ...func(panicError interface{})) {
	func() {
		defer HandleCrash(panicHandlers...)
		handler()
	}()
}

func HandleCrash(additionalHandlers ...func(panicError interface{})) {
	if r := recover(); r != any(nil) {
		for _, fn := range additionalHandlers {
			fn(r)
		}
	}
}

func checkDone(cancel context.CancelFunc) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	file := "/data/masterstop.dat"
	_ = os.RemoveAll(file)
	for range ticker.C {
		data, err := os.ReadFile(file)
		if err == nil {
			if strings.EqualFold(strings.Trim(string(data), "\n"), "1") {
				cancel()
				log.Printf("benchrun stop\n")
				return
			}
		}
	}
}

func doHttpReq(c *cli.Context, url, method, requestID string, body io.Reader, rsp interface{}) error {
	var ctx = context.Background()
	var cancel context.CancelFunc
	timeout := c.GlobalDuration("timeout")
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	httpReq.Header.Set(constants.Caller, "mastercli")
	if callerHostIP := c.String("callerhostip"); callerHostIP != "" {
		httpReq.Header.Set(constants.CallerHostIP, callerHostIP)
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if http.StatusOK != resp.StatusCode {
		return fmt.Errorf("status code is not 200, but %d", resp.StatusCode)
	}

	err = getBodyData(resp, rsp)
	if err != nil {
		return err
	}
	return nil
}
