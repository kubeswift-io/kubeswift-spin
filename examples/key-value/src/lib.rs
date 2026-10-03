//! key-value exercises Spin's key-value API through the default store.
//!
//!   GET    /kv          list keys
//!   GET    /kv/<key>    read a value
//!   PUT    /kv/<key>    write the request body (at most 64 KiB)
//!   DELETE /kv/<key>    delete a value

use spin_sdk::http::body::IncomingBodyExt;
use spin_sdk::http::{HeaderMap, IntoResponse, Json, Method, Request, StatusCode};
use spin_sdk::http_service;
use spin_sdk::wasip3::http::types::Response as WasiResponse;
use spin_sdk::key_value::Store;

const MAX_VALUE: usize = 64 * 1024;

fn text(status: StatusCode, body: &str) -> anyhow::Result<WasiResponse> {
    Ok((status, HeaderMap::new(), body.to_string()).into_response()?)
}

/// Accepts keys made of ASCII letters, digits, '-', '_' and '.'.
pub fn valid_key(key: &str) -> bool {
    !key.is_empty() && key.len() <= 128 && key.bytes().all(|b| b.is_ascii_alphanumeric() || b"-_.".contains(&b))
}

#[http_service]
async fn handle(req: Request) -> anyhow::Result<impl IntoResponse> {
    let path = req.uri().path().to_string();
    if path == "/healthz" {
        return text(StatusCode::OK, "ok\n");
    }
    let store = Store::open_default().await?;
    if path == "/kv" && req.method() == Method::GET {
        let mut keys = store.get_keys().await.collect().await?;
        keys.sort();
        return Ok(Json(keys).into_response()?);
    }
    let Some(key) = path.strip_prefix("/kv/") else {
        return text(StatusCode::NOT_FOUND, "not found\n");
    };
    if !valid_key(key) {
        return text(StatusCode::BAD_REQUEST, "invalid key\n");
    }
    match *req.method() {
        Method::GET => match store.get(key).await? {
            Some(v) => Ok(v.into_response()?),
            None => text(StatusCode::NOT_FOUND, "no such key\n"),
        },
        Method::PUT => {
            let key = key.to_string();
            let body = req.into_body().bytes().await.map_err(|e| anyhow::anyhow!("read body: {e:?}"))?;
            if body.len() > MAX_VALUE {
                return text(StatusCode::PAYLOAD_TOO_LARGE, "value too large\n");
            }
            store.set(&key, &body).await?;
            text(StatusCode::NO_CONTENT, "")
        }
        Method::DELETE => {
            store.delete(key).await?;
            text(StatusCode::NO_CONTENT, "")
        }
        _ => text(StatusCode::METHOD_NOT_ALLOWED, "method not allowed\n"),
    }
}
