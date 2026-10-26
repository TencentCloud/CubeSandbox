//! The `xattrmap` module is used to translate extended attribute operations
//! between the server (virtiofsd) and the client (the virtio-fs guest kernel
//! module).
//!
//! Here's a non-exhaustive list of use-cases in which it may be beneficial to
//! install an extended attribute mapping:
//!
//! * The guest VM process is executing at a privilege level where it can't
//!   actually modify the extended attribute on the host. In this case, one
//!   may choose to map those guest's extended attributes to a "user."
//!   namespace on the host.
//!
//! * An extended attribute mapping can partition a host's extended attributes
//!   from a guest's to prevent the guest from clobbering extended attributes
//!   that the host has set and depends on.
//!
//! ## Rules
//!
//! The entity that launches virtiofsd may provide an "extended attributes
//! mapping" (or "xattrmap") that defines how extended attributes should be
//! translated. An xattrmap is really just a series of rules with a specific
//! syntax. When translating an xattr, the xattrmap rules are traversed in the
//! order that the mappings were originally written. The traversal is terminated
//! on the first rule that matches the xattr.
//!
//! The xattrmap _must_ have a terminating rule.
//!
//! ### Reference
//!
//! There are two ways of expressing an xattrmap rule:
//!
//! 1. `:type:scope:key:prepend:`
//! 2. `:map:key:prepend:` - this is just syntactic sugar for expressing a common
//!    rule. It is equivalent to `:prefix:all:key:prepend`.
//!
//! An xattrmap is just a series of these rules separated by whitespace. Each rule
//! can have its own delimiter. The colon (`:`) was just used here as an arbitary
//! example. Use a delimiter that you find readable.
//!
//! Let's dissect the xattrmap rule syntax: `:type:scope:key:prepend:`.
//!
//! | type | description |
//! | - | - |
//! | prefix | The value of `key` is prepended to xattrs originating from the client (i.e., `{get,set,remove}xattr()`). The value of `prepend` is stripped from the server's reply to `listxattr()`. |
//! | ok | If the xattr originating from the client is prefixed with `key`, or if an xattr in a server reply is prefixed with `prepend` it passes through unchanged. |
//! | bad | If the xattr originating from the client is prefixed with `key` it is denied with `EPERM`. If the xattr in a server reply is prefixed with `prepend` it is hidden from the client and not included in the reply. |
//! | unsupported | If a client tries to use a name matching 'key' it's denied using ENOTSUP; when the server passes an attribute name matching 'prepend' it's hidden.  In many ways its use is very like 'ok' as either an explicit terminator or for special handling of certain patterns. |
//!
//! `ok` and `bad` can both be used as simple terminators for an xattrmap to
//! satisfy the expectation that every xattrmap has a terminator. For example,
//! `:ok:all:::`, will vacuously terminate all mappings. Placing a rule like
//! this at the end of the xattrmap rules is a common way of providing a
//! terminator.
//!
//! | scope | description |
//! | - | - |
//! | server | Match on xattrnames in the server reply that are prefixed with `prepend`. |
//! | client | Match on xattrnames from the client that are prefixed with `key`. |
//! | all | Matches on both server replies and client requests as described for `server` and `client` scopes. |
//!
//! ### Examples
//!
//! These have been taken almost verbatim from the original virtiofsd
//! documentation in the QEMU source code.
//!
//! #### Example 1
//!
//! ```text
//! :prefix:all::user.virtiofs.:
//! :bad:all:::
//! ```
//!
//! There are two rules in this xattrmap. The first rule prefixes and strips
//! `user.virtiofs.` from client requests and server replies respectively.
//!
//! The second rule hides any non-prefixed extended attributes that the host
//! set.
//!
//! #### Example 2
//!
//! ```text
//! /prefix/all/trusted./user.virtiofs./
//! /bad/server//trusted./
//! /bad/client/user.virtiofs.//
//! /ok/all///
//! ```
//!
//! The first rule prefixes client xattrnames with `trusted.` and strips
//! `user.virtiofs.` from xattrnames included in the server reply.
//!
//! The second rule hides unprefixed `trusted.` attributes on the host.
//!
//! The third rule prevents the guest from manipulating the `user.virtiofs.`
//! namespace directly.
//!
//! The final rule is the terminator and allows all remaining attributes
//! through unchanged.

