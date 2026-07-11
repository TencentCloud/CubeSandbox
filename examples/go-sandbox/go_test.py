# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Run a Go unit test inside a Cube Sandbox.

Writes a small Go module + test into the sandbox, runs `go test -v ./...`,
and asserts it passes via the E2B SDK's `commands.run`.

Prereqs: a Go template (see README), `pip install -r requirements.txt`,
and `CUBE_TEMPLATE_ID` set in the environment.
"""

from __future__ import annotations

import os
import sys

from e2b_code_interpreter import Sandbox

from env_utils import load_local_dotenv

load_local_dotenv()

template_id = os.environ["CUBE_TEMPLATE_ID"]

GOMOD = "module hellogo\n\ngo 1.23\n"

MAIN_GO = """package main

import "fmt"

// Add returns the sum of two integers.
func Add(a, b int) int { return a + b }

func main() {
\tfmt.Println("hello from go", Add(2, 3))
}
"""

MAIN_TEST_GO = """package main

import "testing"

func TestAdd(t *testing.T) {
\tif got := Add(2, 3); got != 5 {
\t\tt.Fatalf("Add(2,3) = %d; want 5", got)
\t}
}
"""


def main() -> int:
    with Sandbox.create(template=template_id) as sandbox:
        print("== go version ==")
        res = sandbox.commands.run("go version")
        print(res.stdout.strip())
        if res.exit_code != 0:
            print("FAILED: `go` not found in template", file=sys.stderr)
            print(res.stderr, file=sys.stderr)
            return 1

        sandbox.files.make_dir("/workspace/hellogo")
        sandbox.files.write("/workspace/hellogo/go.mod", GOMOD)
        sandbox.files.write("/workspace/hellogo/main.go", MAIN_GO)
        sandbox.files.write("/workspace/hellogo/main_test.go", MAIN_TEST_GO)

        print("\n== go test -v ./... ==")
        res = sandbox.commands.run("go test -v ./...", cwd="/workspace/hellogo")
        print(res.stdout)
        if res.stderr:
            print(res.stderr, file=sys.stderr)
        if res.exit_code != 0:
            print(f"FAILED: go test exited with {res.exit_code}", file=sys.stderr)
            return 1

        print("go test passed \u2713")
        return 0


if __name__ == "__main__":
    raise SystemExit(main())
