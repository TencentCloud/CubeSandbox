// SPDX-License-Identifier: BSD-3-Clause
use std::fmt;
use std::num::ParseIntError;
use std::str::FromStr;

/// Expected error conditions with respect to parsing both UidMap and GidMap
#[derive(Debug, Eq, PartialEq)]
pub enum IdMapError {
    /// A delimiter has been found that does not match the delimiter the map started with.
    InvalidDelimiter,
    /// The map is empty or incorrect number of values are provided.
    IncompleteMap,
    /// Wraps the cause of parsing an integer failing.
    InvalidValue(ParseIntError),
}
impl std::error::Error for IdMapError {}

impl fmt::Display for IdMapError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            IdMapError::InvalidDelimiter => write!(
                f,
                "A delimiter has been found that does not match the delimiter the map started with"
            ),
            IdMapError::IncompleteMap => write!(
                f,
                "The map is empty or incorrect number of values are provided"
            ),
            IdMapError::InvalidValue(err) => write!(f, "{}", err),
        }
    }
}

impl From<ParseIntError> for IdMapError {
    fn from(err: ParseIntError) -> Self {
        IdMapError::InvalidValue(err)
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct UidMap {
    pub inside_uid: u32,
    pub outside_uid: u32,
    pub count: u32,
}

impl FromStr for UidMap {
    type Err = IdMapError;

    fn from_str(s: &str) -> Result<Self, Self::Err> {
        let fields = parse_idmap(s, 3)?;

        Ok(UidMap {
            inside_uid: fields[0],
            outside_uid: fields[1],
            count: fields[2],
        })
    }
}

impl fmt::Display for UidMap {
    fn fmt(&self, f: &mut fmt::Formatter) -> fmt::Result {
        write!(
            f,
            ":{}:{}:{}:",
            self.inside_uid, self.outside_uid, self.count
        )
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct GidMap {
    pub inside_gid: u32,
    pub outside_gid: u32,
    pub count: u32,
}

impl FromStr for GidMap {
    type Err = IdMapError;

    fn from_str(s: &str) -> Result<Self, Self::Err> {
        let fields = parse_idmap(s, 3)?;

        Ok(GidMap {
            inside_gid: fields[0],
            outside_gid: fields[1],
            count: fields[2],
        })
    }
}

impl fmt::Display for GidMap {
    fn fmt(&self, f: &mut fmt::Formatter) -> fmt::Result {
        write!(
            f,
            ":{}:{}:{}:",
            self.inside_gid, self.outside_gid, self.count
        )
    }
}

fn parse_idmap(s: &str, expected_len: usize) -> Result<Vec<u32>, IdMapError> {
    let mut s = String::from(s);
    let delimiter = s.pop().ok_or(IdMapError::IncompleteMap)?;
    if delimiter.is_alphanumeric() {
        return Err(IdMapError::InvalidDelimiter);
    }

    let values: Vec<&str> = s
        .strip_prefix(delimiter)
        .ok_or(IdMapError::InvalidDelimiter)?
        .split(delimiter)
        .collect();

    if values.len() != expected_len {
        return Err(IdMapError::IncompleteMap);
    }

    values
        .into_iter()
        .map(|v| v.parse().map_err(IdMapError::InvalidValue))
        .collect()
}

#[derive(Debug, Eq, PartialEq)]
#[repr(u8)]
pub(crate) enum IdMapSetUpPipeMessage {
    Request = 0x1,
    Done = 0x2,
}