#![deny(missing_docs)]

use std::borrow::Cow;
use std::convert::TryFrom;
use std::ffi::{CStr, CString};
use std::fmt;
use std::iter::Peekable;

/// Expected error conditions with respect to parsing an XattrMap or
/// attempting to match on a rule.
#[derive(Debug, Eq, PartialEq)]
pub enum ErrorKind {
    /// Scope is not one of: "all", "server", "client".
    InvalidScope {
        /// The unexpected value parsed from the input stream.
        got: String,

        /// A list of the expected values.
        expected: String,
    },

    /// Type is not one of "prefix", "ok", "bad", or "map".
    InvalidType {
        /// The unexpected value parsed from the input stream.
        got: String,

        /// A list of the expected values.
        expected: String,
    },

    /// A delimiter has been found that does not match the delimiter
    /// the rule started with.
    InvalidDelimiter,

    /// The rule is missing fields.
    IncompleteRule,

    /// There may only be one `map` rule and it must be the final
    /// rule; if this error is returned, then multiple map rules
    /// exist or one exists and it is not the final rule.
    MapRuleViolation,

    /// The input stream doesn't contain any rules.
    NoRulesProvided,

    /// None of the rules matched on the input.
    UnterminatedMapping,
}

impl std::error::Error for ErrorKind {}

impl fmt::Display for ErrorKind {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{self:?}")
    }
}

/// Errors specific to XattrMap operations.
#[derive(Debug, Eq, PartialEq)]
pub struct Error {
    /// The specific error condition that was detected.
    pub cause: ErrorKind,

    /// The culpable rule, if any.
    pub rule: Option<usize>,
}

impl std::error::Error for Error {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        Some(&self.cause)
    }
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{self:?}")
    }
}

impl From<ErrorKind> for Error {
    fn from(ek: ErrorKind) -> Self {
        Self {
            cause: ek,
            rule: None,
        }
    }
}

bitflags::bitflags! {
    struct Scope: u8 {
        const CLIENT = 0b01;
        const SERVER = 0b10;
    }
}

impl Scope {
    fn from_bytes<B: AsRef<[u8]>>(bytes: B) -> Result<Self, ErrorKind> {
        let bytes = bytes.as_ref();

        Ok(match bytes {
            b"all" => Scope::CLIENT | Scope::SERVER,
            b"client" => Scope::CLIENT,
            b"server" => Scope::SERVER,
            _ => {
                return Err(ErrorKind::InvalidScope {
                    got: String::from_utf8_lossy(bytes).into(),
                    expected: ["all", "client", "server"].join(", "),
                })
            }
        })
    }
}

#[derive(Copy, Clone, Debug, Eq, PartialEq)]
enum Type {
    Prefix,
    Okay,
    Bad,
    Unsupported,
    Map,
}

