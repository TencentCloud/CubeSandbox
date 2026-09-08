# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Kubernetes discovery and test-owned fixtures for the existing SDK E2E runner."""

from __future__ import annotations

import json
import os
import subprocess
import uuid
from contextlib import contextmanager
from dataclasses import dataclass
from typing import Any


@dataclass(frozen=True)
class KubernetesConfig:
    context: str
    namespace: str
    release: str
    cluster_domain: str = "cluster.local"
    mock_image: str = "busybox:1.37"
    ready_timeout: int = 120

    @classmethod
    def from_env(cls):
        required = {
            k: os.environ.get(f"SDK_E2E_K8S_{k.upper()}", "").strip()
            for k in ("context", "namespace", "release")
        }
        if not all(required.values()):
            raise RuntimeError(
                "set SDK_E2E_K8S_CONTEXT, SDK_E2E_K8S_NAMESPACE and "
                "SDK_E2E_K8S_RELEASE explicitly (kubeconfig is not changed)"
            )
        return cls(
            **required,
            cluster_domain=os.environ.get(
                "SDK_E2E_K8S_CLUSTER_DOMAIN", "cluster.local"
            ),
            mock_image=os.environ.get("SDK_E2E_K8S_MOCK_IMAGE", "busybox:1.37"),
        )


class Kubectl:
    def __init__(self, config: KubernetesConfig):
        self.config = config

    def run(self, *args: str, payload: dict | None = None, timeout: int = 30) -> str:
        command = [
            "kubectl",
            "--context",
            self.config.context,
            "--request-timeout=20s",
            *args,
        ]
        try:
            result = subprocess.run(
                command,
                input=json.dumps(payload) if payload else None,
                text=True,
                capture_output=True,
                timeout=timeout,
                check=False,
            )
        except (OSError, subprocess.TimeoutExpired) as exc:
            raise RuntimeError(f"kubectl {args[0]}: {exc}") from exc
        if result.returncode:
            raise RuntimeError(
                f"kubectl {' '.join(args[:3])}: {result.stderr.strip()[:1500]}"
            )
        return result.stdout

    def get(
        self,
        resource: str,
        *,
        namespace: str | None = None,
        selector: str | None = None,
    ) -> dict:
        args = ["get", resource, "-o", "json"]
        if namespace:
            args += ["-n", namespace]
        if selector:
            args += ["-l", selector]
        return json.loads(self.run(*args))

    def service_get(self, service: dict, path: str) -> Any:
        meta, spec = service["metadata"], service["spec"]
        port = spec["ports"][0].get("name") or str(spec["ports"][0]["port"])
        url = (
            f"/api/v1/namespaces/{meta['namespace']}/services/"
            f"http:{meta['name']}:{port}/proxy{path}"
        )
        return json.loads(self.run("get", "--raw", url))


def ready(resource: dict) -> bool:
    return any(
        c.get("type") == "Ready" and c.get("status") == "True"
        for c in resource.get("status", {}).get("conditions", [])
    )


def workload_errors(resources: list[dict]) -> list[str]:
    errors = []
    for obj in resources:
        kind, name = obj["kind"], obj["metadata"]["name"]
        status, spec = obj.get("status", {}), obj.get("spec", {})
        prefix = f"{kind}/{name}"
        if kind in {"Deployment", "StatefulSet", "DaemonSet"}:
            if status.get("observedGeneration", 0) < obj["metadata"].get(
                "generation", 1
            ):
                errors.append(
                    f"{prefix}: controller has not observed the latest generation"
                )
            if kind == "DaemonSet":
                wanted = status.get("desiredNumberScheduled", 0)
                available = status.get("numberReady", 0)
                updated = status.get("updatedNumberScheduled", 0)
            else:
                wanted = spec.get("replicas", 1)
                available = status.get("readyReplicas", 0)
                updated = status.get("updatedReplicas", 0)
            if available < wanted or updated < wanted:
                errors.append(
                    f"{prefix}: ready={available}, updated={updated}, desired={wanted}"
                )
            if (
                kind == "DaemonSet"
                and wanted == 0
                and obj["metadata"].get("labels", {}).get("app.kubernetes.io/component")
                == "cube-node"
            ):
                errors.append(f"{prefix}: no eligible compute nodes")
        elif kind == "PersistentVolumeClaim" and status.get("phase") != "Bound":
            errors.append(f"{prefix}: phase={status.get('phase', 'unknown')}")
        elif kind == "Pod":
            if status.get("phase") != "Succeeded" and not ready(obj):
                errors.append(
                    f"{prefix}: phase={status.get('phase', 'unknown')}, not Ready"
                )
            for c in status.get("initContainerStatuses", []) + status.get(
                "containerStatuses", []
            ):
                reason = c.get("state", {}).get("waiting", {}).get("reason")
                if reason:
                    errors.append(f"{prefix}/{c['name']}: {reason}")
    return errors


