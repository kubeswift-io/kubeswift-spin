//! A minimal, stateless MCP server over the Streamable HTTP transport,
//! written directly against the MCP specification with no MCP SDK.
//!
//! It implements only what a tool server needs: `initialize`, `ping`,
//! `tools/list` and `tools/call`, answered with single JSON responses (no
//! SSE streams, no sessions). It exposes two pure tools, `word_count` and
//! `sha256`. See README.md for its status and limits.

use serde_json::{Value, json};
use sha2::{Digest, Sha256};
use spin_sdk::http::body::IncomingBodyExt;
use spin_sdk::http::{HeaderMap, HeaderValue, IntoResponse, Method, Request, StatusCode};
use spin_sdk::wasip3::http::types::Response as WasiResponse;
use spin_sdk::{http_service, variables};

/// Protocol revisions whose tool subset this server implements, newest first.
pub const SUPPORTED_VERSIONS: [&str; 3] = ["2025-11-25", "2025-06-18", "2025-03-26"];
const MAX_BODY: usize = 64 * 1024;
const MAX_TEXT: usize = 32 * 1024;

fn reply(status: StatusCode, body: Option<Value>) -> anyhow::Result<WasiResponse> {
    let mut headers = HeaderMap::new();
    let text = match body {
        Some(v) => {
            headers.insert("content-type", HeaderValue::from_static("application/json"));
            v.to_string()
        }
        None => String::new(),
    };
    Ok((status, headers, text).into_response()?)
}

fn rpc_result(id: &Value, result: Value) -> Value {
    json!({"jsonrpc": "2.0", "id": id, "result": result})
}

fn rpc_error(id: &Value, code: i64, message: &str) -> Value {
    json!({"jsonrpc": "2.0", "id": id, "error": {"code": code, "message": message}})
}

/// The tool catalogue returned by `tools/list`.
pub fn tools() -> Value {
    let text_input = json!({
        "type": "object",
        "properties": {"text": {"type": "string", "description": "Input text, at most 32 KiB"}},
        "required": ["text"],
        "additionalProperties": false
    });
    json!([
        {"name": "word_count", "description": "Counts whitespace-separated words in a text.", "inputSchema": text_input},
        {"name": "sha256", "description": "Returns the hex SHA-256 digest of a text.", "inputSchema": text_input}
    ])
}

/// Runs a tool. Tool failures are reported in the result with isError, as
/// the specification requires; protocol errors are returned as Err.
pub fn call_tool(params: &Value) -> Result<Value, (i64, &'static str)> {
    let name = params.get("name").and_then(Value::as_str).ok_or((-32602, "missing tool name"))?;
    let text = params.get("arguments").and_then(|a| a.get("text")).and_then(Value::as_str);
    let out = match (name, text) {
        (_, Some(t)) if t.len() > MAX_TEXT => return Ok(tool_text("text exceeds 32 KiB", true)),
        ("word_count", Some(t)) => t.split_whitespace().count().to_string(),
        ("sha256", Some(t)) => Sha256::digest(t.as_bytes()).iter().map(|b| format!("{b:02x}")).collect(),
        ("word_count" | "sha256", None) => return Ok(tool_text("argument 'text' (string) is required", true)),
        _ => return Err((-32602, "unknown tool")),
    };
    Ok(tool_text(&out, false))
}

fn tool_text(text: &str, is_error: bool) -> Value {
    json!({"content": [{"type": "text", "text": text}], "isError": is_error})
}

/// Handles one JSON-RPC message. Returns None for notifications and
/// responses, which get 202 Accepted with no body.
pub fn handle_message(msg: &Value) -> Option<Value> {
    let id = msg.get("id")?;
    let Some(method) = msg.get("method").and_then(Value::as_str) else {
        return None; // a response from the client
    };
    let params = msg.get("params").cloned().unwrap_or(Value::Null);
    Some(match method {
        "initialize" => {
            let requested = params.get("protocolVersion").and_then(Value::as_str).unwrap_or("");
            let version = SUPPORTED_VERSIONS.iter().find(|v| **v == requested).unwrap_or(&SUPPORTED_VERSIONS[0]);
            rpc_result(id, json!({
                "protocolVersion": version,
                "capabilities": {"tools": {"listChanged": false}},
                "serverInfo": {"name": "kubeswift-spin-mcp-example", "version": env!("CARGO_PKG_VERSION")},
                "instructions": "Example tools running in Spin inside a KubeSwift sandbox."
            }))
        }
        "ping" => rpc_result(id, json!({})),
        "tools/list" => rpc_result(id, json!({"tools": tools()})),
        "tools/call" => match call_tool(&params) {
            Ok(r) => rpc_result(id, r),
            Err((code, message)) => rpc_error(id, code, message),
        },
        _ => rpc_error(id, -32601, "method not found"),
    })
}

/// Rejects browser requests from origins that are not explicitly allowed,
/// the DNS-rebinding protection the transport specification requires.
pub fn origin_allowed(origin: Option<&str>, allowed: &str) -> bool {
    match origin {
        None => true,
        Some(o) => allowed.split(',').map(str::trim).any(|a| !a.is_empty() && a == o),
    }
}

#[http_service]
async fn handle(req: Request) -> anyhow::Result<impl IntoResponse> {
    match req.uri().path() {
        "/healthz" => return reply(StatusCode::OK, None),
        "/mcp" => {}
        _ => return reply(StatusCode::NOT_FOUND, None),
    }
    let allowed = variables::get("allowed_origins").await.unwrap_or_default();
    let origin = req.headers().get("origin").and_then(|v| v.to_str().ok()).map(str::to_string);
    if !origin_allowed(origin.as_deref(), &allowed) {
        return reply(StatusCode::FORBIDDEN, None);
    }
    if req.method() != Method::POST {
        // This server opens no server-to-client streams.
        return reply(StatusCode::METHOD_NOT_ALLOWED, None);
    }
    let body = req.into_body().bytes().await.map_err(|e| anyhow::anyhow!("read body: {e:?}"))?;
    if body.len() > MAX_BODY {
        return reply(StatusCode::PAYLOAD_TOO_LARGE, None);
    }
    let msg: Value = match serde_json::from_slice(&body) {
        Ok(v) => v,
        Err(_) => return reply(StatusCode::BAD_REQUEST, Some(rpc_error(&Value::Null, -32700, "parse error"))),
    };
    if !msg.is_object() {
        return reply(StatusCode::BAD_REQUEST, Some(rpc_error(&Value::Null, -32600, "invalid request")));
    }
    match handle_message(&msg) {
        Some(resp) => reply(StatusCode::OK, Some(resp)),
        None => reply(StatusCode::ACCEPTED, None),
    }
}
