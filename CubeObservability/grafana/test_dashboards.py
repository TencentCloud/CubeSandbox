#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
import json
import re
import unittest
from pathlib import Path

from generate_dashboards import CHART, LEVELS, ROOT, SOURCE, dashboard


class DashboardTests(unittest.TestCase):
    def test_all_exported_metrics_in_each_level(self):
        source = (ROOT / "Cubelet/plugins/cube/internals/resourcemetrics/prometheus.go").read_text()
        metrics = set(re.findall(r'NewDesc\("(cubesandbox_[^"]+)"', source))
        self.assertEqual(len(metrics), 24)
        for level in LEVELS:
            with self.subTest(level=level):
                doc = json.loads((SOURCE / f"cubesandbox-{level}.json").read_text())
                expressions = "\n".join(target["expr"] for panel in doc["panels"] for target in panel.get("targets", []))
                self.assertTrue(metrics.issubset(set(re.findall(r"cubesandbox_[a-z_]+", expressions))))
                self.assertNotIn("clamp_min", expressions)
                self.assertNotIn("or vector(0)", expressions)
                self.assertIn("unless on (cluster,node,sandbox_id,container_id)", expressions)
                self.assertNotIn("sum(cubesandbox_guest_workload_metrics_epoch{", expressions)
                for panel in doc["panels"]:
                    for target in panel.get("targets", []):
                        self.assertIn('job="cubesandbox-resource"', target["expr"])

    def test_dependent_dropdowns_and_scope(self):
        for level, expected in (("cluster", ["datasource", "cluster"]),
                                ("node", ["datasource", "cluster", "node"]),
                                ("sandbox", ["datasource", "cluster", "node", "sandbox_id", "container_id"])):
            doc = dashboard(level)
            variables = doc["templating"]["list"]
            self.assertEqual([item["name"] for item in variables], expected)
            for item in variables[1:]:
                self.assertEqual(item["refresh"], 2)
                self.assertEqual(item["definition"], item["query"]["query"])
                self.assertEqual(item["multi"], item["name"] == "container_id")
                for parent in expected[1:expected.index(item["name"])]:
                    self.assertIn("${" + parent + ":regex}", item["definition"])
            self.assertIn("guest_workload_memory_current_bytes", variables[1]["definition"])
            expressions = "\n".join(t["expr"] for p in doc["panels"] for t in p.get("targets", []))
            if level == "cluster":
                self.assertNotIn("${node:", expressions)
                self.assertNotIn("${sandbox_id:", expressions)
            if level == "node":
                self.assertNotIn("${sandbox_id:", expressions)

    def test_unique_ids_layout_and_mirrors(self):
        uids = set()
        for level in LEVELS:
            filename = f"cubesandbox-{level}.json"
            self.assertEqual((SOURCE / filename).read_bytes(), (CHART / filename).read_bytes())
            doc = json.loads((SOURCE / filename).read_text())
            self.assertNotIn(doc["uid"], uids)
            uids.add(doc["uid"])
            self.assertEqual(len(doc["panels"]), len({p["id"] for p in doc["panels"]}))
            rectangles = []
            for p in doc["panels"]:
                g = p["gridPos"]
                rect = (g["x"], g["y"], g["x"] + g["w"], g["y"] + g["h"])
                for other in rectangles:
                    self.assertFalse(rect[0] < other[2] and other[0] < rect[2] and rect[1] < other[3] and other[1] < rect[3])
                rectangles.append(rect)

    def test_simple_logs_and_es_dropdowns(self):
        for filename in ("cubesandbox-logs.json", "cubesandbox-logs-elasticsearch.json"):
            doc = json.loads((SOURCE / filename).read_text())
            self.assertEqual((SOURCE / filename).read_bytes(), (CHART / filename).read_bytes())
            self.assertFalse(any("stderr share" in p["title"] or "Top sandboxes" in p["title"] for p in doc["panels"]))
            self.assertTrue(any(p["type"] == "logs" for p in doc["panels"]))
            self.assertTrue(any(p["title"] == "Component logs (Kubernetes Helm opt-in)" and p["collapsed"] for p in doc["panels"]))
        variables = {v["name"]: v for v in doc["templating"]["list"]}
        self.assertIn("${cluster:lucene}", variables["node"]["query"])
        self.assertIn("${cluster:lucene}", variables["sandbox_id"]["query"])
        self.assertIn("${node:lucene}", variables["sandbox_id"]["query"])

    def test_deployment_assets_are_wired(self):
        for filename in ("CubeObservability/Dockerfile", "deploy/one-click/build-release-bundle.sh",
                         "deploy/one-click/observability/grafana/install.sh", "deploy/kubernetes/chart/templates/observability.yaml"):
            content = (ROOT / filename).read_text()
            for level in LEVELS:
                self.assertIn("cubesandbox-" + level, content, filename)
        self.assertFalse((SOURCE / "cubesandbox-host-sandbox.json").exists())


if __name__ == "__main__":
    unittest.main()
