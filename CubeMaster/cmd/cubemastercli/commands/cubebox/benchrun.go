// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	jsoniter "github.com/json-iterator/go"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/integration"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/semaphore"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/urfave/cli"
)

var MultiRun = cli.Command{
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
			Name:  "sleep_before_req",
			Value: 0,
			Usage: "sleep before req container",
		},
		&cli.BoolFlag{
			Name: "same",

			Usage: "需要严格并发运行的场景",
		},
		&cli.IntFlag{
			Name:  "delcc",
			Usage: "concurrency of destroy",
		},
		cli.StringFlag{
			Name:  "hostid,t",
			Usage: "内部调试参数,需指定http头部才有效果,指定物理机ID生产容器",
		},
		cli.StringFlag{
			Name:  "hostip,s",
			Usage: "内部调试参数,需指定http头部才有效果,指定物理IP生产容器",
		},
		&cli.BoolFlag{
			Name:  "testmocksch",
			Usage: "test mock scheduling",
		},
		&cli.StringFlag{
			Name:  "biztype",
			Value: "all",
			Usage: "bussiness type,default is all,[all/sh/shtcb/gz/bj]",
		},
		&cli.BoolFlag{
			Name:  "testmultireq",
			Usage: "test multi req json from file stub",
		},
		cli.StringFlag{
			Name:  "multireq_user_id",
			Value: "123456789",
			Usage: "多函数对应的user_id参数",
		},
		&cli.IntFlag{
			Name:  "async_retry_max",
			Value: 100,
			Usage: "max retry count for async retry queue, 0 means give up immediately after sync retry failed",
		},
	},
}

type retryTask struct {
	containerID string
	reqByte     []byte
	wg          *wrapWg
	retryCount  int
	lastRetry   time.Time
}

type asyncRetryQueue struct {
	mu     sync.Mutex
	tasks  []*retryTask
	ctx    context.Context
	cancel context.CancelFunc
}

var (
	totalRunSuccCnt int64
	totalRunErr     int64
	totalDelSuccCnt int64
	totalDelErr     int64

	lock      sync.RWMutex
	runMetric map[string]*costMetric = make(map[string]*costMetric)

	lockRemove   sync.RWMutex
	removeMetric map[string]*costMetric = make(map[string]*costMetric)

	pertentis = []float64{0.05, 0.5, 0.8, 0.99}
	printAll  = false

	norm = false

	serverList []string
	port       string

	globalRetryQueue *asyncRetryQueue

	asyncRetryMax int
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

	conMap sync.Map

	delLimiter *semaphore.Weighted

	reqEveryConcurrent int64

	lb *integration.LoadBalancer
}

func newAsyncRetryQueue(ctx context.Context) *asyncRetryQueue {
	queueCtx, cancel := context.WithCancel(ctx)
	queue := &asyncRetryQueue{
		tasks:  make([]*retryTask, 0),
		ctx:    queueCtx,
		cancel: cancel,
	}

	go queue.processLoop()
	return queue
}

func (q *asyncRetryQueue) addTask(task *retryTask) {
	q.mu.Lock()
	defer q.mu.Unlock()
	task.lastRetry = time.Now()
	q.tasks = append(q.tasks, task)
	log.Printf("[AsyncRetry] Added task to queue, containerID: %s, total tasks: %d\n", task.containerID, len(q.tasks))
}

func (q *asyncRetryQueue) processLoop() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-q.ctx.Done():
			log.Printf("[AsyncRetry] Queue processor stopped\n")
			return
		case <-ticker.C:
			q.processTasks()
		}
	}
}

