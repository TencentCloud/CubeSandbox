---
title: 升级后 CUBE_PROXY_DNSMASQ_MODE 被重置导致 DNS 预检失败
author: zhengzongwei
date: 2026-10-27
tags:
  - one-click
  - upgrade
  - dns
  - dnsmasq
lang: zh-CN
---

# 升级后 CUBE_PROXY_DNSMASQ_MODE 被重置导致 DNS 预检失败

## 问题现象

在没有 `resolvectl` 和 NetworkManager 的主机上（如 Debian 13 最小安装，
ifupdown + dhclient），首次安装时使用以下命令可以成功：

```bash
CUBE_PROXY_DNSMASQ_MODE=standalone ./install.sh
```

但下一次升级运行时，预检阶段直接报错退出：

```text
[one-click] ERROR: DNS setup requires resolvectl or NetworkManager (or set CUBE_PROXY_DNSMASQ_MODE=standalone)
```

即使升级命令行上原样传入了同一个环境变量。

## 环境信息

- Cube Sandbox 版本：v0.7.3（one-click，先安装后升级）
- 宿主机 OS：Debian 13 (trixie) 最小安装 —— 无 `resolvectl`、无
  NetworkManager
- 部署模式：单节点控制面（`role=control`）
- 相关组件：`deploy/one-click/install.sh`、
  `deploy/one-click/lib/common.sh`

## 根因分析

`CUBE_PROXY_DNSMASQ_MODE`（由
[#740](https://github.com/TencentCloud/CubeSandbox/pull/740) 引入）只在
`check_dns_preflight` 中被校验和消费，安装器本身并不会把它写入
`.one-click.env`：bundle `.env` 中提供的值会在安装阶段被原样拷贝进
`.one-click.env`，因此该通道本身能保留，只有**仅通过进程环境变量**传入
的值会丢失。同时它也不在 `ONE_CLICK_TOGGLE_KEYS` 名单里，因此保护其他
显式覆盖项的快照/重放机制不覆盖它。

升级运行时，`merge_env_three_way` 用 `env.example`（默认值
`networkmanager`）加旧运行时 env（缺这个 key）生成合并 env，随后
`load_env_file` 会 source 该合并文件——**覆盖运维通过进程环境变量传入
的值**。预检看到的变成 `networkmanager`，既没有 `resolvectl` 也没有
加载 NetworkManager，直接退出。

触发条件：首次安装**仅通过进程环境变量**传递
`CUBE_PROXY_DNSMASQ_MODE`，且宿主机没有 `resolvectl` /
NetworkManager。

## 解决方案

### 1. 修复（本次变更）

把该 key 注册进 `ONE_CLICK_TOGGLE_KEYS`，让快照/重放机制在升级 merge
后保留显式的进程环境和 bundle-`.env` 意图；并用 `upsert_env_kv` 将
解析后的值持久化到其他 toggle key 旁边。之后的升级不再丢失。
`.env` 中的值会在 `load_env_file` 之后按 shell 语义回放，因此
`CUBE_PROXY_DNSMASQ_MODE="standalone"` 这类带引号或行尾注释的写法也能生效；
`online-install.sh` 的下载前预检也镜像了同样的 DNS 检查，因此文档中的
`curl ... | CUBE_PROXY_DNSMASQ_MODE=standalone bash` 安装路径同样可用。

### 2. 已受影响主机的临时规避

重新运行升级前，先把模式写进运行时 env 文件：

```bash
echo "CUBE_PROXY_DNSMASQ_MODE=standalone" >> /usr/local/services/cubetoolbox/.one-click.env
./install.sh --mode auto   # 按原参数
```

自带 `resolvectl` 或 NetworkManager 的主机不受影响。

> 如果 bundle `.env` 里仍有旧版生效的
> `CUBE_PROXY_DNSMASQ_MODE=networkmanager` 行，仅当运行时已是 `standalone`
> 时安装器才会忽略它（并打印原因），上面写入的值会保留；仍建议删除该旧行。

> 注意：`CUBE_PROXY_DNSMASQ_MODE` 属于安装器开关，bundle `.env` 中出现的
> 该 key 通常会被视为显式设置。模板中该行已注释掉（代码内默认值为
> `networkmanager`），因此新拷贝的 `cp env.example .env` 不会再覆盖该模式。
> 升级时若运行时已是非默认模式（`standalone`），`.env` 里的 `networkmanager`
> 会被当作旧版整份拷贝，忽略并打印日志；这类主机要切回 `networkmanager`，
> 请使用进程环境变量或直接改 `.one-click.env`。首次安装（无运行时值）时
> `.env` 仍然权威。

## 参考

- 修复 PR：本次变更（one-click 持久化）
- 引入该模式的 PR：[#740](https://github.com/TencentCloud/CubeSandbox/pull/740)
- 代码位置：`deploy/one-click/install.sh` →
  `check_dns_preflight()` / `deploy/one-click/lib/common.sh` →
  `ONE_CLICK_TOGGLE_KEYS`
