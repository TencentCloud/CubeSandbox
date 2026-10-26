#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""
Host-side ivshmem mmap benchmark.

This script measures host access to `/dev/shm/ivshmem-{sandbox_id}`.
It does not, by itself, prove guest communication.
"""

from __future__ import annotations

import argparse
import mmap
import os
import sys
import time
from concurrent.futures import ThreadPoolExecutor, as_completed

from cubesandbox import Sandbox


IVSHMEM_SIZE = 1024 * 1024
DEFAULT_SHM_WAIT_TIMEOUT = 60


class IvshmemBenchmark:
    def __init__(self, sandbox_id: str, ivshmem_path: str, iterations: int = 10000):
        self.sandbox_id = sandbox_id
        self.ivshmem_path = ivshmem_path
        self.iterations = iterations

    def run_all_tests(self) -> dict:
        if not os.path.exists(self.ivshmem_path):
            return {"error": f"ivshmem file not found: {self.ivshmem_path}"}

        with open(self.ivshmem_path, "r+b", buffering=0) as f, mmap.mmap(f.fileno(), IVSHMEM_SIZE) as mm:
            return {
                "single_byte": self._test_single_byte(mm),
                "block_100b": self._test_block(mm, 100, self.iterations),
                "block_1kb": self._test_block(mm, 1024, self.iterations),
                "block_100kb": self._test_block(mm, 100 * 1024, min(self.iterations, 1000)),
            }

    def _test_single_byte(self, mm: mmap.mmap) -> dict:
        start = time.perf_counter()
        for i in range(self.iterations):
            mm[i % IVSHMEM_SIZE] = 65
        elapsed = time.perf_counter() - start
        return {
            "latency_us": round((elapsed / self.iterations) * 1_000_000, 3),
            "ops_per_sec": int(self.iterations / elapsed),
        }

    def _test_block(self, mm: mmap.mmap, block_size: int, iterations: int) -> dict:
        block = b"X" * block_size
        start = time.perf_counter()
        for i in range(iterations):
            offset = (i * block_size) % (IVSHMEM_SIZE - block_size)
            mm[offset : offset + block_size] = block
        elapsed = time.perf_counter() - start
        return {
            "block_size": block_size,
            "latency_us": round((elapsed / iterations) * 1_000_000, 3),
            "throughput_mb": round((block_size * iterations) / elapsed / (1024 * 1024), 2),
        }


def wait_for_shm_file(sandbox_id: str, timeout: int = DEFAULT_SHM_WAIT_TIMEOUT) -> str:
    path = f"/dev/shm/ivshmem-{sandbox_id}"
    deadline = time.time() + timeout
    while time.time() < deadline:
        if os.path.exists(path):
            return path
        time.sleep(1)
    raise FileNotFoundError(f"ivshmem file not created: {path}")


def create_sandbox(template_id: str) -> tuple[str, str]:
    sandbox = Sandbox.create(template=template_id)
    try:
        path = wait_for_shm_file(sandbox.sandbox_id)
        return sandbox.sandbox_id, path
    except Exception:
        sandbox.kill()
        raise


def cleanup_sandbox(sandbox_id: str) -> None:
    try:
        Sandbox.connect(sandbox_id).kill()
    except Exception as exc:  # noqa: BLE001
        print(f"cleanup failed for {sandbox_id}: {exc}", file=sys.stderr)


def run_benchmark_single(sandbox_id: str, ivshmem_path: str, iterations: int) -> tuple[str, dict]:
    benchmark = IvshmemBenchmark(sandbox_id, ivshmem_path, iterations)
    return sandbox_id, benchmark.run_all_tests()


def print_results(sandbox_id: str, results: dict) -> None:
    print(f"\n{'=' * 70}")
    print(f"Sandbox: {sandbox_id}")
    print("=" * 70)

    if "error" in results:
        print(f"ERROR: {results['error']}")
        return

    print("\nSingle-byte write:")
    print(f"  Latency: {results['single_byte']['latency_us']} us/op")
    print(f"  Throughput: {results['single_byte']['ops_per_sec']:,} ops/s")

    for key, result in results.items():
        if not key.startswith("block_"):
            continue
        size = result["block_size"]
        size_str = f"{size}B" if size < 1024 else f"{size // 1024}KB"
        print(f"\n{size_str} block write:")
        print(f"  Latency: {result['latency_us']} us/op")
        print(f"  Throughput: {result['throughput_mb']} MB/s")


def print_summary(all_results: list[tuple[str, dict]]) -> None:
    success = [r for _, r in all_results if "error" not in r]
    if not success:
        return

    avg_latency = sum(r["single_byte"]["latency_us"] for r in success) / len(success)
    avg_1kb = sum(r["block_1kb"]["throughput_mb"] for r in success) / len(success)
    avg_100kb = sum(r["block_100kb"]["throughput_mb"] for r in success) / len(success)

    print(f"\n{'=' * 70}")
    print("Summary")
    print("=" * 70)
    print(f"Sandboxes tested: {len(all_results)}")
    print(f"Successful runs: {len(success)}")
    print(f"Average single-byte latency: {avg_latency:.3f} us")
    print(f"Average 1KB throughput: {avg_1kb:.2f} MB/s")
    print(f"Average 100KB throughput: {avg_100kb:.2f} MB/s")


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Host-side ivshmem mmap benchmark")
    parser.add_argument("--sandbox-id", help="Benchmark an existing sandbox")
    parser.add_argument("--template", help="ivshmem-enabled template ID used to create temporary sandboxes")
    parser.add_argument("--count", type=int, default=1, help="Number of sandboxes to benchmark")
    parser.add_argument("--iterations", type=int, default=10000, help="Benchmark iterations")
    parser.add_argument("--parallel", action="store_true", help="Run multiple benchmarks in parallel")
    parser.add_argument("--cleanup", action="store_true", help="Delete temporary sandboxes after the run")
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    all_results: list[tuple[str, dict]] = []
    sandboxes_to_cleanup: list[str] = []

    try:
        if args.sandbox_id:
            path = f"/dev/shm/ivshmem-{args.sandbox_id}"
            sandbox_id, results = run_benchmark_single(args.sandbox_id, path, args.iterations)
            all_results.append((sandbox_id, results))
            print_results(sandbox_id, results)
            return 0

        if not args.template:
            raise SystemExit("--template is required unless --sandbox-id is provided")

        sandboxes = []
        print(f"Creating {args.count} sandbox(es) from template {args.template}...")
        for index in range(args.count):
            sandbox_id, path = create_sandbox(args.template)
            sandboxes.append((sandbox_id, path))
            sandboxes_to_cleanup.append(sandbox_id)
            print(f"  [{index + 1}/{args.count}] {sandbox_id}")

        if args.parallel and len(sandboxes) > 1:
            with ThreadPoolExecutor(max_workers=len(sandboxes)) as executor:
                futures = {
                    executor.submit(run_benchmark_single, sandbox_id, path, args.iterations): sandbox_id
                    for sandbox_id, path in sandboxes
                }
                for future in as_completed(futures):
                    sandbox_id, results = future.result()
                    all_results.append((sandbox_id, results))
                    print_results(sandbox_id, results)
        else:
            for sandbox_id, path in sandboxes:
                sandbox_id, results = run_benchmark_single(sandbox_id, path, args.iterations)
                all_results.append((sandbox_id, results))
                print_results(sandbox_id, results)

        if len(all_results) > 1:
            print_summary(all_results)
        return 0
    finally:
        if args.cleanup:
            for sandbox_id in sandboxes_to_cleanup:
                cleanup_sandbox(sandbox_id)


if __name__ == "__main__":
    raise SystemExit(main())
