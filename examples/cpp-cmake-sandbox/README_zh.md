# C/C++ CMake 沙箱

[English README](README.md)

这是一个面向隔离 C/C++ 构建的 CubeSandbox 模板。它在
`cubesandbox-base` 上安装 GCC、CMake、Ninja 和 ccache，并演示暂停和恢复
后 CMake 构建目录及编译缓存仍然可用。

## 包含内容

```text
cpp-cmake-sandbox/
├── Dockerfile            # 可用于 Cube 的构建镜像
├── sample/               # 最小 CMake 工程和 CTest 用例
├── build_and_resume.py   # SDK 构建、快照、恢复和重新构建示例
├── env_utils.py          # 本地 .env 加载和校验
├── .env.example          # Cube 连接配置
└── requirements.txt      # 主机侧 SDK 依赖
```

## 前置条件

- 已部署的 CubeSandbox 和 `cubemastercli`。
- 构建机上的 Docker，以及 Cube 节点可拉取的镜像仓库。
- 运行主机侧脚本的机器安装 Python 3.9+。

## 构建模板

```bash
docker build --platform linux/amd64 \
  -t <your-registry>/cubesandbox-cpp-cmake:latest \
  examples/cpp-cmake-sandbox
docker push <your-registry>/cubesandbox-cpp-cmake:latest
```

镜像固定使用 `ghcr.io/tencentcloud/cubesandbox-base:2026.16`，因此 `envd`
已经在 `49983` 端口提供 Cube SDK 所需的命令服务。

## 在 CubeSandbox 中注册

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/cubesandbox-cpp-cmake:latest \
  --writable-layer-size 2G \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health

cubemastercli tpl watch --job-id <job-id>
```

任务状态变为 `READY` 后，记录输出中的 `template_id`。

## 运行快照缓存示例

```bash
cd examples/cpp-cmake-sandbox
cp .env.example .env
# 设置 E2B_API_URL、E2B_API_KEY 和 CUBE_TEMPLATE_ID。
pip install -r requirements.txt
python build_and_resume.py
```

脚本依次会：

1. 将 `sample/` 复制到沙箱工作区，使用 `cmake -G Ninja` 和 `ccache` 构建。
2. 执行 CTest 和生成的二进制文件。
3. 调用 `sandbox.pause()` 保存 MicroVM 状态并释放计算资源。
4. 使用 `Sandbox.connect(sandbox_id)` 重连，修改一个源文件的时间戳并重新
   构建。`ccache --show-stats` 会展示保留下来的 CMake 树和
   `~/.cache/ccache` 编译缓存。

脚本通过 `finally` 清理沙箱，即使构建失败也不会遗留运行中的实例。它不会
创建持久快照；如需长期保留或从快照批量创建实例，请参考
[`snapshot-rollback-clone`](../snapshot-rollback-clone) 示例。

## 本地镜像冒烟测试

以下检查不依赖 Cube 集群，只验证镜像中的工具链：

```bash
docker build -t cubesandbox-cpp-cmake:local .
docker run --rm cubesandbox-cpp-cmake:local sh -lc \
  'cp -a /opt/cpp-cmake-sandbox/sample /tmp/demo && \
   cmake -S /tmp/demo -B /tmp/demo/build -G Ninja \
     -DCMAKE_CXX_COMPILER_LAUNCHER=ccache && \
   cmake --build /tmp/demo/build && \
   ctest --test-dir /tmp/demo/build --output-on-failure'
```

## 资源和安全说明

- 建议从 `2G` 可写层开始；大型代码库或生成较多构建产物时应提高该数值。
- 自带工具链无需访问外网。构建不可信代码且无需下载依赖时，应保持
  `allow_internet_access=False`。
- 只有源文件和编译器输入兼容时，ccache 才会加速重复编译。基础镜像、编译器
  或构建参数发生变化时出现缓存未命中是正常现象。
- 示例保持最小化。使用 Conan 或 vcpkg 时，可在派生镜像中预装依赖，或仅通过
  CubeEgress 放行必要的仓库主机。

## 参考

- [自带镜像接入](../../docs/zh/guide/tutorials/bring-your-own-image.md)
- [快照、回滚与克隆](../snapshot-rollback-clone)
- [代码沙箱快速入门](../code-sandbox-quickstart)
