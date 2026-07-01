import os
import time
import json
import shlex
from dotenv import load_dotenv
from e2b_code_interpreter import Sandbox

script_dir = os.path.dirname(os.path.abspath(__file__))
load_dotenv(os.path.join(script_dir, ".env"), override=False)

def require_env(name: str) -> str:
    value = os.getenv(name)
    if not value:
        raise RuntimeError(
            f"Missing required environment variable: {name}. "
            "Copy examples/gin-api-sandbox/.env.example to examples/gin-api-sandbox/.env and fill in the values, "
            "or export it in your shell environment."
        )
    return value


def write_file_checked(sandbox: Sandbox, path: str, content: str) -> None:
    try:
        result = sandbox.files.write(path, content)
    except Exception as exc:
        raise RuntimeError(f"Failed to write file {path}: {exc}") from exc

    if result is not None:
        print(f"write result for {path}: {result}")

    if os.getenv("VERIFY_FILE_WRITES") != "1":
        return

    verify = sandbox.commands.run(
        f"test -f {shlex.quote(path)}",
        timeout=30,
    )
    if verify.exit_code != 0:
        raise RuntimeError(
            f"File write verification failed for {path}\n"
            f"stdout: {verify.stdout}\n"
            f"stderr: {verify.stderr}"
        )

def run_http_check(
    sandbox: Sandbox,
    name: str,
    command: str,
    expected_fields: dict[str, object],
) -> None:
    result = sandbox.commands.run(command, timeout=30)
    print(f"\n== test {name} ==")
    print("exit_code:", result.exit_code)
    print(result.stdout)
    print(result.stderr)

    if result.exit_code != 0:
        raise RuntimeError(
            f"HTTP check failed for {name}\n"
            f"stdout: {result.stdout}\n"
            f"stderr: {result.stderr}"
        )

    try:
        response = json.loads(result.stdout.strip())
    except json.JSONDecodeError as exc:
        raise RuntimeError(
            f"Invalid JSON response for {name}: {exc}\n"
            f"stdout: {result.stdout}"
        ) from exc

    if not isinstance(response, dict):
        raise RuntimeError(
            f"Unexpected JSON response type for {name}. Expected object.\n"
            f"stdout: {result.stdout}"
        )

    for field, expected_value in expected_fields.items():
        actual_value = response.get(field)
        if actual_value != expected_value:
            raise RuntimeError(
                f"Unexpected JSON field for {name}. "
                f"Expected {field}={expected_value!r}, got {actual_value!r}\n"
                f"response: {response}"
            )


def read_gin_log(sandbox: Sandbox) -> str:
    try:
        return sandbox.files.read("/tmp/gin.log")
    except Exception as exc:
        return f"Failed to read /tmp/gin.log: {exc}"


def start_gin_server(sandbox: Sandbox) -> None:
    print("\n== start gin server ==")

    result = sandbox.commands.run(
        "pkill -f '^/tmp/gin-app$' >/dev/null 2>&1 || true; "
        "rm -f /tmp/gin.log; "
        "nohup /tmp/gin-app > /tmp/gin.log 2>&1 & echo $!",
        timeout=30,
    )
    print("exit_code:", result.exit_code)
    print(result.stdout)
    print(result.stderr)

    if result.exit_code != 0:
        raise RuntimeError(
            "Failed to start Gin server\n"
            f"stdout: {result.stdout}\n"
            f"stderr: {result.stderr}"
        )

    app_pid = result.stdout.strip().splitlines()[-1] if result.stdout.strip() else ""
    if not app_pid:
        raise RuntimeError("Failed to capture Gin server process id")

    for _ in range(20):
        health = sandbox.commands.run(
            "curl -fsS --max-time 2 http://127.0.0.1:8080/health",
            timeout=5,
        )
        if health.exit_code == 0:
            print(f"server ready, pid={app_pid}")
            return

        proc = sandbox.commands.run(
            f"kill -0 {shlex.quote(app_pid)} >/dev/null 2>&1",
            timeout=5,
        )
        if proc.exit_code != 0:
            raise RuntimeError(
                "server process exited before becoming ready\n"
                f"{read_gin_log(sandbox)}"
            )

        time.sleep(0.5)

    raise RuntimeError(
        "server failed to become ready within 10 seconds\n"
        f"{read_gin_log(sandbox)}"
    )

# Agent generated file: go.mod
go_mod = """module gin-multi-file-demo

go 1.22

require github.com/gin-gonic/gin v1.10.0
"""

# Agent generated file: main.go
main_go = r'''package main

import (
    "gin-multi-file-demo/routes"

    "github.com/gin-gonic/gin"
)

func main() {
    r := gin.Default()

    routes.RegisterHealthRoutes(r)
    routes.RegisterUserRoutes(r)

    r.Run(":8080")
}
'''