func (q *asyncRetryQueue) processTasks() {
	q.mu.Lock()
	if len(q.tasks) == 0 {
		q.mu.Unlock()
		return
	}

	tasksToProcess := make([]*retryTask, len(q.tasks))
	copy(tasksToProcess, q.tasks)
	q.mu.Unlock()

	log.Printf("[AsyncRetry] Processing %d tasks in queue\n", len(tasksToProcess))

	remainingTasks := make([]*retryTask, 0)
	for _, task := range tasksToProcess {

		if time.Since(task.lastRetry) < 5*time.Second {
			remainingTasks = append(remainingTasks, task)
			continue
		}

		var err error
		err = doDestroySandbox(task.wg.cliContext, task.containerID)

		if err == nil {
			log.Printf("[AsyncRetry] Successfully deleted container: %s after %d retries\n", task.containerID, task.retryCount)
			continue
		}

		task.retryCount++
		task.lastRetry = time.Now()

		if asyncRetryMax > 0 && task.retryCount > asyncRetryMax {
			log.Printf("[AsyncRetry] Giving up on container: %s after %d retries (max: %d), error: %s\n", task.containerID, task.retryCount, asyncRetryMax, err.Error())
			continue
		} else if asyncRetryMax == 0 {

			log.Printf("[AsyncRetry] Async retry disabled (max=0), giving up on container: %s, error: %s\n", task.containerID, err.Error())
			continue
		}

		log.Printf("[AsyncRetry] Retry failed for container: %s, retry count: %d, error: %s\n", task.containerID, task.retryCount, err.Error())
		remainingTasks = append(remainingTasks, task)
	}

	q.mu.Lock()
	q.tasks = remainingTasks
	q.mu.Unlock()

	if len(remainingTasks) > 0 {
		log.Printf("[AsyncRetry] Remaining tasks in queue: %d\n", len(remainingTasks))
	}
}

func (q *asyncRetryQueue) stop() {
	if q != nil {
		q.cancel()
	}
}

func (q *asyncRetryQueue) getQueueSize() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.tasks)
}

func initWragWaitGroup(tmpWg *wrapWg) {
	if len(tmpWg.cliContext.Args()) >= 1 {
		cnt := 0
		for _, arg := range tmpWg.cliContext.Args() {
			_, err := getParams(arg)
			if err != nil {
				log.Printf("Multitun getParams err. %s\n", err.Error())
				continue
			}
			cnt += 1
		}

		tmpWg.concurrentTotal = tmpWg.concurrentEveryReq * cnt
	}

	setpercents(tmpWg.cliContext)
	if tmpWg.cliContext.IsSet("delcc") {
		tmpWg.delLimiter = semaphore.NewWeighted(tmpWg.cliContext.Int64("delcc"))
	}

	if tmpWg.cliContext.Bool("testmocksch") {
		reqFormatList := integration.GetAllFormatList()
		tmpWg.lb = integration.NewLoadBalancer(reqFormatList[tmpWg.cliContext.String("biztype")])
	}
}
func multiRunAction(c *cli.Context) error {
	if c.NArg() == 0 {
		return errors.New("must specify at least one config file")
	}

	printAll = c.Bool("printall")
	norm = c.Bool("norm")

	tmpWg := &wrapWg{
		cliContext:         c,
		wg:                 &sync.WaitGroup{},
		cnt:                c.Int64("runcnt"),
		concurrentEveryReq: c.Int("runcc"),
		concurrentTotal:    1,
	}
	initWragWaitGroup(tmpWg)
	log.Printf("concurrentTotal:%d,cnt:%d\n", tmpWg.concurrentTotal, tmpWg.cnt)

	serverList = getServerAddrs(c)
	if len(serverList) == 0 {
		log.Printf("no server addr\n")
		return errors.New("no server addr")
	}
	port = c.GlobalString("port")

	asyncRetryMax = c.Int("async_retry_max")
	if asyncRetryMax < 0 {
		asyncRetryMax = 0
	}
	log.Printf("Async retry max count: %d\n", asyncRetryMax)

	tmpWg.doneCtx, tmpWg.cancel = context.WithCancel(context.Background())
	defer tmpWg.cancel()

	globalRetryQueue = newAsyncRetryQueue(tmpWg.doneCtx)
	defer func() {
		if globalRetryQueue != nil {
			queueSize := globalRetryQueue.getQueueSize()
			if queueSize > 0 {
				log.Printf("[AsyncRetry] Waiting for remaining %d tasks to complete...\n", queueSize)

				time.Sleep(5 * time.Second)
			}
			globalRetryQueue.stop()
		}
	}()

	go checkDone(tmpWg.cancel)

	if err := initConcurrentSync(tmpWg); err != nil {
		return err
	}

	for _, arg := range c.Args() {
		reqByte, err := getParams(arg)
		if err != nil {
			log.Printf("Multitun getParams err. %s\n", err.Error())
			continue
		}
		tmpWg.wg.Add(1)
		go func() {
			defer tmpWg.wg.Done()
			tmpReq := reqByte
			if err = doWork(tmpWg, tmpReq); err != nil {
				log.Printf("Multitun workerwithrm err. %s\n", err.Error())
				return
			}
		}()
	}
	tmpWg.wg.Wait()
	printRunResult(c)
	if !c.Bool("norm") {
		printDestroyResult(c)
	}
	if c.Bool("testmocksch") {
		printBenchStdDevResult(c)
	}
	return nil
}

