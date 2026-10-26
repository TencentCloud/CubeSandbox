---
title: "Cube Sandbox: A Deep Dive into Secure Sandbox Networking"
date: 2026-06-23
author: Cube Sandbox Team
description: "AI Agents give machines autonomous execution power — and open a Pandora's box of data exfiltration and credential abuse. CubeSandbox builds an end-to-end network security system — from virtual switching to application-layer auditing — on a foundation of KVM MicroVM isolation, an eBPF in-kernel network datapath, and L7 proxy deep inspection. This article dissects the design and implementation of core components like CubeVS, CubeProxy, and CubeEgress, and shows how Cube balances open execution with security and control."
featured: false
---

# Cube Sandbox: A Deep Dive into Secure Sandbox Networking

AI Agents give machines autonomous execution power — and open a Pandora's box of data exfiltration and credential abuse. CubeSandbox builds an end-to-end network security system — from virtual switching to application-layer auditing — on a foundation of KVM MicroVM isolation, an eBPF in-kernel network datapath, and L7 proxy deep inspection. This article dissects the design and implementation of core components like CubeVS, CubeProxy, and CubeEgress, and shows how Cube balances open execution with security and control.

## 1. Business Context and Requirements

### 1.1 The Security Challenges of AI Agents

AI Agents are evolving from "conversational assistants" into "autonomous executors" — they can write code, call APIs, operate browsers, and run system commands. This autonomy brings dramatic efficiency gains, but also introduces unprecedented security challenges:

| Risk Dimension | Typical Scenario | Potential Impact |
|---|---|---|
| **Code Execution Risk** | LLM-generated code contains malicious operations (`rm -rf /`, reverse shell) | Host compromised, affects co-tenants |
| **Data Exfiltration** | Agent sends user privacy / corporate secrets to unauthorized external APIs | Compliance violations, business loss |
| **Credential Abuse** | Agent steals or misuses API Key/Token for unauthorized operations | Resource theft, service disruption |
| **Supply Chain Attacks** | Agent `pip install`s a malicious package, executes mining/backdoor code | Lateral movement, persistent intrusion |
| **Escape Attacks** | Exploits kernel vulnerability to escape from container to host | Entire cluster compromised |

Traditional container solutions (Docker/Containerd) **share the host kernel** — once a kernel vulnerability exists, an attacker can escape directly from the container to the host. For AI Agent scenarios that execute **untrusted code**, the shared-kernel isolation model is no longer safe.

### 1.2 Network Security Requirements

The network layer of an AI Agent sandbox must solve the following core problems:

1. **Isolation**: Networks between sandboxes, and between sandboxes and the host, must be fully isolated.
2. **Controllability**: Precisely control which external services (IP/domain/protocol) each sandbox can access.
3. **Observability**: All outbound traffic must be auditable and traceable.
4. **Transparency**: Zero intrusion into Agent code — no application logic changes required.
5. **High Performance**: Network policies must not become a bottleneck for thousand-level concurrency.

## 2. CubeSandbox Architecture Overview

### 2.1 System Architecture

CubeSandbox adopts a **clear layered architecture**, from top to bottom: API gateway layer, orchestration & scheduling layer, compute node layer, and virtualization layer:

![CubeSandbox system architecture](./assets/2026-06-23-cubesandbox-network-deep-dive/01-system-architecture.jpg)

### 2.2 Network Architecture

The network data plane includes both sandbox-ingress and sandbox-egress traffic. L4 forwarding and policy enforcement are handled by high-performance in-kernel eBPF, while L7 routing and deep inspection are handled by user-space nginx/openresty:

- **Sandbox Ingress Traffic**
  1. CubeProxy: An nginx/openresty-based reverse proxy that performs high-performance routing via HTTP headers.
  2. CubeVS (Ingress): Pure in-kernel eBPF high-performance forwarding, implementing stateless portmapping conversion.

