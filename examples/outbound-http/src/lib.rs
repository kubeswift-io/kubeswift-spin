//! outbound-http makes one controlled outbound request and reports the
//! result. It is used to observe the egress behavior of KubeSwift sandbox
//! network modes.

use serde::Serialize;
use spin_sdk::http::{HeaderMap, IntoResponse, Json, Request, StatusCode};
use spin_sdk::{http_service, variables};

#[derive(Serialize)]
struct Outcome {
    target: String,
    ok: bool,
    status: Option<u16>,
    error: Option<String>,
}

#[http_service]
async fn handle(req: Request) -> anyhow::Result<impl IntoResponse> {
    match req.uri().path() {
        "/healthz" => Ok((StatusCode::OK, HeaderMap::new(), "ok\n".to_string()).into_response()?),
        "/fetch" => {
            let target = variables::get("target_url").await?;
            let outcome = match spin_sdk::http::get(&target).await {
                Ok(resp) => Outcome {
                    target,
                    ok: resp.status().is_success(),
                    status: Some(resp.status().as_u16()),
                    error: None,
                },
                Err(e) => Outcome {
                    target,
                    ok: false,
                    status: None,
                    error: Some(e.to_string()),
                },
            };
            let code = if outcome.ok { StatusCode::OK } else { StatusCode::BAD_GATEWAY };
            let resp = Json(outcome).into_response()?;
            resp.set_status_code(code.as_u16())
                .map_err(|_| anyhow::anyhow!("invalid status code"))?;
            Ok(resp)
        }
        _ => Ok((StatusCode::NOT_FOUND, HeaderMap::new(), "not found\n".to_string()).into_response()?),
    }
}
