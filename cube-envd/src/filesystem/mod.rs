mod entries;
mod error;
mod model;

pub mod files;
mod routes;
mod watch;

pub use routes::{list_dir, make_dir, move_entry, remove, stat};
pub use watch::watch_dir;

#[cfg(test)]
mod tests;
