use axum::{
    body::{to_bytes, Body},
    extract::{FromRequest, Multipart, Query, Request},
    http::{header, HeaderMap, StatusCode},
    response::Response,
};
use chrono::{DateTime, Utc};
use serde::Deserialize;
use std::{
    fs, io,
    path::{Path, PathBuf},
    time::UNIX_EPOCH,
};

const MAX_FILE_BODY_SIZE: usize = 64 * 1024 * 1024;

#[derive(serde::Serialize)]
struct UploadEntry {
    name: String,
    path: String,
    #[serde(rename = "type")]
    file_type: &'static str,
}

#[derive(Debug, Default, Deserialize)]
pub(crate) struct FileQuery {
    path: String,
    username: Option<String>,
}

pub async fn get(headers: HeaderMap, Query(query): Query<FileQuery>) -> Response {
    // cube-envd only serves identity (no gzip). Match upstream's refusal
    // behavior when the client explicitly says identity is unacceptable.
    if !identity_acceptable(&headers) {
        return error_response_with_code(
            StatusCode::NOT_ACCEPTABLE,
            "invalid_argument",
            "identity content encoding is not acceptable".to_owned(),
        );
    }
    let path = match fs::canonicalize(&query.path) {
        Ok(path) => path,
        Err(error) => return fs_error_response(error),
    };
    let username = query.username.as_deref().unwrap_or("root");
    if let Err(error) = check_read_permission(&path, username) {
        return fs_error_response(error);
    }
    let metadata = match fs::metadata(&path) {
        Ok(metadata) => metadata,
        Err(error) => return fs_error_response(error),
    };
    if metadata.len() > MAX_FILE_BODY_SIZE as u64 {
        return request_error_response(
            StatusCode::PAYLOAD_TOO_LARGE,
            format!("file exceeds the {} byte limit", MAX_FILE_BODY_SIZE),
        );
    }
    let modified = match modified_seconds(&metadata) {
        Ok(modified) => modified,
        Err(error) => return fs_error_response(error),
    };
    let last_modified = format_http_date(modified);
    if let Some(value) = headers.get(header::IF_MODIFIED_SINCE) {
        if let Ok(value) = value.to_str() {
            if parse_http_date(value).is_some_and(|requested| requested >= modified) {
                return response_with_file_headers(
                    StatusCode::NOT_MODIFIED,
                    &last_modified,
                    None,
                    Body::empty(),
                );
            }
        }
    }
    let data = match fs::read(&path) {
        Ok(data) => data,
        Err(error) => return fs_error_response(error),
    };
    let range = match parse_range(headers.get(header::RANGE), data.len()) {
        Ok(range) => range,
        Err(()) => {
            return response_with_file_headers(
                StatusCode::RANGE_NOT_SATISFIABLE,
                &last_modified,
                Some(format!("bytes */{}", data.len())),
                Body::empty(),
            )
        }
    };
    match range {
        Some((start, end)) => response_with_file_headers(
            StatusCode::PARTIAL_CONTENT,
            &last_modified,
            Some(format!("bytes {start}-{end}/{}", data.len())),
            Body::from(data[start..=end].to_vec()),
        ),
        None => response_with_file_headers(StatusCode::OK, &last_modified, None, Body::from(data)),
    }
}

fn response_with_file_headers(
    status: StatusCode,
    last_modified: &str,
    content_range: Option<String>,
    body: Body,
) -> Response {
    let mut builder = Response::builder()
        .status(status)
        .header(header::CONTENT_TYPE, "text/plain; charset=utf-8")
        .header(header::ACCEPT_RANGES, "bytes")
        .header(header::LAST_MODIFIED, last_modified);
    if let Some(content_range) = content_range {
        builder = builder.header(header::CONTENT_RANGE, content_range);
    }
    builder.body(body).expect("valid file response")
}

