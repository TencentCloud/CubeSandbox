# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

import pytest
from framework.kubernetes import Kubectl, KubernetesConfig, mock_service


@pytest.fixture(scope="session")
def k8s_environment(pytestconfig, sdk_e2e_preflight):
    return pytestconfig._k8s_environment


@pytest.fixture()
def k8s_service(sdk_backend, k8s_environment, sdk_e2e_reporter):
    if sdk_backend != "cubesandbox":
        pytest.skip(
            "Kubernetes placement requires the CubeSandbox distribution_scope extension"
        )
    candidates = k8s_environment["scheduler_nodes"]
    target = next(
        (n for n in candidates if n["max_vms"] <= 0 or n["vms"] < n["max_vms"]),
        candidates[0],
    )
    with mock_service(
        Kubectl(KubernetesConfig.from_env()),
        sdk_e2e_reporter,
        avoid_node=target["node"],
    ) as service:
        service["compute"] = target
        service["dns_ips"] = k8s_environment["dns_ips"]
        sdk_e2e_reporter.record(
            "kubernetes_service_topology",
            compute_node=target["node"],
            endpoint_node=service["endpoint_node"],
            path="same-node"
            if target["node"] == service["endpoint_node"]
            else "cross-node",
        )
        yield service


@pytest.fixture()
def sdk_create_options(request, sdk_create_options):
    options = dict(sdk_create_options)
    if request.node.get_closest_marker("k8s_service"):
        service = request.getfixturevalue("k8s_service")
        options.update(
            distribution_scope=[service["compute"]["id"]],
            allow_internet_access=False,
            network={"allow_out": [*service["dns_ips"], service["ip"]]},
        )
    return options
