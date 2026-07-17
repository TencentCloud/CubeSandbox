# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import argparse
import copy
import hashlib
import json
import os
import random
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Any


AUDIT_PATH = "/workspace/crash-recovery/audit.jsonl"
LOG_PATH = "/workspace/crash-recovery/worker.log"
PID_PATH = "/workspace/crash-recovery/worker.pid"
WORKER_PORT = 8080
DEFAULT_CYCLES = 3
DEFAULT_TRANSFERS_BEFORE_CHECKPOINT = 4
DEFAULT_TRANSFERS_AFTER_CHECKPOINT = 2
DEFAULT_SEED = 20260717
MAX_TRANSFER_AMOUNT = 100


@dataclass(frozen=True)
class EpochPlan:
    before_checkpoint: tuple[dict[str, Any], ...]
    after_checkpoint: tuple[dict[str, Any], ...]
    faulted: dict[str, Any]


def require(condition: bool, message: str) -> None:
    if not condition:
        raise AssertionError(message)


def positive_int(value: str) -> int:
    parsed = int(value)
    if parsed <= 0:
        raise argparse.ArgumentTypeError("value must be greater than zero")

    return parsed


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Run a repeatable multi-checkpoint crash-recovery workload."
    )
    parser.add_argument("--cycles", type=positive_int, default=DEFAULT_CYCLES)
    parser.add_argument(
        "--transfers-before-checkpoint",
        type=positive_int,
        default=DEFAULT_TRANSFERS_BEFORE_CHECKPOINT,
    )
    parser.add_argument(
        "--transfers-after-checkpoint",
        type=positive_int,
        default=DEFAULT_TRANSFERS_AFTER_CHECKPOINT,
    )
    parser.add_argument("--seed", type=int, default=DEFAULT_SEED)

    return parser.parse_args(argv)


def parse_audit(content: str) -> list[dict[str, Any]]:
    return [json.loads(line) for line in content.splitlines() if line.strip()]


def verify_expected_state(
    actual: dict[str, Any], expected: dict[str, Any]
) -> None:
    require(
        actual == expected,
        "worker state does not match the reference model: "
        f"actual={digest(actual)} expected={digest(expected)}",
    )


def generate_transfer(
    rng: random.Random,
    balances: dict[str, int],
    transfer_id: str,
) -> dict[str, Any]:
    sources = sorted(account for account, balance in balances.items() if balance > 0)
    require(sources, "no account has funds available for a transfer")

    source = rng.choice(sources)
    destinations = sorted(account for account in balances if account != source)
    require(destinations, "a transfer requires at least two accounts")

    destination = rng.choice(destinations)
    amount = rng.randint(1, min(balances[source], MAX_TRANSFER_AMOUNT))

    return {
        "id": transfer_id,
        "from": source,
        "to": destination,
        "amount": amount,
    }


def apply_commit(state: dict[str, Any], transfer: dict[str, Any]) -> None:
    (transfer_id, source, destination, amount) = (
        transfer["id"],
        transfer["from"],
        transfer["to"],
        transfer["amount"],
    )
    balances = state["balances"]

    require(transfer_id not in state["seen"], "reference model reused a request ID")
    require(source in balances, "reference model source account is missing")
    require(destination in balances, "reference model destination account is missing")
    require(source != destination, "reference model transfer uses the same account")
    require(amount > 0, "reference model transfer amount is not positive")
    require(
        balances[source] >= amount,
        "reference model transfer exceeds the source balance",
    )

    balances[source] -= amount
    balances[destination] += amount
    state["ledger"].append(copy.deepcopy(transfer))
    state["seen"] = sorted([*state["seen"], transfer_id])
    state["stats"]["committed"] += 1


def apply_duplicate(state: dict[str, Any], transfer: dict[str, Any]) -> None:
    committed = next(
        (item for item in state["ledger"] if item["id"] == transfer["id"]),
        None,
    )
    require(committed == transfer, "reference model duplicate has no matching commit")

    state["stats"]["duplicates"] += 1