fn modified_seconds(metadata: &fs::Metadata) -> io::Result<u64> {
    metadata
        .modified()?
        .duration_since(UNIX_EPOCH)
        .map(|duration| duration.as_secs())
        .map_err(|error| io::Error::other(error.to_string()))
}

fn format_http_date(seconds: u64) -> String {
    DateTime::<Utc>::from_timestamp(seconds as i64, 0)
        .expect("file modification time is representable as an HTTP date")
        .format("%a, %d %b %Y %H:%M:%S GMT")
        .to_string()
}

fn parse_http_date(value: &str) -> Option<u64> {
    DateTime::parse_from_rfc2822(value)
        .ok()
        .and_then(|date| u64::try_from(date.timestamp()).ok())
}

/// Whether the client's `Accept-Encoding` still accepts an unencoded
/// (identity) response. Identity is acceptable unless a `q=0` for `identity`,
/// or for `*` with no explicit `identity` entry, is present.
fn identity_acceptable(headers: &HeaderMap) -> bool {
    let Some(value) = headers.get(header::ACCEPT_ENCODING) else {
        return true;
    };
    let Ok(value) = value.to_str() else {
        return true;
    };

    let mut identity_quality = None;
    let mut wildcard_quality = None;
    for entry in value.split(',') {
        let mut pieces = entry.split(';');
        let name = pieces.next().unwrap_or("").trim().to_ascii_lowercase();
        if name.is_empty() {
            continue;
        }
        let mut quality = 1.0_f32;
        for parameter in pieces {
            if let Some(rest) = parameter.trim().strip_prefix("q=") {
                if let Ok(parsed) = rest.trim().parse::<f32>() {
                    quality = parsed;
                }
            }
        }
        match name.as_str() {
            "identity" => identity_quality = Some(quality),
            "*" => wildcard_quality = Some(quality),
            _ => {}
        }
    }

    identity_quality.or(wildcard_quality).unwrap_or(1.0) > 0.0
}

/// `/files/compose` stays in the route table so callers get an explicit 501
/// instead of a misleading 404.
pub async fn compose() -> Response {
    error_response_with_code(
        StatusCode::NOT_IMPLEMENTED,
        "unimplemented",
        "/files/compose is not implemented by cube-envd".to_owned(),
    )
}

fn parse_range(
    value: Option<&axum::http::HeaderValue>,
    length: usize,
) -> Result<Option<(usize, usize)>, ()> {
    let Some(value) = value else {
        return Ok(None);
    };
    let value = value.to_str().map_err(|_| ())?;
    let range = value.strip_prefix("bytes=").ok_or(())?;
    if range.contains(',') {
        // Multi-range requests are not implemented; serve the whole file with
        // 200 (upstream returns 206 multipart/byteranges).
        return Ok(None);
    }
    if length == 0 {
        return Err(());
    }
    let (start, end) = range.split_once('-').ok_or(())?;
    if start.is_empty() {
        let suffix = end.parse::<usize>().map_err(|_| ())?;
        if suffix == 0 {
            return Err(());
        }
        let start = length.saturating_sub(suffix);
        return Ok(Some((start, length - 1)));
    }
    let start = start.parse::<usize>().map_err(|_| ())?;
    if start >= length {
        return Err(());
    }
    let end = if end.is_empty() {
        length - 1
    } else {
        end.parse::<usize>().map_err(|_| ())?.min(length - 1)
    };
    if start > end {
        return Err(());
    }
    Ok(Some((start, end)))
}