def compute_placement(registry: list[dict], pods: list[dict]) -> list[dict]:
    """Map CubeOps' scheduler view to this release's actual compute Pods."""
    candidates = []
    for node in registry:
        if not node.get("Healthy") or node.get("SchedulingDisabled"):
            continue
        for pod in pods:
            status = pod.get("status", {})
            if node.get("IP") not in {status.get("podIP"), status.get("hostIP")}:
                continue
            if not ready(pod) or not pod.get("spec", {}).get("nodeName"):
                continue
            candidates.append(
                {
                    "id": node.get("InstanceID") or node["IP"],
                    "node": pod["spec"]["nodeName"],
                    "pod": pod["metadata"]["name"],
                    "quota_cpu": node.get("QuotaCpu"),
                    "quota_mem": node.get("QuotaMem"),
                    "cpu_usage": node.get("QuotaCpuUsage", 0),
                    "mem_usage": node.get("QuotaMemUsage", 0),
                    "max_vms": node.get("MaxMvmLimit", 0),
                    "vms": node.get("mvm_num", 0),
                }
            )
            break
    return candidates


def run_kubernetes_preflight(config: KubernetesConfig, reporter) -> dict:
    kube = Kubectl(config)
    errors, warnings = [], []
    details: dict[str, Any] = {
        "context": config.context,
        "namespace": config.namespace,
        "release": config.release,
    }
    selector = f"app.kubernetes.io/instance={config.release}"

    def observe(name, action, default=None, optional=False):
        try:
            return action()
        except (RuntimeError, ValueError, OSError, subprocess.TimeoutExpired) as exc:
            (warnings if optional else errors).append(f"{name}: {exc}")
            return default

    def helm_status():
        result = subprocess.run(
            [
                "helm",
                "status",
                config.release,
                "-n",
                config.namespace,
                "--kube-context",
                config.context,
                "-o",
                "json",
            ],
            capture_output=True,
            text=True,
            timeout=30,
            check=False,
        )
        if result.returncode:
            raise RuntimeError(result.stderr.strip()[:1000])
        release = json.loads(result.stdout)
        return {
            "status": release.get("info", {}).get("status"),
            "revision": release.get("version"),
            "chart": release.get("chart", {}).get("metadata", {}).get("version"),
        }

    details["helm"] = observe("Helm release", helm_status)
    if details["helm"] and details["helm"]["status"] != "deployed":
        errors.append(
            f"Helm release is {details['helm']['status']!r}, expected deployed"
        )
    version = observe(
        "Kubernetes version", lambda: json.loads(kube.run("version", "-o", "json")), {}
    )
    details["kubernetes_version"] = version.get("serverVersion", {}).get("gitVersion")
    nodes = observe("Kubernetes nodes", lambda: kube.get("nodes")["items"], [])
    details["nodes"] = [
        {
            "name": n["metadata"]["name"],
            "ready": ready(n),
            "unschedulable": n.get("spec", {}).get("unschedulable", False),
            **n.get("status", {}).get("nodeInfo", {}),
        }
        for n in nodes
    ]
    objects = []
    for kind in ("deployments", "daemonsets", "statefulsets", "pods", "pvc"):
        objects.extend(
            observe(
                kind,
                lambda k=kind: kube.get(
                    k, namespace=config.namespace, selector=selector
                )["items"],
                [],
            )
        )
    errors.extend(workload_errors(objects))
    if not objects:
        errors.append("no workloads found for the Helm release label")
    details["workloads"] = [
        {
            "kind": o["kind"],
            "name": o["metadata"]["name"],
            "status": o.get("status", {}),
        }
        for o in objects
    ]
    compute = [
        o
        for o in objects
        if o["kind"] == "Pod"
        and o["metadata"].get("labels", {}).get("app.kubernetes.io/component")
        == "cube-node"
        and not o["metadata"].get("deletionTimestamp")
    ]
    if not compute:
        errors.append("no cube-node Pods found; install/enable the compute DaemonSet")
    details["compute_placement"] = [
        {
            "pod": p["metadata"]["name"],
            "node": p.get("spec", {}).get("nodeName"),
            "images": [c["image"] for c in p["spec"]["containers"]],
        }
        for p in compute
    ]
    services = observe(
        "Services",
        lambda: kube.get("services", namespace=config.namespace, selector=selector)[
            "items"
        ],
        [],
    )
    ops = [
        s
        for s in services
        if s["metadata"].get("labels", {}).get("app.kubernetes.io/component")
        == "ops"
    ]
    registry = (
        observe(
            "CubeOps scheduler node view",
            lambda: kube.service_get(ops[0], "/internal/v1/nodes"),
            [],
        )
        if ops
        else []
    )
    if not ops:
        errors.append(
            "no chart-managed cube-ops Service found for registration discovery"
        )
    if not isinstance(registry, list):
        errors.append("CubeOps returned an unexpected node view (expected a list)")
        registry = []
    candidates = compute_placement(registry, compute)
    details["scheduler_nodes"] = candidates
    if not candidates:
        errors.append(
            "no healthy, scheduling-enabled registered node maps to a Ready cube-node Pod"
        )
    else:
        # Quota ratios are also applied by CubeMaster. Record raw usage instead
        # of inventing an effective free-CPU figure; actual creation proves fit.
        if all(n["max_vms"] > 0 and n["vms"] >= n["max_vms"] for n in candidates):
            errors.append("all registered compute nodes have reached MaxMvmLimit")
    dns = observe(
        "cluster DNS Service",
        lambda: kube.get(
            "services", namespace="kube-system", selector="k8s-app=kube-dns"
        )["items"],
        [],
    )
    dns_ips = sorted(
        {
            ip
            for s in dns
            for ip in s.get("spec", {}).get("clusterIPs", [])
            if ip != "None"
        }
    )
    if not dns_ips:
        errors.append("no cluster DNS IPs discovered from kube-system/k8s-app=kube-dns")
    details["dns_ips"] = dns_ips
    cni = observe(
        "CNI discovery",
        lambda: kube.get("daemonsets", namespace="kube-system")["items"],
        [],
        optional=True,
    )
    details["cni_candidates"] = [
        {
            "name": d["metadata"]["name"],
            "images": [c["image"] for c in d["spec"]["template"]["spec"]["containers"]],
        }
        for d in cni
        if any(
            x in d["metadata"]["name"].lower()
            for x in (
                "calico",
                "cilium",
                "flannel",
                "kindnet",
                "weave",
                "antrea",
                "terway",
            )
        )
    ]
    details["runtime_checks"] = []
    for pod in compute:
        if not ready(pod):
            continue
        name = pod["metadata"]["name"]
        cubelet = next(c for c in pod["spec"]["containers"] if c["name"] == "cubelet")
        data_dir = next(
            (
                m["mountPath"]
                for m in cubelet.get("volumeMounts", [])
                if m["name"] == "data-cubelet"
            ),
            "/data/cubelet",
        )
        # Read-only exec; no SSH or privileged diagnostic Pods are required.
        output = observe(
            f"runtime assets on {name}",
            lambda p=name, d=data_dir: kube.run(
                "exec",
                "-n",
                config.namespace,
                p,
                "-c",
                "cubelet",
                "--",
                "sh",
                "-c",
                "test -c /dev/kvm && echo present:kvm || echo missing:kvm; "
                "command -v cube-runtime >/dev/null && echo present:runtime || echo missing:runtime; "
                'test -S "$1/cubelet.sock" && echo present:socket || echo missing:socket',
                "--",
                d,
            ),
            optional=True,
        )
        details["runtime_checks"].append(
            {"pod": name, "result": output or "unavailable"}
        )
        if output and "missing:kvm" in output:
            errors.append(
                f"{name}: /dev/kvm is absent; MicroVMs require a KVM-capable Linux node"
            )
        if output and "missing:runtime" in output:
            errors.append(
                f"{name}: cube-runtime not found in PATH; check runtime asset staging"
            )
        if output and "missing:socket" in output:
            errors.append(
                f"{name}: {data_dir}/cubelet.sock is absent; check Cubelet startup"
            )
    details["warnings"] = warnings
    reporter.record(
        "kubernetes_preflight_failed" if errors else "kubernetes_preflight_passed",
        errors=errors,
        **details,
    )
    if errors:
        raise RuntimeError(
            "Kubernetes post-install preflight failed:\n- " + "\n- ".join(errors)
        )
    return details


