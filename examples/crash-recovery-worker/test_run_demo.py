# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

import copy
import random
import subprocess
import sys
import unittest
from unittest.mock import patch

import run_demo
from run_demo import (
    cleanup,
    parse_audit,
    verify_fault,
    verify_state,
    worker_session,
    worker_url,
)


def stable_state():
    transfer = {"id": "tx-001", "from": "alice", "to": "bob", "amount": 100}

    return {
        "initial_balances": {"alice": 1_000, "bob": 500, "carol": 250},
        "balances": {"alice": 900, "bob": 600, "carol": 250},
        "pending": {},
        "ledger": [transfer],
        "seen": ["tx-001"],
        "stats": {"committed": 1, "duplicates": 0, "faults": 0},
    }


def initial_state():
    balances = {"alice": 1_000, "bob": 500, "carol": 250}

    return {
        "initial_balances": balances.copy(),
        "balances": balances.copy(),
        "pending": {},
        "ledger": [],
        "seen": [],
        "stats": {"committed": 0, "duplicates": 0, "faults": 0},
    }


def faulted_transfer():
    return {"id": "tx-crash", "from": "bob", "to": "carol", "amount": 75}


def stable_audit():
    transfer = {"id": "tx-001", "from": "alice", "to": "bob", "amount": 100}

    return "\n".join(
        [
            '{"kind":"initialized","balances":{"alice":1000,"bob":500,"carol":250}}',
            '{"kind":"started","transfer":' + compact(transfer) + "}",
            '{"kind":"committed","transfer":' + compact(transfer) + "}",
            "",
        ]
    )


def compact(value):
    import json

    return json.dumps(value, separators=(",", ":"))


class StateVerificationTest(unittest.TestCase):
    def test_matching_memory_and_audit_are_consistent(self):
        events = parse_audit(stable_audit())

        verify_state(stable_state(), events)

    def test_balance_corruption_is_detected(self):
        state = copy.deepcopy(stable_state())
        state["balances"]["alice"] -= 10
        events = parse_audit(stable_audit())

        with self.assertRaisesRegex(AssertionError, "total balance"):
            verify_state(state, events)

    def test_reference_model_mismatch_is_detected(self):
        verify_expected_state = getattr(run_demo, "verify_expected_state", None)
        self.assertTrue(callable(verify_expected_state))

        actual = stable_state()
        expected = copy.deepcopy(actual)
        expected["balances"]["alice"] -= 1

        with self.assertRaisesRegex(AssertionError, "reference model"):
            verify_expected_state(actual, expected)

    def test_live_verification_reads_memory_and_the_workspace_audit(self):
        read_verified_state = getattr(run_demo, "read_verified_state", None)
        self.assertTrue(callable(read_verified_state))

        class Response:
            def raise_for_status(self):
                pass

            def json(self):
                return stable_state()

        class Session:
            def get(self, url, timeout):
                self.request = (url, timeout)

                return Response()

        class Files:
            def read(self, path, user):
                self.request = (path, user)

                return stable_audit()

        class Sandbox:
            files = Files()

        (sandbox, session, expected) = (Sandbox(), Session(), stable_state())
        (state, audit) = read_verified_state(
            sandbox,
            session,
            "http://worker",
            expected,
        )

        self.assertEqual(state, expected)
        self.assertEqual(audit, stable_audit())
        self.assertEqual(session.request, ("http://worker/state", 5))
        self.assertEqual(
            sandbox.files.request,
            (run_demo.AUDIT_PATH, "root"),
        )


