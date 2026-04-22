# 连接到已有 Cube 集群

如果你只是想尽快跑通，先看示例目录：

- 示例代码：[examples/e2b-dev-sidecar](https://github.com/tencentcloud/CubeSandbox/tree/master/examples/e2b-dev-sidecar)
- 中文说明：[README_zh.md](https://github.com/tencentcloud/CubeSandbox/tree/master/examples/e2b-dev-sidecar/README_zh.md)
- 英文说明：[README.md](https://github.com/tencentcloud/CubeSandbox/tree/master/examples/e2b-dev-sidecar/README.md)

## 为什么需要dev-sidecar?

由于E2B需要沙箱URL是通过泛解析去解析到目标集群的public-ip，因此如果您是在生产环境中部署，您需要在私有dns中，添加一条A记录： `*.cube.app => <your cube master node ip>`.

而我们在开发阶段，在自己电脑上加泛解析是很麻烦的。因此我们做了个dev-sidecar,帮助您在不更改 E2B SDK的情况下，轻松的在开发机上连接Cube集群来创建实例。

这篇文档只做一件事：帮你更快判断自己该怎么填 `dev-sidecar` 的环境变量。

## 先走成功路径

### 场景一：你是在本机先用 `dev-env` 起的 Cube

这是 `dev-sidecar` 最自然、也最推荐的开发路径。

如果你是按 [开发环境（QEMU 虚机）](./dev-environment) 启动的本地开发环境，那么 `examples/e2b-dev-sidecar/env.example` 里的默认值本来就是为这个场景准备的。

直接这样做：

```bash
cd examples/e2b-dev-sidecar
pip install -r requirements.txt
cp env.example .env
```

然后只需要补上模板 ID：

```bash
E2B_API_URL="http://127.0.0.1:13000"
CUBE_REMOTE_PROXY_BASE="https://127.0.0.1:11443"
E2B_API_KEY="dummy"
CUBE_TEMPLATE_ID="<your-template-id>"
```

运行：

```bash
python demo.py
```

这里最关键的一点是：

- `127.0.0.1:13000` 不是随便写的，它对应 `dev-env` 暴露出来的 CubeAPI
- `127.0.0.1:11443` 不是随便写的，它对应 `dev-env` 暴露出来的 CubeProxy

也就是说，如果你本机已经起了 `dev-env`，通常不需要改地址，只需要填模板 ID。

### 场景二：你要连接另一台机器上的已有 Cube 集群

这时也还是用同一个 `dev-sidecar` 示例，只是把默认地址替换成远端集群的地址：

```bash
E2B_API_URL="http://<node-ip>:3000"
CUBE_REMOTE_PROXY_BASE="https://<node-ip>:443"
E2B_API_KEY="dummy"
CUBE_TEMPLATE_ID="<your-template-id>"
```

然后同样运行：

```bash
python demo.py
```

## 只记这几件事

- `E2B_API_URL`
  控制面地址。`dev-env` 默认就是 `http://127.0.0.1:13000`。
- `CUBE_REMOTE_PROXY_BASE`
  数据面地址。`dev-env` 默认就是 `https://127.0.0.1:11443`。
- `E2B_API_KEY`
  SDK 要求非空；如果你的集群开启鉴权，就填真实值，否则写`dummy`。
- `CUBE_TEMPLATE_ID`
  你要创建沙箱时使用的模板 ID。

其他配置项先不用急着看。绝大多数开发接入，先把这四个值弄对就够了。

## 最容易踩的坑

- 本机明明跑的是 `dev-env`，却把地址写成了虚机内地址，而不是宿主机暴露出来的 `13000/11443`
- 把 `CUBE_REMOTE_PROXY_BASE` 错写成 sidecar 自己的监听地址
- 忘了填 `CUBE_TEMPLATE_ID`
- 集群开启了鉴权，但 `E2B_API_KEY` 还在用 dummy

## 进一步阅读

如果你想直接看最容易理解的版本，优先看示例 README：

- [examples/e2b-dev-sidecar/README_zh.md](https://github.com/tencentcloud/CubeSandbox/tree/master/examples/e2b-dev-sidecar/README_zh.md)

如果你已经准备把 sidecar 接到自己的代码里，再回头看示例里的：

- `demo.py`
- `dev_sidecar.py`