def plan_epoch(
    rng: random.Random,
    state: dict[str, Any],
    *,
    epoch: int,
    sequence: int,
    before_checkpoint: int,
    after_checkpoint: int,
) -> tuple[EpochPlan, int]:
    require(epoch > 0, "epoch must be greater than zero")
    require(sequence > 0, "transfer sequence must be greater than zero")
    require(before_checkpoint > 0, "checkpoint workload must not be empty")
    require(after_checkpoint > 0, "replay workload must not be empty")

    (planned, before, after) = (copy.deepcopy(state), [], [])
    for _ in range(before_checkpoint):
        transfer = generate_transfer(
            rng,
            planned["balances"],
            f"tx-{sequence:04d}",
        )
        sequence += 1
        apply_commit(planned, transfer)
        before.append(transfer)

    for _ in range(after_checkpoint):
        transfer = generate_transfer(
            rng,
            planned["balances"],
            f"tx-{sequence:04d}",
        )
        sequence += 1
        apply_commit(planned, transfer)
        after.append(transfer)

    faulted = generate_transfer(
        rng,
        planned["balances"],
        f"tx-fault-{epoch:02d}",
    )

    return EpochPlan(tuple(before), tuple(after), faulted), sequence


def verify_state(state: dict[str, Any], events: list[dict[str, Any]]) -> None:
    initial = state["initial_balances"]
    balances = state["balances"]
    ledger = state["ledger"]
    pending = state["pending"]
    seen = set(state["seen"])
    stats = state["stats"]

    require(set(initial) == set(balances), "account sets differ")
    require(sum(balances.values()) == sum(initial.values()), "total balance changed")

    replayed = initial.copy()
    ledger_ids = []
    for transfer in ledger:
        transfer_id = transfer["id"]
        source = transfer["from"]
        destination = transfer["to"]
        amount = transfer["amount"]

        require(
            source in replayed and destination in replayed,
            "ledger references unknown account",
        )
        require(amount > 0, "ledger contains non-positive amount")

        replayed[source] -= amount
        replayed[destination] += amount
        ledger_ids.append(transfer_id)

    require(replayed == balances, "ledger replay does not match balances")
    require(
        len(ledger_ids) == len(set(ledger_ids)),
        "ledger contains duplicate request IDs",
    )
    require(seen == set(ledger_ids), "seen request IDs do not match ledger")
    require(not pending, "stable state still contains pending transfers")
    require(
        set(pending).isdisjoint(seen),
        "pending and committed transfers overlap",
    )
    require(
        stats["committed"] == len(ledger),
        "committed count does not match ledger",
    )

    initialized = [event for event in events if event["kind"] == "initialized"]
    started = [event["transfer"] for event in events if event["kind"] == "started"]
    committed = [event["transfer"] for event in events if event["kind"] == "committed"]
    duplicates = [event["id"] for event in events if event["kind"] == "duplicate"]
    faults = [event for event in events if event["kind"] == "fault_injected"]

    require(len(initialized) == 1, "audit must contain one initialized event")
    require(
        initialized[0]["balances"] == initial,
        "audit initial balances do not match memory",
    )
    require(started == committed, "audit contains an incomplete transfer")
    require(committed == ledger, "audit committed transfers do not match ledger")
    require(not faults, "stable audit contains a fault event")
    require(
        set(duplicates).issubset(seen),
        "duplicate audit event references unknown request ID",
    )
    require(
        stats["duplicates"] == len(duplicates),
        "duplicate count does not match audit",
    )
    require(
        stats["faults"] == len(faults),
        "fault count does not match audit",
    )


def verify_fault(
    events: list[dict[str, Any]],
    transfer: dict[str, Any],
    *,
    expected_state: dict[str, Any],
) -> None:
    transfer_id = transfer["id"]
    started = [
        event
        for event in events
        if event["kind"] == "started" and event["transfer"]["id"] == transfer_id
    ]
    committed = [
        event
        for event in events
        if event["kind"] == "committed" and event["transfer"]["id"] == transfer_id
    ]
    faults = [
        event
        for event in events
        if event["kind"] == "fault_injected" and event["id"] == transfer_id
    ]
    committed_history = [
        event["transfer"] for event in events if event["kind"] == "committed"
    ]
    started_history = [
        event["transfer"] for event in events if event["kind"] == "started"
    ]

    require(len(started) == 1, "faulted transfer must have one started event")
    require(not committed, "faulted transfer must not be committed")
    require(
        committed_history == expected_state["ledger"],
        "fault audit committed history does not match the reference model",
    )
    require(
        started_history == [*expected_state["ledger"], transfer],
        "fault audit started history does not end with the faulted transfer",
    )
    require(len(faults) == 1, "faulted transfer must have one fault event")
    require(faults[0]["point"] == "after_debit", "unexpected fault point")
    expected_total = sum(expected_state["initial_balances"].values())
    require(
        faults[0]["balance_total"] == expected_total - transfer["amount"],
        "fault audit contains an unexpected balance total",
    )


