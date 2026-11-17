#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
import argparse
import json
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
SOURCE = ROOT / "CubeObservability/grafana/dashboards"
CHART = ROOT / "deploy/kubernetes/chart/files/cube-observability/grafana/dashboards"
DS = {"type": "prometheus", "uid": "$datasource"}
SEEDS = "cubesandbox_node_sandbox_count|cubesandbox_host_sandbox_memory_current_bytes|cubesandbox_guest_workload_memory_current_bytes"
LEVELS = {"cluster": "Cluster", "node": "Node", "sandbox": "Sandbox"}


def variable(name, label, query, multi=False):
    result = {"name": name, "label": label, "type": "query", "datasource": DS,
              "query": {"query": query, "refId": "StandardVariableQuery"},
              "definition": query, "refresh": 2, "sort": 1, "hide": 0,
              "multi": multi, "includeAll": multi, "options": [], "skipUrlSync": False,
              "current": {}}
    if multi:
        result.update(allValue=".*", current={"text": "All", "value": "$__all"})
    return result


def variables(level):
    result = [{"name": "datasource", "label": "Prometheus", "type": "datasource",
               "query": "prometheus", "refresh": 1, "current": {}, "options": [],
               "hide": 0, "multi": False, "includeAll": False, "skipUrlSync": False}]
    result.append(variable("cluster", "Cluster", f'label_values({{__name__=~"{SEEDS}",job="cubesandbox-resource"}}, cluster)'))
    if level != "cluster":
        result.append(variable("node", "Node", f'label_values({{__name__=~"{SEEDS}",job="cubesandbox-resource",cluster=~"${{cluster:regex}}"}}, node)'))
    if level == "sandbox":
        seeds = "cubesandbox_host_sandbox_memory_current_bytes|cubesandbox_guest_workload_memory_current_bytes"
        result.append(variable("sandbox_id", "Sandbox", f'label_values({{__name__=~"{seeds}",job="cubesandbox-resource",cluster=~"${{cluster:regex}}",node=~"${{node:regex}}"}}, sandbox_id)'))
        result.append(variable("container_id", "Guest container", 'label_values(cubesandbox_guest_workload_memory_current_bytes{job="cubesandbox-resource",cluster=~"${cluster:regex}",node=~"${node:regex}",sandbox_id=~"${sandbox_id:regex}"}, container_id)', True))
    return result


def selector(metric, level, guest=False, node_only=False):
    labels = ['job="cubesandbox-resource"', 'cluster=~"${cluster:regex}"']
    if level != "cluster":
        labels.append('node=~"${node:regex}"')
    if level == "sandbox" and not node_only:
        labels.append('sandbox_id=~"${sandbox_id:regex}"')
        if guest:
            labels.append('container_id=~"${container_id:regex}"')
    return metric + "{" + ",".join(labels) + "}"


