# Node.js SDK

`@cubesandbox/sdk` is the official Node.js and TypeScript SDK for CubeSandbox. Its Promise-based, camelCase API follows the Python SDK's feature set while using idiomatic `async`/`await`.

## Requirements and installation

- Node.js 18 or later

```bash
npm install @cubesandbox/sdk
```

The package includes ESM and CommonJS builds and TypeScript declarations.

## Configuration

```bash
export CUBE_API_URL=http://<your-cubeapi-host>:3000
export CUBE_TEMPLATE_ID=<your-template-id>

# Required for remote access when *.cube.app cannot be resolved
export CUBE_PROXY_NODE_IP=<your-cubeproxy-node-ip>
```

## Quick start

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

With TypeScript 5.2 and Node.js 20 or later, explicit resource management can clean up automatically:

```ts
await using sandbox = await Sandbox.create();
const result = await sandbox.runCode("1 + 1");
console.log(result.text);
```

## Main capabilities

- Execute code with persistent variables, streaming output, and idle timeouts.
- Run shell commands and interactive PTY sessions.
- Perform complete filesystem operations and watch directories.
- Pause, reconnect, snapshot, roll back, and clone sandboxes.
- Manage persistent volumes and host-directory mounts.
- Configure node placement and L3/L4 or L7 network policy.
- Use typed APIs from either ESM or CommonJS applications.

For the full API reference, timeout semantics, and development instructions, see the [Node.js SDK README](https://github.com/TencentCloud/CubeSandbox/blob/master/sdk/node/README.md).
