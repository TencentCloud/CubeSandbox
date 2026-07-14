# Template Inspection and Request Preview

Use this page when you have a `template_id` and want to answer one of these questions:

- Is the template ready, and which nodes already have replicas?
- What request was stored with the template itself?
- If I create a sandbox from this template now, what request will actually take effect?

The commands on this page are implemented by `cubemastercli tpl info` and `cubemastercli tpl render`.

## Choose the Right Command

| If you want to know... | Use... |
|------------------------|--------|
| Template status, replicas, version, last error | `tpl info` |
| The stored request body that belongs to the template | `tpl info --include-request` |
| The effective sandbox request after template merge | `tpl render` |
| The request that CubeMaster eventually converts for Cubelet | `tpl render` |

In practice:

- Start with `tpl info` when you want to inspect the template itself.
- Switch to `tpl render` when you want to preview what sandbox creation will use now.

## Inspect the Template Itself

Basic metadata:

```bash
cubemastercli tpl info <template-id>
```

The template ID can be passed as a positional argument (docker/kubectl style) or with `--template-id`; both forms are equivalent.

Raw JSON output:

```bash
cubemastercli tpl info <template-id> --json
```

Include the stored request body:

```bash
cubemastercli tpl info <template-id> --json --include-request
```

Typical response shape:

```json
{
  "template_id": "tpl-xxx",
  "instance_type": "cubebox",
  "status": "READY",
  "replicas": [
    {
      "node_id": "node-a",
      "status": "READY"
    }
  ],
  "create_request": {
    "containers": [],
    "volumes": [],
    "annotations": {
      "cube.master.appsnapshot.template.id": "tpl-xxx"
    }
  }
}
```

How to read `create_request`:

- It is the request body stored with the template definition.
- It is useful when you want to inspect what the template was originally built from or committed with.
- It is not the final request for every future sandbox creation. Later user input can still be merged on top.

## Preview the Effective Sandbox Request

Preview using only the template:

```bash
cubemastercli tpl render --template-id <template-id> --json
```

Preview with your own request overrides from a file:

```bash
cubemastercli tpl render -f req.json --template-id <template-id> --json
```

`tpl render` accepts input from a request file. If you also pass `--template-id`, the flag overrides the template annotation in that file.

Typical response shape:

```json
{
  "api_request": {
    "annotations": {
      "cube.master.appsnapshot.template.id": "tpl-xxx",
      "cube.master.appsnapshot.template.version": "v2"
    }
  },
  "merged_request": {
    "containers": [
      {
        "name": "main"
      }
    ],
    "volumes": [
      {
        "name": "workspace"
      }
    ],
    "network_type": "tap"
  },
  "cubelet_request": {
    "containers": [
      {
        "name": "main"
      }
    ],
    "volumes": [
      {
        "name": "workspace"
      }
    ]
  }
}
```

## Which Layer Should You Read?

`tpl render` returns three views of the request:

- `api_request`: the request received by CubeMaster before template resolution and merge.
- `merged_request`: the request after template resolution and request merge. This is usually the most useful layer for users.
- `cubelet_request`: the request generated from the merged result for Cubelet.

If you only have time to inspect one layer, start with `merged_request`.

## Recommended Troubleshooting Flow

When you only have a `template_id` and want to understand what sandbox creation will use:

1. Check template status and replicas.

```bash
cubemastercli tpl info <template-id>
```

2. If you need to know what the template itself stores, inspect `create_request`.

```bash
cubemastercli tpl info <template-id> --json --include-request
```

3. Preview the effective request that would be used right now.

```bash
cubemastercli tpl render --template-id <template-id> --json
```

4. If your application adds labels, annotations, network settings, or other overrides, place them in `req.json` and preview again.

```bash
cubemastercli tpl render -f req.json --template-id <template-id> --json
```

## What This Preview Does Not Tell You

This preview is useful, but it has clear boundaries:

- It does not tell you which node will finally be selected. Scheduling is still a runtime decision.
- It does not guarantee that a specific node will accept the request at runtime.
- It shows request content, not the final runtime outcome after the sandbox actually starts.
