package resourcemetrics

import (
	"time"

	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
)

func hostSandboxSamplingUnavailable(status *cubeboxstore.StatusStorage) bool {
	if status == nil {
		return true
	}
	return status.IsTerminated() || status.IsPaused()
}

func guestWorkloadSamplingUnavailable(status *cubeboxstore.StatusStorage) bool {
	return hostSandboxSamplingUnavailable(status) || status.Get().RollingBack
}

func waitForSamplerDispatch(ctxDone <-chan struct{}, interval time.Duration, completed <-chan struct{}) bool {
	deadline := time.Now().Add(interval)
	timer := time.NewTimer(interval)
	defer func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}()
	select {
	case <-ctxDone:
		return false
	case <-timer.C:
		if completed == nil {
			return true
		}
		select {
		case <-ctxDone:
			return false
		case <-completed:
			return true
		}
	case <-completed:
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return true
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(remaining)
		select {
		case <-ctxDone:
			return false
		case <-timer.C:
			return true
		}
	}
}

func notifySamplerCompletion(completed chan<- struct{}) {
	select {
	case completed <- struct{}{}:
	default:
	}
}

func batchDispatchInterval(collectionInterval time.Duration, maxConcurrentRequests, eligibleCount int) time.Duration {
	if eligibleCount <= maxConcurrentRequests {
		return collectionInterval
	}
	batches := (eligibleCount + maxConcurrentRequests - 1) / maxConcurrentRequests
	interval := collectionInterval / time.Duration(batches)
	if interval < time.Millisecond {
		return time.Millisecond
	}
	return interval
}
