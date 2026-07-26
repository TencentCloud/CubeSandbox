# Java Spring Boot 开发测试沙箱

[English](README.md)

这个示例提供一个面向企业 Java 后端开发与测试的 CubeSandbox 模板。模板在受控构建阶段预热
Maven 依赖缓存；demo 阶段使用离线 Maven 构建，启动真实 Spring Boot HTTP 服务，为工作区
创建快照，再从 checkpoint fork 一个新沙箱，并验证 fork 后的沙箱可以复用编译好的 jar 和
有状态任务数据。两个运行时沙箱都使用默认拒绝的公网出口策略。

## 展示能力

1. **隔离 MicroVM 中的 Spring Boot Web 服务**：应用在 `8080` 端口暴露
   `/health`、`/api/info`、`POST /api/tasks` 和 `GET /api/tasks/{id}`。
2. **模板预热 Maven 缓存**：Dockerfile 执行 `mvn dependency:go-offline`；demo 使用
   `mvn --offline package`，checkpoint 保存 `/workspace/.m2/repository` 和 `target/*.jar`。
3. **有状态工作区 fork**：任务状态写入
   `/tmp/cubesandbox-spring/state/tasks.json`，fork 后的新沙箱可以继续读取。
4. **Web 服务路由**：demo 通过 `sandbox.get_host(8080)` 走 CubeSandbox 路由访问
   Spring Boot 服务。
5. **验证默认拒绝出口**：demo 使用 `allow_internet_access=False` 创建沙箱，先证明公网 HTTPS
   请求被阻断，再使用 Maven `--offline` 完成构建。

## 为什么这体现 CubeSandbox 价值

Java 后端项目第一次运行常常要先下载 Maven 依赖、编译代码，然后测试才能开始。CubeSandbox
模板可以捕获受控构建阶段产生的依赖缓存，快照则可以保留后续构建产物和服务状态。这个模式适合
Agent 驱动的后端调试、API 契约测试、回归复现，以及需要真实 JVM 服务的一次性功能分支环境。

## 文件说明

- `Dockerfile`：使用 digest 固定基础镜像的 Java 21 + Maven 镜像，并预热 Maven 依赖缓存。
- `pom.xml` 与 `src/main/...`：最小 Spring Boot 后端服务。
- `scripts/run_demo.py`：基于 E2B 兼容 SDK 和本地 dev sidecar 的端到端 demo。
- `env.example`：本地 CubeSandbox API/proxy 配置模板。
- `output/`：成功运行后下载的 manifest。

## 第一步：构建镜像

在 Cube 节点运行时可以访问的位置构建镜像：

```bash
docker build -t cubesandbox-java-springboot-web:latest .
```

## 第二步：注册模板

注册镜像时暴露 envd 的 `49983` 和 Spring Boot 的 `8080`：

```bash
cubemastercli tpl create-from-image \
    --image               cubesandbox-java-springboot-web:latest \
    --writable-layer-size 2G \
    --expose-port         49983 \
    --expose-port         8080 \
    --probe               49983 \
    --probe-path          /health
```

记录返回的模板 ID，例如 `tpl-xxxxxxxxxxxxxxxxxxxxxxxx`。

## 第三步：配置客户端

安装本地依赖：

```bash
pip3 install -r requirements.txt
```

运行 20 个聚焦 Spring Boot 与任务状态的测试：

```bash
mvn test
```

创建 `.env` 并填写模板 ID：

```bash
cp env.example .env
```

本地 dev VM 中通常类似这样：

```bash
E2B_API_URL="http://127.0.0.1:13000"
CUBE_REMOTE_PROXY_BASE="https://127.0.0.1:11443"
CUBE_TEMPLATE_ID="tpl-xxxxxxxxxxxxxxxxxxxxxxxx"
E2B_API_KEY=e2b_dummyapikeyforlocaltest
```

## 第四步：运行 Demo

```bash
python3 scripts/run_demo.py
```

脚本会执行：

1. 基于 Java/Spring Boot 模板创建默认拒绝出口的沙箱。
2. 证明公网 HTTPS 请求被阻断。
3. 上传 Spring Boot 项目到 `/workspace/java-springboot-web`。
4. 使用模板预热的 Maven 缓存执行 `mvn --offline -DskipTests package`，构建 jar。
5. 启动服务，并通过 CubeSandbox 路由调用 `/health`、`/api/info` 和
   `POST /api/tasks`。
6. 验证任务状态已写入 `/tmp/cubesandbox-spring/state/tasks.json`。
7. 停止 JVM、确认进程退出、落盘状态，再创建 checkpoint snapshot。
8. 从 snapshot 启动一个新的默认拒绝出口沙箱。
9. 验证 `/workspace/.m2/repository`、`target/*.jar` 和任务状态文件都被继承。
10. 直接用继承的 jar 启动 Spring Boot，并读取原始任务。
11. 下载 `output/manifest.json`，作为本次运行的结构化证明。

manifest 示例：

```json
{
  "scenario": "java_springboot_devtest_sandbox",
  "features": [
    "spring_boot_web_service",
    "snapshot_warmed_maven_cache",
    "stateful_workspace_fork",
    "default_deny_egress_offline_build"
  ],
  "state_inherited": true,
  "artifact_inherited": true,
  "maven_cache_inherited": true,
  "internet_access_denied": true,
  "state_flushed_before_snapshot": true
}
```

## 受限出口说明

Dockerfile 在受控的模板构建阶段完成 Maven 依赖预热。运行时两个沙箱都设置
`allow_internet_access=False`；demo 先验证公网 HTTPS 请求失败，再使用 Maven offline 模式
构建。生产或合规集群建议使用以下方式之一：

- 在模板镜像中预先下载好依赖。
- 通过 `settings.xml` 指向内部 Maven 仓库镜像。
- 在受控网络环境完成构建和缓存预热，把缓存保存在模板中，再通过 snapshot/fork 支撑重复测试。

本 demo 的 fork 阶段直接使用继承的 jar 启动，不需要重新下载依赖。

## 资源建议

- 最低建议：2 vCPU / 2 GiB 内存。
- writable layer 建议至少 2 GiB，用于 Maven cache 和构建输出。
- JVM 容器参数示例：

```bash
SPRING_BOOT_JAVA_OPTS="-XX:+UseContainerSupport -XX:MaxRAMPercentage=75"
```

## 已知限制

- v1 不引入 Redis、MySQL、Kafka 或多服务编排，避免掩盖 snapshot/fork 主线。
- 模板镜像构建需要受控网络，除非基础镜像和 Maven 依赖均由内部镜像提供。
- snapshot 耗时取决于本地 CubeSandbox 部署和存储后端。