def dashboard(level):
    panels = []
    y = 0
    ident = 0

    def add(title, queries=None, unit="short", kind="timeseries", description="", full=False):
        nonlocal y, ident
        ident += 1
        width = 24 if full or kind in ("row", "text", "table") else 12
        last = panels[-1] if panels else None
        x = 0
        if width == 12 and last and last["gridPos"]["w"] == 12 and last["gridPos"]["x"] == 0:
            x = 12
            py = last["gridPos"]["y"]
        else:
            py = y
            y += 1 if kind == "row" else 5 if kind == "text" else 8
        panel = {"id": ident, "title": title, "type": kind,
                 "gridPos": {"h": 1 if kind == "row" else 5 if kind == "text" else 8,
                             "w": width, "x": x, "y": py}, "description": description}
        if kind == "row":
            panel.update(collapsed=False, panels=[])
        elif kind == "text":
            panel["options"] = {"mode": "markdown", "content": description}
        else:
            panel.update(datasource=DS, targets=[{"refId": chr(65 + i), "expr": expr,
                        "legendFormat": legend, "range": kind != "table", "instant": kind == "table",
                        "format": "table" if kind == "table" else "time_series"}
                        for i, (expr, legend) in enumerate(queries or [])],
                        fieldConfig={"defaults": {"unit": unit, "noValue": "No sample / not exported",
                                     "color": {"mode": "palette-classic"}}, "overrides": []},
                        options={"legend": {"displayMode": "table", "placement": "bottom", "calcs": ["lastNotNull"]},
                                 "tooltip": {"mode": "multi", "sort": "desc"}})
            if kind == "table":
                target_level = "node" if level == "cluster" else "sandbox"
                url = f'/d/cubesandbox-{target_level}?var-cluster=${{__data.fields.cluster}}&var-node=${{__data.fields.node}}'
                if target_level == "sandbox":
                    url += '&var-sandbox_id=${__data.fields.sandbox_id}'
                panel["fieldConfig"]["overrides"] = [{"matcher": {"id": "byName", "options": "node" if level == "cluster" else "sandbox_id"}, "properties": [{"id": "links", "value": [{"title": "Open details", "url": url}]}]}]
                panel["options"] = {"showHeader": True, "cellHeight": "sm"}
        panels.append(panel)

    identity = "cluster,node,sandbox_id,container_id"

    def aggregate(expr, guest=False):
        if level == "sandbox" and guest:
            return f"sum by (container_id) ({expr})"
        return f"sum({expr})"

    def s(metric, guest=False):
        return selector(metric, level, guest)

    def rate(metric, guest=False):
        expr = f"rate({s(metric, guest)}[$__rate_interval])"
        if guest:
            expr = f'({expr} unless on ({identity}) (changes({s("cubesandbox_guest_workload_metrics_epoch", True)}[$__rate_interval]) > 0))'
        return expr

    def legend(text, guest=False):
        return text + (" {{container_id}}" if guest and level == "sandbox" else "")

    about = (f"### {LEVELS[level]} resource metrics\n"
             "Host Sandbox measures the host cgroup (including runtime); Guest Workload measures the workload inside the sandbox. "
             "These are sandbox resource totals, **not physical node/cluster capacity**, and must not be added together. "
             "CPU rates are cores; throttled time is seconds/second. Limits are sums of exported finite limits, not physical capacity. "
             "No sample is not zero. Guest rates exclude observed epoch-change windows; they are not exact rollback accounting. "
             "Dropdowns are scoped by cluster → node → sandbox → guest container, and refresh with the selected time range. "
             "Node inventory counters are node context, not a sandbox running-state count.")
    add("About", kind="text", description=about)
    add("Inventory and sample coverage", kind="row")
    node_count = selector("cubesandbox_node_sandbox_count", level, node_only=True)
    node_available = selector("cubesandbox_node_sandbox_available_count", level, node_only=True)
    add("Node context: sandbox inventory / available samples", [(f"sum(max by (cluster,node) ({node_count}))", "Inventory"), (f"sum(max by (cluster,node) ({node_available}))", "Available samples")])
    add("Host / guest sampled workloads", [(f'count({s("cubesandbox_host_sandbox_memory_current_bytes")})', "Host samples"), (f'count({s("cubesandbox_guest_workload_memory_current_bytes", True)})', "Guest samples")])
    for guest in (False, True):
        prefix = "cubesandbox_guest_workload_" if guest else "cubesandbox_host_sandbox_"
        scope_name = "Guest Workload" if guest else "Host Sandbox"
        add(scope_name + " · CPU", kind="row")
        usage = prefix + "cpu_usage_seconds_total"
        limit = prefix + "cpu_limit_cores"
        add("CPU usage / exported limit", [(aggregate(rate(usage, guest), guest), legend("Usage", guest)), (aggregate(s(limit, guest), guest), legend("Finite limit", guest))], "cores")
        match = identity if guest else "cluster,node,sandbox_id"
        utilization = f'100 * {aggregate(f"({rate(usage, guest)} and on ({match}) ({s(limit, guest)} > 0))", guest)} / {aggregate(f"(({s(limit, guest)} > 0) and on ({match}) {rate(usage, guest)})", guest)}'
        add("CPU utilization · positive-limit samples only", [(utilization, legend("Utilization", guest))], "percent", description="Usage and denominator include only identities with an exported positive CPU limit.")
        add("CPU user / system usage", [(aggregate(rate(prefix + "cpu_user_seconds_total", guest), guest), legend("User", guest)), (aggregate(rate(prefix + "cpu_system_seconds_total", guest), guest), legend("System", guest))], "cores")
        add("CPU throttled time rate", [(aggregate(rate(prefix + "cpu_throttled_seconds_total", guest), guest), legend("Throttled s/s", guest))], "short")
        periods = aggregate(rate(prefix + "cpu_periods_total", guest), guest)
        throttled = aggregate(rate(prefix + "cpu_throttled_periods_total", guest), guest)
        add("CPU scheduling / throttled periods per second", [(periods, legend("Periods/s", guest)), (throttled, legend("Throttled periods/s", guest))], "ops")
        add("CPU throttled-period ratio", [(f"100 * {throttled} / ({periods} > 0)", legend("Throttled periods", guest))], "percent")
        add(scope_name + " · Memory", kind="row")
        current = prefix + "memory_current_bytes"
        memory_limit = prefix + "memory_limit_bytes"
        failures = prefix + "memory_failures_total"
        add("Memory current / exported finite limit", [(aggregate(s(current, guest), guest), legend("Current", guest)), (aggregate(s(memory_limit, guest), guest), legend("Finite limit", guest))], "bytes")
        memory_utilization = f'100 * {aggregate(f"({s(current, guest)} and on ({match}) ({s(memory_limit, guest)} > 0))", guest)} / {aggregate(f"({s(memory_limit, guest)} > 0)", guest)}'
        add("Memory utilization · positive-limit samples only", [(memory_utilization, legend("Utilization", guest))], "percent")
        add("Memory limit failures · observed rate", [(aggregate(rate(failures, guest), guest), legend("Failures/s", guest))], "ops", description="Cgroup memory limit failures are not a count of OOM kills.")
        add("Exported CPU / memory limit coverage", [(f'count({s(limit, guest)})', "CPU limits"), (f'count({s(memory_limit, guest)})', "Memory limits")], "short")
        if level != "sandbox":
            group = "cluster,node" if level == "cluster" else "cluster,node,sandbox_id"
            add("Workload memory · drill down", [(f'sum by ({group}) ({s(current, guest)})', "Memory")], "bytes", "table", full=True)
    add("Guest lifecycle · current epoch", kind="row")
    epoch = s("cubesandbox_guest_workload_metrics_epoch", True)
    started = s("cubesandbox_guest_workload_metrics_epoch_start_time_seconds", True)
    add("Current guest epoch generation", [(epoch, "{{node}} / {{sandbox_id}} / {{container_id}}")], "short", "table", description="Generation is an identity-local gauge, never sum generations across workloads.", full=True)
    add("Guest epoch start time", [(f"1000 * {started}", "{{node}} / {{sandbox_id}} / {{container_id}}")], "dateTimeAsIso", "table", full=True)
    add("Guest epoch age", [(f"time() - {started}", "{{sandbox_id}} / {{container_id}}")], "s")
    add("Observed guest epoch changes in selected range", [(aggregate(f"changes({epoch}[$__range])", True), legend("Observed changes", True))], "short", description="Only changes visible in the sampled series; not a total of newly created sandboxes or exact rollback events.")
    links = [{"title": LEVELS[target] + " metrics", "type": "link", "url": f"/d/cubesandbox-{target}", "includeVars": True, "keepTime": True, "targetBlank": False} for target in LEVELS if target != level]
    log_url = '/d/cubesandbox-logs?var-cluster=${cluster:percentencode}'
    if level != "cluster":
        log_url += '&var-node=${node:percentencode}'
    if level == "sandbox":
        log_url += '&var-sandbox_id=${sandbox_id:percentencode}'
    links.append({"title": "Logs", "type": "link", "url": log_url, "includeVars": False, "keepTime": True, "targetBlank": False})
    return {"id": None, "uid": "cubesandbox-" + level, "title": "CubeSandbox · " + LEVELS[level] + " Metrics",
            "schemaVersion": 39, "version": 1, "editable": True, "style": "dark", "timezone": "browser",
            "tags": ["cubesandbox", "metrics", level], "refresh": "30s", "time": {"from": "now-1h", "to": "now"},
            "timepicker": {}, "annotations": {"list": []}, "links": links, "templating": {"list": variables(level)}, "panels": panels}