impl Type {
    fn from_bytes<B: AsRef<[u8]>>(bytes: B) -> Result<Self, ErrorKind> {
        let bytes = bytes.as_ref();

        Ok(match bytes {
            b"prefix" => Type::Prefix,
            b"ok" => Type::Okay,
            b"bad" => Type::Bad,
            b"unsupported" => Type::Unsupported,
            b"map" => Type::Map,
            _ => {
                return Err(ErrorKind::InvalidType {
                    got: String::from_utf8_lossy(bytes).into(),
                    expected: ["prefix", "ok", "bad", "map"].join(", "),
                })
            }
        })
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
struct Rule {
    scope: Scope,
    type_: Type,
    key: CString,
    prepend: CString,
}

impl Rule {
    fn matches(&self, scope: Scope, xattr_name: &[u8]) -> bool {
        if !self.scope.contains(scope) {
            return false;
        }

        match scope {
            Scope::CLIENT => xattr_name.starts_with(self.key.to_bytes()),
            Scope::SERVER => xattr_name.starts_with(self.prepend.to_bytes()),
            _ => panic!("ambiguous scope"),
        }
    }

    fn from_tokens<I>(tokens: &mut Peekable<I>) -> Result<Self, ErrorKind>
    where
        I: Iterator<Item = char>,
    {
        // The caller has already trimmed the whitespace leading up to here,
        // so the next element should be a rule delimiter.
        let delim = tokens.next().ok_or(ErrorKind::InvalidDelimiter)?;

        // This exists instead of using take_while() because take_while() will
        // consume the delimiter (if it exists) and it won't complain if it doesn't
        // exist. That means that we wouldn't be able to check for an unterminated
        // rule error like this:
        //      :prefix:all:trusted.:user.vm.
        //                                   ^ missing ':'
        let mut next_token = || {
            let mut bytes = vec![];
            loop {
                if let Some(ch) = tokens.peek() {
                    if !ch.eq(&delim) {
                        bytes.push(*ch as u8);
                        let _ = tokens.next();
                    } else {
                        // advance past delimiter
                        let _ = tokens.next();
                        break;
                    }
                } else {
                    // Ran out of tokens without finding a terminating delimiter
                    return Err(ErrorKind::IncompleteRule);
                }
            }

            Ok(bytes)
        };

        let type_ = Type::from_bytes(next_token()?)?;

        Ok(match type_ {
            Type::Map => Rule {
                type_,
                scope: Scope::CLIENT | Scope::SERVER,
                key: CString::new(next_token()?).unwrap(),
                prepend: CString::new(next_token()?).unwrap(),
            },
            Type::Prefix | Type::Okay | Type::Bad | Type::Unsupported => {
                let scope = Scope::from_bytes(next_token()?)?;

                Rule {
                    type_,
                    scope,
                    key: CString::new(next_token()?).unwrap(),
                    prepend: CString::new(next_token()?).unwrap(),
                }
            }
        })
    }

    fn expand_map_type(rule: Self) -> Vec<Self> {
        assert_eq!(rule.type_, Type::Map);

        // 1st: Prefix matches/everything
        let mut rules = vec![Rule {
            type_: Type::Prefix,
            scope: Scope::CLIENT | Scope::SERVER,
            key: rule.key.clone(),
            prepend: rule.prepend.clone(),
        }];

        let last_rule_type = if !rule.key.as_bytes().is_empty() {
            // 2nd: Hide non-prefixed but matching entries on the host, and
            // stop the client accessing prefixed attributes directly
            rules.push(Rule {
                type_: Type::Bad,
                scope: Scope::CLIENT | Scope::SERVER,
                key: rule.prepend,
                prepend: rule.key,
            });

            Type::Okay
        } else {
            Type::Bad
        };

        // Last: Everything else
        rules.push(Rule {
            type_: last_rule_type,
            scope: Scope::CLIENT | Scope::SERVER,
            key: CString::new("").unwrap(),
            prepend: CString::new("").unwrap(),
        });

        rules
    }
}

/// A return value that indicates the xattr name input has passed through
/// the XattrMap where a rule was successfully matched and applied to the
/// xattrname.
#[derive(Debug, Eq, PartialEq)]
pub enum AppliedRule<'a> {
    /// Server should the interior value onward through to the requested operation.
    Pass(Cow<'a, CStr>),

    /// Server should return EPERM (i.e., this matched on a `bad` rule).
    Deny,

    /// Server should return ENOTSUP (i.e., this matched on a `unsupported` rule).
    Unsupported,
}

/// A collection of well-formed xattr translation rules.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct XattrMap {
    rules: Vec<Rule>,
}

impl XattrMap {
    /// Applies xattrmap rules to a single extended attribute name.
    ///
    /// This should be called *before* any other extended attribute
    /// operation is performed on the host file system.
    ///
    /// Client request -> this method -> {get,set,remove}xattr() -> server response
    ///
    /// See also: getxattr(2), setxattr(2), removexattr(2)
    pub fn map_client_xattr<'a>(&self, xattr_name: &'a CStr) -> Result<AppliedRule<'a>, Error> {
        let rule = self.find_rule(Scope::CLIENT, xattr_name.to_bytes())?;

