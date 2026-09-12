# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from copy import deepcopy
from types import SimpleNamespace

import pytest
from framework.kubernetes import (
    Kubectl,
    KubernetesConfig,
    compute_placement,
    mock_service,
    run_kubernetes_preflight,
    workload_errors,
)
from framework.kubernetes_suite import select_post_install_tests

pytestmark = pytest.mark.framework


class Reporter:
    def __init__(self):
        self.events = []

    def record(self, event, **fields):
        self.events.append((event, fields))


def pod():
    return {
        "kind": "Pod",
        "metadata": {
            "name": "cube-node-a",
            "labels": {"app.kubernetes.io/component": "cube-node"},
        },
        "spec": {
            "nodeName": "node-a",
            "containers": [{"name": "cubelet", "image": "cubelet:test"}],
        },
        "status": {
            "phase": "Running",
            "hostIP": "10.0.0.1",
            "podIP": "10.42.0.2",
            "conditions": [{"type": "Ready", "status": "True"}],
        },
    }


def test_pending_pvc_and_image_pull_are_actionable():
    pending = pod()
    pending["status"]["conditions"] = []
    pending["status"]["containerStatuses"] = [
        {"name": "cubelet", "state": {"waiting": {"reason": "ImagePullBackOff"}}}
    ]
    errors = workload_errors(
        [
            pending,
            {
                "kind": "PersistentVolumeClaim",
                "metadata": {"name": "data"},
                "status": {"phase": "Pending"},
            },
        ]
    )
    assert any("ImagePullBackOff" in e for e in errors)
    assert any("PersistentVolumeClaim/data: phase=Pending" in e for e in errors)


def test_ready_but_stale_controller_does_not_pass():
    obj = {
        "kind": "Deployment",
        "metadata": {"name": "api", "generation": 3},
        "spec": {"replicas": 2},
        "status": {"observedGeneration": 2, "readyReplicas": 2, "updatedReplicas": 1},
    }
    errors = workload_errors([obj])
    assert any("generation" in e for e in errors)
    assert any("updated=1" in e for e in errors)


def test_zero_compute_placement_is_not_ready():
    obj = {
        "kind": "DaemonSet",
        "metadata": {
            "name": "node",
            "generation": 1,
            "labels": {"app.kubernetes.io/component": "cube-node"},
        },
        "status": {"observedGeneration": 1, "desiredNumberScheduled": 0},
    }
    assert "no eligible compute nodes" in " ".join(workload_errors([obj]))


@pytest.mark.parametrize(
    "change", [{"Healthy": False}, {"SchedulingDisabled": True}, {"IP": "10.0.0.9"}]
)
def test_registry_requires_healthy_local_compute(change):
    node = {"InstanceID": "cube-a", "IP": "10.0.0.1", "Healthy": True}
    assert compute_placement([node], [pod()])[0]["node"] == "node-a"
    node.update(change)
    assert compute_placement([node], [pod()]) == []


def test_preflight_collects_failures_without_creating_resources(monkeypatch):
    calls = []

    def get(self, resource, **kwargs):
        calls.append(resource)
        return {"items": []}

    def run(self, *args, **kwargs):
        calls.append(args[0])
        return '{"serverVersion":{"gitVersion":"v1.36.1"}}'

    monkeypatch.setattr(Kubectl, "get", get)
    monkeypatch.setattr(Kubectl, "run", run)
    monkeypatch.setattr(
        "framework.kubernetes.subprocess.run",
        lambda *a, **k: SimpleNamespace(returncode=1, stderr="release: not found"),
    )
    reporter = Reporter()
    with pytest.raises(RuntimeError, match="Kubernetes post-install preflight failed"):
        run_kubernetes_preflight(KubernetesConfig("test", "cube", "cube"), reporter)
    event, data = reporter.events[-1]
    assert event == "kubernetes_preflight_failed"
    assert any("release: not found" in e for e in data["errors"])
    assert any("no cube-node" in e for e in data["errors"])
    assert any("DNS" in e for e in data["errors"])
    assert "create" not in calls and "delete" not in calls