func dealDebugParam(wg *wrapWg, req *types.CreateCubeSandboxReq) {
	if wg.cliContext.String("hostid") != "" {
		req.InsId = wg.cliContext.String("hostid")
		req.Annotations[constants.AnnotationsDebug] = "true"
	} else if wg.cliContext.String("hostip") != "" {
		req.InsIp = wg.cliContext.String("hostip")
		req.Annotations[constants.AnnotationsDebug] = "true"
	}
}

func getContext(c *cli.Context) (context.Context, context.CancelFunc) {
	var ctx = context.Background()
	var cancel context.CancelFunc
	timeout := c.GlobalDuration("timeout")
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	return ctx, cancel
}

func doCreateByDefault(wg *wrapWg, reqByte []byte, index int) (s string, err error) {
	requestID := uuid.New().String()
	defer func() {
		if err != nil {
			atomic.AddInt64(&totalRunErr, 1)
		}
	}()
	req := &types.CreateCubeSandboxReq{}
	if wg.cliContext.Bool("testmocksch") {
		req = wg.lb.GetCreateCubeSandboxReq()
	} else {
		if err := json.Unmarshal(reqByte, &req); err != nil {
			log.Printf("doCreateSandbox_Unmarshal err. %s\n", err.Error())
			return "", err
		}
	}
	dealDebugParam(wg, req)
	req.RequestID = requestID
	body := []byte(utils.InterfaceToString(req))
	url := fmt.Sprintf("http://%s/cube/sandbox", net.JoinHostPort(serverList[rand.Int()%len(serverList)], port))

	ctx, cancel := getContext(wg.cliContext)
	defer cancel()

	startTime := time.Now()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(body))
	if err != nil {
		log.Printf("doCreateSandbox_httpReq err. %s. RequestId: %s\n", err.Error(), requestID)
		return "", err
	}
	httpReq.Header.Set(constants.Caller, "mastercli")
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
		},
	}
	defer client.CloseIdleConnections()
	resp, err := client.Do(httpReq)
	cost := time.Since(startTime).Milliseconds()
	if err != nil {
		log.Printf("doCreateSandbox_Do err. %s. RequestId: %s\n", err.Error(), requestID)
		return "", err
	}
	defer resp.Body.Close()
	if http.StatusOK != resp.StatusCode {
		log.Printf("doCreateSandbox_status err. %d. RequestId: %s\n", resp.StatusCode, requestID)
		return "", err
	}

	rsp := &types.CreateCubeSandboxRes{}
	err = getBodyData(resp, rsp)
	if err != nil {
		log.Printf("doCreateSandbox_getBodyData err. %s. RequestId: %s\n", err.Error(), requestID)
		return "", err
	}
	log.Printf("doCreateSandbox RequestId:%s,sandBoxId:%s,Ip:%s,HostID:%s,HostIP:%s,code:%d, message:%s,cost:%v\n",
		req.RequestID, rsp.SandboxID, rsp.SandboxIP, rsp.HostID, rsp.HostIP, rsp.Ret.RetCode, rsp.Ret.RetMsg, cost)
	if rsp.Ret.RetCode != 200 {
		return "", errors.New(rsp.Ret.RetMsg)
	}
	atomic.AddInt64(&totalRunSuccCnt, 1)
	addRunCost(req_cost_in_ms, cost)
	for k, v := range rsp.ExtInfo {
		t, _ := strconv.ParseInt(string(v), 10, 64)
		addRunCost(k, t)
	}
	return rsp.SandboxID, nil
}

func doCreateSandbox(wg *wrapWg, reqByte []byte, index int) (s string, err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("panic %v\n", string(debug.Stack()))
			err = fmt.Errorf("panic %v", r)
		}
		if err != nil {
			time.Sleep(5 * time.Second)
			atomic.AddInt64(&totalRunErr, 1)
		}
	}()

	return doCreateByDefault(wg, reqByte, index)
}

