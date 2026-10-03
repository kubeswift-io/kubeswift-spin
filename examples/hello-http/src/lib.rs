//! hello-http: the canonical kubeswift-spin smoke test.
//!
//! Responses are deterministic so an end-to-end test can compare them byte
//! for byte.

use spin_sdk::http::{HeaderMap, HeaderValue, IntoResponse, Request, StatusCode};
use spin_sdk::http_service;

/// Body returned by `/` and `/hello`.
pub const GREETING: &str = "Hello from Spin on KubeSwift\n";

/// Maps a request path to a status code and body.
pub fn route(path: &str) -> (StatusCode, &'static str) {
    match path {
        "/" | "/hello" => (StatusCode::OK, GREETING),
        "/healthz" => (StatusCode::OK, "ok\n"),
        _ => (StatusCode::NOT_FOUND, "not found\n"),
    }
}

#[http_service]
async fn handle(req: Request) -> anyhow::Result<impl IntoResponse> {
    let (status, body) = route(req.uri().path());
    let mut headers = HeaderMap::new();
    headers.insert("content-type", HeaderValue::from_static("text/plain; charset=utf-8"));
    Ok((status, headers, body.to_string()))
}
