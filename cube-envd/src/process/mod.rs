mod model;
mod stream;
mod registry;
mod routes;

pub use model::ProcessRegistry;
pub use routes::{
    close_stdin, connect, list, send_input, send_signal, start, stream_input, update,
};

#[cfg(test)]
mod tests;
