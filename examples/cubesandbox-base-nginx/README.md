# cubesandbox-base-nginx demo

A minimal image that stacks nginx on top of [`cubesandbox-base`](../../docker/Dockerfile.cube-base),
so you can test the "Bring Your Own Image" flow end-to-end without any
real application.

- envd listens on `:49983` (Cube readiness probe) — inherited from the base image.
- nginx listens on `:80` and serves a tiny static page.

See [Bring Your Own Image (envd)](../../docs/guide/tutorials/bring-your-own-image.md)
for the full tutorial.

## Build

```bash
docker build -t cubesandbox-demo-nginx:latest .
```

## Run & verify locally

```bash
docker run --rm -d \
    -p 8080:80 \
    -p 49983:49983 \
    --name cube-demo-nginx \
    cubesandbox-demo-nginx:latest

# nginx: should print the demo landing page HTML
curl -s http://127.0.0.1:8080/

# envd readiness probe: should return 204
curl -s -o /dev/null -w "envd /health => %{http_code}\n" \
    http://127.0.0.1:49983/health

docker rm -f cube-demo-nginx
```

## Register as a Cube template

```bash
cubemastercli tpl create-from-image \
    --image       <your-registry>/cubesandbox-demo-nginx:latest \
    --writable-layer-size 1G \
    --expose-port 49983 \
    --expose-port 80 \
    --probe       49983 \
    --probe-path  /health
```

`--probe 49983 --probe-path /health` points Cube at envd (guaranteed to
return `204` within ~1s); nginx's `:80` stays exposed for your actual
traffic.

## Try it with the E2B SDK

After registering the template, [`test_files.py`](./test_files.py)
boots a sandbox from it and does two things:

1. reads `/etc/nginx/nginx.conf` via `sandbox.files.read(...)`
2. sends an HTTPS request to the sandbox's port `80` and prints the
   nginx response

```bash
pip install -r requirements.txt

cp env.example .env
# fill in E2B_API_URL and CUBE_TEMPLATE_ID

python3 test_files.py
```