pub async fn post(request: Request) -> Response {
    let query = match request.uri().query() {
        Some(raw_query) => match serde_urlencoded::from_str::<FileQuery>(raw_query) {
            Ok(query) => query,
            Err(error) => {
                return request_error_response(StatusCode::BAD_REQUEST, error.to_string());
            }
        },
        None => {
            return request_error_response(
                StatusCode::BAD_REQUEST,
                "path query parameter is required".to_owned(),
            );
        }
    };
    if query.path.is_empty() {
        return request_error_response(
            StatusCode::BAD_REQUEST,
            "path query parameter is required".to_owned(),
        );
    }
    let username = query.username.as_deref().unwrap_or("root");
    if let Err(error) = validate_username(username) {
        return fs_error_response(error);
    }

    let is_multipart = request
        .headers()
        .get(header::CONTENT_TYPE)
        .and_then(|value| value.to_str().ok())
        .map(|value| value.starts_with("multipart/form-data"))
        .unwrap_or(false);

    let data = if is_multipart {
        let mut multipart = match Multipart::from_request(request, &()).await {
            Ok(multipart) => multipart,
            Err(error) => {
                return request_error_response(StatusCode::BAD_REQUEST, error.to_string());
            }
        };
        let mut file_data = None;
        loop {
            match multipart.next_field().await {
                Ok(Some(field)) => {
                    if field.name() == Some("file") {
                        match field.bytes().await {
                            Ok(data) => {
                                file_data = Some(data);
                                break;
                            }
                            Err(error) => {
                                return request_error_response(
                                    StatusCode::BAD_REQUEST,
                                    error.to_string(),
                                );
                            }
                        }
                    }
                }
                Ok(None) => break,
                Err(error) => {
                    return request_error_response(StatusCode::BAD_REQUEST, error.to_string());
                }
            }
        }
        match file_data {
            Some(data) => data,
            None => {
                return request_error_response(
                    StatusCode::BAD_REQUEST,
                    "multipart field 'file' is required".to_owned(),
                );
            }
        }
    } else {
        match to_bytes(request.into_body(), MAX_FILE_BODY_SIZE).await {
            Ok(data) => data,
            Err(error) => {
                return request_error_response(StatusCode::PAYLOAD_TOO_LARGE, error.to_string());
            }
        }
    };

    let path = match writable_path(&query.path) {
        Ok(path) => path,
        Err(error) => return fs_error_response(error),
    };
    if let Some(parent) = path.parent() {
        if let Err(error) = fs::create_dir_all(parent) {
            return fs_error_response(error);
        }
    }
    if let Err(error) = write_file(&path, &data) {
        return fs_error_response(error);
    }
    if let Err(error) = apply_owner(&path, username) {
        return fs_error_response(error);
    }
    let entry = UploadEntry {
        name: path
            .file_name()
            .and_then(|name| name.to_str())
            .unwrap_or_default()
            .to_owned(),
        path: path.to_string_lossy().into_owned(),
        file_type: "file",
    };
    Response::builder()
        .status(StatusCode::OK)
        .header(header::CONTENT_TYPE, "text/plain; charset=utf-8")
        .body(Body::from(
            serde_json::to_string(&[entry]).expect("upload entry is serializable"),
        ))
        .expect("valid upload response")
}

fn writable_path(raw_path: &str) -> io::Result<PathBuf> {
    let requested = lexical_normalize(Path::new(raw_path));
    if requested.exists() {
        return fs::canonicalize(requested);
    }

    let mut missing = Vec::new();
    let mut current = requested.as_path();
    while !current.exists() {
        let name = current.file_name().ok_or_else(|| {
            io::Error::new(io::ErrorKind::InvalidInput, "path has no existing parent")
        })?;
        missing.push(name.to_owned());
        current = current.parent().ok_or_else(|| {
            io::Error::new(io::ErrorKind::InvalidInput, "path has no existing parent")
        })?;
    }
    let mut resolved = fs::canonicalize(current)?;
    for component in missing.iter().rev() {
        resolved.push(component);
    }
    Ok(resolved)
}