def digest(value: Any) -> str:
    encoded = json.dumps(value, sort_keys=True, separators=(",", ":")).encode()

    return hashlib.sha256(encoded).hexdigest()[:16]


def wait_ready(session, url: str, *, timeout: float = 30.0) -> None:
    import requests

    deadline = time.monotonic() + timeout
    last_error: Exception | None = None
    while time.monotonic() < deadline:
        try:
            response = session.get(f"{url}/health", timeout=2)
            if response.status_code == 200 and response.json().get("status") == "ok":
                return
            last_error = RuntimeError(
                f"unexpected health response {response.status_code}: {response.text[:160]}"
            )
        except requests.RequestException as error:
            last_error = error

        time.sleep(0.5)

    raise TimeoutError(f"worker did not become ready: {last_error}")


def wait_unavailable(session, url: str, *, timeout: float = 10.0) -> None:
    import requests

    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            response = session.get(f"{url}/health", timeout=1)
            if response.status_code != 200:
                return
        except requests.RequestException:
            return

        time.sleep(0.25)

    raise TimeoutError("worker remained reachable after fault injection")


def read_state(session, url: str) -> dict[str, Any]:
    response = session.get(f"{url}/state", timeout=5)
    response.raise_for_status()

    return response.json()


def read_verified_state(
    sandbox,
    session,
    url: str,
    expected: dict[str, Any],
) -> tuple[dict[str, Any], str]:
    state = read_state(session, url)
    audit = sandbox.files.read(AUDIT_PATH, user="root")
    events = parse_audit(audit)

    verify_state(state, events)
    verify_expected_state(state, expected)

    return state, audit


def submit(session, url: str, transfer: dict[str, Any], *, fault: bool = False):
    headers = {"x-fault-point": "after_debit"} if fault else None

    return session.post(f"{url}/transfers", json=transfer, headers=headers, timeout=10)


def commit_and_verify(
    sandbox,
    session,
    url: str,
    expected: dict[str, Any],
    transfer: dict[str, Any],
) -> tuple[dict[str, Any], str]:
    response = submit(session, url, transfer)
    response.raise_for_status()
    require(
        response.json()["outcome"] == "committed",
        f"transfer {transfer['id']} was not committed",
    )

    apply_commit(expected, transfer)

    return read_verified_state(sandbox, session, url, expected)


def commit_batch(
    sandbox,
    session,
    url: str,
    expected: dict[str, Any],
    transfers: tuple[dict[str, Any], ...],
    *,
    epoch: int,
    cycles: int,
    phase: str,
) -> tuple[dict[str, Any], str]:
    require(transfers, f"{phase} transfer batch must not be empty")

    (state, audit) = ({}, "")
    for index, transfer in enumerate(transfers, start=1):
        (state, audit) = commit_and_verify(
            sandbox,
            session,
            url,
            expected,
            transfer,
        )
        print(
            f"[epoch {epoch}/{cycles}] {phase} transfer "
            f"{index}/{len(transfers)} verified: id={transfer['id']} "
            f"ledger={len(state['ledger'])} state={digest(state)}"
        )

    return state, audit


def start_worker(sandbox) -> None:
    command = f"""
        set -eu
        install -d {Path(AUDIT_PATH).parent}
        nohup env \
            WORKER_ADDR=0.0.0.0:{WORKER_PORT} \
            AUDIT_PATH={AUDIT_PATH} \
            /usr/local/bin/crash-recovery-worker \
            >{LOG_PATH} 2>&1 </dev/null &
        echo $! >{PID_PATH}
    """
    result = sandbox.commands.run(command, timeout=15, user="root")
    if result.exit_code != 0:
        raise RuntimeError(
            f"failed to start worker: exit={result.exit_code}\n{result.stderr}"
        )


def worker_url(sandbox) -> str:
    proxy_ip = os.environ.get("CUBE_PROXY_NODE_IP")
    if proxy_ip:
        proxy_port = int(os.environ.get("CUBE_PROXY_PORT_HTTP", "80"))

        return f"http://{proxy_ip}:{proxy_port}"

    scheme = os.environ.get("CUBE_WORKER_SCHEME", "http")

    return f"{scheme}://{sandbox.get_host(WORKER_PORT)}"