class MockKube:
    config = KubernetesConfig("test", "cube", "cube")

    def __init__(self, fail_at=None, changed_uid=False):
        self.calls, self.objects = [], []
        self.fail_at, self.changed_uid = fail_at, changed_uid

    def run(self, *args, payload=None, **kwargs):
        self.calls.append(args)
        if payload:
            self.objects.append(deepcopy(payload))
        if args[0] == self.fail_at:
            raise RuntimeError("injected failure")
        if payload and payload["kind"] == "Namespace":
            return '{"metadata":{"uid":"owned-uid"}}'
        return ""

    def get(self, resource, **kwargs):
        if resource.startswith("namespace/"):
            return {
                "metadata": {"uid": "other-uid" if self.changed_uid else "owned-uid"}
            }
        if resource == "service/http":
            return {"spec": {"clusterIP": "10.96.0.5"}}
        return {"spec": {"nodeName": "node-b"}, "status": {"podIP": "10.42.0.8"}}


@pytest.mark.parametrize("failure", ["wait", None])
def test_mock_cleanup_after_readiness_or_test_failure(failure):
    kube, reporter = MockKube(fail_at=failure), Reporter()
    with (
        pytest.raises(RuntimeError, match="injected failure"),
        mock_service(kube, reporter, avoid_node="node-a"),
    ):
        raise RuntimeError("injected failure")
    assert any(c[0:2] == ("delete", "namespace") for c in kube.calls)
    assert reporter.events[-1][1]["outcome"] == "passed"
    created_pod = next(o for o in kube.objects if o["kind"] == "Pod")
    assert created_pod["spec"]["automountServiceAccountToken"] is False
    assert "nodeName" not in created_pod["spec"]  # scheduler evaluates affinity/taints


def test_cleanup_refuses_replaced_namespace():
    kube, reporter = MockKube(changed_uid=True), Reporter()
    with pytest.raises(RuntimeError, match="UID changed"), mock_service(kube, reporter):
        pass
    assert not any(c[0] == "delete" for c in kube.calls)
    assert reporter.events[-1][1]["outcome"] == "failed"


def test_two_mock_runs_have_distinct_ownership():
    kube, reporter = MockKube(), Reporter()
    with mock_service(kube, reporter) as a, mock_service(kube, reporter) as b:
        assert a["namespace"] != b["namespace"]


def test_context_is_explicit_and_not_globally_changed(monkeypatch):
    commands = []

    def run(command, **kwargs):
        commands.append(command)
        return SimpleNamespace(returncode=0, stdout='{"items":[]}', stderr="")

    monkeypatch.setattr("framework.kubernetes.subprocess.run", run)
    Kubectl(KubernetesConfig("chosen-context", "cube", "cube")).get(
        "pods", namespace="cube"
    )
    assert commands[0][0:3] == ["kubectl", "--context", "chosen-context"]
    assert "use-context" not in commands[0]


def test_suite_reuses_shared_cases():
    def item(nodeid, marked=False):
        return SimpleNamespace(nodeid=nodeid, get_closest_marker=lambda name: marked)

    preflight = item(
        "cases/kubernetes/test_post_install.py::test_kubernetes_preflight", True
    )
    shared = item(
        "cases/lifecycle/test_create.py::test_create_returns_usable_sandbox[cubesandbox]"
    )
    other = item("cases/lifecycle/test_pause_resume.py::test_roundtrip[cubesandbox]")
    assert select_post_install_tests([preflight, shared, other])[0] == [
        preflight,
        shared,
    ]