- **Sandbox Egress Traffic**
  1. CubeVS (Egress): Pure in-kernel eBPF high-performance forwarding, implementing IP filtering, domain filtering, SNAT, session tracking, and other sandbox outbound capabilities.
  2. CubeEgress: An nginx/openresty-based L7 proxy implementing HTTPS interception, credential injection, access auditing, and other security capabilities, extensible via Lua.

![CubeSandbox network architecture](./assets/2026-06-23-cubesandbox-network-deep-dive/02-network-architecture.jpg)

### 2.3 Host Network Management

network-agent implements TAP pool management and allocation, PortMapping port allocation, forwarding policy data distribution, and state persistence.

- **TAP device pooling**: Pre-creates 500+ TAP devices so that sandbox startup requires no TAP initialization, reducing sandbox creation time by 59+ms.
- **CubeVS and CubeEgress forwarding policy data distribution**, processing network traffic according to user-defined policies.

![CubeSandbox host network management](./assets/2026-06-23-cubesandbox-network-deep-dive/03-host-network-management.jpg)

## 3. Sandbox Network Virtualization: CubeVS

### 3.1 Design Goals

Traditional container networking solutions (Linux Bridge, OVS, iptables NAT) have per-packet processing overhead that grows with the number of tenants. CubeVS replaces the entire traditional network stack with three small eBPF programs, implementing ARP proxy, policy enforcement, L7 proxy selection, SNAT, session creation, reverse NAT, port mapping proxy, DNAT to sandbox IP, and transparent proxy source IP preservation.

- **Point-to-point low latency**: Each sandbox has a dedicated TAP device — no shared bridges, no software switch hops, no shared L2 domain, no ARP flooding or STP needed.
- **In-kernel policy enforcement**: Network policies execute in eBPF with minimal CPU overhead and high performance. Each TAP has its own policy trie, so updates don't affect each other.
- **Scalable NAT**: SNAT port allocation uses a lock-protected watermark pool + conflict-detection insertion, avoiding iptables rule explosion.
- **Security isolation**: TAPs are naturally unreachable from each other — communication is only possible through paths controlled by eBPF programs.

![CubeVS design](./assets/2026-06-23-cubesandbox-network-deep-dive/04-cubevs-design.jpg)

### 3.2 Network Access Control: CIDR Policy IP Filtering via LPM Trie

CubeVS implements per-sandbox outbound IP policies in the eBPF kernel layer, using the **LPM (Longest Prefix Match) Trie** data structure for CIDR matching. The **per-device design** means updating one sandbox's policy doesn't require traversing or locking other sandboxes' maps.

**Evaluation order:**

Priority: **allow > deny > default-allow**. You can set a full deny (`0.0.0.0/0`) and then selectively open up with allow rules for fine-grained control.

**Default policy**

CubeVS allows sandboxes to access the internet by default, but blocks access to the following ranges to ensure sandboxes cannot reach the host's internal network. These can be opened via `allow_out`, or you can set `allow_internet_access=False` to block internet access entirely — equivalent to adding a full deny (`0.0.0.0/0`) to `deny_out`.

| CIDR | Description |
|---|---|
| `10.0.0.0/8` | Private network Class A |
| `127.0.0.0/8` | Loopback address |
| `169.254.0.0/16` | Link-local (sandbox internal gateway segment) |
| `172.16.0.0/12` | Private network Class B |
| `192.168.0.0/16` | Private network Class C |

### 3.3 Network Access Control: DNS-Based Domain Policy Filtering via LPM Trie

IP-layer policies can only match on destination IP via CIDR. However, AI Agent sandbox network access typically targets domains (e.g., `api.openai.com`), and a single domain may resolve to multiple different IPs. CubeVS implements domain-level fine-grained control through its **DNS policy engine**: CubeVS intercepts DNS queries (UDP port 53) at the eBPF layer, extracts the queried domain, and if it's allowlisted in `allow_out`, creates a domain tracking record. When CubeVS receives the DNS response matching that record, it adds the response's IP addresses to `allow_out` to permit subsequent traffic to that domain, and sets a TTL to ensure stale IPs are removed from `allow_out`.

