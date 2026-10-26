# Cube Sandbox API 参考

## 已实现接口

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/health` | 健康检查（无需认证） |
| GET | `/sandboxes` | 列出所有 sandbox（v1） |
| GET | `/v2/sandboxes` | 列出 sandbox（v2，支持 state/metadata 过滤、limit） |
| POST | `/sandboxes` | 创建 sandbox |
| GET | `/sandboxes/:id` | 查询单个 sandbox 详情 |
| DELETE | `/sandboxes/:id` | 销毁 sandbox |
| POST | `/sandboxes/:id/pause` | 暂停 sandbox（保留内存快照） |
| POST | `/sandboxes/:id/connect` | 连接/恢复 sandbox |

## Sandbox.create() 参数

| 参数 | 类型 | 说明 |
|------|------|------|
| `template` | str | 沙箱模板 ID（必填） |
| `allow_internet_access` | bool | 是否允许出网，默认 True |
| `network` | dict | 网络策略，`allow_out` 或 `deny_out`（CIDR 列表） |
| `metadata` | dict | 扩展元数据，如 `host-mount`（JSON 字符串） |
| `timeout` | int | 沙箱最大存活秒数 |

## 执行结果字段

```python
result = sb.run_code("...")
result.stdout   # 标准输出列表
result.stderr   # 标准错误列表
result.error    # 执行异常（None 表示成功）
result.results  # 富文本输出（图表、HTML 等）
```

## 环境变量

| 变量 | 必填 | 说明 |
|------|------|------|
| `CUBE_TEMPLATE_ID` | ✅ | 沙箱模板 ID |
| `E2B_API_URL` | ✅ | Cube API 地址，如 `http://localhost:3000` |
| `E2B_API_KEY` | ✅ | 任意非空字符串（SDK 校验用） |
| `SSL_CERT_FILE` | HTTPS 时必填 | CA 根证书路径 |