def worker_session(sandbox):
    import requests

    session = requests.Session()
    session.headers["accept-encoding"] = "identity"

    if sandbox.traffic_access_token:
        session.headers["e2b-traffic-access-token"] = sandbox.traffic_access_token

    if os.environ.get("CUBE_PROXY_NODE_IP"):
        session.headers["Host"] = sandbox.get_host(WORKER_PORT)

    return session


def cleanup(sandbox, snapshot_ids: list[str], delete_snapshot) -> None:
    try:
        sandbox.kill()
    except Exception as error:
        print(f"[cleanup] failed to kill sandbox {sandbox.sandbox_id}: {error}")

    try:
        sandbox.close()
    except Exception as error:
        print(f"[cleanup] failed to close sandbox {sandbox.sandbox_id}: {error}")

    for snapshot_id in snapshot_ids:
        try:
            delete_snapshot(snapshot_id)
        except Exception as error:
            print(f"[cleanup] failed to delete snapshot {snapshot_id}: {error}")


def main(argv: list[str] | None = None) -> None:
    import requests
    from dotenv import load_dotenv

    args = parse_args(argv)
    load_dotenv(Path(__file__).with_name(".env"), override=False)

    from cubesandbox import Sandbox

    template_id = os.environ.get("CUBE_TEMPLATE_ID")
    if not template_id:
        raise RuntimeError("CUBE_TEMPLATE_ID is required")

    (snapshot_ids, session) = ([], None)
    sandbox = Sandbox.create(
        template=template_id,
        timeout=900,
        network={"allow_public_traffic": False},
    )
    sandbox_id = sandbox.sandbox_id
    print(
        f"[setup] Sandbox created: {sandbox_id} "
        f"cycles={args.cycles} seed={args.seed}"
    )

    try:
        start_worker(sandbox)
        url = worker_url(sandbox)
        session = worker_session(sandbox)
        wait_ready(session, url)
        print(
            f"[setup] Worker ready: {url} "
            f"before={args.transfers_before_checkpoint} "
            f"after={args.transfers_after_checkpoint}"
        )

        initial_state = read_state(session, url)
        initial_audit = sandbox.files.read(AUDIT_PATH, user="root")
        initial_events = parse_audit(initial_audit)
        verify_state(initial_state, initial_events)

        (expected, rng, sequence) = (
            copy.deepcopy(initial_state),
            random.Random(args.seed),
            1,
        )
        print(
            f"[setup] Initial state verified: state={digest(initial_state)} "
            f"audit={digest(initial_events)}"
        )

        for epoch in range(1, args.cycles + 1):
            (plan, sequence) = plan_epoch(
                rng,
                expected,
                epoch=epoch,
                sequence=sequence,
                before_checkpoint=args.transfers_before_checkpoint,
                after_checkpoint=args.transfers_after_checkpoint,
            )
            print(
                f"[epoch {epoch}/{args.cycles}] Starting from "
                f"committed={expected['stats']['committed']} "
                f"duplicates={expected['stats']['duplicates']}"
            )

            (observed_state, checkpoint_audit) = commit_batch(
                sandbox,
                session,
                url,
                expected,
                plan.before_checkpoint,
                epoch=epoch,
                cycles=args.cycles,
                phase="checkpoint",
            )
            checkpoint_state = copy.deepcopy(observed_state)
            sync = sandbox.commands.run(
                f"sync {AUDIT_PATH}",
                timeout=10,
                user="root",
            )
            if sync.exit_code != 0:
                raise RuntimeError(f"failed to flush audit file: {sync.stderr}")

            snapshot = sandbox.create_snapshot()
            snapshot_ids.append(snapshot.snapshot_id)
            print(
                f"[epoch {epoch}/{args.cycles}] Checkpoint created: "
                f"snapshot={snapshot.snapshot_id} state={digest(checkpoint_state)} "
                f"audit={digest(parse_audit(checkpoint_audit))}"
            )

            commit_batch(
                sandbox,
                session,
                url,
                expected,
                plan.after_checkpoint,
                epoch=epoch,
                cycles=args.cycles,
                phase="post-checkpoint",
            )

            try:
                response = submit(session, url, plan.faulted, fault=True)
                if response.status_code < 500:
                    raise AssertionError(
                        "fault request unexpectedly returned "
                        f"{response.status_code}: {response.text}"
                    )
            except requests.RequestException:
                pass

            wait_unavailable(session, url)
            dirty_audit = sandbox.files.read(AUDIT_PATH, user="root")
            dirty_events = parse_audit(dirty_audit)
            verify_fault(
                dirty_events,
                plan.faulted,
                expected_state=expected,
            )
            require(
                dirty_audit != checkpoint_audit,
                "fault did not change the durable audit",
            )
            print(
                f"[epoch {epoch}/{args.cycles}] Fault verified: "
                f"id={plan.faulted['id']} audit={digest(dirty_events)} "
                "worker=unavailable"
            )

            result = sandbox.rollback(snapshot.snapshot_id)
            require(sandbox.sandbox_id == sandbox_id, "rollback changed the sandbox ID")
            require(
                result.get("sandboxID", sandbox_id) == sandbox_id,
                "rollback response contains a different sandbox ID",
            )

            session.close()
            session = worker_session(sandbox)
            wait_ready(session, url, timeout=60)

            expected = copy.deepcopy(checkpoint_state)
            (restored_state, restored_audit) = read_verified_state(
                sandbox,
                session,
                url,
                expected,
            )
            require(
                restored_audit == checkpoint_audit,
                "rollback did not restore the checkpoint audit",
            )
            require(
                all(
                    transfer["id"] not in restored_state["seen"]
                    for transfer in plan.after_checkpoint
                ),
                "rollback retained post-checkpoint transfers",
            )
            print(
                f"[epoch {epoch}/{args.cycles}] Rollback verified: "
                f"snapshot={snapshot.snapshot_id} "
                f"discarded={len(plan.after_checkpoint)} "
                f"state={digest(restored_state)} "
                f"audit={digest(parse_audit(restored_audit))}"
            )

            commit_batch(
                sandbox,
                session,
                url,
                expected,
                plan.after_checkpoint,
                epoch=epoch,
                cycles=args.cycles,
                phase="replayed",
            )

            response = submit(session, url, plan.faulted)
            response.raise_for_status()
            require(
                response.json()["outcome"] == "committed",
                f"fault retry {plan.faulted['id']} was not committed",
            )

            apply_commit(expected, plan.faulted)
            (committed_state, audit) = read_verified_state(
                sandbox,
                session,
                url,
                expected,
            )
            print(
                f"[epoch {epoch}/{args.cycles}] Fault retry verified: "
                f"id={plan.faulted['id']} ledger={len(committed_state['ledger'])} "
                f"state={digest(committed_state)}"
            )

            response = submit(session, url, plan.faulted)
            response.raise_for_status()
            require(
                response.json()["outcome"] == "duplicate",
                f"fault retry {plan.faulted['id']} was not deduplicated",
            )

            apply_duplicate(expected, plan.faulted)
            (duplicate_state, audit) = read_verified_state(
                sandbox,
                session,
                url,
                expected,
            )
            require(
                duplicate_state["balances"] == committed_state["balances"],
                "duplicate retry changed balances",
            )
            require(
                duplicate_state["ledger"] == committed_state["ledger"],
                "duplicate retry changed the ledger",
            )
            print(
                f"[epoch {epoch}/{args.cycles}] Duplicate verified: "
                f"id={plan.faulted['id']} "
                f"duplicates={duplicate_state['stats']['duplicates']}"
            )
            print(
                f"[epoch {epoch}/{args.cycles}] PASS: "
                f"committed={duplicate_state['stats']['committed']} "
                f"state={digest(duplicate_state)}"
            )

        expected_commits = args.cycles * (
            args.transfers_before_checkpoint
            + args.transfers_after_checkpoint
            + 1
        )
        require(
            expected["stats"]["committed"] == expected_commits,
            "final committed count does not match the workload plan",
        )
        require(
            expected["stats"]["duplicates"] == args.cycles,
            "final duplicate count does not match the recovery cycles",
        )
        require(
            expected["stats"]["faults"] == 0,
            "rolled-back fault counters remained in the final state",
        )
        print(
            f"[summary] PASS: cycles={args.cycles} checkpoints={len(snapshot_ids)} "
            f"committed={expected['stats']['committed']} "
            f"duplicates={expected['stats']['duplicates']} seed={args.seed}"
        )
    finally:
        try:
            if session is not None:
                session.close()
        except Exception as error:
            print(f"[cleanup] failed to close worker session: {error}")

        cleanup(sandbox, snapshot_ids, Sandbox.delete_snapshot)


if __name__ == "__main__":
    main()
