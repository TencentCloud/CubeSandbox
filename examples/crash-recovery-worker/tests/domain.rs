// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

use crash_recovery_worker::{Error, Fault, Outcome, Transfer, Worker};

fn audit(path: &std::path::Path) -> Vec<serde_json::Value> {
    std::fs::read_to_string(path)
        .expect("read audit file")
        .lines()
        .map(|line| serde_json::from_str(line).expect("parse audit event"))
        .collect()
}

fn transfer(id: &str) -> Transfer {
    Transfer {
        id: id.to_owned(),
        from: "alice".to_owned(),
        to: "bob".to_owned(),
        amount: 100,
    }
}

#[test]
fn committed_transfer_preserves_balances_and_records_transfer() {
    let dir = tempfile::tempdir().expect("create temporary directory");
    let audit = dir.path().join("audit.jsonl");
    let mut worker = Worker::open(&audit).expect("open worker");

    let outcome = worker
        .transfer(transfer("tx-001"), Fault::None)
        .expect("commit transfer");

    assert_eq!(outcome, Outcome::Committed);

    let state = worker.state();
    assert_eq!(state.balances.values().sum::<i64>(), 1_750);
    assert_eq!(state.balances["alice"], 900);
    assert_eq!(state.balances["bob"], 600);
    assert_eq!(state.ledger, vec![transfer("tx-001")]);
    assert_eq!(state.seen, ["tx-001".to_owned()].into());
    assert!(state.pending.is_empty());
    assert_eq!(state.stats.committed, 1);
}

#[test]
fn fault_after_debit_leaves_an_observable_inconsistent_state() {
    let dir = tempfile::tempdir().expect("create temporary directory");
    let audit = dir.path().join("audit.jsonl");
    let mut worker = Worker::open(&audit).expect("open worker");

    let outcome = worker
        .transfer(transfer("tx-crash"), Fault::AfterDebit)
        .expect("inject fault");

    assert_eq!(outcome, Outcome::FaultInjected);

    let state = worker.state();
    assert_eq!(state.balances.values().sum::<i64>(), 1_650);
    assert_eq!(state.balances["alice"], 900);
    assert_eq!(state.balances["bob"], 500);
    assert_eq!(state.pending["tx-crash"], transfer("tx-crash"));
    assert!(state.ledger.is_empty());
    assert!(state.seen.is_empty());
    assert_eq!(state.stats.committed, 0);
    assert_eq!(state.stats.faults, 1);
}

#[test]
fn duplicate_request_id_does_not_apply_transfer_twice() {
    let dir = tempfile::tempdir().expect("create temporary directory");
    let audit = dir.path().join("audit.jsonl");
    let mut worker = Worker::open(&audit).expect("open worker");

    worker
        .transfer(transfer("tx-001"), Fault::None)
        .expect("commit transfer");
    let balances = worker.state().balances.clone();

    let outcome = worker
        .transfer(transfer("tx-001"), Fault::None)
        .expect("handle duplicate");

    assert_eq!(outcome, Outcome::Duplicate);

    let state = worker.state();
    assert_eq!(state.balances, balances);
    assert_eq!(state.ledger, vec![transfer("tx-001")]);
    assert_eq!(state.stats.committed, 1);
    assert_eq!(state.stats.duplicates, 1);
}

#[test]
fn duplicate_is_recognized_before_rechecking_the_current_balance() {
    let dir = tempfile::tempdir().expect("create temporary directory");
    let audit = dir.path().join("audit.jsonl");
    let mut worker = Worker::open(&audit).expect("open worker");
    let mut request = transfer("tx-large");
    request.amount = 900;

    worker
        .transfer(request.clone(), Fault::None)
        .expect("commit transfer");
    let outcome = worker
        .transfer(request, Fault::None)
        .expect("handle duplicate");

    assert_eq!(outcome, Outcome::Duplicate);
    assert_eq!(worker.state().balances["alice"], 100);
    assert_eq!(worker.state().ledger.len(), 1);
}

#[test]
fn reused_request_id_with_different_payload_is_rejected() {
    let dir = tempfile::tempdir().expect("create temporary directory");
    let audit = dir.path().join("audit.jsonl");
    let mut worker = Worker::open(&audit).expect("open worker");

    worker
        .transfer(transfer("tx-001"), Fault::None)
        .expect("commit transfer");
    let before = worker.state().clone();
    let mut conflicting = transfer("tx-001");
    conflicting.amount = 50;

    let error = worker
        .transfer(conflicting, Fault::None)
        .expect_err("reject conflicting request ID");

    assert!(matches!(error, Error::RequestConflict(id) if id == "tx-001"));
    assert_eq!(worker.state(), &before);
}

#[test]
fn audit_failure_does_not_leave_memory_partially_updated() {
    let dir = tempfile::tempdir().expect("create temporary directory");
    let path = dir.path().join("audit.jsonl");
    let mut worker = Worker::open(&path).expect("open worker");
    let before = worker.state().clone();
    std::fs::remove_dir_all(dir.path()).expect("remove audit directory");

    let error = worker
        .transfer(transfer("tx-io-error"), Fault::None)
        .expect_err("surface audit failure");

    assert!(matches!(error, Error::Io(_)));
    assert_eq!(worker.state(), &before);
}

