---
title: WSL2 - Two DNS Problems That Block One-Click Install and the Sandbox Data Plane
author: Tantanovo
date: 2026-08-03
tags:
  - deployment
  - networking
  - dns
  - wsl
lang: en-US
---

# WSL2: Two DNS Problems That Block One-Click Install and the Sandbox Data Plane

WSL2 is a convenient way to try Cube Sandbox on a Windows machine, and
[quickstart](../quickstart.md) lists it as a supported platform. Two DNS
problems are specific to WSL and are not covered elsewhere in the docs. They
occur at different stages and look nothing alike, so they are documented
separately below.

Both were reproduced and fixed on a real single-node deployment; the versions
are in [Environment](#environment).

## Symptom

### Problem 1: preflight exits 3 before anything is downloaded

`online-install.sh` stops immediately with:

```text
[online-install] ERROR: DNS setup requires resolvectl or NetworkManager.
```

The exit code is `3`. Nothing has been downloaded or changed on the system yet.

### Problem 2: the sandbox is created, but `run_code` fails to resolve

Installation succeeds and the control plane is healthy — the sandbox is created
in about a second — but the first data-plane call fails:

```text
[1] sandbox created in 0.99s
    sandbox_id: 9e0b35c6183f4e969e1a6a8f6bdc5629     <- control plane OK
httpx.ConnectError: [Errno -2] Name or service not known   <- data plane fails
```

The error mentions neither DNS nor `cube.app`, which makes this one hard to
attribute. It can also appear *after* a previously working setup, because it is
triggered by restarting WSL rather than by anything you changed.

## Environment

- Cube Sandbox version: v0.6.0 (`cubemastercli` `8721dd15`, built 2026-07-24)
- Deployment mode: one-click, single node (control + compute on the same host)
- Host OS / kernel: Ubuntu 24.04.4 LTS on WSL2, `6.18.33.2-microsoft-standard-WSL2`, glibc 2.39
- Related components: `online-install.sh` preflight, CoreDNS (`cube-sandbox-coredns`), CubeProxy, `deploy/one-click/scripts/systemd/dns-host-route-up.sh`

Other WSL preconditions were already satisfied and are not the subject of this
page: `/dev/kvm` exists (recent WSL enables nested virtualization by default),
`/data/cubelet` is a loopback XFS volume with `reflink=1` (see
[#311](https://github.com/TencentCloud/CubeSandbox/issues/311)), `/sys/fs/bpf`
is mounted, and the cgroup v2 `cpu` controller is exposed.

## Root Cause

### Problem 1: WSL ships neither `resolvectl` nor NetworkManager

The preflight requires at least one supported resolver manager
(`deploy/one-click/online-install.sh`):

```bash
# DNS check (requires resolvectl or NetworkManager loaded status)
if ! command -v resolvectl >/dev/null 2>&1; then
  if command -v systemctl >/dev/null 2>&1; then
    nm_load_state="$(systemctl show -p LoadState --value NetworkManager 2>/dev/null || true)"
    if [[ "${nm_load_state}" != "loaded" ]]; then
      echo "[online-install] ERROR: DNS setup requires resolvectl or NetworkManager." >&2
      exit 3
    fi
  ...
```

The check itself is correct, and the requirement is documented in
[self-build-deploy](../self-build-deploy.md) ("DNS routing: `systemd-resolved`
(preferred) or `NetworkManager + dnsmasq`"). Two things make it easy to get
stuck on WSL anyway:

- A default Ubuntu-on-WSL install has **neither**. `systemd-resolved` is not
  installed, and NetworkManager is not present, so the first branch fails.
- The message names the *command* `resolvectl`, but the command is shipped by
  the **`systemd-resolved` package**. The package name does not appear in the
  error, and `apt install resolvectl` does not exist, so the next step is not
  obvious.

### Problem 2: WSL rewrites `/etc/resolv.conf` on every start

The E2B-compatible SDK reaches a sandbox through a per-sandbox hostname of the
form `<port>-<sandboxId>.cube.app`. Because the sandbox ID changes every time,
the client needs wildcard resolution for `*.cube.app`; the bundled CoreDNS
provides it, and `deploy/one-click/scripts/systemd/dns-host-route-up.sh` routes
`cube.app` queries to it. On both DNS backends the client-facing nameserver is
`169.254.254.53` (the dummy-link address): on the `systemd-resolved` path the
script attaches that address to the `cube-dns0` link via `resolvectl`, and on
the `dnsmasq` fallback path it writes the same address into `/etc/resolv.conf`.
`127.0.0.54` is only CoreDNS's internal loopback bind, which `dnsmasq` forwards
`cube.app` queries to; it never appears in `/etc/resolv.conf`. See
[HTTPS and domain](../https-and-domain.md) for the full addressing scheme.

WSL, by default, regenerates `/etc/resolv.conf` on every start. On the
`dnsmasq` fallback path that silently discards the nameserver the installer
wrote; on the `systemd-resolved` path the routing lives on the `cube-dns0`
dummy link and is dropped too, because WSL tears the link down. Either way the
client loses the wildcard resolution, so:

- The control plane keeps working, because it is reached over `127.0.0.1:3000`
  and needs no name resolution.
- The data plane breaks, because `*.cube.app` no longer resolves.

This is why the failure can appear on a deployment that worked yesterday: the
trigger is a WSL restart, not a configuration change.

## Resolution

### Problem 1: install `systemd-resolved` to provide `resolvectl`

```bash
sudo apt-get update
sudo apt-get install -y systemd-resolved
sudo systemctl enable --now systemd-resolved

command -v resolvectl   # should print a path
```

Re-run `online-install.sh` afterwards. If `systemd-resolved` is unavailable on
your distribution, the documented alternative is `NetworkManager + dnsmasq`;
the preflight accepts that path as long as `NetworkManager` reports
`LoadState=loaded`.

### Problem 2: stop WSL from rewriting `resolv.conf`, then write it

Both steps are required — doing only one of them does not survive a restart.

```bash
# 1. tell WSL to leave resolv.conf alone
#    if /etc/wsl.conf already has a [network] section, add the key to that
#    section instead of appending a second one
sudo tee -a /etc/wsl.conf >/dev/null <<'EOF'

[network]
generateResolvConf = false
EOF

# 2. resolv.conf is usually a symlink into /run, so remove it before writing
#    223.5.5.5 is only an example upstream; use any resolver reachable on
#    your network
sudo rm -f /etc/resolv.conf
sudo tee /etc/resolv.conf >/dev/null <<'EOF'
nameserver 169.254.254.53
nameserver 223.5.5.5
EOF
```

Three details matter here:

- `/etc/resolv.conf` is typically a **symlink** into `/run/...`. Writing
  through the symlink does not persist, so it has to be removed first.
- If `/etc/wsl.conf` already contains a `[network]` section, put
  `generateResolvConf = false` inside it rather than appending a second
  section.
- Keep an upstream resolver as the second entry. With only the CoreDNS address,
  `*.cube.app` resolves but general internet resolution breaks. The installer's
  own `write_host_resolv_conf` preserves the host's existing upstream
  automatically; here the file is written by hand, so choose an upstream that
  works on your network.

The nameserver to write is `169.254.254.53` regardless of which DNS backend
you are on: it is the dummy-link address that both the `systemd-resolved` and
the `dnsmasq` paths expose to clients. (`127.0.0.54` is only CoreDNS's internal
loopback bind, which `dnsmasq` forwards `cube.app` queries to; it is not a
client-facing resolver and must not be written to `/etc/resolv.conf` — a
loopback address there would be unreachable from inside Docker containers,
which is exactly the problem the dummy-link address was introduced to solve.)
You can confirm the live address from the running Corefile.

Verify both directions before moving on (`dig` comes from `dnsutils` /
`bind9-dnsutils`, which Ubuntu does not install by default — the `getent`
checks work without it):

```bash
# wildcard sandbox domain must resolve
getent hosts foo.cube.app
# or, to query CoreDNS directly: sudo apt-get install -y dnsutils
dig +short +tcp +timeout=3 foo.cube.app @169.254.254.53

# ordinary resolution must still work
getent hosts github.com
```

After a `wsl --shutdown`, re-check that the file survived:

```bash
cat /etc/resolv.conf
```

### If you would rather not touch host DNS

The project already offers ways to avoid wildcard DNS entirely, which are
useful on WSL:

- **Path-based access** — reach a sandbox through
  `http://<cube-proxy-host>:<http-port>/sandbox/<sandbox-id>/<container-port>/`
  with no DNS or certificate setup. Good for HTTP APIs; single-page apps are
  better served by the host-based route because CubeProxy does not rewrite HTML
  bodies.
- **`examples/e2b-dev-sidecar`** — a local proxy that intercepts SDK data-plane
  requests and rewrites the `Host` header before forwarding to CubeProxy. It
  needs neither wildcard DNS nor a trusted self-signed certificate.

See [HTTPS and domain](../https-and-domain.md) for both.

## Notes for WSL Restarts

Several pieces of state do not survive `wsl --shutdown`, and all produce
confusing errors on the next run:

- The loopback XFS mount at `/data/cubelet` is not remounted automatically. Add
  it back before starting the services (see
  [#311](https://github.com/TencentCloud/CubeSandbox/issues/311)).
- `/etc/resolv.conf` is regenerated unless `generateResolvConf = false` is set
  as described above.
- On the `systemd-resolved` path, the `cube-dns0` dummy link and its
  `~cube.app` routing are also gone until the service unit restarts; on the
  `dnsmasq` fallback path the same applies to the link and the
  `server=/cube.app/...` forwarder rule.

## References

- Related docs: [Quickstart](../quickstart.md),
  [Self-build deployment](../self-build-deploy.md),
  [HTTPS and domain](../https-and-domain.md)
- Related issue: [#311](https://github.com/TencentCloud/CubeSandbox/issues/311)
  (XFS loopback workaround for WSL),
  [#411](https://github.com/TencentCloud/CubeSandbox/issues/411)
  (running Cube Sandbox on WSL2)
- Verified while working on [#644](https://github.com/TencentCloud/CubeSandbox/issues/644);
  deployment evidence is attached to
  [#1238](https://github.com/TencentCloud/CubeSandbox/pull/1238)