fn lexical_normalize(path: &Path) -> PathBuf {
    let mut normalized = PathBuf::new();
    for component in path.components() {
        match component {
            std::path::Component::Prefix(prefix) => normalized.push(prefix.as_os_str()),
            std::path::Component::RootDir => normalized.push(Path::new("/")),
            std::path::Component::CurDir => {}
            std::path::Component::ParentDir => {
                if !normalized.pop() && !path.is_absolute() {
                    normalized.push("..");
                }
            }
            std::path::Component::Normal(component) => normalized.push(component),
        }
    }
    normalized
}

fn write_file(path: &Path, data: &[u8]) -> io::Result<()> {
    use std::os::unix::fs::PermissionsExt;
    // Preserve an existing file's mode on overwrite; new files get 0644.
    let mode = fs::metadata(path)
        .ok()
        .map(|metadata| metadata.permissions().mode() & 0o7777)
        .unwrap_or(0o644);
    fs::write(path, data)?;
    fs::set_permissions(path, fs::Permissions::from_mode(mode))?;
    Ok(())
}

fn validate_username(username: &str) -> io::Result<()> {
    if username.is_empty() || username == "root" {
        return Ok(());
    }
    #[cfg(unix)]
    if nix::unistd::User::from_name(username)
        .map_err(|error| io::Error::other(error.to_string()))?
        .is_none()
    {
        return Err(io::Error::new(
            io::ErrorKind::NotFound,
            format!("user {username} not found"),
        ));
    }
    Ok(())
}

fn check_read_permission(path: &Path, username: &str) -> io::Result<()> {
    validate_username(username)?;
    if username.is_empty() || username == "root" {
        return Ok(());
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::MetadataExt;
        let user = nix::unistd::User::from_name(username)
            .map_err(|error| io::Error::other(error.to_string()))?
            .ok_or_else(|| io::Error::new(io::ErrorKind::NotFound, "user not found"))?;
        let metadata = fs::metadata(path)?;
        let mode = metadata.mode();
        let readable = if metadata.uid() == user.uid.as_raw() {
            mode & 0o400 != 0
        } else if metadata.gid() == user.gid.as_raw() {
            mode & 0o040 != 0
        } else {
            mode & 0o004 != 0
        };
        if !readable {
            return Err(io::Error::new(
                io::ErrorKind::PermissionDenied,
                format!("user {username} cannot read {}", path.display()),
            ));
        }
    }
    Ok(())
}

fn apply_owner(path: &Path, username: &str) -> io::Result<()> {
    if username.is_empty() || username == "root" {
        return Ok(());
    }
    #[cfg(unix)]
    {
        let user = nix::unistd::User::from_name(username)
            .map_err(|error| io::Error::other(error.to_string()))?
            .ok_or_else(|| io::Error::new(io::ErrorKind::NotFound, "user not found"))?;
        nix::unistd::chown(path, Some(user.uid), Some(user.gid))
            .map_err(|error| io::Error::other(error.to_string()))?;
    }
    Ok(())
}

fn fs_error_response(error: io::Error) -> Response {
    let (status, code) = match error.kind() {
        io::ErrorKind::NotFound => (StatusCode::NOT_FOUND, "not_found"),
        io::ErrorKind::PermissionDenied => (StatusCode::FORBIDDEN, "permission_denied"),
        io::ErrorKind::InvalidInput => (StatusCode::BAD_REQUEST, "invalid_argument"),
        _ => (StatusCode::INTERNAL_SERVER_ERROR, "internal"),
    };
    error_response_with_code(status, code, error.to_string())
}

fn request_error_response(status: StatusCode, message: String) -> Response {
    error_response_with_code(status, "invalid_argument", message)
}

