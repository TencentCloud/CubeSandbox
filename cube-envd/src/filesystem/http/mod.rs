//! The HTTP semantics of `GET /files`: Range, conditional requests,
//! content-encoding negotiation, HTTP dates and Content-Disposition.
//!
//! Answers how a download request is negotiated (206 / 304 / 412 / 416 / 406).
//! The five submodules move verbatim from
//! `rest/{preconditions,ranges,encoding,httpdate,content_disposition}.rs`;
//! their semantics track upstream `http.ServeContent` and `net/http`, and this
//! file only aggregates them. Pure functions: no I/O, no state.

pub mod content_disposition;
pub mod encoding;
pub mod httpdate;
pub mod preconditions;
pub mod ranges;
