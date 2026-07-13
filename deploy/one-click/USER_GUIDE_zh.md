# Cube Sandbox one-click 新版用户操作指南

本文面向 **纯 systemd 模式** 的新版 one-click。

适用原则：

- **新安装统一由 systemd 托管**
- **不再提供 systemd / 非 systemd 模式切换**
- `ONE_CLICK_DEPLOY_ROLE` 只用于区分节点角色：`control` / `compute`
- 旧的 shell `up/down` 脚本不再是新版本的日常运行入口

## 1. 你需要知道的变化

新版 one-click 安装完成后，会自动注册 systemd 单元：

- 控制节点：`cube-sandbox-control.target`
- 计算节点：`cube-sandbox-compute.target`

常见理解可以直接简化为：

- **安装**：`install.sh` / `install-compute.sh`
- **停机**：`down.sh`
- **健康检查**：`smoke.sh`
- **日常启停/排障**：`systemctl` / `journalctl`

## 2. 控制节点安装

在目标机上解压发布包后执行：

```bash
tar -xzf cube-sandbox-one-click-<version>.tar.gz
cd cube-sandbox-one-click-<version>
cp env.example .env
sudo ./install.sh
```

固定安装目录：

```bash
/usr/local/services/cubetoolbox
```

安装完成后会自动：

- 把 systemd unit 安装到 `/etc/systemd/system/`
- 按控制节点角色执行 `enable --now`
- 运行一轮 quickcheck（默认开启）

控制节点安装成功后，默认可访问：

```bash
http://<目标机IP>:12088
```

## 3. 计算节点安装

在计算节点机器上：

```bash
tar -xzf cube-sandbox-one-click-<version>.tar.gz
cd cube-sandbox-one-click-<version>
cp env.example .env
```

至少修改 `.env`：

```bash
ONE_CLICK_DEPLOY_ROLE=compute
ONE_CLICK_CONTROL_PLANE_IP=10.0.0.11
```

如果要显式指定当前计算节点 IP，再补充：

```bash
CUBE_SANDBOX_NODE_IP=10.0.0.12
```

然后执行：

```bash
sudo ./install-compute.sh
```

说明：

- `install-compute.sh` 本质上是设置 `ONE_CLICK_DEPLOY_ROLE=compute` 后调用 `install.sh`
- 计算节点安装后只会启动 compute 侧所需服务
- 计算节点需要能访问控制节点 `8089/tcp`
- 控制节点需要能访问计算节点 `9999/tcp`

## 4. 常用命令

### 4.1 基础健康检查

在发布包目录下执行：

```bash
sudo ./smoke.sh
```

它会调用安装目录中的 quickcheck，检查：

- systemd units 是否 active
- 控制节点依赖容器是否 ready
- `network-agent` / `cubemaster` / `cube-api` 健康接口
- socket、配置文件、运行时资源是否存在

如果你已经不在发布包目录，也可以直接执行：

```bash
sudo /usr/local/services/cubetoolbox/scripts/one-click/quickcheck.sh
```

### 4.2 停止服务

```bash
sudo ./down.sh
```

该命令会按当前安装角色停止：

- 控制节点：`cube-sandbox-control.target`
- 计算节点：`cube-sandbox-compute.target`

## 5. 用 systemd 管理服务

### 5.1 查看角色主 target 状态

控制节点：

```bash
sudo systemctl status cube-sandbox-control.target
```

计算节点：

```bash
sudo systemctl status cube-sandbox-compute.target
```

### 5.2 启动角色服务

控制节点：

```bash
sudo systemctl start cube-sandbox-control.target
```

计算节点：

```bash
sudo systemctl start cube-sandbox-compute.target
```

说明：安装时已经执行过 `enable --now`，一般不需要再手工 `enable`。

### 5.3 停止角色服务

控制节点：

```bash
sudo systemctl stop cube-sandbox-control.target
```

计算节点：

```bash
sudo systemctl stop cube-sandbox-compute.target
```

### 5.4 查看常用单元状态

控制节点常见：

```bash
sudo systemctl status \
  cube-sandbox-network-agent.service \
  cube-sandbox-cubelet.service \
  cube-sandbox-cubemaster.service \
  cube-sandbox-cube-api.service \
  cube-sandbox-mysql.service \
  cube-sandbox-redis.service \
  cube-sandbox-cube-proxy.service \
  cube-sandbox-coredns.service \
  cube-sandbox-dns.service
```

如果启用了 WebUI，再看：

```bash
sudo systemctl status cube-sandbox-webui.service
```

计算节点常见：

```bash
sudo systemctl status \
  cube-sandbox-network-agent.service \
  cube-sandbox-cubelet.service
```

### 5.5 查看日志

例如：

