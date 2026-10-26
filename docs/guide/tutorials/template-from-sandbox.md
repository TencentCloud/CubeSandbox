# Commit a Running Sandbox as a Template

This guide explains how to turn the current filesystem and memory state of a running sandbox into a new template. This is useful when writing a Dockerfile is impractical or when you already have a sandbox with the desired environment and want to reuse it later.

Before you begin, consider reading [Templates Overview](../templates.md) to understand the related concepts, including OCI images, template snapshots, ports, probes, and `envd`.

## Work in the Sandbox

After creating a sandbox, use `cubecli` to enter it, much like using a Docker command. You can then install dependencies, run commands, and make other changes inside the sandbox:

```bash
cubecli exec -it <sandbox-id> bash
```

## Commit the Sandbox

```bash
cubemastercli tpl commit --sandbox-id <sandbox-id>
```

After a successful commit, CubeMaster generates a new `tpl-...` template ID and creates its initial replica on the source sandbox's node.

## Specify the Configuration Used to Create Sandboxes from the Template (Advanced)

When you run `tpl commit`, CubeMaster saves not only the sandbox's current filesystem and memory state, but also a `CreateCubeSandboxReq` for the template. When the template is later used to create a sandbox, this request restores settings such as the containers, startup command, ports, and network configuration.

By default, no additional file is needed. CubeMaster automatically restores the source sandbox's original create request:

```bash
cubemastercli tpl commit --sandbox-id <sandbox-id>
```

Use `--file <path>` only when the new template should use a different create configuration. The file must contain a complete `CreateCubeSandboxReq` in JSON format:

```bash
cubemastercli tpl commit \
  --sandbox-id <sandbox-id> \
  --file template-request.json
```

Here, `--file` does **not** copy a file into the template, nor does it modify only the fields present in the JSON document. The request in the file **completely replaces** the original create request restored by CubeMaster; the two requests are not merged. For example, if the file omits the original containers, startup command, environment variables, or exposed ports, those settings are not preserved automatically and the resulting template may not start as expected.

The following `template-request.json` is a simplified example that only illustrates the field structure:

```json
{
  "instance_type": "cubebox",
  "network_type": "tap",
  "cube_network_config": {
    "allowInternetAccess": true,
    "allowOut": ["10.0.0.0/8"],
    "denyOut": []
  }
}
```

> This simplified JSON may not be sufficient to replace your source sandbox's create request. Before using `--file`, make sure the file contains every setting required to run the new template. If you only want to preserve the current sandbox's files and runtime state, do not pass `--file`.

When using `--file`, the following command-line options can also modify `cube_network_config` in the file request:

| Option | Effect |
| --- | --- |
| `--allow-internet-access=false` | Explicitly set the request's internet access value. |
| `--allow-out-cidr <cidr>` | Append an allowed egress CIDR; repeat for multiple CIDRs. |
| `--deny-out-cidr <cidr>` | Append a denied egress CIDR; repeat for multiple CIDRs. |

These network options modify only the request supplied through `--file`, so they must be used together with `--file`. For example:

```bash
cubemastercli tpl commit \
  --sandbox-id <sandbox-id> \
  --file template-request.json \
  --allow-internet-access=false
```