        Ok(match rule.type_ {
            Type::Okay => AppliedRule::Pass(Cow::Borrowed(xattr_name)),
            Type::Bad => AppliedRule::Deny,
            Type::Unsupported => AppliedRule::Unsupported,
            Type::Prefix => {
                let mut concat = rule.prepend.as_bytes().to_vec();
                concat.extend_from_slice(xattr_name.to_bytes());
                AppliedRule::Pass(Cow::Owned(CString::new(concat).unwrap()))
            }
            Type::Map => panic!("Unexpanded MAP rule was found."),
        })
    }

    /// Applies xattrmap rules to a list of extended attribute names.
    ///
    /// This should be called *before* replying to the client with the list
    /// of extended attribute names.
    ///
    /// Client request -> listxattr() -> this method -> server response
    ///
    /// See also: listxattr(2)
    pub fn map_server_xattrlist(&self, xattr_names: Vec<u8>) -> Result<Vec<u8>, Error> {
        let mut filtered = Vec::with_capacity(xattr_names.len());
        let unprocessed = xattr_names.split(|b| *b == 0).filter(|bs| !bs.is_empty());

        for xattr_name in unprocessed {
            let rule = self.find_rule(Scope::SERVER, xattr_name)?;

            let processed = match rule.type_ {
                Type::Bad | Type::Unsupported => continue, // hide this from the client
                Type::Okay => xattr_name,
                Type::Prefix => &xattr_name[rule.prepend.as_bytes().len()..], // strip prefix
                Type::Map => panic!("Unexpanded MAP rule was found."),
            };

            filtered.extend_from_slice(processed);
            filtered.push(0);
        }

        if filtered.is_empty() {
            filtered.push(0);
        }

        filtered.shrink_to_fit();

        Ok(filtered)
    }

    fn find_rule(&self, scope: Scope, xattr_name: &[u8]) -> Result<&Rule, Error> {
        let rule = self
            .rules
            .iter()
            .find(|r| r.matches(scope, xattr_name))
            .ok_or(ErrorKind::UnterminatedMapping)
            .map_err(|e| Error {
                cause: e,
                rule: None,
            })?;

        Ok(rule)
    }
}

impl TryFrom<&str> for XattrMap {
    type Error = Error;