```bash
sudo journalctl -u cube-sandbox-cubelet.service -n 200 --no-pager
sudo journalctl -u cube-sandbox-network-agent.service -n 200 --no-pager
sudo journalctl -u cube-sandbox-cubemaster.service -n 200 --no-pager
sudo journalctl -u cube-sandbox-cube-api.service -n 200 --no-pager
```

实时追日志：

```bash
sudo journalctl -u cube-sandbox-cubelet.service -f
```

## 6. 常见运维动作

### 6.1 重启单个服务

```bash
sudo systemctl restart cube-sandbox-cubelet.service
sudo systemctl restart cube-sandbox-network-agent.service
```

控制节点上也常见：

```bash
sudo systemctl restart cube-sandbox-cubemaster.service
sudo systemctl restart cube-sandbox-cube-api.service
```

### 6.2 重做一轮健康检查

```bash
sudo ./smoke.sh
```

或：

```bash
sudo /usr/local/services/cubetoolbox/scripts/one-click/quickcheck.sh
```

### 6.3 查看节点注册信息（控制节点）

```bash
curl -fsS http://127.0.0.1:8089/internal/meta/nodes
```

## 7. 手动更新核心二进制

如果你拿到的是手动更新包，例如：

```bash
cube-manual-update-*.tar.gz
```

可以执行：

```bash
sudo ./deploy-manual.sh /path/to/cube-manual-update-*.tar.gz
```

该脚本会：

- 备份当前核心二进制
- 替换 `cubemaster` / `cubemastercli` / `cubelet` / `cubecli` / `network-agent`（按角色处理）
- 用 `systemctl restart` 重启相关核心服务
- 默认再跑一轮 quickcheck

如需跳过 quickcheck：

```bash
sudo ONE_CLICK_SKIP_QUICKCHECK=1 ./deploy-manual.sh /path/to/cube-manual-update-*.tar.gz
```

## 8. 推荐排障顺序

当安装或运行异常时，建议按下面顺序排查：

### 第一步：看角色 target

```bash
sudo systemctl status cube-sandbox-control.target
# 或
sudo systemctl status cube-sandbox-compute.target
```

### 第二步：跑 quickcheck

```bash
sudo ./smoke.sh
```

### 第三步：看关键服务日志

```bash
sudo journalctl -u cube-sandbox-cubelet.service -n 200 --no-pager
sudo journalctl -u cube-sandbox-network-agent.service -n 200 --no-pager
```

控制节点再补充：

```bash
sudo journalctl -u cube-sandbox-cubemaster.service -n 200 --no-pager
sudo journalctl -u cube-sandbox-cube-api.service -n 200 --no-pager
sudo journalctl -u cube-sandbox-mysql.service -n 200 --no-pager
sudo journalctl -u cube-sandbox-redis.service -n 200 --no-pager
```

### 第四步：确认配置是否写对

重点检查安装目录中的：

```bash
/usr/local/services/cubetoolbox/.one-click.env
```

尤其是：

- `ONE_CLICK_DEPLOY_ROLE`
- `ONE_CLICK_CONTROL_PLANE_IP`
- `ONE_CLICK_CONTROL_PLANE_CUBEMASTER_ADDR`
- `CUBE_SANDBOX_NODE_IP`
- `WEB_UI_ENABLE`

## 9. 重要说明

### 9.1 新版本没有“是否启用 systemd”的开关

不需要，也不应该再通过环境变量选择 systemd / 非 systemd 模式。

新安装统一按 systemd 方式运行。

### 9.2 旧 shell 启停脚本不是新版本操作入口

对于新版本用户，日常操作请使用：

- `install.sh`
- `install-compute.sh`
- `down.sh`
- `smoke.sh`
- `systemctl`
- `journalctl`

旧的 legacy shell 启停逻辑仅用于安装器兼容历史版本升级，不属于新版本的用户操作界面。

### 9.3 安装目录中的运行时环境文件

安装后实际生效的环境文件是：

```bash
/usr/local/services/cubetoolbox/.one-click.env
```

安装器会把 `.env` 的内容复制进去，并补充运行期所需字段。

如果你后续修改了这里的配置，通常需要按变更内容重启相关 systemd 服务后才会生效。

## 10. 最短操作清单

### 控制节点

```bash
cp env.example .env
sudo ./install.sh
sudo ./smoke.sh
sudo ./down.sh
sudo systemctl start cube-sandbox-control.target
```

### 计算节点

```bash
cp env.example .env
# 编辑 ONE_CLICK_DEPLOY_ROLE=compute
# 编辑 ONE_CLICK_CONTROL_PLANE_IP=<control-node-ip>
sudo ./install-compute.sh
sudo ./smoke.sh
sudo ./down.sh
sudo systemctl start cube-sandbox-compute.target
```
