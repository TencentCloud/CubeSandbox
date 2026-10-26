// Copyright (c) 2019 Ant Financial
//
// SPDX-License-Identifier: Apache-2.0
//

use serde::{Deserialize, Serialize};

use std::error;
use std::fmt::{Display, Formatter, Result as FmtResult};
use std::fs::File;
use std::io;

pub type Result<T> = std::result::Result<T, Error>;

#[derive(Debug)]
pub enum Error {
    Io(io::Error),
    Json(serde_json::Error),
}

impl Display for Error {
    fn fmt(&self, f: &mut Formatter) -> FmtResult {
        match *self {
            Error::Io(ref e) => e.fmt(f),
            Error::Json(ref e) => e.fmt(f),
        }
    }
}

impl error::Error for Error {
    fn cause(&self) -> Option<&dyn error::Error> {
        match *self {
            Error::Io(ref e) => Some(e),
            Error::Json(ref e) => Some(e),
        }
    }
}

impl From<io::Error> for Error {
    fn from(e: io::Error) -> Error {
        Error::Io(e)
    }
}

impl From<serde_json::Error> for Error {
    fn from(e: serde_json::Error) -> Error {
        Error::Json(e)
    }
}

pub fn to_writer<W, T>(o: &T, w: W) -> Result<()>
where
    W: io::Write,
    T: Serialize,
{
    Ok(serde_json::to_writer(w, o)?)
}

pub fn serialize<T>(o: &T, path: &str) -> Result<()>
where
    T: Serialize,
{
    let f = File::create(path)?;
    Ok(serde_json::to_writer(f, o)?)
}

pub fn to_string<T>(o: &T) -> Result<String>
where
    T: Serialize,
{
    Ok(serde_json::to_string(o)?)
}

pub fn deserialize<T>(path: &str) -> Result<T>
where
    for<'a> T: Deserialize<'a>,
{
    let f = File::open(path)?;
    Ok(serde_json::from_reader(f)?)
}