    fn try_from(input: &str) -> Result<Self, Error> {
        let trimmed = input.trim();
        let mut unparsed = trimmed.chars().peekable();
        let mut rules: Vec<Rule> = vec![];

        while unparsed.peek().is_some() {
            // Skip any whitespace between rules
            if let Some(ch) = unparsed.peek() {
                if ch.is_ascii_whitespace() {
                    let _ = unparsed.next();
                    continue;
                }
            }

            let rule = Rule::from_tokens(&mut unparsed).map_err(|e| Error {
                cause: e,
                rule: Some(rules.len() + 1),
            })?;

            if rule.type_ == Type::Map {
                // There may only be one 'map' rule and it must be the final rule
                if unparsed.peek().is_some() {
                    return Err(Error {
                        rule: Some(rules.len() + 1),
                        cause: ErrorKind::MapRuleViolation,
                    });
                }
                rules.append(&mut Rule::expand_map_type(rule));
            } else {
                rules.push(rule);
            };
        }

        if rules.is_empty() {
            return Err(ErrorKind::NoRulesProvided.into());
        }

        Ok(Self { rules })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_parser_can_parse_single_rule() {
        let input = ":prefix:client:trusted.:user.virtiofs.:";
        let actual = XattrMap::try_from(input).unwrap();
        let expected = XattrMap {
            rules: vec![Rule {
                type_: Type::Prefix,
                scope: Scope::CLIENT,
                key: CString::new("trusted.").unwrap(),
                prepend: CString::new("user.virtiofs.").unwrap(),
            }],
        };

        assert_eq!(actual, expected);
    }

    #[test]
    fn test_parser_can_parse_multiple_valid_rules() {
        let input = ":prefix:all::user.virtiofs.::bad:all:::";
        let actual = XattrMap::try_from(input).unwrap();
        let expected = XattrMap {
            rules: vec![
                Rule {
                    type_: Type::Prefix,
                    scope: Scope::CLIENT | Scope::SERVER,
                    key: CString::new("").unwrap(),
                    prepend: CString::new("user.virtiofs.").unwrap(),
                },
                Rule {
                    type_: Type::Bad,
                    scope: Scope::CLIENT | Scope::SERVER,
                    key: CString::new("").unwrap(),
                    prepend: CString::new("").unwrap(),
                },
            ],
        };

        assert_eq!(actual, expected);
    }

    #[test]
    fn test_parser_can_parse_rules_separated_by_whitespace() {
        let input = r#"
        /prefix/all/trusted./user.virtiofs./
        /bad/server//trusted./
        /bad/client/user.virtiofs.//
        /ok/all///
        "#;

        let actual = XattrMap::try_from(input).unwrap();
        let expected = XattrMap {
            rules: vec![
                Rule {
                    type_: Type::Prefix,
                    scope: Scope::CLIENT | Scope::SERVER,
                    key: CString::new("trusted.").unwrap(),
                    prepend: CString::new("user.virtiofs.").unwrap(),
                },
                Rule {
                    type_: Type::Bad,
                    scope: Scope::SERVER,
                    key: CString::new("").unwrap(),
                    prepend: CString::new("trusted.").unwrap(),
                },
                Rule {
                    type_: Type::Bad,
                    scope: Scope::CLIENT,
                    key: CString::new("user.virtiofs.").unwrap(),
                    prepend: CString::new("").unwrap(),
                },
                Rule {
                    type_: Type::Okay,
                    scope: Scope::CLIENT | Scope::SERVER,
                    key: CString::new("").unwrap(),
                    prepend: CString::new("").unwrap(),
                },
            ],
        };

        assert_eq!(actual, expected);
    }

    #[test]
    fn test_parser_emits_incomplete_rule_error() {
        let input = ":prefix:client:hi";
        let actual = XattrMap::try_from(input).unwrap_err();
        let expected = Error {
            rule: Some(1),
            cause: ErrorKind::IncompleteRule,
        };
        assert_eq!(actual, expected);
    }

    #[test]
    fn test_parser_emits_error_when_multiple_map_rules_exist() {
        let input = ":map:trusted.:virtiofs.user.::map:trusted.:virtiofs.user.:";
        let actual = XattrMap::try_from(input).unwrap_err();
        let expected = Error {
            rule: Some(1),
            cause: ErrorKind::MapRuleViolation,
        };
        assert_eq!(actual, expected);
    }

    #[test]
    fn test_parser_expands_map_rule_with_empty_key() {
        let input = ":map::user.virtiofs.:";
        let actual = XattrMap::try_from(input).unwrap();
        let expected = XattrMap {
            rules: vec![
                Rule {
                    type_: Type::Prefix,
                    scope: Scope::CLIENT | Scope::SERVER,
                    key: CString::new("").unwrap(),
                    prepend: CString::new("user.virtiofs.").unwrap(),
                },
                Rule {
                    type_: Type::Bad,
                    scope: Scope::CLIENT | Scope::SERVER,
                    key: CString::new("").unwrap(),
                    prepend: CString::new("").unwrap(),
                },
            ],
        };
        assert_eq!(actual, expected);
    }

    #[test]
    fn test_parser_expands_map_rule_with_key_and_prepend() {
        let input = ":map:trusted.:user.virtiofs.:";
        let actual = XattrMap::try_from(input).unwrap();
        let expected = XattrMap {
            rules: vec![
                Rule {
                    type_: Type::Prefix,
                    scope: Scope::CLIENT | Scope::SERVER,
                    key: CString::new("trusted.").unwrap(),
                    prepend: CString::new("user.virtiofs.").unwrap(),
                },
                Rule {
                    type_: Type::Bad,
                    scope: Scope::CLIENT | Scope::SERVER,
                    key: CString::new("user.virtiofs.").unwrap(),
                    prepend: CString::new("trusted.").unwrap(),
                },
                Rule {
                    type_: Type::Okay,
                    scope: Scope::CLIENT | Scope::SERVER,
                    key: CString::new("").unwrap(),
                    prepend: CString::new("").unwrap(),
                },
            ],
        };
        assert_eq!(actual, expected);
    }

    #[test]
    fn test_parser_emits_error_when_invalid_type_is_used() {
        let input = ":TOMATOPIRATE:trusted.:virtiofs.user.:";
        assert!(XattrMap::try_from(input).is_err());
    }

    #[test]
    fn test_parser_emits_error_when_invalid_scope_is_used() {
        let input = "/prefix/helloworld///";
        assert!(XattrMap::try_from(input).is_err());
    }

    #[test]
    fn test_parser_emits_error_when_no_rules_are_provided() {
        let input = " ";
        let actual = XattrMap::try_from(input).unwrap_err();
        let expected = Error {
            rule: None,
            cause: ErrorKind::NoRulesProvided,
        };
        assert_eq!(actual, expected);
    }

    #[test]
    fn test_parser_can_parse_rules_with_different_delimiters() {
        let input = ":prefix:all:trusted.:user.virtiofs.: /prefix/all/trusted./user.virtiofs./";
        let expected_rule = Rule {
            type_: Type::Prefix,
            scope: Scope::CLIENT | Scope::SERVER,
            key: CString::new("trusted.").unwrap(),
            prepend: CString::new("user.virtiofs.").unwrap(),
        };
        let expected = XattrMap {
            rules: vec![expected_rule.clone(), expected_rule],
        };

        let actual = XattrMap::try_from(input).unwrap();
        assert_eq!(actual, expected);
    }

    #[test]
    fn test_rule_ok_all() {
        let map = XattrMap {
            rules: vec![Rule {
                type_: Type::Okay,
                scope: Scope::CLIENT | Scope::SERVER,
                key: CString::new("").unwrap(),
                prepend: CString::new("").unwrap(),
            }],
        };
        let input = CString::new("user.virtiofs.potato").unwrap();
        let actual = map.map_client_xattr(&input).unwrap();
        let expected = AppliedRule::Pass(CString::new("user.virtiofs.potato").unwrap().into());
        assert_eq!(actual, expected);
    }

    #[test]
    fn test_rule_bad_hides_xattr_names_from_client() {
        let input = b"security.secret\x00boring_attr".to_vec();
        let map = XattrMap {
            rules: vec![
                Rule {
                    type_: Type::Bad,
                    scope: Scope::SERVER,
                    key: CString::new("").unwrap(),
                    prepend: CString::new("security.").unwrap(),
                },
                Rule {
                    type_: Type::Okay,
                    scope: Scope::CLIENT | Scope::SERVER,
                    key: CString::new("").unwrap(),
                    prepend: CString::new("").unwrap(),
                },
            ],
        };

        let actual = map.map_server_xattrlist(input).unwrap();
        let expected = b"boring_attr\x00";
        assert_eq!(actual.as_slice(), expected);
    }

    #[test]
    fn test_rule_unsupported_hides_xattr_names_from_client() {
        let input = b"security.secret\x00boring_attr".to_vec();
        let map = XattrMap {
            rules: vec![
                Rule {
                    type_: Type::Unsupported,
                    scope: Scope::SERVER,
                    key: CString::new("").unwrap(),
                    prepend: CString::new("security.").unwrap(),
                },
                Rule {
                    type_: Type::Okay,
                    scope: Scope::CLIENT | Scope::SERVER,
                    key: CString::new("").unwrap(),
                    prepend: CString::new("").unwrap(),
                },
            ],
        };

        let actual = map.map_server_xattrlist(input).unwrap();
        let expected = b"boring_attr\x00";
        assert_eq!(actual.as_slice(), expected);
    }

    #[test]
    fn test_rule_bad_denies_the_client_request() {
        let map = XattrMap {
            rules: vec![Rule {
                type_: Type::Bad,
                scope: Scope::CLIENT,
                key: CString::new("").unwrap(),
                prepend: CString::new("").unwrap(),
            }],
        };

        let input = CString::new("virtiofs.").unwrap();
        let actual = map.map_client_xattr(&input).unwrap();
        let expected = AppliedRule::Deny;
        assert_eq!(actual, expected);
    }

    #[test]
    fn test_rule_unsupported_not_support_the_client_request() {
        let map = XattrMap {
            rules: vec![Rule {
                type_: Type::Unsupported,
                scope: Scope::CLIENT,
                key: CString::new("").unwrap(),
                prepend: CString::new("").unwrap(),
            }],
        };

        let input = CString::new("virtiofs.").unwrap();
        let actual = map.map_client_xattr(&input).unwrap();
        let expected = AppliedRule::Unsupported;
        assert_eq!(actual, expected);
    }

    #[test]
    fn test_rule_prefix_prepends_xattr_names_from_client() {
        let map = XattrMap {
            rules: vec![Rule {
                type_: Type::Prefix,
                scope: Scope::CLIENT | Scope::SERVER,
                key: CString::new("trusted.").unwrap(),
                prepend: CString::new("virtiofs.user.").unwrap(),
            }],
        };

        let input = CString::new("trusted.secret_thing").unwrap();
        let actual = map.map_client_xattr(&input).unwrap();
        let expected = AppliedRule::Pass(Cow::Owned(
            CString::new("virtiofs.user.trusted.secret_thing").unwrap(),
        ));
        assert_eq!(actual, expected);
    }

    #[test]
    fn test_rule_prefix_strips_prefixes_from_server() {
        let map = XattrMap {
            rules: vec![Rule {
                type_: Type::Prefix,
                scope: Scope::SERVER,
                key: CString::new("").unwrap(),
                prepend: CString::new("virtiofs.user.").unwrap(),
            }],
        };

        let list = b"virtiofs.user.x".to_vec();
        let actual = map.map_server_xattrlist(list).unwrap();
        let expected = b"x\x00".to_vec();
        assert_eq!(actual, expected);
    }

    #[test]
    fn test_rule_ok_allows_xattr_names_to_pass_through_unchanged() {
        let map = XattrMap {
            rules: vec![Rule {
                type_: Type::Okay,
                scope: Scope::CLIENT | Scope::SERVER,
                key: CString::new("allow.").unwrap(),
                prepend: CString::new("allow.").unwrap(),
            }],
        };

        let input = CString::new("allow.y").unwrap();
        let actual = map.map_client_xattr(&input).unwrap();
        let expected = AppliedRule::Pass(Cow::Owned(CString::new("allow.y").unwrap()));
        assert_eq!(actual, expected);

        let list = b"allow.y\x00".to_vec();
        let expected = list.clone();
        let actual = map.map_server_xattrlist(list).unwrap();
        assert_eq!(actual, expected);
    }
}
