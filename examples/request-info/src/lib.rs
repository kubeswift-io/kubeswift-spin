//! request-info returns safe facts about the request it received.
//!
//! Only an explicit allowlist of headers is echoed. Authorization, cookies
//! and every other header are never returned, so the response is safe to
//! show in logs and tests.

use serde::Serialize;
use spin_sdk::http::{HeaderMap, IntoResponse, Json, Request, StatusCode};
use spin_sdk::{http_service, variables};

/// Headers that may be echoed back. Everything else is dropped.
pub const ECHOED_HEADERS: [&str; 4] = ["accept", "content-type", "user-agent", "x-request-id"];

#[derive(Serialize)]
struct Info {
    method: String,
    path: String,
    query: Option<String>,
    headers: Vec<(String, String)>,
    app_version: String,
    greeting: String,
}

/// Returns the allowlisted headers in a stable order.
pub fn safe_headers(headers: &HeaderMap) -> Vec<(String, String)> {
    ECHOED_HEADERS
        .iter()
        .filter_map(|name| {
            headers
                .get(*name)
                .and_then(|v| v.to_str().ok())
                .map(|v| (name.to_string(), v.chars().take(256).collect()))
        })
        .collect()
}

#[http_service]
async fn handle(req: Request) -> anyhow::Result<impl IntoResponse> {
    if req.uri().path() == "/healthz" {
        return Ok((StatusCode::OK, HeaderMap::new(), "ok\n".to_string()).into_response()?);
    }
    let info = Info {
        method: req.method().to_string(),
        path: req.uri().path().to_string(),
        query: req.uri().query().map(str::to_string),
        headers: safe_headers(req.headers()),
        app_version: variables::get("app_version").await?,
        greeting: variables::get("greeting").await?,
    };
    Ok(Json(info).into_response()?)
}