class WorkloadTest(unittest.TestCase):
    def test_seeded_transfers_are_reproducible_and_always_legal(self):
        generate_transfer = getattr(run_demo, "generate_transfer", None)
        apply_commit = getattr(run_demo, "apply_commit", None)
        self.assertTrue(callable(generate_transfer))
        self.assertTrue(callable(apply_commit))

        def generate(seed):
            (rng, state, transfers) = (random.Random(seed), initial_state(), [])
            for index in range(40):
                transfer = generate_transfer(
                    rng,
                    state["balances"],
                    f"tx-{index + 1:04d}",
                )

                self.assertNotEqual(transfer["from"], transfer["to"])
                self.assertGreater(transfer["amount"], 0)
                self.assertLessEqual(
                    transfer["amount"],
                    state["balances"][transfer["from"]],
                )

                apply_commit(state, transfer)
                transfers.append(transfer)

            return transfers, state

        (first_transfers, first_state) = generate(20260717)
        (second_transfers, second_state) = generate(20260717)

        self.assertEqual(first_transfers, second_transfers)
        self.assertEqual(first_state, second_state)
        self.assertEqual(sum(first_state["balances"].values()), 1_750)
        self.assertEqual(first_state["stats"]["committed"], 40)

    def test_duplicate_only_updates_the_duplicate_counter(self):
        apply_duplicate = getattr(run_demo, "apply_duplicate", None)
        self.assertTrue(callable(apply_duplicate))

        (state, before) = (stable_state(), stable_state())
        apply_duplicate(state, state["ledger"][0])

        self.assertEqual(state["balances"], before["balances"])
        self.assertEqual(state["ledger"], before["ledger"])
        self.assertEqual(state["seen"], before["seen"])
        self.assertEqual(state["stats"]["committed"], 1)
        self.assertEqual(state["stats"]["duplicates"], 1)

    def test_three_epochs_can_rollback_and_replay_the_planned_workload(self):
        plan_epoch = getattr(run_demo, "plan_epoch", None)
        apply_commit = getattr(run_demo, "apply_commit", None)
        self.assertTrue(callable(plan_epoch))
        self.assertTrue(callable(apply_commit))

        (rng, state, sequence, request_ids) = (
            random.Random(20260717),
            initial_state(),
            1,
            set(),
        )
        for epoch in range(1, 4):
            (plan, sequence) = plan_epoch(
                rng,
                state,
                epoch=epoch,
                sequence=sequence,
                before_checkpoint=4,
                after_checkpoint=2,
            )

            self.assertEqual(len(plan.before_checkpoint), 4)
            self.assertEqual(len(plan.after_checkpoint), 2)

            for transfer in plan.before_checkpoint:
                apply_commit(state, transfer)

            checkpoint = copy.deepcopy(state)
            for transfer in plan.after_checkpoint:
                apply_commit(state, transfer)

            self.assertLessEqual(
                plan.faulted["amount"],
                state["balances"][plan.faulted["from"]],
            )

            state = checkpoint
            for transfer in [*plan.after_checkpoint, plan.faulted]:
                apply_commit(state, transfer)

            epoch_ids = {
                transfer["id"]
                for transfer in [
                    *plan.before_checkpoint,
                    *plan.after_checkpoint,
                    plan.faulted,
                ]
            }
            self.assertEqual(len(epoch_ids), 7)
            self.assertTrue(request_ids.isdisjoint(epoch_ids))
            request_ids.update(epoch_ids)

        self.assertEqual(len(state["ledger"]), 21)
        self.assertEqual(state["stats"]["committed"], 21)
        self.assertEqual(sum(state["balances"].values()), 1_750)

    def test_commit_updates_the_reference_model_and_verifies_both_state_copies(self):
        commit_and_verify = getattr(run_demo, "commit_and_verify", None)
        self.assertTrue(callable(commit_and_verify))

        transfer = {"id": "tx-001", "from": "alice", "to": "bob", "amount": 100}

        class Response:
            status_code = 201

            def __init__(self, body):
                self.body = body

            def raise_for_status(self):
                pass

            def json(self):
                return self.body

        class Session:
            def post(self, url, json, headers, timeout):
                self.post_request = (url, json, headers, timeout)

                return Response({"outcome": "committed"})

            def get(self, url, timeout):
                self.get_request = (url, timeout)

                return Response(stable_state())

        class Files:
            def read(self, path, user):
                return stable_audit()

        class Sandbox:
            files = Files()

        (sandbox, session, expected) = (Sandbox(), Session(), initial_state())
        (state, audit) = commit_and_verify(
            sandbox,
            session,
            "http://worker",
            expected,
            transfer,
        )

        self.assertEqual(state, stable_state())
        self.assertEqual(expected, stable_state())
        self.assertEqual(audit, stable_audit())
        self.assertEqual(
            session.post_request,
            ("http://worker/transfers", transfer, None, 10),
        )


class ArgumentTest(unittest.TestCase):
    def test_long_running_workload_defaults_and_overrides(self):
        parse_args = getattr(run_demo, "parse_args", None)
        self.assertTrue(callable(parse_args))

        defaults = parse_args([])
        self.assertEqual(
            (
                defaults.cycles,
                defaults.transfers_before_checkpoint,
                defaults.transfers_after_checkpoint,
                defaults.seed,
            ),
            (3, 4, 2, 20260717),
        )

        configured = parse_args(
            [
                "--cycles",
                "5",
                "--transfers-before-checkpoint",
                "8",
                "--transfers-after-checkpoint",
                "3",
                "--seed",
                "42",
            ]
        )
        self.assertEqual(
            (
                configured.cycles,
                configured.transfers_before_checkpoint,
                configured.transfers_after_checkpoint,
                configured.seed,
            ),
            (5, 8, 3, 42),
        )