func doDestroySandbox(clictx *cli.Context, sandboxID string) error {
	startTime := time.Now()
	err := doInnerDestroySandbox(clictx, sandboxID, map[string]string{constants.Caller: "mastercli"}, "")
	if err != nil {
		atomic.AddInt64(&totalDelErr, 1)
		return err
	}
	atomic.AddInt64(&totalDelSuccCnt, 1)
	cost := time.Since(startTime).Milliseconds()
	addDestroyCost(req_cost_in_ms, cost)
	return nil
}

func doInnerDestroySandbox(clictx *cli.Context, sandboxID string, filter map[string]string, instanceType string) error {
	ctx, cancel := getContext(clictx)
	defer cancel()
	reqC := &types.DeleteCubeSandboxReq{
		RequestID: uuid.New().String(),
		SandboxID: sandboxID,
		Filter: &types.CubeSandboxFilter{
			LabelSelector: filter,
		},
		InstanceType: instanceType,
		Sync:         true,
	}
	body, err := jsoniter.Marshal(reqC)
	if err != nil {
		log.Printf("doDestroySandbox failure:%v\n", err)
		return err
	}
	url := fmt.Sprintf("http://%s/cube/sandbox", net.JoinHostPort(serverList[rand.Int()%len(serverList)], port))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, bytes.NewBuffer(body))
	if err != nil {
		log.Printf("doDestroySandbox failure:%v\n", err)
		return err
	}
	httpReq.Header.Set(constants.Caller, "mastercli")
	startTime := time.Now()
	client := httpClient()

	defer client.CloseIdleConnections()

	resp, err := client.Do(httpReq)

	if err != nil {
		log.Printf("doDestroySandbox failure:%v\n", err)
		return err
	}
	defer resp.Body.Close()
	if http.StatusOK != resp.StatusCode {
		log.Printf("doDestroySandbox failure:%v\n", resp.Status)
		return err
	}
	rsp := &types.DeleteCubeSandboxRes{}
	err = getBodyData(resp, rsp)
	if err != nil {
		log.Printf("doDestroySandbox failure:%v\n", err)
		return err
	}
	cost := time.Since(startTime).Milliseconds()
	log.Printf("doDestroySandbox RequestId:%s,sandBoxId:%s, code:%d, message:%s,cost:%d\n", rsp.RequestID, sandboxID,
		rsp.Ret.RetCode, rsp.Ret.RetMsg, cost)
	if rsp.Ret.RetCode != 200 {
		return errors.New(rsp.Ret.RetMsg)
	}
	addDestroyCost(req_cost_in_ms, cost)
	for k, v := range rsp.ExtInfo {
		t, _ := strconv.ParseInt(string(v), 10, 64)
		addDestroyCost(k, t)
	}
	return nil
}

func doWork(wg *wrapWg, reqByte []byte) error {
	concurrencyWg := &sync.WaitGroup{}
	for idx := 0; idx < wg.concurrentEveryReq; idx++ {
		index := idx + 1
		GoWithWaitGroup(concurrencyWg, func() {
			index := index
			for i := int64(0); i < wg.cnt; i++ {
				sleep := wg.cliContext.Duration("sleep_before_req")
				time.Sleep(sleep)
				select {
				case <-wg.doneCtx.Done():
					return
				default:
				}

				wg.waitConccurrent(i)
				containerID, err := doCreateSandbox(wg, reqByte, index)
				if err == nil {
					if norm {
						continue
					}

					sleep := wg.cliContext.Duration("sleep_before_del")
					if sleep > 0 {
						time.Sleep(sleep)
					}

					retry := 0
					const maxSyncRetry = 10
					for {
						err = doDestroySandbox(wg.cliContext, containerID)
						if err == nil {
							break
						}
						retry++
						if retry >= maxSyncRetry {
							log.Printf("[SyncRetry] Failed to remove container after %d retries: %s, adding to async retry queue\n", maxSyncRetry, err.Error())

							if globalRetryQueue != nil {
								task := &retryTask{
									containerID: containerID,
									reqByte:     reqByte,
									wg:          wg,
									retryCount:  maxSyncRetry,
								}
								globalRetryQueue.addTask(task)
							}
							break
						}
						time.Sleep(time.Second)
					}
				}
				if err != nil {
					log.Printf("doWork err:%v\n", err)
					if wg.cliContext.Bool("fail_exit") {
						wg.cancel()
					}
				}
			}
		})
	}
	concurrencyWg.Wait()
	return nil
}