@pytest.mark.parametrize(
    "runtime", ["present:kvm\npresent:runtime\npresent:socket", "denied", "missing:kvm"]
)
def test_preflight_uses_scheduler_view_and_distinguishes_unavailable_exec(
    monkeypatch, runtime
):
    compute = pod()
    dns = {"spec": {"clusterIPs": ["10.96.0.10"]}}
    ops = {
        "metadata": {
            "name": "ops",
            "namespace": "cube",
            "labels": {"app.kubernetes.io/component": "ops"},
        },
        "spec": {"ports": [{"port": 8090}]},
    }

    def get(self, resource, **kwargs):
        if resource == "pods":
            return {"items": [compute]}
        if resource == "services":
            return {"items": [dns] if kwargs["namespace"] == "kube-system" else [ops]}
        return {"items": []}

    def run(self, *args, **kwargs):
        if args[0] == "exec":
            if runtime == "denied":
                raise RuntimeError("Forbidden: pods/exec")
            return runtime
        return '{"serverVersion":{"gitVersion":"v1.36.1"}}'

    monkeypatch.setattr(Kubectl, "get", get)
    monkeypatch.setattr(Kubectl, "run", run)
    monkeypatch.setattr(
        Kubectl,
        "service_get",
        lambda *a: [
            {
                "Healthy": True,
                "IP": "10.0.0.1",
                "InstanceID": "cube-a",
                "QuotaCpu": 4000,
                "QuotaMem": 4096,
            }
        ],
    )
    monkeypatch.setattr(
        "framework.kubernetes.subprocess.run",
        lambda *a, **k: SimpleNamespace(
            returncode=0, stdout='{"info":{"status":"deployed"},"version":1}', stderr=""
        ),
    )
    reporter = Reporter()
    if runtime == "missing:kvm":
        with pytest.raises(RuntimeError, match="/dev/kvm is absent"):
            run_kubernetes_preflight(KubernetesConfig("test", "cube", "cube"), reporter)
    else:
        report = run_kubernetes_preflight(
            KubernetesConfig("test", "cube", "cube"), reporter
        )
        assert report["scheduler_nodes"][0]["id"] == "cube-a"
        assert report["dns_ips"] == ["10.96.0.10"]
        if runtime == "denied":
            assert any("Forbidden" in w for w in report["warnings"])
            assert report["runtime_checks"][0]["result"] == "unavailable"


def test_namespace_created_before_client_timeout_is_cleaned():
    import json

    class LostResponseKube(MockKube):
        def run(self, *args, payload=None, **kwargs):
            if payload and payload["kind"] == "Namespace":
                self.namespace = deepcopy(payload)
                self.namespace["metadata"]["uid"] = "owned-uid"
                raise RuntimeError("response timed out")
            if args[:2] == ("get", "namespace"):
                return json.dumps(self.namespace)
            return super().run(*args, payload=payload, **kwargs)

    kube, reporter = LostResponseKube(), Reporter()
    with (
        pytest.raises(RuntimeError, match="response timed out"),
        mock_service(kube, reporter),
    ):
        pytest.fail("setup should not yield")
    assert any(c[:2] == ("delete", "namespace") for c in kube.calls)


@pytest.mark.parametrize(
    "present,verify_error,close_error",
    [
        (False, False, False),
        (True, False, False),
        (False, True, False),
        (False, False, True),
    ],
)
def test_strict_cleanup_confirms_absence(
    monkeypatch, present, verify_error, close_error
):
    from framework import cleanup

    def missing():
        raise RuntimeError("sandbox not found")

    def close():
        if close_error:
            raise RuntimeError("close broken")

    class API:
        def __init__(self, config):
            pass

        def delete_sandbox(self, sandbox_id):
            pass

        def get_sandbox(self, sandbox_id):
            if verify_error:
                raise RuntimeError("API unavailable")
            return {"sandboxID": sandbox_id} if present else {}

        def close(self):
            pass

    monkeypatch.setattr(cleanup, "ApiClient", API)
    adapter = SimpleNamespace(
        backend="cubesandbox",
        sandbox_id="test",
        info=missing,
        kill=missing,
        close=close,
    )
    errors = cleanup.safe_kill(adapter, SimpleNamespace(), verify_absent=True)
    assert bool(errors) == (present or verify_error or close_error)