- Domains can only be allowlisted in `allow_out`, not configured in `deny_out`.
- To allow only specific domain traffic, it's recommended to configure a full deny policy (`0.0.0.0/0`) in `deny_out`.
- When domains are configured in `allow_out`, Cube automatically adds the template's configured DNS server IP to `allow_out`.
- Limitation: This capability depends on the sandbox obtaining domain IPs via DNS queries over UDP port 53 — not via other methods like configuring hosts.

#### Coordination with L7 Policies

DNS domain filtering policies provide domain-level control at the eBPF kernel layer. CubeEgress provides more fine-grained control at L7, including precise control over http/https, SNI, host, path, and method.

When `rules` policies are configured in the sandbox network config, Cube automatically allowlists the host and SNI domains in `allow_out` and marks traffic to those domains for delivery to CubeEgress for L7 processing — such as credential hosting, access auditing, or L7-level denial.

- **eBPF DNS policy**: Quickly rejects known malicious domains, reducing traffic reaching the L7 proxy.
- **CubeEgress L7 policy**: Fine-grained domain wildcard (`*.example.com`), path prefix (`/v1/*`), HTTP method, and other conditional matching.

## 4. Sandbox Ingress Gateway: CubeProxy

CubeProxy is the sandbox's **ingress traffic gateway**, responsible for routing external requests to the correct sandbox instance. Built on OpenResty (Nginx + Lua), it uses Host-header routing, supports wildcard DNS (`*.sandbox.cube.app`), supports TLS SNI, and is easy to configure DNS for.

**Format**: `{port}-{sandbox_id}.{domain}`

CubeProxy queries Redis for the sandbox's portmapping information on the host — i.e., it looks up `(HostIP, mapping-port)` by `(container_port, sandbox_id)` and forwards the request to the corresponding host machine.

## 5. Application-Layer Security Enhancement: CubeEgress

### 5.1 Design Positioning

CubeEgress is the sandbox's **egress security gateway**, performing deep inspection of HTTP/HTTPS traffic at L7. It is the **user-space enhancement layer** over CubeVS's in-kernel policies, solving needs that the kernel layer cannot handle: domain matching, credential injection, content auditing, etc.

Core capability matrix:

| Capability | Description |
|---|---|
| HTTPS transparent interception | MITM dynamic certificate + TLS termination |
| Domain/path/method matching | L7 policy engine, first-match-wins |
| Credential injection | Proxy-layer auto-injection, Agent never touches credentials |
| Data desensitization | Sensitive field replacement in audit logs |
| Access auditing | JSONL structured logs |
| Request filtering | Allow or deny requests |

Limitation: This capability depends on the sandbox obtaining domain IPs via DNS queries over UDP port 53 — not via other methods like configuring hosts.

### 5.2 On-Demand Traffic Steering: eBPF-to-L7-Proxy Traffic Dispatch

Not all traffic requires L7 inspection. Only http/https traffic to domains with rules configured in the `rules` policy is **on-demand steered** by CubeVS to the CubeDev device, entering the kernel stack. Combined with iptables TProxy mechanism and the user-space socket `IP_TRANSPARENT` option (currently unsupported by stock nginx/openresty — Cube provides a patch and a pre-patched openresty container image), packets are transparently forwarded to OpenResty's user-space socket for reception, preserving the sandbox's original destination IP.

![CubeEgress on-demand traffic steering](./assets/2026-06-23-cubesandbox-network-deep-dive/05-cubeegress-traffic.jpg)

### 5.3 HTTPS Transparent Interception: Dynamic Certificate Generation

CubeEgress implements deep inspection of HTTPS traffic through a MITM (Man-in-the-Middle) proxy. The core mechanism is **dynamic certificate generation**:

#### Certificate Architecture

