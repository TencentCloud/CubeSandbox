# Kubernetes 安装后功能验证

沿用 SDK E2E runner 执行九项检查：部署预检、已有创建/删除/命令/文件用例，
以及 Service ClusterIP/FQDN、公网 DNS/HTTPS、CubeProxy 自定义端口访问。

## 环境与运行

- 已安装 Helm release，包含 chart 管理的 CubeOps 和 Ready 计算 Pod；Linux
  节点提供 KVM。测试端安装 kubectl、Helm 和 SDK 测试依赖。
- Ready 模板包含 Python 3、可信 TLS 根证书，配置集群 DNS，并在 `exposedPorts`
  声明 8088（或下方自定义端口）。CubeAPI/CubeProxy 可达，认证配置沿用主 README。
- 需要读取 Helm 元数据、节点、工作负载、PVC、Service，通过 `services/proxy`
  读取 CubeOps；创建/读取/删除测试 namespace，创建/读取其中的 Pod/Service。
  可选 `pods/exec` 权限用于只读检查 KVM、运行时和 socket，无需 SSH。

在 `tests/e2e/sdk_compat` 目录执行：

```bash
export SDK_E2E_K8S_CONTEXT=my-cluster
export SDK_E2E_K8S_NAMESPACE=cube
export SDK_E2E_K8S_RELEASE=cube
export CUBE_API_URL=http://127.0.0.1:3000
export CUBE_TEMPLATE_ID=tpl-your-ready-template
# 按主 README 设置 CUBE_API_KEY 和可达的 CubeProxy 地址。
export SDK_E2E_REPORT_DIR=reports/kubernetes
pytest --run-e2e --k8s-post-install
```

两个参数均需提供。先检查 Helm、工作负载、PVC、镜像拉取、注册节点落点与
已报告容量，再执行现有 SDK/模板预检。报告包含版本、内核、镜像、DNS IP 和
可发现的 CNI DaemonSet。可选 exec 不可用时记录 warning；实际发现 KVM、运行时
或 socket 缺失则失败。真实创建沙箱才证明可调度容量足够。

## 网络路径与清理

每个 Service 用例创建唯一 namespace、非特权 HTTP Pod 和 Service。沙箱固定
到健康注册节点，HTTP Pod 优先调度到另一节点；报告记录 `same-node` 或
`cross-node`，单节点结果不能代表跨节点验证。

创建沙箱时关闭一般公网访问，显式放行 CoreDNS 和 Service IP。FQDN 请求使用
模板配置的集群 DNS，不修改 `/etc/resolv.conf`。HTTP 响应必须匹配本次运行标记。
CubeProxy 用例在模板声明端口启动沙箱 HTTP 服务，并验证其标记。Service 测试
依赖原生 CubeSandbox `distribution_scope`，其他 SDK 后端跳过；通用 allow/deny
用例仍在原有套件中。

| 可选变量 | 默认值 |
| --- | --- |
| `SDK_E2E_K8S_CLUSTER_DOMAIN` | `cluster.local` |
| `SDK_E2E_K8S_MOCK_IMAGE` | `busybox:1.37`，需提供 sh/httpd |
| `SDK_E2E_K8S_CUSTOM_PORT` | `8088`，模板必须声明 |
| `SDK_E2E_K8S_PUBLIC_URL` | `https://example.com/`，应返回 HTTP 200 |

沿用 trace、JSONL、重试及失败保留沙箱的调试选项。删除 namespace 前检查 UID，
沙箱清理后通过 API 确认资源消失；清理失败会使测试失败。强制终止可能遗留资源，
可根据 `kubernetes_mock_created` 事件定位。建议先串行执行；报告中列明跳过项和
保留沙箱，跳过公网用例不能代表公网验证通过。本套件不构成 CNI 全面认证。
