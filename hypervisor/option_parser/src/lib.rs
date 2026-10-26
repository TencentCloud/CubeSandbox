// Copyright © 2020 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

use std::collections::HashMap;
use std::fmt;
use std::num::ParseIntError;
use std::str::FromStr;

#[derive(Default)]
pub struct OptionParser {
    options: HashMap<String, OptionParserValue>,
}

struct OptionParserValue {
    value: Option<String>,
    requires_value: bool,
}

#[derive(Debug)]
pub enum OptionParserError {
    UnknownOption(String),
    InvalidSyntax(String),
    Conversion(String, String),
}

impl fmt::Display for OptionParserError {
    fn fmt(&self, f: &mut fmt::Formatter) -> fmt::Result {
        match self {
            OptionParserError::UnknownOption(s) => write!(f, "unknown option: {}", s),
            OptionParserError::InvalidSyntax(s) => write!(f, "invalid syntax:{}", s),
            OptionParserError::Conversion(field, value) => {
                write!(f, "unable to convert {} for {}", value, field)
            }
        }
    }
}
type OptionParserResult<T> = std::result::Result<T, OptionParserError>;

fn split_commas(s: &str) -> OptionParserResult<Vec<String>> {
    let mut list: Vec<String> = Vec::new();
    let mut opened_brackets = 0;
    let mut in_quotes = false;
    let mut current = String::new();

    for c in s.trim().chars() {
        match c {
            '[' => {
                opened_brackets += 1;
                current.push('[');
            }
            ']' => {
                opened_brackets -= 1;
                if opened_brackets < 0 {
                    return Err(OptionParserError::InvalidSyntax(s.to_owned()));
                }
                current.push(']');
            }
            '"' => in_quotes = !in_quotes,
            ',' => {
                if opened_brackets > 0 || in_quotes {
                    current.push(',')
                } else {
                    list.push(current);
                    current = String::new();
                }
            }
            c => current.push(c),
        }
    }
    list.push(current);

    if opened_brackets != 0 || in_quotes {
        return Err(OptionParserError::InvalidSyntax(s.to_owned()));
    }

    Ok(list)
}

impl OptionParser {
    pub fn new() -> Self {
        Self {
            options: HashMap::new(),
        }
    }

    pub fn parse(&mut self, input: &str) -> OptionParserResult<()> {
        if input.trim().is_empty() {
            return Ok(());
        }

        for option in split_commas(input)?.iter() {
            let parts: Vec<&str> = option.splitn(2, '=').collect();

            match self.options.get_mut(parts[0]) {
                None => return Err(OptionParserError::UnknownOption(parts[0].to_owned())),
                Some(value) => {
                    if value.requires_value {
                        if parts.len() != 2 {
                            return Err(OptionParserError::InvalidSyntax((*option).to_owned()));
                        }
                        value.value = Some(parts[1].trim().to_owned());
                    } else {
                        value.value = Some(String::new());
                    }
                }
            }
        }

        Ok(())
    }

    pub fn add(&mut self, option: &str) -> &mut Self {
        self.options.insert(
            option.to_owned(),
            OptionParserValue {
                value: None,
                requires_value: true,
            },
        );

        self
    }

    pub fn add_valueless(&mut self, option: &str) -> &mut Self {
        self.options.insert(
            option.to_owned(),
            OptionParserValue {
                value: None,
                requires_value: false,
            },
        );

        self
    }

    pub fn get(&self, option: &str) -> Option<String> {
        self.options
            .get(option)
            .and_then(|v| v.value.clone())
            .and_then(|s| if s.is_empty() { None } else { Some(s) })
    }

    pub fn is_set(&self, option: &str) -> bool {
        self.options
            .get(option)
            .and_then(|v| v.value.as_ref())
            .is_some()
    }

    pub fn convert<T: FromStr>(&self, option: &str) -> OptionParserResult<Option<T>> {
        match self.get(option) {
            None => Ok(None),
            Some(v) => Ok(Some(v.parse().map_err(|_| {
                OptionParserError::Conversion(option.to_owned(), v.to_owned())
            })?)),
        }
    }
}

pub struct Toggle(pub bool);

pub enum ToggleParseError {
    InvalidValue(String),
}

impl FromStr for Toggle {
    type Err = ToggleParseError;

    fn from_str(s: &str) -> std::result::Result<Self, Self::Err> {
        match s.to_lowercase().as_str() {
            "" => Ok(Toggle(false)),
            "on" => Ok(Toggle(true)),
            "off" => Ok(Toggle(false)),
            "true" => Ok(Toggle(true)),
            "false" => Ok(Toggle(false)),
            _ => Err(ToggleParseError::InvalidValue(s.to_owned())),
        }
    }
}

pub struct ByteSized(pub u64);

#[derive(Debug)]
pub enum ByteSizedParseError {
    InvalidValue(String),
}

impl FromStr for ByteSized {
    type Err = ByteSizedParseError;

    fn from_str(s: &str) -> std::result::Result<Self, Self::Err> {
        Ok(ByteSized({
            let s = s.trim();
            let shift = if s.ends_with('K') {
                10
            } else if s.ends_with('M') {
                20
            } else if s.ends_with('G') {
                30
            } else {
                0
            };

            let s = s.trim_end_matches(|c| c == 'K' || c == 'M' || c == 'G');
            s.parse::<u64>()
                .map_err(|_| ByteSizedParseError::InvalidValue(s.to_owned()))?
                << shift
        }))
    }
}

pub struct IntegerList(pub Vec<u64>);

pub enum IntegerListParseError {
    InvalidValue(String),
}

impl FromStr for IntegerList {
    type Err = IntegerListParseError;

