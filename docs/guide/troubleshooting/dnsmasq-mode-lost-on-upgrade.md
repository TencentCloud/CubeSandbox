---
title: Upgrade Resets CUBE_PROXY_DNSMASQ_MODE and Fails the DNS Preflight
author: zhengzongwei
date: 2026-10-27
tags:
  - one-click
  - upgrade
  - dns
  - dnsmasq
lang: en-US
---

# Upgrade Resets CUBE_PROXY_DNSMASQ_MODE and Fails the DNS Preflight

## Symptom

A host without `resolvectl` or NetworkManager (e.g. Debian 13 minimal,
ifupdown + dhclient) installs Cube Sandbox successfully with:

```bash
CUBE_PROXY_DNSMASQ_MODE=standalone ./install.sh
```

The next upgrade run then fails in the preflight with:

```text
[one-click] ERROR: DNS setup requires resolvectl or NetworkManager (or set CUBE_PROXY_DNSMASQ_MODE=standalone)
```

even though the exact same environment variable is passed again on the
upgrade command line.

## Environment

- Cube Sandbox version: v0.7.3 (one-click, install then upgrade)
- Host OS: Debian 13 (trixie) minimal — no `resolvectl`, no NetworkManager
- Deployment mode: single-node control (`role=control`)
- Related components: `deploy/one-click/install.sh`, `deploy/one-click/lib/common.sh`

## Root Cause

`CUBE_PROXY_DNSMASQ_MODE` (introduced by
[#740](https://github.com/TencentCloud/CubeSandbox/pull/740)) is validated
and consumed by `check_dns_preflight`, but the installer never writes it to
`.one-click.env` on its own: a value supplied in the bundle `.env` is copied
verbatim into `.one-click.env` by the install step, so that channel already
survives, while a value supplied **only through the process environment** is
lost. The key is also absent from `ONE_CLICK_TOGGLE_KEYS`, so the
snapshot/replay mechanism that protects other explicit overrides does not
cover it.

On the next upgrade run, `merge_env_three_way` builds the merged env from
`env.example` (where the default is `networkmanager`) plus the old runtime
env (where the key is missing), and `load_env_file` sources that merged
file — **clobbering the operator's process-environment value**. The
preflight then sees `networkmanager`, finds neither `resolvectl` nor a
loaded NetworkManager unit, and exits.

Trigger condition: fresh install relies on `CUBE_PROXY_DNSMASQ_MODE`
passed **only via the process environment**, and the host has no
`resolvectl` / NetworkManager.

## Resolution

### 1. Fix (this change)

Register the key in `ONE_CLICK_TOGGLE_KEYS` so the snapshot/replay
mechanism preserves explicit process-env and bundle-`.env` intent across
the upgrade merge, and persist the resolved value via `upsert_env_kv`
next to the other toggle keys. Future upgrades keep the mode stable.
The `.env` value is replayed shell-interpreted (after `load_env_file`), so
quoted or annotated entries such as
`CUBE_PROXY_DNSMASQ_MODE="standalone"` work. The `online-install.sh`
bootstrap preflight mirrors the same DNS check, so the documented
`curl ... | CUBE_PROXY_DNSMASQ_MODE=standalone bash` path also works.

### 2. Workaround for already-affected hosts

Append the mode to the runtime env file before re-running the upgrade:

```bash
echo "CUBE_PROXY_DNSMASQ_MODE=standalone" >> /usr/local/services/cubetoolbox/.one-click.env
./install.sh --mode auto   # with the usual env/flags
```

Hosts that do have `resolvectl` or NetworkManager are unaffected either
way.

> If the bundle `.env` still carries the old active
> `CUBE_PROXY_DNSMASQ_MODE=networkmanager` line, the installer ignores it
> (and logs why) only when the runtime already holds `standalone`, keeping the
> value written above; remove the stale line anyway.

> Caveat: `CUBE_PROXY_DNSMASQ_MODE` is an installer toggle, so a key
> present in the bundle `.env` normally counts as an explicit choice. The
> template ships the line commented out (the code default is
> `networkmanager`), so a fresh `cp env.example .env` no longer overrides
> the mode. On an upgrade where the runtime already holds a non-default mode
> (`standalone`), a `.env` value of `networkmanager` is treated as a stale
> full copy, ignored and logged; to switch that host back to
> `networkmanager`, use the process environment or edit `.one-click.env`.
> On a fresh install (no runtime value) `.env` stays authoritative.

## References

- Fixing PR: this change (one-click persistence)
- Introducing PR: [#740](https://github.com/TencentCloud/CubeSandbox/pull/740)
- Kernel-doc style reference: `deploy/one-click/install.sh` →
  `check_dns_preflight()` / `deploy/one-click/lib/common.sh` →
  `ONE_CLICK_TOGGLE_KEYS`