#[test]
fn non_positive_amount_is_rejected_without_mutation() {
    let dir = tempfile::tempdir().expect("create temporary directory");
    let audit = dir.path().join("audit.jsonl");
    let mut worker = Worker::open(&audit).expect("open worker");
    let before = worker.state().clone();
    let mut request = transfer("tx-invalid");
    request.amount = 0;

    let error = worker
        .transfer(request, Fault::None)
        .expect_err("reject invalid amount");

    assert!(matches!(error, Error::InvalidAmount));
    assert_eq!(worker.state(), &before);
}

#[test]
fn blank_request_id_is_rejected_without_mutation() {
    let dir = tempfile::tempdir().expect("create temporary directory");
    let audit = dir.path().join("audit.jsonl");
    let mut worker = Worker::open(&audit).expect("open worker");
    let before = worker.state().clone();
    let mut request = transfer("   ");
    request.id = "   ".to_owned();

    let error = worker
        .transfer(request, Fault::None)
        .expect_err("reject blank request ID");

    assert!(matches!(error, Error::InvalidId));
    assert_eq!(worker.state(), &before);
}

#[test]
fn unknown_account_is_rejected_without_mutation() {
    let dir = tempfile::tempdir().expect("create temporary directory");
    let audit = dir.path().join("audit.jsonl");
    let mut worker = Worker::open(&audit).expect("open worker");
    let before = worker.state().clone();
    let mut request = transfer("tx-unknown");
    request.from = "nobody".to_owned();

    let error = worker
        .transfer(request, Fault::None)
        .expect_err("reject unknown account");

    assert!(matches!(error, Error::AccountNotFound(account) if account == "nobody"));
    assert_eq!(worker.state(), &before);
}

#[test]
fn insufficient_balance_is_rejected_without_mutation() {
    let dir = tempfile::tempdir().expect("create temporary directory");
    let audit = dir.path().join("audit.jsonl");
    let mut worker = Worker::open(&audit).expect("open worker");
    let before = worker.state().clone();
    let mut request = transfer("tx-too-large");
    request.amount = 1_001;

    let error = worker
        .transfer(request, Fault::None)
        .expect_err("reject insufficient balance");

    assert!(matches!(
        error,
        Error::InsufficientBalance {
            account,
            available: 1_000,
            requested: 1_001,
        } if account == "alice"
    ));
    assert_eq!(worker.state(), &before);
}

#[test]
fn transfer_to_same_account_is_rejected_without_mutation() {
    let dir = tempfile::tempdir().expect("create temporary directory");
    let audit = dir.path().join("audit.jsonl");
    let mut worker = Worker::open(&audit).expect("open worker");
    let before = worker.state().clone();
    let mut request = transfer("tx-same-account");
    request.to = request.from.clone();

    let error = worker
        .transfer(request, Fault::None)
        .expect_err("reject same account");

    assert!(matches!(error, Error::SameAccount));
    assert_eq!(worker.state(), &before);
}

#[test]
fn committed_transfer_is_persisted_as_a_complete_audit_sequence() {
    let dir = tempfile::tempdir().expect("create temporary directory");
    let path = dir.path().join("audit.jsonl");
    let mut worker = Worker::open(&path).expect("open worker");

    worker
        .transfer(transfer("tx-001"), Fault::None)
        .expect("commit transfer");

    let events = audit(&path);
    let kinds: Vec<_> = events.iter().map(|event| event["kind"].as_str()).collect();

    assert_eq!(
        kinds,
        [Some("initialized"), Some("started"), Some("committed")]
    );
    assert_eq!(events[1]["transfer"]["id"], "tx-001");
    assert_eq!(events[2]["transfer"]["id"], "tx-001");
}

#[test]
fn injected_fault_is_persisted_with_the_inconsistent_balance_total() {
    let dir = tempfile::tempdir().expect("create temporary directory");
    let path = dir.path().join("audit.jsonl");
    let mut worker = Worker::open(&path).expect("open worker");

    worker
        .transfer(transfer("tx-crash"), Fault::AfterDebit)
        .expect("inject fault");

    let events = audit(&path);
    let kinds: Vec<_> = events.iter().map(|event| event["kind"].as_str()).collect();

    assert_eq!(
        kinds,
        [Some("initialized"), Some("started"), Some("fault_injected")]
    );
    assert_eq!(events[2]["id"], "tx-crash");
    assert_eq!(events[2]["point"], "after_debit");
    assert_eq!(events[2]["balance_total"], 1_650);
}

#[test]
fn duplicate_request_is_recorded_without_another_commit_event() {
    let dir = tempfile::tempdir().expect("create temporary directory");
    let path = dir.path().join("audit.jsonl");
    let mut worker = Worker::open(&path).expect("open worker");

    worker
        .transfer(transfer("tx-001"), Fault::None)
        .expect("commit transfer");
    worker
        .transfer(transfer("tx-001"), Fault::None)
        .expect("handle duplicate");

    let events = audit(&path);
    let kinds: Vec<_> = events.iter().map(|event| event["kind"].as_str()).collect();

    assert_eq!(
        kinds,
        [
            Some("initialized"),
            Some("started"),
            Some("committed"),
            Some("duplicate"),
        ]
    );
    assert_eq!(events[3]["id"], "tx-001");
}
