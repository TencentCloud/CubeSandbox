use axum::{
    body::Body,
    extract::Request,
    http::{header, Method, StatusCode},
    middleware::Next,
    response::Response,
};

const ALLOW_HEADERS: &str = "Content-Type, Authorization, X-Access-Token, e2b-traffic-access-token, cube-traffic-access-token, Connect-Protocol-Version, Connect-Content-Encoding, Range, If-Modified-Since";

pub async fn layer(request: Request, next: Next) -> Response {
    if request.method() == Method::OPTIONS {
        return preflight_response();
    }
    let response = next.run(request).await;
    with_cors_headers(response)
}

fn preflight_response() -> Response {
    with_cors_headers(
        Response::builder()
            .status(StatusCode::NO_CONTENT)
            .body(Body::empty())
            .expect("valid preflight response"),
    )
}

fn with_cors_headers(mut response: Response) -> Response {
    let headers = response.headers_mut();
    headers.insert(header::ACCESS_CONTROL_ALLOW_ORIGIN, "*".parse().unwrap());
    headers.insert(
        header::ACCESS_CONTROL_ALLOW_METHODS,
        "GET, POST, OPTIONS".parse().unwrap(),
    );
    headers.insert(
        header::ACCESS_CONTROL_ALLOW_HEADERS,
        ALLOW_HEADERS.parse().unwrap(),
    );
    headers.insert(header::ACCESS_CONTROL_MAX_AGE, "86400".parse().unwrap());
    headers.insert(
        header::ACCESS_CONTROL_EXPOSE_HEADERS,
        "Accept-Ranges, Content-Length, Content-Range, Last-Modified"
            .parse()
            .unwrap(),
    );
    response
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn preflight_has_cors_contract() {
        let response = preflight_response();
        assert_eq!(response.status(), StatusCode::NO_CONTENT);
        assert_eq!(response.headers()[header::ACCESS_CONTROL_ALLOW_ORIGIN], "*");
        assert!(response.headers()[header::ACCESS_CONTROL_ALLOW_METHODS]
            .to_str()
            .unwrap()
            .contains("OPTIONS"));
        assert!(response.headers()[header::ACCESS_CONTROL_ALLOW_HEADERS]
            .to_str()
            .unwrap()
            .contains("Range"));
    }

    #[test]
    fn regular_response_gets_cors_headers() {
        let response = with_cors_headers(Response::new(Body::empty()));
        assert_eq!(response.headers()[header::ACCESS_CONTROL_ALLOW_ORIGIN], "*");
    }

    #[test]
    fn allow_headers_cover_connect_and_conditional_requests() {
        let response = preflight_response();
        let headers = response.headers()[header::ACCESS_CONTROL_ALLOW_HEADERS]
            .to_str()
            .unwrap();
        for expected in ["Connect-Protocol-Version", "Range", "If-Modified-Since"] {
            assert!(headers.contains(expected), "missing {expected}");
        }
    }
}
