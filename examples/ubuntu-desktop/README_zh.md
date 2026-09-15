# Ubuntu 桌面沙箱

[English](README.md)

在浏览器里打开的 Ubuntu 22.04 桌面：默认 GNOME、左侧 Dock、文件管理器和终端，键鼠、截图、剪贴板都可用。画面通过 noVNC 在 `6080` 端口输出。

## 本地试跑

需要本机已安装 Docker。

```bash
cd examples/ubuntu-desktop
./run-local.sh
```

构建完成后，浏览器打开 [http://127.0.0.1:6080/](http://127.0.0.1:6080/) 即可看到桌面。

> 在国内构建较慢时，Dockerfile 默认使用腾讯云 Ubuntu 镜像源。需要官方源时加 `--build-arg APT_MIRROR=`。

停掉：

```bash
docker rm -f cube-ubuntu-desktop
```

## 在 Cube 里使用

### 1. 做成模板

把镜像推到集群能拉到的仓库，然后：

```bash
cubemastercli tpl create-from-image \
  --image <registry>/ubuntu-desktop:v1 \
  --alias ubuntu-desktop \
  --writable-layer-size 16Gi \
  --cpu 4000 \
  --memory 8192 \
  --expose-port 6080 \
  --expose-port 49983 \
  --probe 6080 \
  --probe-path / \
  --allow-internet-access
```

### 2. 开沙箱

```python
from cubesandbox import Sandbox

sb = Sandbox.create(template="ubuntu-desktop", timeout=-1)
print("桌面:", f"https://{sb.get_host(6080)}/")
```

打开返回的地址，就是这台沙箱的桌面。

### 3. 程序操作桌面

沙箱里的桌面跑在 `DISPLAY=:0`，程序里带上这个变量即可：

```python
sb.commands.run("DISPLAY=:0 gnome-screenshot -f /tmp/desk.png")
print(sb.files.read("/tmp/desk.png"))
```

## 文件

| 文件 | 作用 |
|------|------|
| `Dockerfile` | 桌面镜像 |
| `start-desktop.sh` | 启动桌面 |
| `run-local.sh` | 本地一键试跑 |
| `docker-compose.yml` | 本地编排 |
