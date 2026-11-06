#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
CubeSandbox 模板压测脚本

功能：
1. 批量并发创建模板（create-from-image）
2. 批量并发查询模板（列表 + 详情）
3. 批量并发删除模板
4. 压测结束后验证 DB 一致性（template_definitions / rootfs_artifacts / image_jobs 行数和状态）
5. 验证缓存一致性（API 查询结果 vs 直连 DB 结果）

用法：
    python3 template-bench.py \
        --master http://127.0.0.1:8089 \
        --db-host 127.0.0.1 --db-port 3306 --db-user cube --db-pass cube_pass --db-name cube_mvp \
        --image cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-browser:latest \
        --create-concurrency 10 --create-total 20 \
        --query-concurrency 50 --query-total 200 \
        --delete-concurrency 10

    # 只跑查询和删除（假设模板已存在）
    python3 template-bench.py --skip-create --skip-build-wait ...
"""

import argparse
import json
import sys
import time
import uuid
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass, field
from typing import Optional

import requests
import pymysql


@dataclass
class BenchStats:
    """压测统计数据"""
    total: int = 0
    success: int = 0
    failed: int = 0
    latencies: list = field(default_factory=list)

    def add(self, ok: bool, latency: float):
        self.total += 1
        if ok:
            self.success += 1
        else:
            self.failed += 1
        self.latencies.append(latency)

    def percentile(self, p: float) -> float:
        if not self.latencies:
            return 0.0
        s = sorted(self.latencies)
        idx = int(len(s) * p / 100)
        return s[min(idx, len(s) - 1)]

    def summary(self) -> str:
        return (
            f"总数: {self.total}, 成功: {self.success}, 失败: {self.failed}, "
            f"P50: {self.percentile(50):.2f}s, P95: {self.percentile(95):.2f}s, "
            f"P99: {self.percentile(99):.2f}s"
        )


class TemplateBench:
    def __init__(self, args):
        self.master = args.master.rstrip("/")
        self.db_config = {
            "host": args.db_host,
            "port": args.db_port,
            "user": args.db_user,
            "password": args.db_pass,
            "database": args.db_name,
        }
        self.image = args.image
        self.writable_layer_size = args.writable_layer_size
        self.instance_type = args.instance_type
        self.network_type = args.network_type
        self.expose_ports = args.expose_ports
        self.probe_port = args.probe_port
        self.probe_path = args.probe_path
        self.skip_create = args.skip_create
        self.skip_build_wait = args.skip_build_wait
        self.skip_query = args.skip_query
        self.skip_delete = args.skip_delete
        self.create_concurrency = args.create_concurrency
        self.create_total = args.create_total
        self.query_concurrency = args.query_concurrency
        self.query_total = args.query_total
        self.delete_concurrency = args.delete_concurrency
        self.build_timeout = args.build_timeout
        self.prefix = args.prefix or f"bench-{uuid.uuid4().hex[:8]}"

        self.created_template_ids = []
        self.created_job_ids = []

    def log(self, msg: str):
        print(f"[{time.strftime('%H:%M:%S')}] {msg}", flush=True)

    # ========== HTTP API 调用 ==========

    def create_template(self, template_id: str) -> tuple[bool, float, Optional[str]]:
        """创建模板，返回 (success, latency, job_id)"""
        start = time.time()
        try:
            payload = {
                "requestID": f"req-{uuid.uuid4().hex[:16]}",
                "template_id": template_id,
                "source_image_ref": self.image,
                "writable_layer_size": self.writable_layer_size,
                "instance_type": self.instance_type,
                "network_type": self.network_type,
            }
            if self.expose_ports:
                payload["exposed_ports"] = self.expose_ports
            if self.probe_port:
                payload["probe_port"] = self.probe_port
                if self.probe_path:
                    payload["probe_path"] = self.probe_path

            resp = requests.post(
                f"{self.master}/cube/template/from-image",
                json=payload,
                timeout=30,
            )
            latency = time.time() - start
            if resp.status_code != 200:
                return False, latency, None
            data = resp.json()
            if data.get("ret", {}).get("retCode") != 0:
                return False, latency, None
            job_id = data.get("job", {}).get("job_id")
            return True, latency, job_id
        except Exception as e:
            return False, time.time() - start, None

    def get_job_status(self, job_id: str) -> tuple[bool, Optional[dict]]:
        """查询 job 状态，返回 (success, job_info)"""
        try:
            resp = requests.get(
                f"{self.master}/cube/template/from-image",
                params={"job_id": job_id},
                timeout=10,
            )
            if resp.status_code != 200:
                return False, None
            data = resp.json()
            if data.get("ret", {}).get("retCode") != 0:
                return False, None
            return True, data.get("job")
        except Exception:
            return False, None

    def wait_for_build(self, job_id: str, timeout: int = 600) -> tuple[bool, float]:
        """等待构建完成（READY/FAILED），返回 (success, elapsed_seconds)"""
        start = time.time()
        while time.time() - start < timeout:
            ok, job = self.get_job_status(job_id)
            if not ok or job is None:
                time.sleep(2)
                continue
            status = job.get("status", "").upper()
            if status == "READY":
                return True, time.time() - start
            if status == "FAILED":
                self.log(f"  job {job_id} FAILED: {job.get('error_message', 'unknown')}")
                return False, time.time() - start
            time.sleep(2)
        return False, time.time() - start

    def list_templates(self) -> tuple[bool, float, Optional[list]]:
        """查询模板列表"""
        start = time.time()
        try:
            resp = requests.get(
                f"{self.master}/cube/template",
                params={"limit": 1000},
                timeout=10,
            )
            latency = time.time() - start
            if resp.status_code != 200:
                return False, latency, None
            data = resp.json()
            if data.get("ret", {}).get("retCode") != 0:
                return False, latency, None
            return True, latency, data.get("templates", [])
        except Exception:
            return False, time.time() - start, None

    def get_template(self, template_id: str) -> tuple[bool, float, Optional[dict]]:
        """查询单个模板详情"""
        start = time.time()
        try:
            resp = requests.get(
                f"{self.master}/cube/template/{template_id}",
                timeout=10,
            )
            latency = time.time() - start
            if resp.status_code != 200:
                return False, latency, None
            data = resp.json()
            if data.get("ret", {}).get("retCode") != 0:
                return False, latency, None
            return True, latency, data.get("template")
        except Exception:
            return False, time.time() - start, None

    def delete_template(self, template_id: str) -> tuple[bool, float]:
        """删除模板"""
        start = time.time()
        try:
            resp = requests.delete(
                f"{self.master}/cube/template/{template_id}",
                timeout=30,
            )
            latency = time.time() - start
            if resp.status_code != 200:
                return False, latency
            data = resp.json()
            if data.get("ret", {}).get("retCode") != 0:
                return False, latency
            return True, latency
        except Exception:
            return False, time.time() - start

    # ========== DB 直连验证 ==========

    def get_db_connection(self):
        return pymysql.connect(**self.db_config, cursorclass=pymysql.cursors.DictCursor)

    def query_db(self, sql: str, params: tuple = ()) -> list:
        conn = self.get_db_connection()
        try:
            with conn.cursor() as cur:
                cur.execute(sql, params)
                return cur.fetchall()
        finally:
            conn.close()

    def verify_db_consistency(self) -> dict:
        """验证 DB 一致性，返回统计结果"""
        result = {}

        # template_definitions
        rows = self.query_db(
            "SELECT template_id, status, kind FROM t_cube_template_definition WHERE template_id LIKE %s",
            (f"{self.prefix}-%",),
        )
        result["template_definitions"] = {
            "total": len(rows),
            "by_status": {},
            "by_kind": {},
        }
        for r in rows:
            result["template_definitions"]["by_status"][r["status"]] = (
                result["template_definitions"]["by_status"].get(r["status"], 0) + 1
            )
            result["template_definitions"]["by_kind"][r["kind"]] = (
                result["template_definitions"]["by_kind"].get(r["kind"], 0) + 1
            )

        # rootfs_artifacts
        rows = self.query_db(
            "SELECT artifact_id, status FROM t_template_image_rootfs_artifact WHERE artifact_id IN ("
            "  SELECT artifact_id FROM t_cube_template_replica WHERE template_id LIKE %s"
            ")",
            (f"{self.prefix}-%",),
        )
        result["rootfs_artifacts"] = {
            "total": len(rows),
            "by_status": {},
        }
        for r in rows:
            result["rootfs_artifacts"]["by_status"][r["status"]] = (
                result["rootfs_artifacts"]["by_status"].get(r["status"], 0) + 1
            )

        # image_jobs
        rows = self.query_db(
            "SELECT job_id, status, phase FROM t_template_image_job WHERE template_id LIKE %s",
            (f"{self.prefix}-%",),
        )
        result["image_jobs"] = {
            "total": len(rows),
            "by_status": {},
            "by_phase": {},
        }
        for r in rows:
            result["image_jobs"]["by_status"][r["status"]] = (
                result["image_jobs"]["by_status"].get(r["status"], 0) + 1
            )
            result["image_jobs"]["by_phase"][r["phase"]] = (
                result["image_jobs"]["by_phase"].get(r["phase"], 0) + 1
            )

        # template_replicas
        rows = self.query_db(
            "SELECT replica_id, status FROM t_cube_template_replica WHERE template_id LIKE %s",
            (f"{self.prefix}-%",),
        )
        result["template_replicas"] = {
            "total": len(rows),
            "by_status": {},
        }
        for r in rows:
            result["template_replicas"]["by_status"][r["status"]] = (
                result["template_replicas"]["by_status"].get(r["status"], 0) + 1
            )

        return result

    def verify_cache_consistency(self, sample_size: int = 20) -> dict:
        """验证缓存一致性：API 查询结果 vs DB 直连结果"""
        result = {
            "sample_size": 0,
            "api_success": 0,
            "db_match": 0,
            "db_mismatch": 0,
            "mismatches": [],
        }

        # 从 DB 查出所有压测模板
        db_templates = self.query_db(
            "SELECT template_id, status FROM t_cube_template_definition WHERE template_id LIKE %s LIMIT %s",
            (f"{self.prefix}-%", sample_size),
        )
        result["sample_size"] = len(db_templates)

        for row in db_templates:
            template_id = row["template_id"]
            db_status = row["status"]

            # 调 API 查询
            ok, _, api_template = self.get_template(template_id)
            if not ok or api_template is None:
                continue
            result["api_success"] += 1

            api_status = api_template.get("status", "").upper()
            if api_status == db_status.upper():
                result["db_match"] += 1
            else:
                result["db_mismatch"] += 1
                result["mismatches"].append({
                    "template_id": template_id,
                    "api_status": api_status,
                    "db_status": db_status,
                })

        return result

    # ========== 压测阶段 ==========

    def bench_create(self):
        """阶段 1：批量并发创建模板"""
        self.log(f"=== 阶段 1：批量创建模板（并发 {self.create_concurrency}，总数 {self.create_total}）===")
        stats = BenchStats()

        def create_one(i: int):
            template_id = f"{self.prefix}-tpl-{i:04d}"
            ok, latency, job_id = self.create_template(template_id)
            stats.add(ok, latency)
            if ok:
                self.created_template_ids.append(template_id)
                if job_id:
                    self.created_job_ids.append(job_id)
            return ok

        with ThreadPoolExecutor(max_workers=self.create_concurrency) as executor:
            futures = [executor.submit(create_one, i) for i in range(self.create_total)]
            for future in as_completed(futures):
                future.result()

        self.log(f"创建完成：{stats.summary()}")
        self.log(f"成功创建 {len(self.created_template_ids)} 个模板")
        return stats

    def bench_wait_build(self):
        """阶段 2：等待所有构建完成"""
        if self.skip_build_wait:
            self.log("=== 阶段 2：跳过构建等待 ===")
            return

        self.log(f"=== 阶段 2：等待构建完成（超时 {self.build_timeout}s）===")
        stats = BenchStats()

        def wait_one(job_id: str):
            ok, elapsed = self.wait_for_build(job_id, self.build_timeout)
            stats.add(ok, elapsed)
            return ok

        with ThreadPoolExecutor(max_workers=self.create_concurrency) as executor:
            futures = [executor.submit(wait_one, job_id) for job_id in self.created_job_ids]
            for future in as_completed(futures):
                future.result()

        self.log(f"构建完成：{stats.summary()}")
        self.log(f"成功构建 {stats.success} 个，失败 {stats.failed} 个")
        return stats

    def bench_query(self):
        """阶段 3：批量并发查询"""
        self.log(f"=== 阶段 3：批量查询（并发 {self.query_concurrency}，总数 {self.query_total}）===")
        stats = BenchStats()
        cache_hits = 0

        # 混合查询：70% 查详情，30% 查列表
        def query_one(i: int):
            nonlocal cache_hits
            if i % 10 < 7 and self.created_template_ids:
                # 查详情
                template_id = self.created_template_ids[i % len(self.created_template_ids)]
                ok, latency, _ = self.get_template(template_id)
            else:
                # 查列表
                ok, latency, _ = self.list_templates()
            stats.add(ok, latency)
            return ok

        with ThreadPoolExecutor(max_workers=self.query_concurrency) as executor:
            futures = [executor.submit(query_one, i) for i in range(self.query_total)]
            for future in as_completed(futures):
                future.result()

        self.log(f"查询完成：{stats.summary()}")
        return stats

    def bench_delete(self):
        """阶段 4：批量并发删除"""
        self.log(f"=== 阶段 4：批量删除（并发 {self.delete_concurrency}）===")
        stats = BenchStats()

        def delete_one(template_id: str):
            ok, latency = self.delete_template(template_id)
            stats.add(ok, latency)
            return ok

        with ThreadPoolExecutor(max_workers=self.delete_concurrency) as executor:
            futures = [executor.submit(delete_one, tid) for tid in self.created_template_ids]
            for future in as_completed(futures):
                future.result()

        self.log(f"删除完成：{stats.summary()}")
        return stats

    def bench_verify(self):
        """阶段 5：验证 DB 和缓存一致性"""
        self.log("=== 阶段 5：验证 DB 一致性 ===")
        db_result = self.verify_db_consistency()
        print(json.dumps(db_result, indent=2, ensure_ascii=False))

        self.log("=== 阶段 6：验证缓存一致性 ===")
        cache_result = self.verify_cache_consistency()
        print(json.dumps(cache_result, indent=2, ensure_ascii=False))

        return {"db": db_result, "cache": cache_result}

    def run(self):
        """执行完整压测流程"""
        self.log(f"压测前缀: {self.prefix}")
        self.log(f"Master: {self.master}")
        self.log(f"镜像: {self.image}")

        report = {}

        if not self.skip_create:
            report["create"] = self.bench_create()
            report["build"] = self.bench_wait_build()
        else:
            self.log("跳过创建阶段")

        if not self.skip_query:
            report["query"] = self.bench_query()
        else:
            self.log("跳过查询阶段")

        if not self.skip_delete:
            report["delete"] = self.bench_delete()
        else:
            self.log("跳过删除阶段")

        report["verify"] = self.bench_verify()

        self.log("=== 压测完成 ===")
        return report


def main():
    parser = argparse.ArgumentParser(description="CubeSandbox 模板压测脚本")
    parser.add_argument("--master", default="http://127.0.0.1:8089", help="CubeMaster 地址")
    parser.add_argument("--db-host", default="127.0.0.1", help="MySQL 地址")
    parser.add_argument("--db-port", type=int, default=3306, help="MySQL 端口")
    parser.add_argument("--db-user", default="cube", help="MySQL 用户")
    parser.add_argument("--db-pass", default="cube_pass", help="MySQL 密码")
    parser.add_argument("--db-name", default="cube_mvp", help="MySQL 数据库名")

    parser.add_argument("--image", required=True, help="模板镜像引用")
    parser.add_argument("--writable-layer-size", default="1G", help="可写层大小")
    parser.add_argument("--instance-type", default="cubebox", help="实例类型")
    parser.add_argument("--network-type", default="tap", help="网络类型")
    parser.add_argument("--expose-ports", type=int, nargs="+", help="暴露端口列表")
    parser.add_argument("--probe-port", type=int, help="探针端口")
    parser.add_argument("--probe-path", help="探针路径")

    parser.add_argument("--create-concurrency", type=int, default=10, help="创建并发数")
    parser.add_argument("--create-total", type=int, default=20, help="创建总数")
    parser.add_argument("--query-concurrency", type=int, default=50, help="查询并发数")
    parser.add_argument("--query-total", type=int, default=200, help="查询总数")
    parser.add_argument("--delete-concurrency", type=int, default=10, help="删除并发数")
    parser.add_argument("--build-timeout", type=int, default=600, help="单个构建超时（秒）")

    parser.add_argument("--skip-create", action="store_true", help="跳过创建阶段")
    parser.add_argument("--skip-build-wait", action="store_true", help="跳过构建等待")
    parser.add_argument("--skip-query", action="store_true", help="跳过查询阶段")
    parser.add_argument("--skip-delete", action="store_true", help="跳过删除阶段")
    parser.add_argument("--prefix", help="压测模板前缀（默认自动生成）")

    args = parser.parse_args()

    bench = TemplateBench(args)
    bench.run()


if __name__ == "__main__":
    main()
