# Node.js SDK

`@cubesandbox/sdk` 是 CubeSandbox 官方 Node.js 和 TypeScript SDK。它使用 Promise、camelCase 和 `async`/`await`，功能范围与 Python SDK 保持一致。

## 环境要求与安装

- Node.js 18 或更高版本

```bash
npm install @cubesandbox/sdk
```

该包同时提供 ESM、CommonJS 构建和 TypeScript 类型声明。

## 配置

```bash
export CUBE_API_URL=http://<your-cubeapi-host>:3000
export CUBE_TEMPLATE_ID=<your-template-id>

# 无法解析 *.cube.app 时，远程访问需要配置
export CUBE_PROXY_NODE_IP=<your-cubeproxy-node-ip>
```

## 快速开始

```ts
import { Sandbox } from "@cubesandbox/sdk";

const sandbox = await Sandbox.create();

try {
  const code = await sandbox.runCode('print("hello from CubeSandbox")');
  console.log(code.logs.stdout);

  const command = await sandbox.commands.run("uname -a");
  console.log(command.stdout);

  await sandbox.files.write("/tmp/hello.txt", "Hello, world!");
  console.log(await sandbox.files.read("/tmp/hello.txt"));
} finally {
  await sandbox.kill();
}
```

使用 TypeScript 5.2 和 Node.js 20 或更高版本时，可以通过显式资源管理自动清理沙箱：

```ts
await using sandbox = await Sandbox.create();
const result = await sandbox.runCode("1 + 1");
console.log(result.text);
```

## 主要能力

- 执行代码，支持变量持久化、流式输出和空闲超时。
- 执行 Shell 命令和交互式 PTY 会话。
- 完整的文件系统操作和目录监听。
- 暂停、重连、创建快照、回滚和克隆沙箱。
- 管理持久卷和宿主机目录挂载。
- 配置节点调度以及 L3/L4、L7 网络策略。
- 在 ESM 或 CommonJS 应用中使用完整类型 API。

完整 API 参考、超时语义和开发说明请查看 [Node.js SDK README](https://github.com/TencentCloud/CubeSandbox/blob/master/sdk/node/README.md)。