    fn from_str(s: &str) -> std::result::Result<Self, Self::Err> {
        let mut integer_list = Vec::new();
        let ranges_list: Vec<&str> = s
            .trim()
            .trim_matches(|c| c == '[' || c == ']')
            .split(',')
            .collect();

        for range in ranges_list.iter() {
            let items: Vec<&str> = range.split('-').collect();

            if items.len() > 2 {
                return Err(IntegerListParseError::InvalidValue((*range).to_string()));
            }

            let start_range = items[0]
                .parse::<u64>()
                .map_err(|_| IntegerListParseError::InvalidValue(items[0].to_owned()))?;

            integer_list.push(start_range);

            if items.len() == 2 {
                let end_range = items[1]
                    .parse::<u64>()
                    .map_err(|_| IntegerListParseError::InvalidValue(items[1].to_owned()))?;
                if start_range >= end_range {
                    return Err(IntegerListParseError::InvalidValue((*range).to_string()));
                }

                for i in start_range..end_range {
                    integer_list.push(i + 1);
                }
            }
        }

        Ok(IntegerList(integer_list))
    }
}

pub trait TupleValue {
    fn parse_value(input: &str) -> Result<Self, TupleError>
    where
        Self: Sized;
}

impl TupleValue for u64 {
    fn parse_value(input: &str) -> Result<Self, TupleError> {
        input.parse::<u64>().map_err(TupleError::InvalidInteger)
    }
}

impl TupleValue for Vec<u8> {
    fn parse_value(input: &str) -> Result<Self, TupleError> {
        Ok(IntegerList::from_str(input)
            .map_err(TupleError::InvalidIntegerList)?
            .0
            .iter()
            .map(|v| *v as u8)
            .collect())
    }
}

impl TupleValue for Vec<u64> {
    fn parse_value(input: &str) -> Result<Self, TupleError> {
        Ok(IntegerList::from_str(input)
            .map_err(TupleError::InvalidIntegerList)?
            .0)
    }
}

pub struct Tuple<S, T>(pub Vec<(S, T)>);

pub enum TupleError {
    InvalidValue(String),
    SplitOutsideBrackets(OptionParserError),
    InvalidIntegerList(IntegerListParseError),
    InvalidInteger(ParseIntError),
}

impl<S: FromStr, T: TupleValue> FromStr for Tuple<S, T> {
    type Err = TupleError;

    fn from_str(s: &str) -> std::result::Result<Self, Self::Err> {
        let mut list: Vec<(S, T)> = Vec::new();

        let body = s
            .trim()
            .strip_prefix('[')
            .and_then(|s| s.strip_suffix(']'))
            .ok_or_else(|| TupleError::InvalidValue(s.to_string()))?;

        let tuples_list = split_commas(body).map_err(TupleError::SplitOutsideBrackets)?;
        for tuple in tuples_list.iter() {
            let items: Vec<&str> = tuple.split('@').collect();

            if items.len() != 2 {
                return Err(TupleError::InvalidValue((*tuple).to_string()));
            }

            let item1 = items[0]
                .parse::<S>()
                .map_err(|_| TupleError::InvalidValue(items[0].to_owned()))?;
            let item2 = TupleValue::parse_value(items[1])?;

            list.push((item1, item2));
        }

        Ok(Tuple(list))
    }
}

#[derive(Default)]
pub struct StringList(pub Vec<String>);

pub enum StringListParseError {
    InvalidValue(String),
}

impl FromStr for StringList {
    type Err = StringListParseError;

    fn from_str(s: &str) -> std::result::Result<Self, Self::Err> {
        let string_list: Vec<String> = s
            .trim()
            .trim_matches(|c| c == '[' || c == ']')
            .split(',')
            .map(|e| e.to_owned())
            .collect();

        Ok(StringList(string_list))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_option_parser() {
        let mut parser = OptionParser::new();
        parser
            .add("size")
            .add("mergeable")
            .add("hotplug_method")
            .add("hotplug_size")
            .add("topology")
            .add("cmdline");

        assert!(parser.parse("size=128M,hanging_param").is_err());
        assert!(parser.parse("size=128M,too_many_equals=foo=bar").is_err());
        assert!(parser.parse("size=128M,file=/dev/shm").is_err());

        assert!(parser.parse("size=128M").is_ok());
        assert_eq!(parser.get("size"), Some("128M".to_owned()));
        assert!(!parser.is_set("mergeable"));
        assert!(parser.is_set("size"));

        assert!(parser.parse("size=128M,mergeable=on").is_ok());
        assert_eq!(parser.get("size"), Some("128M".to_owned()));
        assert_eq!(parser.get("mergeable"), Some("on".to_owned()));

        assert!(parser
            .parse("size=128M,mergeable=on,topology=[1,2]")
            .is_ok());
        assert_eq!(parser.get("size"), Some("128M".to_owned()));
        assert_eq!(parser.get("mergeable"), Some("on".to_owned()));
        assert_eq!(parser.get("topology"), Some("[1,2]".to_owned()));

        assert!(parser
            .parse("size=128M,mergeable=on,topology=[[1,2],[3,4]]")
            .is_ok());
        assert_eq!(parser.get("size"), Some("128M".to_owned()));
        assert_eq!(parser.get("mergeable"), Some("on".to_owned()));
        assert_eq!(parser.get("topology"), Some("[[1,2],[3,4]]".to_owned()));

        assert!(parser.parse("topology=[").is_err());
        assert!(parser.parse("topology=[[[]]]]").is_err());

        assert!(parser.parse("cmdline=\"console=ttyS0,9600n8\"").is_ok());
        assert_eq!(
            parser.get("cmdline"),
            Some("console=ttyS0,9600n8".to_owned())
        );
        assert!(parser.parse("cmdline=\"").is_err());
        assert!(parser.parse("cmdline=\"\"\"").is_err());
    }
}
