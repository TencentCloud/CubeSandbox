// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

use std::collections::{BTreeMap, BTreeSet};
use std::fs::{self, File};
use std::io::{self, BufWriter, Write};
use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};

#[derive(Debug)]
pub enum Error {
    AccountNotFound(String),
    InsufficientBalance {
        account: String,
        available: i64,
        requested: i64,
    },
    InvalidAmount,
    InvalidId,
    Io(io::Error),
    Json(serde_json::Error),
    RequestConflict(String),
    SameAccount,
}

impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::AccountNotFound(account) => write!(f, "account not found: {account}"),
            Self::InsufficientBalance {
                account,
                available,
                requested,
            } => write!(
                f,
                "insufficient balance for {account}: available {available}, requested {requested}"
            ),
            Self::InvalidAmount => f.write_str("amount must be positive"),
            Self::InvalidId => f.write_str("request ID must not be blank"),
            Self::Io(error) => error.fmt(f),
            Self::Json(error) => error.fmt(f),
            Self::RequestConflict(id) => {
                write!(
                    f,
                    "request ID was already used with a different payload: {id}"
                )
            }
            Self::SameAccount => f.write_str("source and destination accounts must differ"),
        }
    }
}

impl std::error::Error for Error {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            Self::AccountNotFound(_)
            | Self::InsufficientBalance { .. }
            | Self::InvalidAmount
            | Self::InvalidId
            | Self::RequestConflict(_)
            | Self::SameAccount => None,
            Self::Io(error) => Some(error),
            Self::Json(error) => Some(error),
        }
    }
}

impl From<io::Error> for Error {
    fn from(error: io::Error) -> Self {
        Self::Io(error)
    }
}

impl From<serde_json::Error> for Error {
    fn from(error: serde_json::Error) -> Self {
        Self::Json(error)
    }
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
pub struct Transfer {
    pub id: String,
    pub from: String,
    pub to: String,
    pub amount: i64,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Fault {
    None,
    AfterDebit,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum Outcome {
    Committed,
    Duplicate,
    FaultInjected,
}

#[derive(Clone, Debug, Default, Deserialize, Eq, PartialEq, Serialize)]
pub struct Stats {
    pub committed: usize,
    pub duplicates: usize,
    pub faults: usize,
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
pub struct State {
    pub initial_balances: BTreeMap<String, i64>,
    pub balances: BTreeMap<String, i64>,
    pub pending: BTreeMap<String, Transfer>,
    pub ledger: Vec<Transfer>,
    pub seen: BTreeSet<String>,
    pub stats: Stats,
}

#[derive(Clone, Debug, Serialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
enum Event {
    Committed {
        transfer: Transfer,
    },
    Duplicate {
        id: String,
    },
    FaultInjected {
        id: String,
        point: &'static str,
        balance_total: i64,
    },
    Initialized {
        balances: BTreeMap<String, i64>,
    },
    Started {
        transfer: Transfer,
    },
}

pub struct Worker {
    audit: PathBuf,
    events: Vec<Event>,
    state: State,
}

impl Worker {
    pub fn open(path: impl AsRef<Path>) -> Result<Self, Error> {
        let initial_balances = BTreeMap::from([
            ("alice".to_owned(), 1_000),
            ("bob".to_owned(), 500),
            ("carol".to_owned(), 250),
        ]);

        let mut worker = Self {
            audit: path.as_ref().to_owned(),
            events: vec![Event::Initialized {
                balances: initial_balances.clone(),
            }],
            state: State {
                balances: initial_balances.clone(),
                initial_balances,
                pending: BTreeMap::new(),
                ledger: Vec::new(),
                seen: BTreeSet::new(),
                stats: Stats::default(),
            },
        };
        worker.persist()?;

        Ok(worker)
    }

    pub fn transfer(&mut self, transfer: Transfer, fault: Fault) -> Result<Outcome, Error> {
        if transfer.id.trim().is_empty() {
            return Err(Error::InvalidId);
        }

        if transfer.amount <= 0 {
            return Err(Error::InvalidAmount);
        }

        if transfer.from == transfer.to {
            return Err(Error::SameAccount);
        }

        if self.state.seen.contains(&transfer.id) {
            let committed = self
                .state
                .ledger
                .iter()
                .find(|item| item.id == transfer.id)
                .expect("seen request ID has a ledger entry");
            if committed != &transfer {
                return Err(Error::RequestConflict(transfer.id));
            }

            let (state, events) = (self.state.clone(), self.events.clone());

            self.state.stats.duplicates += 1;
            self.events.push(Event::Duplicate { id: transfer.id });
            self.persist_or_restore(state, events)?;

            return Ok(Outcome::Duplicate);
        }

        for account in [&transfer.from, &transfer.to] {
            if !self.state.balances.contains_key(account) {
                return Err(Error::AccountNotFound(account.clone()));
            }
        }

        let available = self.state.balances[&transfer.from];
        if available < transfer.amount {
            return Err(Error::InsufficientBalance {
                account: transfer.from.clone(),
                available,
                requested: transfer.amount,
            });
        }

        let (state, events) = (self.state.clone(), self.events.clone());

        self.events.push(Event::Started {
            transfer: transfer.clone(),
        });
        self.state
            .pending
            .insert(transfer.id.clone(), transfer.clone());

        *self
            .state
            .balances
            .get_mut(&transfer.from)
            .expect("source account exists") -= transfer.amount;

        if fault == Fault::AfterDebit {
            self.state.stats.faults += 1;
            self.events.push(Event::FaultInjected {
                id: transfer.id,
                point: "after_debit",
                balance_total: self.state.balances.values().sum(),
            });
            self.persist_or_restore(state, events)?;

            return Ok(Outcome::FaultInjected);
        }

        *self
            .state
            .balances
            .get_mut(&transfer.to)
            .expect("destination account exists") += transfer.amount;

        self.state.seen.insert(transfer.id.clone());
        self.state.pending.remove(&transfer.id);
        self.state.ledger.push(transfer.clone());
        self.state.stats.committed += 1;
        self.events.push(Event::Committed { transfer });
        self.persist_or_restore(state, events)?;

        Ok(Outcome::Committed)
    }

    pub fn state(&self) -> &State {
        &self.state
    }

    fn persist(&mut self) -> Result<(), Error> {
        let tmp = self.audit.with_extension("jsonl.tmp");
        let file = File::create(&tmp)?;
        let mut writer = BufWriter::new(file);

        for event in &self.events {
            serde_json::to_writer(&mut writer, event)?;
            writer.write_all(b"\n")?;
        }

        writer.flush()?;
        writer.get_ref().sync_all()?;
        fs::rename(tmp, &self.audit)?;

        Ok(())
    }

    fn persist_or_restore(&mut self, state: State, events: Vec<Event>) -> Result<(), Error> {
        if let Err(error) = self.persist() {
            (self.state, self.events) = (state, events);

            return Err(error);
        }

        Ok(())
    }
}
