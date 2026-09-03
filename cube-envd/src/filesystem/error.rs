use std::path::Path;

use crate::connect::{Code, RpcError};

pub(super) fn filesystem_error(path: &Path, error: std::io::Error) -> RpcError {
    if error.kind() == std::io::ErrorKind::NotFound {
        RpcError::new(
            Code::NotFound,
            format!("path {} was not found", path.display()),
        )
    } else {
        RpcError::new(
            Code::Internal,
            format!("access {}: {error}", path.display()),
        )
    }
}
