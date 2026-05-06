// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	"github.com/google/uuid"
	"github.com/pkg/errors"
	"github.com/rcrowley/go-metrics"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/cmd/cubecli/commands"
	"github.com/urfave/cli/v2"
	"golang.org/x/sync/semaphore"
	"google.golang.org/grpc"
)

var MultiRun = &cli.Command{
	Name:      "multirun",
	Usage:     "run one or more containers",
	ArgsUsage: "[flags] req.json1 [req.json2, ...]",
	Action:    multiRunAction,
	Flags: []cli.Flag{
		&cli.Int64Flag{
			Name:  "runcnt",
			Value: 1,
			Usage: "loop every req count",
		},
		&cli.IntFlag{
			Name:  "runcc",
			Value: 1,
			Usage: "concurrency of every req.json",
		},
		&cli.BoolFlag{
			Name:  "addrm",
			Usage: "bench add/remove",
		},
		&cli.BoolFlag{
			Name:  "norm",
			Usage: "bench not remove",
		},
		&cli.BoolFlag{
			Name:  "printall",
			Usage: "print all result metric",
		},
		&cli.StringFlag{
			Name:  "percents",
			Value: "0.05,0.5,0.8,0.99",
			Usage: "Percentiles param",
		},
		&cli.BoolFlag{
			Name:  "fail_exit",
			Usage: "exit cli when create failure",
		},
		&cli.DurationFlag{
			Name:  "sleep_before_del",
			Value: 0,
			Usage: "sleep before delete container",
		},
		&cli.DurationFlag{
			Name:  "sleep_after_del",
			Value: 0,
			Usage: "sleep after delete container",
		},
		&cli.BoolFlag{
			Name:  "same",
			Usage: "需要严格并发运行的场景",
		},
		&cli.IntFlag{
			Name:  "delcc",
			Usage: "concurrency of destroy",
		},
		&cli.StringFlag{
			Name:    "rmimage",
			Aliases: []string{"i"},
			Usage:   "image to be deleted",
		},
		&cli.StringFlag{
			Name:  "disk-state",
			Value: "UNATTACHED",
			Usage: "disk-state",
		},
		&cli.StringFlag{
			Name:  "tag-value",
			Value: "",
			Usage: "tag-value",
		},
		&cli.StringFlag{
			Name:  "tag-key",
			Value: "cubedisk",
			Usage: "tag-key",
		},
		&cli.StringFlag{
			Name:  "dynamic-config-path",
			Value: "/usr/local/services/cubetoolbox/Cubelet/dynamicconf/conf.yaml",
			Usage: "dynamic-config-path",
		},
	},
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

var (
	totalRunSuccCnt int64
	totalRunErr     int64
	lock            sync.RWMutex
	runMetric       map[string]*costMetric
	totalDelSuccCnt int64
	totalDelErr     int64
	lockRemove      sync.RWMutex
	removeMetric    map[string]*costMetric
	pertentis       = []float64{0.05, 0.5, 0.8, 0.99}
	printAll        = false
)

type wrapWg struct {
	cliContext      *cli.Context
	wg              *sync.WaitGroup
	doneCtx         context.Context
	cancel          context.CancelFunc
	annotaionFn     sync.Once
	annotation      map[string]string
	cnt             int64
	concurrentTotal int

	concurrentEveryReq int
	conn               *grpc.ClientConn

	conMap     sync.Map
	delLimiter *semaphore.Weighted

	reqEveryConcurrent int64
}
type conWg struct {
	conCtx    context.Context
	conCancel context.CancelFunc
	conwg     *sync.WaitGroup
}

func (w *wrapWg) acquire(ctx context.Context) error {
	if w.delLimiter == nil {
		return nil
	}
	ctxd, dcancel := context.WithTimeout(ctx, 30*time.Second)
	defer dcancel()
	return w.delLimiter.Acquire(ctxd, 1)
}

func (w *wrapWg) release() {
	if w.delLimiter == nil {
		return
	}
	w.delLimiter.Release(1)
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
func doConcurrentSync(tmpWg *wrapWg) error {
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
func multiRunAction(c *cli.Context) error {
	if c.NArg() == 0 {
		return errors.Wrap(errdefs.ErrInvalidArgument, "must specify at least one config file")
	}
	runMetric = make(map[string]*costMetric)
	removeMetric = make(map[string]*costMetric)
	printAll = c.Bool("printall")
	tmpWg := &wrapWg{
		cliContext:         c,
		wg:                 &sync.WaitGroup{},
		cnt:                c.Int64("runcnt"),
		concurrentEveryReq: c.Int("runcc"),
		concurrentTotal:    1,
	}

	if len(tmpWg.cliContext.Args().Slice()) >= 1 {
		cnt := 0
		for _, arg := range c.Args().Slice() {
			_, err := getParams(arg)
			if err != nil {
				log.Printf("Multitun getParams err. %s", err.Error())
				continue
			}
			cnt += 1
		}

		tmpWg.concurrentTotal = tmpWg.concurrentEveryReq * cnt
	}
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
		log.Printf("pertentis:%v", pertentis)
	}
	log.Printf("Args:%v", c.Args())
	conn, _, cancel, err := commands.NewGrpcConn(c)
	if err != nil {
		log.Printf("connect err. %s", err.Error())
		return err
	}
	if c.IsSet("delcc") {
		tmpWg.delLimiter = semaphore.NewWeighted(c.Int64("delcc"))
	}
	defer conn.Close()
	defer cancel()
	tmpWg.conn = conn
	tmpWg.doneCtx, tmpWg.cancel = context.WithCancel(context.Background())
	defer tmpWg.cancel()
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		file := "/data/stop.dat"
		_ = os.RemoveAll(file)
		for range ticker.C {
			data, err := os.ReadFile(file)
			if err == nil {
				if strings.EqualFold(strings.Trim(string(data), "\n"), "1") {
					tmpWg.cancel()
					log.Printf("benchrun stop")
					return
				}
			}
		}
	}()
	if err := doConcurrentSync(tmpWg); err != nil {
		return err
	}
	if tmpWg.cliContext.String("rmimage") != "" {
		var err error
		cntdClient, err = containerd.New(c.String("address"), containerd.WithDefaultPlatform(platforms.Default()))
		if err != nil {
			return fmt.Errorf("init containerd connect failed.%s", err)
		}
	}

	for _, arg := range c.Args().Slice() {
		reqByte, err := getParams(arg)
		if err != nil {
			log.Printf("Multitun getParams err. %s", err.Error())
			continue
		}
		tmpWg.wg.Add(1)
		go func() {
			defer tmpWg.wg.Done()
			tmpReq := reqByte
			if c.Bool("addrm") || c.Bool("norm") {
				_ = workerwithrm(tmpWg, tmpReq)
			} else {
				_, _ = runReq(tmpWg, tmpReq)
			}
		}()
	}
	tmpWg.wg.Wait()
	printRunResult(c)
	if c.Bool("addrm") {
		printDestroyResult(c)
	}
	return nil
}
func workerwithrm(wg *wrapWg, reqByte []byte) error {
	concurrencyWg := &sync.WaitGroup{}
	for idx := 0; idx < wg.concurrentEveryReq; idx++ {
		GoWithWaitGroup(concurrencyWg, func() {
			for i := int64(0); i < wg.cnt; i++ {
				select {
				case <-wg.doneCtx.Done():
					return
				default:
				}

				wg.waitConccurrent(i)
				containerID, err := runReq(wg, reqByte)
				if err == nil {
					if wg.cliContext.Bool("norm") {
						continue
					}
					sleep := wg.cliContext.Duration("sleep_before_del")
					time.Sleep(sleep)
					retry := 0
					for {
						if err = remove(wg, containerID); err == nil {
							break
						}
						retry++
						if retry > 10 {
							log.Printf("remove container err. %s", err.Error())
							break
						}
						time.Sleep(time.Second)
					}
				}

				if err != nil {
					if wg.cliContext.Bool("fail_exit") {
						wg.cancel()
					}
				}

				if img := wg.cliContext.String("rmimage"); img != "" {
					if err := removeImage(wg.cliContext, img); err != nil {
						log.Printf("remove image err. %s", err.Error())
					}
				}
				sleep := wg.cliContext.Duration("sleep_after_del")
				time.Sleep(sleep)
			}
		})
	}
	concurrencyWg.Wait()
	return nil
}
func getProbeDuration(req *cubebox.RunCubeSandboxRequest) time.Duration {
	t := time.Duration(0)
	for _, c := range req.Containers {
		if c.GetProbe() != nil {
			t += time.Duration(c.GetProbe().TimeoutMs) * time.Millisecond
			t += time.Duration(c.GetProbe().InitialDelayMs) * time.Millisecond
		}
	}
	return t
}
func runReq(wg *wrapWg, reqByte []byte) (string, error) {
	req := &cubebox.RunCubeSandboxRequest{}
	if err := json.Unmarshal(reqByte, req); err != nil {
		return "", err
	}
	wg.annotaionFn.Do(
		func() {
			wg.annotation = req.Annotations
		})
	var ctx = context.Background()
	var cancel context.CancelFunc
	if t := getProbeDuration(req); t > 0 {
		ctx = namespaces.WithNamespace(ctx, wg.cliContext.String("namespace"))
		timeout := wg.cliContext.Duration("timeout")
		if timeout > 0 {
			ctx, cancel = context.WithTimeout(ctx, timeout+t+time.Second)
		} else {
			ctx, cancel = context.WithCancel(ctx)
		}
	} else {
		ctx, cancel = commands.AppContext(wg.cliContext)
	}
	defer cancel()
	req.RequestID = uuid.New().String()
	if req.Labels != nil {
		req.Labels["X-Caller"] = "cubecli"
	} else {
		req.Labels = make(map[string]string)
		req.Labels["X-Caller"] = "cubecli"
	}
	client := cubebox.NewCubeboxMgrClient(wg.conn)
	startTime := time.Now()
	resp, err := client.Create(ctx, req)
	cost := time.Since(startTime).Milliseconds()
	if err != nil {
		log.Printf("RunContainer err. %s. RequestId: %s", err.Error(), req.RequestID)
		time.Sleep(5 * time.Second)
		atomic.AddInt64(&totalRunErr, 1)
		return "", err
	}
	log.Printf("RunContainer RequestId:%s,sandBoxId:%s,Ip:%s,code:%d, message:%s,cost:%v", resp.RequestID,
		resp.SandboxID,
		resp.SandboxIP,
		resp.Ret.RetCode, resp.Ret.RetMsg, cost)
	if resp.Ret.RetCode != errorcode.ErrorCode_Success {
		atomic.AddInt64(&totalRunErr, 1)
		time.Sleep(5 * time.Second)
		return "", fmt.Errorf("%s", resp.Ret.RetMsg)
	}
	atomic.AddInt64(&totalRunSuccCnt, 1)
	addRunCost(req_cost_in_ms, cost)
	for k, v := range resp.ExtInfo {
		if strings.HasPrefix(k, "cube-ext") {
			log.Printf("%s:%s", k, string(v))
			continue
		}
		t, err := strconv.ParseInt(string(v), 10, 64)
		if err == nil {
			addRunCost(k, t)
		}
	}
	return resp.SandboxID, nil
}
func remove(wg *wrapWg, containerID string) error {
	ctx, cancel := commands.AppContext(wg.cliContext)
	defer cancel()
	err := wg.acquire(ctx)
	if err == nil {
		defer wg.release()
	} else {
		return err
	}
	startTime := time.Now()
	client := cubebox.NewCubeboxMgrClient(wg.conn)
	req := &cubebox.DestroyCubeSandboxRequest{
		RequestID:   uuid.New().String(),
		SandboxID:   containerID,
		Annotations: wg.annotation,
	}
	resp, err := client.Destroy(ctx, req)
	cost := time.Since(startTime).Milliseconds()
	if err != nil {
		log.Printf("destroy failure:%v", err)
		atomic.AddInt64(&totalDelErr, 1)
		return err
	}
	log.Printf("Remove ContainerRequestId:%s,sandBoxId:%s, code:%d, message:%s,cost:%d", resp.RequestID, containerID,
		resp.Ret.RetCode, resp.Ret.RetMsg, cost)
	if resp.Ret.RetCode != errorcode.ErrorCode_Success {
		atomic.AddInt64(&totalDelErr, 1)
		return fmt.Errorf("%s", resp.Ret.RetMsg)
	}
	atomic.AddInt64(&totalDelSuccCnt, 1)
	addDestroyCost(req_cost_in_ms, cost)
	for k, v := range resp.ExtInfo {
		t, _ := strconv.ParseInt(string(v), 10, 64)
		addDestroyCost(k, t)
	}
	return nil
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
func printPercentiles(id string, h metrics.Histogram) {
	var buff strings.Builder
	ps := h.Percentiles(pertentis)
	buff.WriteString(fmt.Sprintf("min:%d\t", h.Min()))
	buff.WriteString(fmt.Sprintf("max:%d\t", h.Max()))
	for i, d := range pertentis {
		buff.WriteString(fmt.Sprintf("p%d:%d\t", int(d*100), int(ps[i]*1000)/1000))
	}
	log.Printf("%v:[%v]", id, buff.String())
}
func printRunResult(c *cli.Context) {
	log.Printf("totalRunSuccCnt:%v", totalRunSuccCnt)
	log.Printf("totalRunErr:%v", totalRunErr)
	if totalRunSuccCnt > 0 {
		log.Printf("runMetric:")
		for k, v := range runMetric {
			printPercentiles(k, v.histogram)
			if c.Bool("printall") {
				sort.Sort(v.data)
				log.Printf("%v:%v", k, v.data)
			}
		}
	}
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
	log.Printf("totalDelSuccCnt:%v", totalDelSuccCnt)
	log.Printf("totalDelErr:%v", totalDelErr)
	if totalDelSuccCnt > 0 {
		log.Printf("removeMetric:")
		for k, v := range removeMetric {
			printPercentiles(k, v.histogram)
			if c.Bool("printall") {
				sort.Sort(v.data)
				log.Printf("%v:%v", k, v.data)
			}
		}
	}
}
func getParams(path string) ([]byte, error) {
	content, err := readAllFile(path)
	if err != nil {
		return nil, err
	}
	req := cubebox.RunCubeSandboxRequest{}
	if err := json.Unmarshal(content, &req); err != nil {
		return nil, err
	}
	return content, nil
}

func readRunSandboxReqFromFile(path string) (*cubebox.RunCubeSandboxRequest, error) {
	req := &cubebox.RunCubeSandboxRequest{}

	content, err := readAllFile(path)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(content, &req); err != nil {
		return nil, err
	}
	return req, nil
}

func readAllFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	content, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	return content, nil
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
