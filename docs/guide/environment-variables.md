# Environment Variables

This page explains how to set and use environment variables in a sandbox, and
the default environment variables available inside a sandbox.

CubeSandbox provides three ways to set environment variables:

- when creating a template;
- when creating a sandbox; and
- for one command, PTY, or code-execution call.

CubeSandbox also provides multiple ways to interact with a sandbox. Environment
variables set at different stages have different visibility in each interaction
path.

## When creating a template

Cube supports multiple ways to set environment variables while creating a
template: use `ENV` in a Dockerfile, pass them through the SDK, or pass them to
`cubemastercli`.

```dockerfile
ENV DEMO_ENV=template-only
```

```python
from cubesandbox import Template

job = Template.build(
    envs={"DEMO_ENV": "template-only"},
)
```

When envd starts a command or PTY, it does not inherit the OCI process
environment. Therefore, environment variables set while building a template
are not visible to SDK commands handled by envd.

```bash{7}
cubemastercli tpl create-from-image \
  --image cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49999 \
  --env DEMO_ENV=template-only
```

The variable is not visible or effective when executing through
`sandbox.commands.run` or `sandbox.pty.create`.

```python
from cubesandbox import Sandbox

with Sandbox.create("tpl-xxx") as sandbox:
    print(sandbox.commands.run("echo -n DEMO_ENV=$DEMO_ENV").stdout)
    # DEMO_ENV=
```

Code executed through `sandbox.run_code` can read the variable. `run_code` is
executed through the Code Interpreter / Jupyter kernel rather than envd, so it
can read image `ENV` values and template build environment variables.

```python
with Sandbox.create("tpl-xxx") as sandbox:
    print(sandbox.run_code("import os; print(os.environ.get('DEMO_ENV'))").logs.stdout[0].strip())
    # template-only
```

> `sandbox.commands.run()` is generally used for shell commands, installing
> software, starting services, and running arbitrary CLIs or compilers. It is
> handled by envd and therefore does not inherit environment variables set when
> creating the template.
>
> `sandbox.run_code()` is used for Python, JavaScript, R, and similar code. It
> inherits environment variables set when creating the template.

Template environment variables are unavailable to envd-handled commands such as
`commands.run` and PTY. They are visible to sandbox processes, including the
main process and long-running services; `run_code` is one such path. They are
also visible in an interactive terminal opened with `cubecli exec`.

```bash
$ cubecli exec -it <sandbox-id> bash
$ echo $DEMO_ENV
template-only
```

## When creating a sandbox

You can set environment variables while creating a sandbox. They belong only to
that sandbox; they are not written back to the template and are not injected
into the container main process, long-running services, or `cubecli exec`.

```python
with Sandbox.create(
    template="tpl-example",
    envs={"DEMO_ENV": "sandbox-only"},
) as sandbox:
    print(sandbox.commands.run('echo -n "DEMO_ENV=$DEMO_ENV"').stdout)
    # DEMO_ENV=sandbox-only
```

> These sandbox-creation environment variables can be read by `commands.run`,
> but `run_code` cannot read them by default because its lightweight code
> interpreter does not inject sandbox environment variables from envd into the
> Jupyter kernel. To make `run_code()` automatically inherit
> `Sandbox.create(envs=...)`, use the repository's
> [run-code-env-inheritance derived image](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/run-code-env-inheritance/README.md).

## For one command, PTY, or code execution

Call-level `envs` apply only to the current call and override a variable with
the same name set when creating the sandbox:

```python
with Sandbox.create(
    template="tpl-example",
    envs={"DEMO_ENV": "sandbox-only"},
) as sandbox:
    print(sandbox.commands.run('echo -n "DEMO_ENV=$DEMO_ENV"',
    envs={"DEMO_ENV": "one-shot"}).stdout)
    # DEMO_ENV=one-shot
```

These environment variables are visible only to the current call.

## Environment variable visibility summary

| Source | commands.run / SDK PTY | cubecli exec / OCI Exec | Main process / services |
|---|---|---|---|
| Image `ENV`, template build envs | Do not rely on it / usually unavailable | Available | Available |
| `Sandbox.create(envs=...)` | Available | Unavailable | Unavailable |
| command / PTY call `envs` | Available only for the current call; highest priority | Not applicable | No effect |
| `run_code(..., envs=...)` | Not applicable | Available only for the current execution | Not applicable |
