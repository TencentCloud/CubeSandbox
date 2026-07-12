# Ruby + Sinatra 沙箱模板

[English](README.md)

这是一个可复用的 Ruby Web 开发模板：预装 Ruby、Bundler、Sinatra 与 Puma，
在 `4567` 端口提供最小有状态 API，并保留 Cube 基础镜像的 `envd:49983`。

## 适用场景

- 隔离运行不可信或 AI 生成的 Ruby 应用
- 可复现的 Sinatra API 开发与测试
- 使用 pause/resume 保存有状态长任务
- 扩展为 Rails、Hanami、Sidekiq 或 Ruby Agent 运行底座

## 构建并注册模板

```bash
docker build -t ruby-sinatra-cube:latest examples/ruby-sinatra-sandbox
docker tag ruby-sinatra-cube:latest <registry>/ruby-sinatra-cube:latest
docker push <registry>/ruby-sinatra-cube:latest

cubemastercli tpl create-from-image \
  --image <registry>/ruby-sinatra-cube:latest \
  --writable-layer-size 2G \
  --expose-port 49983 \
  --expose-port 4567 \
  --probe 49983 \
  --probe-path /health
```

模板探针检查继承的 `envd`，业务流量走 `4567`。`Gemfile` 固定依赖版本，
避免不同构建得到不一致的运行环境。

## 运行最小示例

```bash
cd examples/ruby-sinatra-sandbox
python -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt
cp .env.example .env
# 在 .env 中填写 E2B_API_URL 与 CUBE_TEMPLATE_ID。
python run_example.py
```

脚本会创建沙箱、等待 `/health`、递增持久化计数器，并输出 CubeProxy URL。

## 快照断点续跑

```bash
python resume_example.py
```

脚本将 `41` 写入 `/workspace/data/counter.txt`，暂停 MicroVM，再连接同一沙箱并
验证文件仍然存在。可变数据应放在 `/workspace`，依赖则应在镜像构建阶段安装。

## 安全与资源建议

- 建议最低配置：1 vCPU、512 MiB 内存、2 GiB 可写层。
- 应用运行时无需出网。执行不可信 Ruby 代码时使用
  `allow_internet_access=False` 或显式白名单。
- 不要把密钥写入镜像。短期配置通过 `Sandbox.create(envs=...)` 注入，高价值密钥
  使用 CubeEgress 链路注入。
- `49983` 保留给 `envd`，只暴露用户实际需要的业务端口。

## 已知限制与排错

| 现象 | 原因 / 处理方式 |
| --- | --- |
| 模板探针超时 | 探测 `49983/health`，不要探测 Sinatra 端口 |
| 4567 端口返回 `502` | 等待 Puma 启动；用 `sandbox.commands.run("ps aux")` 检查进程 |
| TLS 证书校验失败 | 将 `REQUESTS_CA_BUNDLE` 指向部署 CA |
| `bundle install` 失败 | 构建阶段放行 `rubygems.org` 或配置内部镜像 |
| 原生扩展 gem 构建失败 | 在 Dockerfile 构建层加入对应系统头文件 |
| pause/resume 不可用 | 升级到 CubeSandbox `>= 0.3.0` |

注册模板前也可在本地发布 `4567` 和 `49983` 端口进行镜像冒烟测试。