fn error_response_with_code(status: StatusCode, code: &str, message: String) -> Response {
    let payload = serde_json::json!({"code": code, "message": message});
    Response::builder()
        .status(status)
        .header(header::CONTENT_TYPE, "application/json")
        .body(Body::from(payload.to_string()))
        .expect("valid file error response")
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::{
        body::to_bytes,
        http::{HeaderValue, Request},
    };
    use tempfile::tempdir;

    #[tokio::test]
    async fn raw_file_write_and_read_round_trip() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("nested").join("file.txt");
        let request = Request::builder()
            .uri(format!("/files?path={}", path.display()))
            .header(header::CONTENT_TYPE, "application/octet-stream")
            .body(Body::from("content"))
            .unwrap();
        assert_eq!(post(request).await.status(), StatusCode::OK);

        let response = get(
            HeaderMap::new(),
            Query(FileQuery {
                path: path.to_string_lossy().into_owned(),
                username: None,
            }),
        )
        .await;
        assert_eq!(response.status(), StatusCode::OK);
        assert_eq!(
            to_bytes(response.into_body(), MAX_FILE_BODY_SIZE)
                .await
                .unwrap(),
            "content"
        );
    }

    #[tokio::test]
    async fn missing_file_returns_not_found() {
        let response = get(
            HeaderMap::new(),
            Query(FileQuery {
                path: "/tmp/cube-envd-file-that-does-not-exist".to_owned(),
                username: None,
            }),
        )
        .await;
        assert_eq!(response.status(), StatusCode::NOT_FOUND);
        let body = to_bytes(response.into_body(), MAX_FILE_BODY_SIZE)
            .await
            .unwrap();
        assert_eq!(
            serde_json::from_slice::<serde_json::Value>(&body).unwrap()["code"],
            "not_found"
        );
    }

    #[tokio::test]
    async fn multipart_file_write_is_supported() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("multipart.txt");
        let boundary = "cube-envd-test-boundary";
        let body = format!(
            "--{boundary}\r\nContent-Disposition: form-data; name=\"file\"; filename=\"multipart.txt\"\r\nContent-Type: application/octet-stream\r\n\r\ncontent-mp\r\n--{boundary}--\r\n"
        );
        let request = Request::builder()
            .uri(format!("/files?path={}", path.display()))
            .header(
                header::CONTENT_TYPE,
                format!("multipart/form-data; boundary={boundary}"),
            )
            .body(Body::from(body))
            .unwrap();

        assert_eq!(post(request).await.status(), StatusCode::OK);
        assert_eq!(fs::read_to_string(path).unwrap(), "content-mp");
    }

    #[tokio::test]
    async fn write_path_resolves_parent_dot_dot_components() {
        let directory = tempdir().unwrap();
        let direct_path = directory.path().join("escape-check.txt");
        let dot_dot_path = directory
            .path()
            .join("nested")
            .join("..")
            .join("escape-check.txt");
        let request = Request::builder()
            .uri(format!("/files?path={}", dot_dot_path.display()))
            .header(header::CONTENT_TYPE, "application/octet-stream")
            .body(Body::from("resolved"))
            .unwrap();

        assert_eq!(post(request).await.status(), StatusCode::OK);
        assert_eq!(fs::read_to_string(direct_path).unwrap(), "resolved");
        assert!(!directory.path().join("nested").is_dir());
    }

    #[tokio::test]
    async fn range_response_returns_requested_bytes_and_metadata() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("range.txt");
        fs::write(&path, b"0123456789").unwrap();
        let mut headers = HeaderMap::new();
        headers.insert(header::RANGE, "bytes=0-3".parse().unwrap());

        let response = get(
            headers,
            Query(FileQuery {
                path: path.to_string_lossy().into_owned(),
                username: None,
            }),
        )
        .await;

        assert_eq!(response.status(), StatusCode::PARTIAL_CONTENT);
        assert_eq!(response.headers()[header::CONTENT_RANGE], "bytes 0-3/10");
        assert_eq!(response.headers()[header::ACCEPT_RANGES], "bytes");
        assert_eq!(
            to_bytes(response.into_body(), MAX_FILE_BODY_SIZE)
                .await
                .unwrap(),
            "0123"
        );
    }

    #[tokio::test]
    async fn invalid_range_returns_416_with_total_length() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("range-invalid.txt");
        fs::write(&path, b"0123456789").unwrap();
        let mut headers = HeaderMap::new();
        headers.insert(header::RANGE, "bytes=999999-".parse().unwrap());

        let response = get(
            headers,
            Query(FileQuery {
                path: path.to_string_lossy().into_owned(),
                username: None,
            }),
        )
        .await;

        assert_eq!(response.status(), StatusCode::RANGE_NOT_SATISFIABLE);
        assert_eq!(response.headers()[header::CONTENT_RANGE], "bytes */10");
    }

    #[tokio::test]
    async fn matching_if_modified_since_returns_304() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("conditional.txt");
        fs::write(&path, b"unchanged").unwrap();
        let first = get(
            HeaderMap::new(),
            Query(FileQuery {
                path: path.to_string_lossy().into_owned(),
                username: None,
            }),
        )
        .await;
        let last_modified = first.headers()[header::LAST_MODIFIED].clone();
        let mut headers = HeaderMap::new();
        headers.insert(header::IF_MODIFIED_SINCE, last_modified);

        let response = get(
            headers,
            Query(FileQuery {
                path: path.to_string_lossy().into_owned(),
                username: None,
            }),
        )
        .await;

        assert_eq!(response.status(), StatusCode::NOT_MODIFIED);
        assert!(to_bytes(response.into_body(), MAX_FILE_BODY_SIZE)
            .await
            .unwrap()
            .is_empty());
    }

    fn range_header(value: &str) -> axum::http::HeaderValue {
        axum::http::HeaderValue::from_str(value).unwrap()
    }

    #[test]
    fn parse_range_handles_closed_open_and_suffix() {
        assert_eq!(
            parse_range(Some(&range_header("bytes=2-4")), 10).unwrap(),
            Some((2, 4))
        );
        assert_eq!(
            parse_range(Some(&range_header("bytes=5-")), 10).unwrap(),
            Some((5, 9))
        );
        assert_eq!(
            parse_range(Some(&range_header("bytes=-3")), 10).unwrap(),
            Some((7, 9))
        );
    }

    #[test]
    fn parse_range_clamps_end_beyond_length() {
        assert_eq!(
            parse_range(Some(&range_header("bytes=2-100")), 10).unwrap(),
            Some((2, 9))
        );
    }

    #[test]
    fn parse_range_rejects_invalid_forms() {
        for value in ["items=0-1", "bytes=10-", "bytes=5-2", "bytes=-0", "bytes="] {
            assert!(
                parse_range(Some(&range_header(value)), 10).is_err(),
                "{value} should be rejected"
            );
        }
        assert!(parse_range(Some(&range_header("bytes=0-1")), 0).is_err());
    }

    #[test]
    fn parse_range_serves_whole_file_for_multi_range() {
        assert_eq!(
            parse_range(Some(&range_header("bytes=0-1,3-4")), 10).unwrap(),
            None
        );
    }

    #[test]
    fn parse_range_returns_none_without_header() {
        assert_eq!(parse_range(None, 10).unwrap(), None);
    }

    #[test]
    fn http_date_round_trips() {
        let formatted = format_http_date(1_700_000_000);
        assert_eq!(parse_http_date(&formatted), Some(1_700_000_000));
        assert_eq!(parse_http_date("not a date"), None);
    }

    #[tokio::test]
    async fn post_without_path_returns_bad_request() {
        let request = Request::builder().body(Body::from("x")).unwrap();
        assert_eq!(post(request).await.status(), StatusCode::BAD_REQUEST);
    }

    #[tokio::test]
    async fn multipart_without_file_field_returns_bad_request() {
        let boundary = "cube-envd-no-file";
        let body = format!(
            "--{boundary}\r\nContent-Disposition: form-data; name=\"not_file\"\r\n\r\nvalue\r\n--{boundary}--\r\n"
        );
        let request = Request::builder()
            .uri("/files?path=/tmp/cube-envd-mp-not-file")
            .header(
                header::CONTENT_TYPE,
                format!("multipart/form-data; boundary={boundary}"),
            )
            .body(Body::from(body))
            .unwrap();
        assert_eq!(post(request).await.status(), StatusCode::BAD_REQUEST);
    }

    #[tokio::test]
    async fn write_preserves_existing_mode() {
        use std::os::unix::fs::PermissionsExt;

        let directory = tempdir().unwrap();
        let path = directory.path().join("mode.txt");
        fs::write(&path, b"old").unwrap();
        fs::set_permissions(&path, fs::Permissions::from_mode(0o600)).unwrap();

        let request = Request::builder()
            .uri(format!("/files?path={}", path.display()))
            .header(header::CONTENT_TYPE, "application/octet-stream")
            .body(Body::from("new"))
            .unwrap();
        assert_eq!(post(request).await.status(), StatusCode::OK);
        assert_eq!(fs::read_to_string(&path).unwrap(), "new");
        let mode = fs::metadata(&path).unwrap().permissions().mode() & 0o7777;
        assert_eq!(mode, 0o600);
    }

    #[test]
    fn identity_is_acceptable_by_default() {
        assert!(identity_acceptable(&HeaderMap::new()));
        for value in [
            "gzip",
            "gzip, deflate, br",
            "identity;q=0.5",
            "*;q=0.5",
            "gzip;q=0",
        ] {
            let mut headers = HeaderMap::new();
            headers.insert(
                header::ACCEPT_ENCODING,
                HeaderValue::from_str(value).unwrap(),
            );
            assert!(
                identity_acceptable(&headers),
                "expected acceptable: {value}"
            );
        }
    }

    #[test]
    fn identity_is_refused_when_explicitly_unacceptable() {
        for value in ["identity;q=0", "*;q=0", "gzip, *;q=0"] {
            let mut headers = HeaderMap::new();
            headers.insert(
                header::ACCEPT_ENCODING,
                HeaderValue::from_str(value).unwrap(),
            );
            assert!(!identity_acceptable(&headers), "expected refused: {value}");
        }
    }

    #[tokio::test]
    async fn get_refuses_unacceptable_identity_with_406() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("identity.txt");
        fs::write(&path, b"content").unwrap();

        let mut headers = HeaderMap::new();
        headers.insert(
            header::ACCEPT_ENCODING,
            HeaderValue::from_static("identity;q=0"),
        );
        let response = get(
            headers,
            Query(FileQuery {
                path: path.to_string_lossy().into_owned(),
                username: None,
            }),
        )
        .await;
        assert_eq!(response.status(), StatusCode::NOT_ACCEPTABLE);
    }

    #[tokio::test]
    async fn files_compose_is_explicitly_unimplemented() {
        let response = compose().await;
        assert_eq!(response.status(), StatusCode::NOT_IMPLEMENTED);
        let body = to_bytes(response.into_body(), MAX_FILE_BODY_SIZE)
            .await
            .unwrap();
        assert_eq!(
            serde_json::from_slice::<serde_json::Value>(&body).unwrap()["code"],
            "unimplemented"
        );
    }

    #[tokio::test]
    async fn multi_range_request_serves_the_whole_file() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("multi-range.txt");
        fs::write(&path, b"0123456789").unwrap();

        let mut headers = HeaderMap::new();
        headers.insert(header::RANGE, HeaderValue::from_static("bytes=0-1,3-4"));
        let response = get(
            headers,
            Query(FileQuery {
                path: path.to_string_lossy().into_owned(),
                username: None,
            }),
        )
        .await;
        assert_eq!(response.status(), StatusCode::OK);
        assert_eq!(
            to_bytes(response.into_body(), MAX_FILE_BODY_SIZE)
                .await
                .unwrap(),
            "0123456789"
        );
    }
}
