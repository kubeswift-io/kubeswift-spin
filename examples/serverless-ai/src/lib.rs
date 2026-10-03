//! serverless-ai sends a prompt to an inference endpoint through Spin's LLM
//! API. Spin's runtime configuration decides where inference runs: with
//! `llm_compute` type "remote_http" and api_type "open_ai", Spin forwards the
//! request to any OpenAI-compatible server (vLLM, LocalAI, llama.cpp). The
//! application never sees the endpoint URL or its credentials.
//!
//!   POST /ask   body: the prompt (plain text, at most 8 KiB)

use serde::Serialize;
use spin_sdk::http::body::IncomingBodyExt;
use spin_sdk::http::{HeaderMap, IntoResponse, Json, Method, Request, StatusCode};
use spin_sdk::llm::{self, InferencingModel, InferencingParams};
use spin_sdk::wasip3::http::types::Response as WasiResponse;
use spin_sdk::{http_service, variables};

const MAX_PROMPT: usize = 8 * 1024;

#[derive(Serialize)]
struct Answer {
    model: String,
    text: String,
    prompt_tokens: u32,
    generated_tokens: u32,
}

fn text(status: StatusCode, body: &str) -> anyhow::Result<WasiResponse> {
    Ok((status, HeaderMap::new(), body.to_string()).into_response()?)
}

#[http_service]
async fn handle(req: Request) -> anyhow::Result<impl IntoResponse> {
    match (req.method().clone(), req.uri().path()) {
        (_, "/healthz") => text(StatusCode::OK, "ok\n"),
        (Method::POST, "/ask") => {
            let body = req.into_body().bytes().await.map_err(|e| anyhow::anyhow!("read body: {e:?}"))?;
            if body.is_empty() || body.len() > MAX_PROMPT {
                return text(StatusCode::BAD_REQUEST, "prompt must be between 1 byte and 8 KiB\n");
            }
            let prompt = String::from_utf8_lossy(&body);
            // The model name must be listed in ai_models in spin.toml. The
            // inference server maps it to a real model (served-model-name
            // in vLLM, a model alias in LocalAI or llama.cpp).
            let model = variables::get("llm_model").await?;
            let params = InferencingParams { max_tokens: 256, ..Default::default() };
            match llm::infer_with_options(InferencingModel::Other(&model), &prompt, params) {
                Ok(r) => Ok(Json(Answer {
                    model,
                    text: r.text,
                    prompt_tokens: r.usage.prompt_token_count,
                    generated_tokens: r.usage.generated_token_count,
                })
                .into_response()?),
                Err(e) => text(StatusCode::BAD_GATEWAY, &format!("inference failed: {e:?}\n")),
            }
        }
        _ => text(StatusCode::NOT_FOUND, "not found\n"),
    }
}
