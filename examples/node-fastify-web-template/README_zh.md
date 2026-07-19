# Node.js / Fastify Web 模板

[English](README.md)

本示例展示如何构建一个可用于 CubeSandbox 的 Node.js Web 开发沙箱模板。模板同时运行用于 SDK 访问的 CubeSandbox envd，以及作为用户 Web 服务的 TypeScript Fastify API。

## 示例内容

- 自定义 Node.js Web 开发沙箱模板
- 运行在 `3000` 端口的 Fastify + TypeScript API
- 运行在 `49983` 端口的 CubeSandbox envd 集成
- `/workspace/state` 下的有状态工作区数据
- 通过 CubeSandbox 兼容的 E2B SDK 演示快照 / 恢复行为
- 基于 Docker 的可复现沙箱运行时

## 技术栈

| 组件 | 选型 |
|------|------|
| 基础镜像 | `node:24-bookworm-slim` |
| CubeSandbox envd 来源 | `ghcr.io/tencentcloud/cubesandbox-base:2026.16` |
| Web 框架 | Fastify |
| 语言 | TypeScript |
| 本地开发运行器 | `tsx` |
| 构建方式 | `tsc` |
| 运行命令 | `node dist/main.js` |

## 本地开发

```bash
npm install
npm run dev
```

构建并运行编译后的服务：

```bash
npm run build
npm start
```

运行测试：

```bash
npm test
```

测试覆盖状态文件损坏、请求体 schema 校验失败、非法 JSON，以及使用随机端口启动真实 HTTP listener 的集成验证。

服务默认监听 `0.0.0.0:3000`。可以通过 `PORT` 修改 Web API 端口，通过 `STATE_DIR` 修改状态目录。

常用接口：

| 接口 | 说明 |
|------|------|
| `GET /` | HTML 入口页 |
| `GET /health` | 就绪检查 |
| `GET /api/info` | 运行时信息 |
| `POST /api/counter` | 递增 `/workspace/state/counter.json` |
| `POST /api/write-note` | 追加笔记到 `/workspace/state/notes.jsonl` |

## Docker 构建与本地验证

```bash
docker build -t cube-node-fastify-web:latest .

docker run --rm -d \
  -p 49983:49983 \
  -p 3000:3000 \
  --name cube-node-fastify-web \
  cube-node-fastify-web:latest

curl -s -o /dev/null -w "envd /health => %{http_code}\n" \
  http://127.0.0.1:49983/health

curl -s http://127.0.0.1:3000/health

docker rm -f cube-node-fastify-web
```

`49983` 端口用于 CubeSandbox envd 和 SDK 操作，`3000` 端口用于本模板暴露的 Fastify Web 服务。

## 创建 CubeSandbox 模板

先将镜像推送到 CubeSandbox 节点可访问的镜像仓库，然后创建模板：

```bash
docker tag cube-node-fastify-web:latest <your-registry>/cube-node-fastify-web:latest
docker push <your-registry>/cube-node-fastify-web:latest

cubemastercli -a 127.0.0.1 -p 8089 tpl create-from-image \
  --image <your-registry>/cube-node-fastify-web:latest \
  --writable-layer-size 1G \
  --expose-port 49983 \
  --expose-port 3000 \
  --probe 49983 \
  --probe-path /health
```

将输出的模板 ID 填入 `.env` 的 `CUBE_TEMPLATE_ID`。

## 运行 Python 演示

```bash
pip install -r requirements.txt
cp .env.example .env
```

编辑 `.env`：

| 变量 | 说明 |
|------|------|
| `E2B_API_KEY` | CubeSandbox / E2B 兼容 API Key |
| `E2B_API_URL` | CubeAPI 地址，例如 `http://127.0.0.1:3000` |
| `CUBE_TEMPLATE_ID` | 基于此 Docker 镜像创建的模板 ID |

`E2B_API_URL` 指向宿主机侧的 CubeAPI，而不是本模板的 Fastify 服务。两者处于不同网络上下文，因此都使用 `3000` 端口并不冲突；`8089` 属于 CubeMaster，由 `cubemastercli` 使用。

运行基础 Web API 演示：

```bash
python run_demo.py
```

运行快照 / 恢复演示：

```bash
python snapshot_resume.py
```

快照 / 恢复演示会先递增计数器，随后暂停沙箱，重新连接同一个沙箱，再次等待 Fastify 就绪，并再次递增计数器。计数器文件存放在 `/workspace/state` 下，因此恢复后计数值应当继续递增。

## 资源建议

- 建议使用 `1G` 或更大的可写层。
- 本模板适合轻量 Web API、Mock 后端和 Agent 工具服务。
- 如果加入构建工具、数据库或更重的应用依赖，建议提高 CPU 和内存配置。

## 已知限制

- 这是演示模板，不是生产加固过的 Node.js 镜像。
- 将 `node_modules` 放入镜像会增加镜像体积。
- 镜像仓库必须能被 CubeSandbox 节点访问。
- 恢复沙箱后，应先等待 Web 服务就绪接口可用，再发送应用请求。