@contextmanager
def mock_service(kube: Kubectl, reporter, *, avoid_node: str | None = None):
    """Create one isolated namespace; delete only that namespace with matching UID."""
    name = "cube-e2e-" + uuid.uuid4().hex[:12]
    labels = {"app.kubernetes.io/managed-by": "cube-sdk-e2e", "cube-e2e-run": name}
    namespace = {
        "apiVersion": "v1",
        "kind": "Namespace",
        "metadata": {"name": name, "labels": labels},
    }
    uid = None
    try:
        try:
            created = json.loads(
                kube.run("create", "-f", "-", "-o", "json", payload=namespace)
            )
        except (RuntimeError, ValueError):
            # The API may have committed the namespace before the client lost
            # its response. Recover ownership, not a blind name-only delete.
            observed = kube.run(
                "get", "namespace", name, "--ignore-not-found", "-o", "json"
            )
            if observed:
                metadata = json.loads(observed)["metadata"]
                if all(
                    metadata.get("labels", {}).get(k) == v for k, v in labels.items()
                ):
                    uid = metadata["uid"]
            reporter.record("kubernetes_mock_setup_failed", namespace=name, uid=uid)
            raise
        uid = created["metadata"]["uid"]
        reporter.record("kubernetes_mock_created", namespace=name, uid=uid)
        pod_spec: dict[str, Any] = {
            "restartPolicy": "Never",
            "terminationGracePeriodSeconds": 1,
            "automountServiceAccountToken": False,
            "securityContext": {
                "runAsNonRoot": True,
                "runAsUser": 1000,
                "seccompProfile": {"type": "RuntimeDefault"},
            },
            "containers": [
                {
                    "name": "http",
                    "image": kube.config.mock_image,
                    "command": [
                        "sh",
                        "-ec",
                        (
                            f"mkdir -p /tmp/http; printf '%s' {name} > /tmp/http/index.html; "
                            "exec httpd -f -p 8080 -h /tmp/http"
                        ),
                    ],
                    "ports": [{"containerPort": 8080}],
                    "readinessProbe": {
                        "httpGet": {"path": "/", "port": 8080},
                        "periodSeconds": 1,
                    },
                    "resources": {
                        "requests": {"cpu": "10m", "memory": "16Mi"},
                        "limits": {"cpu": "100m", "memory": "32Mi"},
                    },
                    "securityContext": {
                        "allowPrivilegeEscalation": False,
                        "capabilities": {"drop": ["ALL"]},
                    },
                }
            ],
        }
        if avoid_node:
            pod_spec["affinity"] = {
                "nodeAffinity": {
                    "preferredDuringSchedulingIgnoredDuringExecution": [
                        {
                            "weight": 100,
                            "preference": {
                                "matchFields": [
                                    {
                                        "key": "metadata.name",
                                        "operator": "NotIn",
                                        "values": [avoid_node],
                                    }
                                ]
                            },
                        }
                    ]
                }
            }
        for obj in (
            {
                "apiVersion": "v1",
                "kind": "Pod",
                "metadata": {"name": "http", "namespace": name, "labels": labels},
                "spec": pod_spec,
            },
            {
                "apiVersion": "v1",
                "kind": "Service",
                "metadata": {"name": "http", "namespace": name, "labels": labels},
                "spec": {
                    "selector": labels,
                    "ports": [{"name": "http", "port": 8080, "targetPort": 8080}],
                },
            },
        ):
            kube.run("create", "-f", "-", payload=obj)
        kube.run(
            "wait",
            "-n",
            name,
            "pod/http",
            "--for=condition=Ready",
            f"--timeout={kube.config.ready_timeout}s",
            timeout=kube.config.ready_timeout + 10,
        )
        svc = kube.get("service/http", namespace=name)
        pod = kube.get("pod/http", namespace=name)
        result = {
            "namespace": name,
            "uid": uid,
            "ip": svc["spec"]["clusterIP"],
            "port": 8080,
            "fqdn": f"http.{name}.svc.{kube.config.cluster_domain}",
            "body": name,
            "endpoint_node": pod["spec"]["nodeName"],
            "endpoint_ip": pod["status"]["podIP"],
        }
        reporter.record("kubernetes_mock_ready", **result)
        yield result
    finally:
        if uid is not None:
            # A timeout may have created resources. The namespace owns all of
            # them, including a partially provisioned Service or Pending Pod.
            try:
                current = kube.get(f"namespace/{name}")
                if current["metadata"]["uid"] != uid:
                    raise RuntimeError(
                        f"refusing cleanup: namespace {name} UID changed"
                    )
                kube.run(
                    "delete",
                    "namespace",
                    name,
                    "--wait=true",
                    "--timeout=45s",
                    timeout=55,
                )
                reporter.record(
                    "kubernetes_mock_cleanup", namespace=name, outcome="passed"
                )
            except RuntimeError as exc:
                reporter.record(
                    "kubernetes_mock_cleanup",
                    namespace=name,
                    outcome="failed",
                    error=str(exc),
                )
                raise