- **ECDSA P-256**: Leaf certificates use elliptic curves — faster and smaller than RSA.
- **7-day TTL**: Short validity period reduces leak impact.
- **Cache lock**: lua-resty-lock prevents concurrent generation for the same SNI.
- **CA trust injection**: The Root CA certificate is injected into the sandbox by default during template creation — transparent to the Agent.

### 5.4 L7 Policy Engine

A sandbox's CubeEgress policy can contain multiple rules, each with a match and an action. Rules are matched in order — the first matching rule takes effect and subsequent rules are skipped. Requests that don't match any rule are denied by default.

- match semantics:

| Field | Format | Semantics |
|---|---|---|
| sni | `*.example.com` | Exact match or suffix `*.` wildcard (subdomains only) |
| host | `api.example.com` | Exact match or suffix `*.` wildcard (ignores `:port`) |
| method | `{"GET", "POST"}` | Array — any match passes |
| path | `/v1/*` | Exact match or trailing `*` prefix match |
| scheme | `http` / `https` | Exact match |

- action execution semantics: allow / deny / audit / inject

### 5.5 Credential Hosting and Injection

AI Agent sandboxes **should not directly hold API Keys/Tokens**. CubeEgress's credential injection mechanism lets the sandbox send requests without credentials — the CubeEgress proxy layer outside the sandbox automatically injects them:

Credential injection is protected by multiple checks — if any fails, **injection is abandoned** (the request may still proceed, but without credentials):

| Check |
|---|
| Request must be HTTPS |
| SNI matches the rule |
| Host header must match SNI |
| Upstream certificate verification passes |
| Policy authorizes the request |

### 5.6 Access Auditing

Every network request (whether allowed or denied) generates a structured audit record in JSONL (JSON Lines) format. Sensitive fields in audit logs **never record raw values** — they are protected through multi-layer desensitization:

| Header Type | Desensitization Strategy | Example |
|---|---|---|
| Authorization / Proxy-Authorization | Keep auth scheme, redact value | `Bearer <redacted:Bearer>` |
| Cookie / Set-Cookie | Keep cookie name, redact value | `<redacted; names=session_id,theme>` |
| X-Api-Key | Full redaction | `<redacted>` |
| X-Auth-Token / Token | Full redaction | `<redacted>` |
| Headers containing token/secret/key/password/auth | Full redaction | `<redacted>` |

### 5.7 CubeEgress Extensibility

CubeEgress is an http/https transparent proxy built on OpenResty. It currently implements basic request filtering, credential hosting, and access auditing. If you need deeper detection or other capabilities, you can easily extend it by modifying or adding Lua scripts. See the community repo docs for details: <https://github.com/TencentCloud/CubeSandbox/blob/master/docs/zh/guide/security-proxy.md>

## 6. Summary

CubeSandbox's network system achieves high-performance forwarding through the **"eBPF in-kernel + OpenResty user-space"** combination. Through localized resource allocation, normalized sandbox configuration, and resource pooling, it achieves extreme sandbox creation speed. In the emerging AI Agent security sandbox scenario, it delivers multiple technical innovations:

| Innovation | Traditional Approach | CubeSandbox Approach | Advantage |
|---|---|---|---|
| **Network isolation** | Linux Bridge + iptables | Point-to-point TAP + eBPF | Zero broadcast overhead, in-kernel enforcement |
| **Policy enforcement** | iptables rule chains | LPM Trie + eBPF | O(1) lookup, per-sandbox isolation |
| **Domain filtering** | Application-layer proxy | In-kernel eBPF | High-performance forwarding |
| **HTTPS inspection** | Manual certificate config | Dynamic cert generation (ECDSA P-256) | Zero pre-config, automatic management |
| **Credential management** | Env vars / config files | Proxy-layer injection + multi-check | Agent sandbox never touches credentials |
| **Audit logs** | Text format, needs parsing | JSONL structured + auto-redaction | Compliance-friendly, directly analyzable |

Check out the CubeSandbox project: <https://github.com/TencentCloud/CubeSandbox>