def simplify_logs(document, elasticsearch):
    keep = {"About", "Sandbox logs", "Sandbox log volume by stream", "Sandbox stdout/stderr log query", "Component logs (Kubernetes Helm opt-in)"}
    document["panels"] = [panel for panel in document["panels"] if panel["title"] in keep]
    y = 0
    for panel in document["panels"]:
        old_y = panel["gridPos"]["y"]
        panel["gridPos"].update(x=0, y=y, w=24)
        if panel["type"] == "text":
            panel["options"]["content"] = (
                "## CubeSandbox logs\nSelect cluster and node, then filter sandbox stdout/stderr. "
                + ("Sandbox dropdowns follow cluster → node. The text filter uses Lucene syntax. " if elasticsearch else "Sandbox ID is a regex text filter so historical IDs remain searchable without high-cardinality index labels. The text filter uses RE2. ")
                + "The collapsed component log row requires observability.logs.components.enabled=true (Helm only).")
        for child in panel.get("panels", []):
            child["gridPos"]["y"] += y - old_y
        y += panel["gridPos"]["h"]
    document["links"] = [{"title": label + " metrics", "type": "link", "url": f"/d/cubesandbox-{level}", "includeVars": False, "keepTime": True, "targetBlank": False} for level, label in LEVELS.items()]
    if elasticsearch:
        for item in document["templating"]["list"]:
            if item["name"] not in ("node", "sandbox_id"):
                continue
            query = json.loads(item["query"])
            query["query"] = "service_name.keyword:cubesandbox-sandbox AND cubesandbox_cluster_id.keyword:${cluster:lucene}"
            if item["name"] == "sandbox_id":
                query["query"] += " AND host_name.keyword:${node:lucene}"
            item["query"] = item["definition"] = json.dumps(query)
    return document


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--check", action="store_true")
    args = parser.parse_args()
    for level in LEVELS:
        content = json.dumps(dashboard(level), ensure_ascii=False, indent=2) + "\n"
        for directory in (SOURCE, CHART):
            path = directory / f"cubesandbox-{level}.json"
            if args.check:
                if not path.exists() or path.read_text() != content:
                    raise SystemExit(f"dashboard drift: {path}")
            else:
                path.write_text(content)
    for filename in ("cubesandbox-logs.json", "cubesandbox-logs-elasticsearch.json"):
        document = simplify_logs(json.loads((SOURCE / filename).read_text()), "elasticsearch" in filename)
        content = json.dumps(document, ensure_ascii=False, indent=2) + "\n"
        for directory in (SOURCE, CHART):
            path = directory / filename
            if args.check:
                if path.read_text() != content:
                    raise SystemExit(f"dashboard drift: {path}")
            else:
                path.write_text(content)


if __name__ == "__main__":
    main()
