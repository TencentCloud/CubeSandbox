# Gin API Sandbox Example

This example provides a CubeSandbox template for running Go Gin API projects. It shows how to build a Go/Gin runtime image, create a CubeSandbox template from it, and use the E2B-compatible SDK to create a sandbox, write a multi-file Gin project, build it, start the API service, and test HTTP endpoints.

This example focuses on a common Go web development scenario and can be used as a starting point for API development, API testing, and generated Gin project validation.

## Files

```text
.
├── Dockerfile
├── .dockerignore
├── .env.example
├── requirements.txt
├── official_sdk_gin_run.py
└── README.md
```

## Use Cases

This template is suitable for:

- running Go Gin API projects inside CubeSandbox
- testing generated or temporary Web API code
- validating multi-file Go projects in an isolated sandbox
- using CubeSandbox as a lightweight runtime for Go-based API examples

## Prerequisites

- CubeSandbox is deployed and CubeAPI is reachable.

- Docker is available on the machine used to build the image.

- `cubemastercli` is available.

- Python dependencies are installed:

  ```bash
  pip install -r requirements.txt
  ```

- The local environment can access Go module mirrors.

## Quick Start

### 1. Build the image

```bash
docker build -t gin-api-sandbox:latest .
```

### 2. Create the template

```bash
cubemastercli tpl create-from-image \
  --image gin-api-sandbox:latest \
  --writable-layer-size 5Gi \
  --expose-port 49983 \
  --expose-port 49999 \
  --expose-port 8080 \
  --probe 49999 \
  --probe-path /health \
  --allow-internet-access
```

Check the template status:

```bash
cubemastercli tpl list
```

Wait until the template becomes `READY`.

### 3. Configure environment variables

```bash
cp .env.example .env
```

Edit `.env`:

```env
E2B_API_URL=http://127.0.0.1:3000
E2B_API_KEY=e2b_000000
CUBE_TEMPLATE_ID=tpl-xxxxxxxxxxxxxxxxxxxxxxxx
```

Replace `CUBE_TEMPLATE_ID` with the template ID created in the previous step.

### 4. Run the SDK example

```bash
python3 official_sdk_gin_run.py
```

## What the Example Does

`official_sdk_gin_run.py` creates a sandbox from the template and writes a multi-file Gin project into `/workspace/app`:

```text
/workspace/app
├── go.mod
├── main.go
├── routes
│   ├── health.go
│   └── user.go
├── handlers
│   └── user_handler.go
└── models
    └── user.go
```

Then it runs the build and start commands inside the sandbox:

```bash
cd /workspace/app && go mod tidy
cd /workspace/app && go build -o /tmp/gin-app .
nohup /tmp/gin-app > /tmp/gin.log 2>&1 &
```

The command above is a simplified view of how the Gin service is started. In `official_sdk_gin_run.py`, the script also stops any previous `/tmp/gin-app` process, removes the old `/tmp/gin.log`, starts the new binary, and polls `GET /health` with `curl -fsS` for up to 10 seconds before running endpoint checks.

Finally, it tests the Gin API:

```bash
curl -fsS http://127.0.0.1:8080/health
curl -fsS http://127.0.0.1:8080/users/1001
curl -fsS -X POST http://127.0.0.1:8080/users \
  -H 'Content-Type: application/json' \
  -d '{"name":"demo-user","age":20}'
```

The Go files in `official_sdk_gin_run.py` are hard-coded for demonstration. In other use cases, they can be replaced with dynamically generated project files before being written into the sandbox.

## Expected Output

The example should show:

- The sandbox is created successfully.
- Go is available in the sandbox.
- Project files are written to `/workspace/app`.
- `go mod tidy` succeeds.
- `go build` succeeds.
- Gin starts on port `8080`.
- `/health`, `/users/:id`, and `POST /users` return JSON responses.
- `/tmp/gin.log` can be read.

## Resource Recommendations

The example is designed to run with the default `cubebox` instance type.

Recommended starting configuration:

- CPU: `2 vCPU`
- Memory: `2 GB`
- Writable layer size: `5Gi`
- Service port: `8080`

For larger Gin projects with more dependencies, increase the writable layer size or memory as needed.

## Known Limitations

- This example runs a single Gin service on port `8080`.
- Database, Redis, message queue, and multi-container scenarios are not included.
- Go module download requires outbound network access from the sandbox.
- The sample project is intended for template validation and API runtime testing, not for production deployment.
- Long-running tasks, checkpoint/resume workflows, and stateful workspace examples are not covered in this template.

## Deployment Notes

- This example assumes that CubeSandbox and CubeAPI have already been deployed.
- The template must expose port `8080`, which is used by the Gin API service.
- Port `49999` is reserved for the CubeSandbox template health probe. It is handled automatically by the CubeSandbox infrastructure; the generated Gin application does not need to bind to this port.
- The generated Gin project is written to `/workspace/app` inside the sandbox.
- The compiled binary is written to `/tmp/gin-app`.
- Runtime logs are written to `/tmp/gin.log`.
- If `go mod tidy` times out, check whether the sandbox has outbound network access and whether the Go module mirror is reachable.