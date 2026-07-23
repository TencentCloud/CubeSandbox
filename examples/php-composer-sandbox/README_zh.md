# PHP + Composer 沙箱模板

[English](README.md)

这是一个可直接用于 CubeSandbox 的 PHP Web 开发模板，内置 Composer。镜像在
`8080` 启动最小 JSON API，并继承 `cubesandbox-base` 在 `49983` 运行 envd。
两个宿主机脚本分别演示通过 CubeProxy 访问 API，以及暂停/恢复后保留
`/workspace` 中的应用状态。

## 包含内容

- Ubuntu 22.04 上的 PHP CLI、`php-mbstring`、`php-xml` 与 Composer。
- 最小 PHP 路由：`/health`、`/api/hello`、以及将状态写入
  `/workspace/state.json` 的 `/api/state`。
- `run_example.py`：创建沙箱，检查 PHP/Composer 版本，并通过 CubeProxy
  调用 API。
- `resume_example.py`：写入状态、暂停 MicroVM、重连，再校验状态文件仍存在。

## 前置条件

- 已运行的 CubeSandbox 部署，且 `cubemastercli` 已连接到该集群。
- 可供所有 CubeMaster 节点拉取镜像的 Docker 与镜像仓库。
- 宿主机 Python 3.9+，用于运行示例脚本。

模板刻意不携带第三方 PHP 包，因此可作为干净的 Composer 起点。需要依赖时，
请修改 `app/composer.json`，提交生成的 `composer.lock`，然后重建镜像。

## 1. 构建并推送镜像

在仓库根目录执行：

```bash
docker build --platform linux/amd64 \
  -t <your-registry>/php-composer-cube:latest \
  examples/php-composer-sandbox
docker push <your-registry>/php-composer-cube:latest
```

若构建工作站无法稳定访问 Ubuntu 默认软件源，可仅在本次构建传入可访问镜像，
例如 `--build-arg APT_MIRROR=https://<reachable-mirror>/ubuntu`。

本地容器冒烟测试（同时暴露应用和 envd 端口）：

```bash
docker run --rm -d --name php-composer-cube \
  -p 8080:8080 -p 49983:49983 \
  php-composer-cube:latest
curl -fsS http://127.0.0.1:8080/health
curl -s -o /dev/null -w 'envd=%{http_code}\n' http://127.0.0.1:49983/health
docker stop php-composer-cube
```

两个 HTTP 检查都必须成功，其中 envd 返回 `204`。

## 2. 注册 Cube 模板

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/php-composer-cube:latest \
  --writable-layer-size 2G \
  --expose-port 49983 \
  --expose-port 8080 \
  --probe 49983 \
  --probe-path /health

cubemastercli tpl watch --job-id <job-id>
```

只有当任务输出 `template_status: READY` 后，才可使用其中的模板 ID。
`49983` 仅用于平台就绪探测，`8080` 才是业务 API 端口。

## 3. 运行示例

```bash
cd examples/php-composer-sandbox
cp .env.example .env
# 在 .env 中设置 E2B_API_URL 和 CUBE_TEMPLATE_ID。
python3 -m pip install -r requirements.txt

# 创建一次性沙箱，并调用 /health 与 /api/hello。
python3 run_example.py

# 写入 /workspace/state.json，暂停、重连后校验状态。
python3 resume_example.py
```

脚本通过 `finally` 或上下文管理器清理创建的沙箱。

## 资源与安全说明

- 建议先分配 `2G` 可写层；若把 Composer 依赖或构建产物保存到镜像/工作区，
  应按实际大小提高该值。
- 示例运行时不需要访问外网。若在沙箱中安装 Composer 包，请配置明确的出口
  白名单，而非开放不限网络。
- 示例 API 没有鉴权，只用于演示。生产服务应在创建沙箱时设置
  `network={"allow_public_traffic": False}`，并携带每个沙箱的访问令牌；
  可参考 `examples/code-sandbox-quickstart/restrict_public_access.py`。
- 暂停会保留 VM 快照和可写工作区；`kill` 会永久删除二者。需要暂停后重连时，
  不要用上下文管理器包裹该沙箱。

## 验证

以下检查不需要 Cube 集群：

```bash
python3 -m unittest discover -s examples/php-composer-sandbox -p 'test_*.py'
python3 -m py_compile examples/php-composer-sandbox/*.py
git diff --check
```
