mod fanout;
mod model;
mod registry;
mod routes;
mod stream;

pub use model::ProcessRegistry;
pub use routes::{
    close_stdin, connect, list, send_input, send_signal, start, stream_input, update,
};

#[cfg(test)]
mod tests;