class FaultVerificationTest(unittest.TestCase):
    def test_unmatched_started_event_proves_the_injected_fault(self):
        events = parse_audit(
            stable_audit()
            + '{"kind":"started","transfer":{"id":"tx-crash","from":"bob","to":"carol","amount":75}}\n'
            + '{"kind":"fault_injected","id":"tx-crash","point":"after_debit","balance_total":1675}\n'
        )

        verify_fault(
            events,
            faulted_transfer(),
            expected_state=stable_state(),
        )

    def test_committed_fault_transfer_is_not_valid_fault_evidence(self):
        events = parse_audit(
            stable_audit()
            + '{"kind":"started","transfer":{"id":"tx-crash","from":"bob","to":"carol","amount":75}}\n'
            + '{"kind":"committed","transfer":{"id":"tx-crash","from":"bob","to":"carol","amount":75}}\n'
        )

        with self.assertRaisesRegex(AssertionError, "must not be committed"):
            verify_fault(
                events,
                faulted_transfer(),
                expected_state=stable_state(),
            )

    def test_fault_audit_must_preserve_the_full_committed_history(self):
        events = parse_audit(
            stable_audit().replace('"amount":100', '"amount":99')
            + '{"kind":"started","transfer":{"id":"tx-crash","from":"bob","to":"carol","amount":75}}\n'
            + '{"kind":"fault_injected","id":"tx-crash","point":"after_debit","balance_total":1675}\n'
        )

        with self.assertRaisesRegex(AssertionError, "committed history"):
            verify_fault(
                events,
                faulted_transfer(),
                expected_state=stable_state(),
            )


class CleanupTest(unittest.TestCase):
    def test_sandbox_is_released_before_all_snapshots_are_deleted(self):
        events = []

        class Sandbox:
            def kill(self):
                events.append("kill")

            def close(self):
                events.append("close")

        def delete_snapshot(snapshot_id):
            events.append(f"delete:{snapshot_id}")

        cleanup(Sandbox(), ["snap-001", "snap-002"], delete_snapshot)

        self.assertEqual(
            events,
            ["kill", "close", "delete:snap-001", "delete:snap-002"],
        )

    def test_close_failure_does_not_skip_snapshot_cleanup(self):
        events = []

        class Sandbox:
            sandbox_id = "sandbox-001"

            def kill(self):
                events.append("kill")

            def close(self):
                events.append("close")
                raise RuntimeError("close failed")

        def delete_snapshot(snapshot_id):
            events.append(f"delete:{snapshot_id}")

        with patch("builtins.print"):
            cleanup(Sandbox(), ["snap-001"], delete_snapshot)

        self.assertEqual(events, ["kill", "close", "delete:snap-001"])


class SessionTest(unittest.TestCase):
    def test_private_sandbox_token_is_attached_to_worker_requests(self):
        class Sandbox:
            traffic_access_token = "traffic-token"

        with patch.dict("os.environ", {}, clear=True):
            session = worker_session(Sandbox())
            self.addCleanup(session.close)

            self.assertEqual(session.headers["accept-encoding"], "identity")
            self.assertEqual(
                session.headers["e2b-traffic-access-token"],
                "traffic-token",
            )

    def test_proxy_ip_uses_direct_http_with_the_virtual_worker_host(self):
        class Sandbox:
            traffic_access_token = "traffic-token"

            def get_host(self, port):
                return f"{port}-sandbox.cube.app"

        with patch.dict(
            "os.environ",
            {
                "CUBE_PROXY_NODE_IP": "10.0.0.8",
                "CUBE_PROXY_PORT_HTTP": "18080",
                "CUBE_WORKER_SCHEME": "https",
            },
        ):
            sandbox = Sandbox()
            session = worker_session(sandbox)
            self.addCleanup(session.close)

            self.assertEqual(worker_url(sandbox), "http://10.0.0.8:18080")
            self.assertEqual(session.headers["Host"], "8080-sandbox.cube.app")


class OptimizedModeTest(unittest.TestCase):
    def test_consistency_checks_are_not_removed_by_python_optimized_mode(self):
        script = """
from run_demo import verify_state

state = {
    "initial_balances": {"alice": 10},
    "balances": {"alice": 9},
    "pending": {},
    "ledger": [],
    "seen": [],
    "stats": {"committed": 0, "duplicates": 0, "faults": 0},
}
verify_state(state, [])
"""
        result = subprocess.run(
            [sys.executable, "-O", "-c", script],
            capture_output=True,
            text=True,
            check=False,
        )

        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)


if __name__ == "__main__":
    unittest.main()
