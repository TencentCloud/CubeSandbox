package cubesandbox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTerminationMetadataInCommandsAndPTY(test *testing.T) {
	for _, scenario := range []struct {
		name   string
		end    string
		signal int
		oom    bool
		cause  string
	}{
		{"normal", `{"exited":true,"status":"exit status 0"}`, 0, false, ""},
		{"oom", `{"exitCode":-1,"signal":9,"oomKilled":true,"killedBy":"oom"}`, 9, true, "oom"},
		{"user", `{"exitCode":-1,"signal":9,"killedBy":"user"}`, 9, false, "user"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", connectContentType)
				writer.Write(connectEnvelope(0, `{"event":{"start":{"pid":42}}}`))
				writer.Write(connectEnvelope(0, `{"event":{"end":`+scenario.end+`}}`))
				writer.Write(connectEnvelope(connectEndStreamFlag, `{}`))
			}))
			defer server.Close()
			sandbox := newPtyTestSandbox(test, server)
			result, err := sandbox.Commands().Run(context.Background(), "command", CommandOptions{})
			if err != nil {
				test.Fatal(err)
			}
			if result.OOMKilled != scenario.oom || result.KilledBy != scenario.cause {
				test.Fatalf("command metadata: %+v", result)
			}
			if scenario.signal == 0 {
				if result.Signal != nil {
					test.Fatalf("normal exit signal: %v", result.Signal)
				}
			} else if result.Signal == nil || *result.Signal != scenario.signal {
				test.Fatalf("command signal: %v", result.Signal)
			}
			handle, err := sandbox.Pty().Create(context.Background(), PtySize{Rows: 24, Cols: 80}, PtyCreateOptions{})
			if err != nil {
				test.Fatal(err)
			}
			if _, err := handle.Wait(nil); err != nil {
				test.Fatal(err)
			}
			signal, present := handle.Signal()
			if signal != scenario.signal || present != (scenario.signal != 0) ||
				handle.OOMKilled() != scenario.oom || handle.KilledBy() != scenario.cause {
				test.Fatalf("PTY metadata: signal=%d present=%v oom=%v cause=%s", signal, present, handle.OOMKilled(), handle.KilledBy())
			}
		})
	}
}
