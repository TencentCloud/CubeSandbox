# 下载与 Release 说明

CubeSandbox 的所有发布物都在同一个 [Releases 页面](https://cnb.cool/CubeSandbox/CubeSandbox/-/releases)上。先按你的场景对号入座，再跳到对应小节照做：

| 你的场景 | 需要手动下载什么 | 去哪一节 |
| --- | --- | --- |
| 标准部署（[快速开始](./quickstart.md)、[裸金属](./bare-metal-deploy.md)） | 什么都不用 | [标准部署](#标准部署-无需手动下载) |
| PVM 部署（云服务器无 `/dev/kvm`） | 宿主机内核包（rpm/deb） | [PVM 部署](#pvm-部署-下载宿主机内核包) |
| 自建发布包 / 替换内核或镜像 | guest 内核、guest 镜像 | [自建部署](#自建部署-下载-guest-内核与镜像) |

::: tip 只是照着快速开始部署？
那你不需要本页面 —— [在线安装脚本](./quickstart.md)会自动下载一键部署包并解析全部产物。
:::

## 标准部署：无需手动下载

一行命令在线安装（详见[快速开始](./quickstart.md)）：

```bash
curl -sL https://cnb.cool/CubeSandbox/CubeSandbox/-/git/raw/master/deploy/one-click/online-install.sh | MIRROR=cn bash
```

需要手动下载整包（`cube-sandbox-one-click-<版本>-<架构>.tar.gz`，架构为 `amd64` / `arm64`）：

```bash
wget https://cnb.cool/CubeSandbox/CubeSandbox/-/releases/download/v0.6.0/cube-sandbox-one-click-v0.6.0-amd64.tar.gz
```

## PVM 部署：下载宿主机内核包

PVM 部署中，唯一需要手动下载的就是宿主机内核包 —— 装上它、重启进入 PVM 内核即可获得 KVM 能力。PVM 的 guest 内核（`vmlinux-pvm`）已内置于一键部署包，安装时设 `CUBE_PVM_ENABLE=1` 即自动激活。安装、GRUB 配置与重启的完整步骤见 [PVM 部署 — 第一步](./pvm-deploy.md#第一步安装-pvm-宿主机内核)，本节只讲怎么拿到包。

内核包发布在专属的 `kernel-release-*` Release 上，直接到发布页下载：

1. 打开 [CubeSandbox Releases 页面](https://cnb.cool/CubeSandbox/CubeSandbox/-/releases)，在过滤框输入 `kernel-release`，打开最新的一个
2. 按你的发行版下载**内核主包**：
   - RPM 系（OpenCloudOS、RHEL、CentOS、TencentOS、Fedora）：`kernel-*opencloudos9.cubesandbox.pvm.host*.x86_64.rpm`
   - DEB 系（Ubuntu、Debian）：`linux-image-*opencloudos9.cubesandbox.pvm.host*_amd64.deb`

<small>资产列表中的 `kernel-headers-*`、`-dbg` 等为可选包，无需下载。</small>

已安装 GitHub CLI 的用户也可免找文件名、按通配直接下载，用法见[附录](#附录-url-规律-最新-tag-与版本-pin)。

> 📌 **OpenCloudOS 9 用户专属快捷路径**
> 内核包已上架 OpenCloudOS 官方 yum 仓库，无需手动下载 rpm，`dnf install` 一行命令可实现直装，整体部署只需 5 条命令。
> 👉 [在 OpenCloudOS 9 上一键部署 CubeSandbox 实测](https://mp.weixin.qq.com/s/oGAaUpze_uB_uzyvuYJSIg)

## 自建部署：下载 guest 内核与镜像

只有在[自行构建发布包](./self-build-deploy.md)或替换组件时才需要下载这些（一键部署包已全部自带）：

```bash
# guest 内核（发布在 kernel-release-* 下，文件名固定）
wget "https://cnb.cool/CubeSandbox/CubeSandbox/-/releases/download/kernel-release-260812-1/vmlinux-amd64"       # 裸金属/KVM，aarch64 用 vmlinux-arm64
wget "https://cnb.cool/CubeSandbox/CubeSandbox/-/releases/download/kernel-release-260812-1/vmlinux-pvm-amd64"    # PVM guest 内核（仅 x86_64）

# guest 镜像（发布在 guest-image-* 下，内含 cube-guest-image-cpu.img）
wget https://cnb.cool/CubeSandbox/CubeSandbox/-/releases/download/guest-image-260820-1/cube-guest-image-amd64.tar.gz
tar -xzf cube-guest-image-amd64.tar.gz
```

<small>URL 中的 tag 会随发布递增（文件名固定，换 tag 即可）；最新 tag 可在 [Releases 页面](https://cnb.cool/CubeSandbox/CubeSandbox/-/releases) 过滤 `kernel-release` / `guest-image` 查看。</small>

自建时把 `vmlinux` 放入 `deploy/one-click/assets/kernel-artifacts/` 即可，详见[本地构建部署 — 1.1 准备内核文件](./self-build-deploy.md#_1-1-准备内核文件)。

## 附录：URL 规律、最新 tag 与版本 pin

**URL 规律** —— 所有资产两个渠道完全一致，把域名换掉即可切换：

| 渠道 | URL 规律 |
| --- | --- |
| CNB 镜像（国内推荐） | `https://cnb.cool/CubeSandbox/CubeSandbox/-/releases/download/<tag>/<资产名>` |
| GitHub（权威源，境外推荐） | `https://github.com/TencentCloud/CubeSandbox/releases/download/<tag>/<资产名>` |

**用 gh 免找文件名** —— 按通配直接下载，或列出所有 kernel-release tag：

```bash
# 列出最新的 kernel-release tag
gh release list --repo TencentCloud/CubeSandbox --limit 30 | grep kernel-release

# 按通配下载（<tag> 替换为上面查到的最新 tag，无需知道精确文件名）
gh release download <tag> --repo TencentCloud/CubeSandbox \
  --pattern 'kernel-6*.x86_64.rpm'   # 或：--pattern 'linux-image-6*_amd64.deb'
```

**版本 pin** —— 每个 `v*` 产品版本在 [`deploy/release-assets.yaml`](https://github.com/TencentCloud/CubeSandbox/blob/master/deploy/release-assets.yaml) 中记录打包所用的 kernel / guest-image tag（也写在包内 `release-manifest.json`），据此可查"我手里的包用的是哪个内核"。专属 tag 的发布节奏独立于产品版本，pin 可能落后于最新 tag —— **PVM 宿主机内核建议始终用最新 `kernel-release-*`**（含内核安全修复），无需与产品版本对齐。

（v0.6.0 及之前的产品 Release 上还直接附有内核/镜像资产，属历史布局，之后不再如此。）