# Agent generated file: routes/health.go
routes_health_go = r'''package routes

import "github.com/gin-gonic/gin"

func RegisterHealthRoutes(r *gin.Engine) {
    r.GET("/health", func(c *gin.Context) {
        c.JSON(200, gin.H{
            "status": "ok",
            "from": "official-sdk-multi-file",
        })
    })
}
'''

# Agent generated file: routes/user.go
routes_user_go = r'''package routes

import (
    "gin-multi-file-demo/handlers"

    "github.com/gin-gonic/gin"
)

func RegisterUserRoutes(r *gin.Engine) {
    r.GET("/users/:id", handlers.GetUser)
    r.POST("/users", handlers.CreateUser)
}
'''

# Agent generated file: handlers/user_handler.go
handlers_user_handler_go = r'''package handlers

import (
    "gin-multi-file-demo/models"

    "github.com/gin-gonic/gin"
)

func GetUser(c *gin.Context) {
    id := c.Param("id")

    c.JSON(200, gin.H{
        "id": id,
        "name": "demo-user",
    })
}

func CreateUser(c *gin.Context) {
    var req models.CreateUserRequest

    if err := c.ShouldBindJSON(&req); err != nil {
        c.JSON(400, gin.H{
            "error": err.Error(),
        })
        return
    }

    c.JSON(200, gin.H{
        "message": "user created",
        "name": req.Name,
        "age": req.Age,
    })
}
'''

# Agent generated file: models/user.go
models_user_go = r'''package models

type CreateUserRequest struct {
    Name string `json:"name"`
    Age  int    `json:"age"`
}
'''

def main() -> None:
    template_id = require_env("CUBE_TEMPLATE_ID")

    with Sandbox.create(template=template_id) as sandbox:
        print("== sandbox info ==")
        print(sandbox.get_info())

        print("\n== go version ==")
        result = sandbox.commands.run("go version", timeout=30)
        print("exit_code:", result.exit_code)
        print(result.stdout)
        print(result.stderr)

        print("\n== clean workspace ==")
        result = sandbox.commands.run("rm -rf /workspace/app/*", timeout=30)
        print("exit_code:", result.exit_code)
        print(result.stdout)
        print(result.stderr)

        print("\n== create project directories ==")
        result = sandbox.commands.run(
            "mkdir -p /workspace/app/routes /workspace/app/handlers /workspace/app/models",
            timeout=30,
        )
        print("exit_code:", result.exit_code)
        print(result.stdout)
        print(result.stderr)

        print("\n== write agent generated files ==")
        write_file_checked(sandbox, "/workspace/app/go.mod", go_mod)
        write_file_checked(sandbox, "/workspace/app/main.go", main_go)
        write_file_checked(sandbox, "/workspace/app/routes/health.go", routes_health_go)
        write_file_checked(sandbox, "/workspace/app/routes/user.go", routes_user_go)
        write_file_checked(sandbox, "/workspace/app/handlers/user_handler.go", handlers_user_handler_go)
        write_file_checked(sandbox, "/workspace/app/models/user.go", models_user_go)

        result = sandbox.commands.run(
            "find /workspace/app -type f | sort",
            timeout=30,
        )
        print(result.stdout)

        print("\n== go mod tidy ==")
        result = sandbox.commands.run(
            "cd /workspace/app && GOPROXY=https://goproxy.cn,direct GOSUMDB=sum.golang.google.cn go mod tidy",
            timeout=180,
        )
        print("exit_code:", result.exit_code)
        print(result.stdout)
        print(result.stderr)

        print("\n== go build ==")
        result = sandbox.commands.run(
            "cd /workspace/app && GOPROXY=https://goproxy.cn,direct GOSUMDB=sum.golang.google.cn go build -o /tmp/gin-app .",
            timeout=180,
        )
        print("exit_code:", result.exit_code)
        print(result.stdout)
        print(result.stderr)

        start_gin_server(sandbox)

        run_http_check(
            sandbox,
            "GET /health",
            "curl -fsS http://127.0.0.1:8080/health",
            {"status": "ok"},
        )

        run_http_check(
            sandbox,
            "GET /users/1001",
            "curl -fsS http://127.0.0.1:8080/users/1001",
            {"id": "1001", "name": "demo-user"},
        )

        run_http_check(
            sandbox,
            "POST /users",
            "curl -fsS -X POST http://127.0.0.1:8080/users "
            "-H 'Content-Type: application/json' "
            "-d '{\"name\":\"demo-user\",\"age\":20}'",
            {"message": "user created", "name": "demo-user", "age": 20},
        )

        print("\n== gin log ==")
        print(sandbox.files.read("/tmp/gin.log"))


if __name__ == "__main__":
    main()
