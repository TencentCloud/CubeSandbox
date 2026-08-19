package CubeLog

import (
	"bytes"
	"strings"
	"testing"
)

func TestCriticalfMatchesFatalf(t *testing.T) {
	var fatalBuf, criticalBuf bytes.Buffer

	l1 := GetLogger("alias-a")
	l1.SetOutput(&fatalBuf)
	l1.Fatalf("boom %d", 7)

	l2 := GetLogger("alias-b")
	l2.SetOutput(&criticalBuf)
	l2.Criticalf("boom %d", 7)

	if fatalBuf.Len() == 0 || criticalBuf.Len() == 0 {
		t.Fatalf("expected both to emit output, got fatal=%d critical=%d", fatalBuf.Len(), criticalBuf.Len())
	}
	if !strings.Contains(fatalBuf.String(), "boom 7") {
		t.Fatalf("Fatalf output missing message: %s", fatalBuf.String())
	}
	if !strings.Contains(criticalBuf.String(), "boom 7") {
		t.Fatalf("Criticalf output missing message: %s", criticalBuf.String())
	}
	if !strings.Contains(criticalBuf.String(), "FATAL") {
		t.Fatalf("Criticalf did not log at FATAL level: %s", criticalBuf.String())
	}
}

func TestCriticalfDoesNotExit(t *testing.T) {
	var buf bytes.Buffer
	l := GetLogger("alias-noexit")
	l.SetOutput(&buf)

	l.Criticalf("still running %s", "after")
	l.Criticalf("second call")

	if strings.Count(buf.String(), "still running after") != 1 {
		t.Fatalf("first message missing: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "second call") {
		t.Fatal("execution did not continue past the first Criticalf")
	}
}
