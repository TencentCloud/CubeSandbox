# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
demo.py — the minimal Go workflow inside a Cube Sandbox.

Steps:
  1. Boot a sandbox from the go-dev-sandbox template
  2. Report the toolchain version
  3. Upload a tiny stdlib-only module (go.mod + main.go + main_test.go)
  4. `go build` it, run the binary, then `go test` it
"""

from cubesandbox import Sandbox

from env import TEMPLATE_ID, check

PROJECT_DIR = "/workspace/demo"

GO_MOD = """module cubedemo

go 1.24
"""

MAIN_GO = """package main

import (
	"fmt"
	"runtime"
)

// Fib returns the n-th Fibonacci number.
func Fib(n int) int {
	a, b := 0, 1
	for i := 0; i < n; i++ {
		a, b = b, a+b
	}
	return a
}

func main() {
	fmt.Printf("%s on %s/%s\\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	fmt.Println("fib(30) =", Fib(30))
}
"""

MAIN_TEST_GO = """package main

import "testing"

func TestFib(t *testing.T) {
	for in, want := range map[int]int{0: 0, 1: 1, 10: 55, 30: 832040} {
		if got := Fib(in); got != want {
			t.Errorf("Fib(%d) = %d, want %d", in, got, want)
		}
	}
}
"""

with Sandbox.create(template=TEMPLATE_ID) as sb:
    print(f"sandbox: {sb.sandbox_id}")

    # 1. Toolchain check — proves the template really carries Go.
    r = check(sb.commands.run("go version"), "go version")
    print(r.stdout.strip())

    # 2. Upload the project. Nothing here reaches the network, so the demo
    #    still works when the sandbox has a restrictive egress policy.
    check(sb.commands.run(f"mkdir -p {PROJECT_DIR}"), "mkdir")
    sb.files.write(f"{PROJECT_DIR}/go.mod", GO_MOD)
    sb.files.write(f"{PROJECT_DIR}/main.go", MAIN_GO)
    sb.files.write(f"{PROJECT_DIR}/main_test.go", MAIN_TEST_GO)

    # 3. Build.
    check(sb.commands.run("go build -o cubedemo .", cwd=PROJECT_DIR), "go build")
    print("build ok")

    # 4. Run.
    r = check(sb.commands.run("./cubedemo", cwd=PROJECT_DIR), "./cubedemo")
    print(r.stdout.strip())

    # 5. Test.
    r = check(sb.commands.run("go test ./...", cwd=PROJECT_DIR), "go test")
    print(r.stdout.strip())

print("OK")
