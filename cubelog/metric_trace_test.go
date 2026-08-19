package CubeLog

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

func captureTrace(t *testing.T, rt *RequestTrace) map[string]interface{} {
	t.Helper()

	var buf bytes.Buffer
	prevWriter := traceStd.writer
	SetTraceOutput(&buf)
	EnableLogMetric()
	defer func() {
		DisableLogMetric()
		traceStd.writer = prevWriter
	}()

	Trace(rt)

	if buf.Len() == 0 {
		t.Fatal("Trace produced no output")
	}
	var got map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("trace line is not JSON: %v\n%s", err, buf.String())
	}
	return got
}

func TestTraceEmitsConfiguredRegionAndClusterDefaults(t *testing.T) {
	prevRegion, prevCluster := defaultRegion, cluster
	SetRegion(Region("ap-guangzhou"))
	SetCluster("cluster-a")
	defer func() {
		defaultRegion, cluster = prevRegion, prevCluster
	}()

	got := captureTrace(t, &RequestTrace{
		Action: "VolumeGC",
		Caller: "cubelet",
		Cost:   time.Millisecond,
	})

	if got["Region"] != "ap-guangzhou" {
		t.Errorf("Region = %v, want ap-guangzhou (the configured default)", got["Region"])
	}
	if got["Cluster"] != "cluster-a" {
		t.Errorf("Cluster = %v, want cluster-a (the configured default)", got["Cluster"])
	}
}

func TestTracePrefersExplicitRegionAndCluster(t *testing.T) {
	prevRegion, prevCluster := defaultRegion, cluster
	SetRegion(Region("ap-guangzhou"))
	SetCluster("cluster-a")
	defer func() {
		defaultRegion, cluster = prevRegion, prevCluster
	}()

	got := captureTrace(t, &RequestTrace{
		Action:  "VolumeGC",
		Region:  "ap-shanghai",
		Cluster: "cluster-b",
		Cost:    time.Millisecond,
	})

	if got["Region"] != "ap-shanghai" {
		t.Errorf("Region = %v, want the explicit ap-shanghai", got["Region"])
	}
	if got["Cluster"] != "cluster-b" {
		t.Errorf("Cluster = %v, want the explicit cluster-b", got["Cluster"])
	}
}
